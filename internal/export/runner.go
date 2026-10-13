// Package export runs one export pass: select ready documents in ID order,
// write them into batch files in parallel, and publish the files in order.
package export

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"runtime/debug"
	"sync"
	"time"

	"autodump/internal/alert"
	"autodump/internal/batch"
	"autodump/internal/dump"
	"autodump/internal/output"
	"autodump/internal/source"
)

// Index finds exportable documents; *source.Index implements it.
type Index interface {
	Scan(ctx context.Context, from uint64, collect bool) ([]uint64, *source.Blocker, error)
}

// Docs reads document tables; *source.Docs implements it.
type Docs interface {
	ShowCreate(ctx context.Context, id uint64) (string, error)
	StreamRows(ctx context.Context, id uint64, fn source.RowFunc) error
}

// Store is where files go; *output.Dir implements it.
type Store interface {
	output.Sink
	Watermark() (uint64, error)
	CleanPartials() (int, error)
}

// Config holds the tuning knobs of a run.
type Config struct {
	StartID        uint64
	BatchSize      int
	MergeThreshold int
	Workers        int
	StallHours     int
	WriteBufferMB  int
	RetryAttempts  int
	RetryBase      time.Duration
	Dump           dump.Options
	Limit          int // export at most this many documents (benchmark); 0 = all
}

// Summary describes a finished run.
type Summary struct {
	From     uint64
	Docs     int
	Files    int
	Rows     int64
	Bytes    int64
	Blocker  *source.Blocker
	Duration time.Duration
}

// Runner exports documents. One Runner lives for the whole process.
type Runner struct {
	cfg    Config
	idx    Index
	docs   Docs
	store  Store
	notify alert.Notifier
	log    *log.Logger

	stallMu   sync.Mutex
	lastStall uint64 // document with an open stall alert, 0 if none
}

// New creates a Runner.
func New(cfg Config, idx Index, docs Docs, store Store, n alert.Notifier, l *log.Logger) *Runner {
	if cfg.Workers < 1 {
		cfg.Workers = 1
	}
	if cfg.RetryAttempts < 1 {
		cfg.RetryAttempts = 1
	}
	return &Runner{cfg: cfg, idx: idx, docs: docs, store: store, notify: n, log: l}
}

// SetLimit caps how many documents the next runs export (0 = no cap).
// Used by benchmarks.
func (r *Runner) SetLimit(n int) { r.cfg.Limit = n }

// Run exports every ready document after the watermark, up to the first one
// that is not ready yet.
func (r *Runner) Run(ctx context.Context) (Summary, error) {
	start := time.Now()
	sum := Summary{}

	if n, err := r.store.CleanPartials(); err != nil {
		return sum, fmt.Errorf("clean partial files: %w", err)
	} else if n > 0 {
		r.log.Printf("removed %d unfinished file(s) from a previous run", n)
	}
	from, err := r.from()
	if err != nil {
		return sum, err
	}
	sum.From = from

	var ids []uint64
	var blocker *source.Blocker
	err = r.retry(ctx, "scan index", func() error {
		var e error
		ids, blocker, e = r.idx.Scan(ctx, from, true)
		return e
	})
	if err != nil {
		return sum, err
	}
	sum.Blocker = blocker
	r.checkStall(blocker)

	if r.cfg.Limit > 0 && len(ids) > r.cfg.Limit {
		ids = ids[:r.cfg.Limit]
	}
	sum.Docs = len(ids)
	r.log.Printf("run: from id %d, %d document(s) ready, blocker %s", from, len(ids), describe(blocker))

	batches := batch.Split(ids, r.cfg.BatchSize, r.cfg.MergeThreshold)
	files, rows, bytes, err := r.exportAll(ctx, batches)
	sum.Files, sum.Rows, sum.Bytes = files, rows, bytes
	sum.Duration = time.Since(start)
	return sum, err
}

// StallCheck only looks for a stuck document; it is cheap and safe to run
// while an export is in progress.
func (r *Runner) StallCheck(ctx context.Context) error {
	from, err := r.from()
	if err != nil {
		return err
	}
	var blocker *source.Blocker
	err = r.retry(ctx, "stall check", func() error {
		var e error
		_, blocker, e = r.idx.Scan(ctx, from, false)
		return e
	})
	if err != nil {
		return err
	}
	r.checkStall(blocker)
	return nil
}

func (r *Runner) from() (uint64, error) {
	w, err := r.store.Watermark()
	if err != nil {
		return 0, fmt.Errorf("read export position: %w", err)
	}
	return max(w+1, r.cfg.StartID), nil
}

func (r *Runner) checkStall(b *source.Blocker) {
	r.stallMu.Lock()
	defer r.stallMu.Unlock()

	if r.lastStall != 0 && (b == nil || b.ID != r.lastStall) {
		r.notify.StallResolved(r.lastStall, fmt.Sprintf("Document %d is ready now, export continues.", r.lastStall))
		r.lastStall = 0
	}
	if b != nil && r.cfg.StallHours > 0 && b.AgeHours >= int64(r.cfg.StallHours) {
		r.notify.Stall(b.ID, fmt.Sprintf(
			"Document %d has not been ready for %d h (limit %d h). Export waits for it; all later documents are held back.",
			b.ID, b.AgeHours, r.cfg.StallHours))
		r.lastStall = b.ID
	}
}

type job struct {
	seq  int
	ids  []uint64
	name string
}

type result struct {
	job
	rows, bytes int64
	err         error
}

// exportAll writes batches with a worker pool and publishes them strictly in
// order. On the first error nothing after the failed batch is published.
func (r *Runner) exportAll(ctx context.Context, batches [][]uint64) (files int, rows, bytes int64, err error) {
	if len(batches) == 0 {
		return 0, 0, 0, nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	jobs := make(chan job)
	results := make(chan result)
	slots := make(chan struct{}, 2*r.cfg.Workers) // bounds finished-but-unpublished files

	go func() {
		defer close(jobs)
		for i, ids := range batches {
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				return
			}
			j := job{seq: i, ids: ids, name: output.FileName(ids[0], ids[len(ids)-1])}
			select {
			case jobs <- j:
			case <-ctx.Done():
				return
			}
		}
	}()

	var wg sync.WaitGroup
	for w := 0; w < r.cfg.Workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				res := result{job: j}
				res.rows, res.bytes, res.err = r.writeFileRetry(ctx, j)
				results <- res
			}
		}()
	}
	go func() { wg.Wait(); close(results) }()

	pending := map[int]result{}
	next := 0
	for res := range results {
		if res.err != nil {
			if err == nil {
				err = fmt.Errorf("file %s: %w", res.name, res.err)
				cancel()
			}
			continue
		}
		pending[res.seq] = res
		for err == nil {
			p, ok := pending[next]
			if !ok {
				break
			}
			delete(pending, next)
			if cerr := r.store.Commit(p.name, p.ids[len(p.ids)-1]); cerr != nil {
				err = fmt.Errorf("publish %s: %w", p.name, cerr)
				cancel()
				_ = r.store.Abort(p.name)
				break
			}
			files++
			rows += p.rows
			bytes += p.bytes
			next++
			<-slots
			r.log.Printf("published %s: %d document(s), %d row(s), %d byte(s)", p.name, len(p.ids), p.rows, p.bytes)
		}
	}
	// Anything finished but not published after a failure is thrown away.
	for _, p := range pending {
		_ = r.store.Abort(p.name)
	}
	return files, rows, bytes, err
}

// writeFileRetry writes one batch file, starting the file over on transient
// database errors (a partly written table can't be resumed).
func (r *Runner) writeFileRetry(ctx context.Context, j job) (rows, bytes int64, err error) {
	delay := r.cfg.RetryBase
	for attempt := 1; ; attempt++ {
		rows, bytes, err = r.writeFile(ctx, j)
		if err == nil || !source.IsTransient(err) || attempt >= r.cfg.RetryAttempts || ctx.Err() != nil {
			return rows, bytes, err
		}
		r.log.Printf("WARN %s: attempt %d failed, retrying in %s: %v", j.name, attempt, delay, err)
		if !sleep(ctx, delay) {
			return rows, bytes, ctx.Err()
		}
		delay = min(delay*2, 5*time.Minute)
	}
}

func (r *Runner) writeFile(ctx context.Context, j job) (rows, bytes int64, err error) {
	f, err := r.store.Begin(j.name)
	if err != nil {
		return 0, 0, err
	}
	closed := false
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v\n%s", p, debug.Stack())
		}
		if err != nil {
			if !closed {
				_ = f.Close()
			}
			_ = r.store.Abort(j.name)
		}
	}()

	cw := &countingWriter{w: f}
	bw := bufio.NewWriterSize(cw, max(r.cfg.WriteBufferMB, 1)<<20)
	if err = dump.WriteFileHeader(bw); err != nil {
		return 0, 0, err
	}
	for _, id := range j.ids {
		if err = ctx.Err(); err != nil {
			return 0, 0, err
		}
		ddl, e := r.docs.ShowCreate(ctx, id)
		if e != nil {
			return 0, 0, fmt.Errorf("document %d: %w", id, e)
		}
		n, e := dump.WriteTable(bw, source.TableName(id), ddl, func(fn source.RowFunc) error {
			return r.docs.StreamRows(ctx, id, fn)
		}, r.cfg.Dump)
		if e != nil {
			return 0, 0, fmt.Errorf("document %d: %w", id, e)
		}
		rows += n
	}
	if err = dump.WriteFileFooter(bw, len(j.ids), rows); err != nil {
		return 0, 0, err
	}
	if err = bw.Flush(); err != nil {
		return 0, 0, err
	}
	closed = true
	if err = f.Close(); err != nil {
		return 0, 0, err
	}
	return rows, cw.n, nil
}

func (r *Runner) retry(ctx context.Context, what string, fn func() error) error {
	delay := r.cfg.RetryBase
	for attempt := 1; ; attempt++ {
		err := fn()
		if err == nil || !source.IsTransient(err) || attempt >= r.cfg.RetryAttempts || ctx.Err() != nil {
			return err
		}
		r.log.Printf("WARN %s: attempt %d failed, retrying in %s: %v", what, attempt, delay, err)
		if !sleep(ctx, delay) {
			return ctx.Err()
		}
		delay = min(delay*2, 5*time.Minute)
	}
}

// AlertKey classifies an error for alert deduplication.
func AlertKey(err error) string {
	var pathErr *fs.PathError
	switch {
	case errors.Is(err, source.ErrNoSuchTable):
		return "missing_table"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case source.IsTransient(err):
		return "db"
	case errors.As(err, &pathErr):
		return "io"
	default:
		return "run"
	}
}

func describe(b *source.Blocker) string {
	if b == nil {
		return "none"
	}
	return fmt.Sprintf("document %d (%d h old)", b.ID, b.AgeHours)
}

func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}
