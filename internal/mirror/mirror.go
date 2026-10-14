// Package mirror keeps the third server in sync with the last RetentionDays of
// exported documents: new document tables are imported, old ones dropped, and
// batch files are copied to (and expired from) a shared folder.
package mirror

import (
	"context"
	"fmt"
	"log"
	"slices"
	"sync"
	"time"

	"autodump/internal/dump"
	"autodump/internal/source"
)

// Index answers age questions; *source.Index implements it.
type Index interface {
	Cutoff(ctx context.Context, days int) (string, error)
	YoungIDs(ctx context.Context, cutoff string, from, to uint64) ([]uint64, error)
	OldIDs(ctx context.Context, cutoff string, ids []uint64) ([]uint64, error)
	RangeAges(ctx context.Context, cutoff string, first, last uint64) (old, notOld int64, err error)
}

// Docs reads document tables; *source.Docs implements it.
type Docs interface {
	ShowCreate(ctx context.Context, id uint64) (string, error)
	StreamRows(ctx context.Context, id uint64, fn source.RowFunc) error
}

// Target is the mirror database; *DBTarget implements it.
type Target interface {
	// Tables lists the complete document tables and leftover staging tables.
	Tables(ctx context.Context) (done, staging []uint64, err error)
	Drop(ctx context.Context, table string) error
	Import(ctx context.Context, id uint64, ddl string, stream func(source.RowFunc) error, opts dump.Options) (int64, error)
}

// Config holds the settings of a sync.
type Config struct {
	StartID       uint64
	RetentionDays int
	FilesDir      string // "" = don't mirror files
	OutputDir     string // where the exported files are
	Workers       int
	RetryAttempts int
	RetryBase     time.Duration
	Dump          dump.Options
}

// Summary describes a finished sync.
type Summary struct {
	Cutoff                    string
	Kept, Added, Dropped      int
	FilesCopied, FilesDeleted int
	Rows                      int64
	Duration                  time.Duration
}

// Syncer syncs the mirror. watermark returns the last exported document id.
type Syncer struct {
	cfg       Config
	idx       Index
	docs      Docs
	target    Target
	watermark func() (uint64, error)
	log       *log.Logger
}

// New creates a Syncer.
func New(cfg Config, idx Index, docs Docs, target Target, watermark func() (uint64, error), l *log.Logger) *Syncer {
	cfg.Workers = max(cfg.Workers, 1)
	cfg.RetryAttempts = max(cfg.RetryAttempts, 1)
	return &Syncer{cfg: cfg, idx: idx, docs: docs, target: target, watermark: watermark, log: l}
}

// Run brings the mirror to "every exported document created within the last
// RetentionDays". Only documents the index proves old are removed; a document
// of unknown age stays.
func (s *Syncer) Run(ctx context.Context) (sum Summary, err error) {
	start := time.Now()
	defer func() { sum.Duration = time.Since(start) }()

	w, err := s.watermark()
	if err != nil {
		return sum, fmt.Errorf("read export position: %w", err)
	}
	var young []uint64
	err = s.retry(ctx, "read index", func() error {
		c, e := s.idx.Cutoff(ctx, s.cfg.RetentionDays)
		if e != nil {
			return e
		}
		sum.Cutoff = c
		young = nil
		if w > 0 && w >= s.cfg.StartID {
			young, e = s.idx.YoungIDs(ctx, c, s.cfg.StartID, w)
		}
		return e
	})
	if err != nil {
		return sum, err
	}

	var have, staging []uint64
	if err = s.retry(ctx, "list mirror tables", func() (e error) {
		have, staging, e = s.target.Tables(ctx)
		return e
	}); err != nil {
		return sum, err
	}
	for _, id := range staging {
		if err = s.target.Drop(ctx, StagingName(id)); err != nil {
			return sum, fmt.Errorf("drop unfinished %s: %w", StagingName(id), err)
		}
	}

	// Drop first: it frees space for the new tables.
	var old []uint64
	if extra := minus(have, young); len(extra) > 0 {
		if err = s.retry(ctx, "read index", func() (e error) {
			old, e = s.idx.OldIDs(ctx, sum.Cutoff, extra)
			return e
		}); err != nil {
			return sum, err
		}
	}
	for _, id := range old {
		if err = s.target.Drop(ctx, source.TableName(id)); err != nil {
			return sum, fmt.Errorf("drop %s: %w", source.TableName(id), err)
		}
		sum.Dropped++
	}

	add := minus(young, have)
	sum.Kept = len(young) - len(add)
	s.log.Printf("mirror: cutoff %s, %d document(s) in window, %d to add, %d dropped",
		sum.Cutoff, len(young), len(add), sum.Dropped)
	sum.Added, sum.Rows, err = s.importAll(ctx, add)
	if err != nil {
		return sum, err
	}

	if s.cfg.FilesDir != "" {
		sum.FilesCopied, sum.FilesDeleted, err = s.syncFiles(ctx, sum.Cutoff, young)
	}
	return sum, err
}

// importAll imports ids with a worker pool and stops at the first error.
func (s *Syncer) importAll(ctx context.Context, ids []uint64) (added int, rows int64, err error) {
	if len(ids) == 0 {
		return 0, 0, nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		mu   sync.Mutex
		wg   sync.WaitGroup
		next int
	)
	for range min(s.cfg.Workers, len(ids)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				if next >= len(ids) || err != nil {
					mu.Unlock()
					return
				}
				id := ids[next]
				next++
				mu.Unlock()

				n, e := s.importOne(ctx, id)

				mu.Lock()
				if e != nil {
					if err == nil {
						err = fmt.Errorf("mirror %s: %w", source.TableName(id), e)
						cancel()
					}
				} else {
					added++
					rows += n
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return added, rows, err
}

func (s *Syncer) importOne(ctx context.Context, id uint64) (rows int64, err error) {
	err = s.retry(ctx, "mirror "+source.TableName(id), func() error {
		ddl, e := s.docs.ShowCreate(ctx, id)
		if e != nil {
			return e
		}
		rows, e = s.target.Import(ctx, id, ddl, func(fn source.RowFunc) error {
			return s.docs.StreamRows(ctx, id, fn)
		}, s.cfg.Dump)
		return e
	})
	return rows, err
}

func (s *Syncer) retry(ctx context.Context, what string, fn func() error) error {
	delay := s.cfg.RetryBase
	for attempt := 1; ; attempt++ {
		err := fn()
		if err == nil || !source.IsTransient(err) || attempt >= s.cfg.RetryAttempts || ctx.Err() != nil {
			return err
		}
		s.log.Printf("WARN %s: attempt %d failed, retrying in %s: %v", what, attempt, delay, err)
		t := time.NewTimer(delay)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		}
		delay = min(delay*2, 5*time.Minute)
	}
}

// minus returns the elements of a not in b, sorted.
func minus(a, b []uint64) []uint64 {
	in := make(map[uint64]struct{}, len(b))
	for _, v := range b {
		in[v] = struct{}{}
	}
	var out []uint64
	for _, v := range a {
		if _, ok := in[v]; !ok {
			out = append(out, v)
		}
	}
	slices.Sort(out)
	return out
}
