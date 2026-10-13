package export

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"autodump/internal/dump"
	"autodump/internal/output"
	"autodump/internal/source"
)

type fakeIndex struct {
	mu      sync.Mutex
	ids     []uint64
	blocker *source.Blocker
	froms   []uint64
}

func (f *fakeIndex) Scan(_ context.Context, from uint64, collect bool) ([]uint64, *source.Blocker, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.froms = append(f.froms, from)
	var out []uint64
	for _, id := range f.ids {
		if id >= from && (f.blocker == nil || id < f.blocker.ID) && collect {
			out = append(out, id)
		}
	}
	return out, f.blocker, nil
}

type fakeDocs struct {
	mu       sync.Mutex
	slow     map[uint64]time.Duration
	fail     map[uint64]error
	failOnce map[uint64]error
}

func (f *fakeDocs) ShowCreate(_ context.Context, id uint64) (string, error) {
	f.mu.Lock()
	d := f.slow[id]
	err := f.fail[id]
	if e, ok := f.failOnce[id]; ok {
		err = e
		delete(f.failOnce, id)
	}
	f.mu.Unlock()
	if d > 0 {
		time.Sleep(d)
	}
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("CREATE TABLE `document_%d` (`id` int)", id), nil
}

func (f *fakeDocs) StreamRows(context.Context, uint64, source.RowFunc) error { return nil }

type fakeNotifier struct {
	mu       sync.Mutex
	stalls   []uint64
	resolved []uint64
}

func (n *fakeNotifier) Info(string)            {}
func (n *fakeNotifier) Failure(string, string) {}
func (n *fakeNotifier) Stall(id uint64, _ string) {
	n.mu.Lock()
	n.stalls = append(n.stalls, id)
	n.mu.Unlock()
}
func (n *fakeNotifier) StallResolved(id uint64, _ string) {
	n.mu.Lock()
	n.resolved = append(n.resolved, id)
	n.mu.Unlock()
}

func seq(from, to uint64) []uint64 {
	var out []uint64
	for i := from; i <= to; i++ {
		out = append(out, i)
	}
	return out
}

func newRunner(t *testing.T, idx Index, docs Docs, workers int) (*Runner, string, *fakeNotifier) {
	t.Helper()
	dir := t.TempDir()
	n := &fakeNotifier{}
	cfg := Config{
		BatchSize: 100, MergeThreshold: 150, Workers: workers, StallHours: 24,
		WriteBufferMB: 1, RetryAttempts: 3, RetryBase: 0,
		Dump: dump.Options{InsertBatchRows: 1000, InsertBatchBytes: 1 << 20},
	}
	return New(cfg, idx, docs, output.NewDir(dir), n, log.New(io.Discard, "", 0)), dir, n
}

func listDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

func TestRunPublishesInOrderWithParallelWorkers(t *testing.T) {
	idx := &fakeIndex{ids: seq(1, 450)}
	slow := map[uint64]time.Duration{}
	for _, id := range seq(1, 100) {
		slow[id] = time.Millisecond // first file finishes last
	}
	r, dir, _ := newRunner(t, idx, &fakeDocs{slow: slow}, 4)

	sum, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"1-100.sql", "101-200.sql", "201-300.sql", "301-450.sql", "last_id.txt"}
	if got := listDir(t, dir); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("files = %v, want %v", got, want)
	}
	if sum.Docs != 450 || sum.Files != 4 || sum.Bytes == 0 {
		t.Fatalf("summary = %+v", sum)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "301-450.sql"))
	if !strings.Contains(string(b), "CREATE TABLE `document_450`") || !strings.Contains(string(b), "150 tables") {
		t.Fatalf("last file content unexpected:\n%.300s", b)
	}

	// The next run resumes after the watermark.
	idx.ids = seq(1, 451)
	if _, err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := idx.froms[len(idx.froms)-1]; got != 451 {
		t.Fatalf("second run scanned from %d, want 451", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "451-451.sql")); err != nil {
		t.Fatal(err)
	}
}

func TestRunStopsPublishingAtFirstFailure(t *testing.T) {
	for _, workers := range []int{1, 4} {
		t.Run(fmt.Sprintf("workers=%d", workers), func(t *testing.T) {
			idx := &fakeIndex{ids: seq(1, 600)}
			docs := &fakeDocs{fail: map[uint64]error{250: errors.New("boom")}}
			r, dir, _ := newRunner(t, idx, docs, workers)

			_, err := r.Run(context.Background())
			if err == nil || !strings.Contains(err.Error(), "document 250") {
				t.Fatalf("err = %v", err)
			}
			got := listDir(t, dir)
			for _, name := range got {
				if strings.HasSuffix(name, output.PartialSuffix) {
					t.Fatalf("partial left behind: %v", got)
				}
				switch name {
				case "201-300.sql", "301-400.sql", "401-500.sql", "501-600.sql":
					t.Fatalf("published past the failure: %v", got)
				}
			}
			if workers == 1 && strings.Join(got, ",") != "1-100.sql,101-200.sql,last_id.txt" {
				t.Fatalf("files = %v", got)
			}
			w, err := output.NewDir(dir).Watermark()
			if err != nil {
				t.Fatal(err)
			}
			if w > 200 {
				t.Fatalf("watermark %d past failure", w)
			}
		})
	}
}

func TestRunRetriesTransientErrors(t *testing.T) {
	idx := &fakeIndex{ids: seq(1, 5)}
	docs := &fakeDocs{failOnce: map[uint64]error{3: driver.ErrBadConn}}
	r, dir, _ := newRunner(t, idx, docs, 1)
	if _, err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := listDir(t, dir); strings.Join(got, ",") != "1-5.sql,last_id.txt" {
		t.Fatalf("files = %v", got)
	}
}

func TestRunStopsAtBlockerAndAlertsStall(t *testing.T) {
	idx := &fakeIndex{ids: seq(90, 110), blocker: &source.Blocker{ID: 98, AgeHours: 30}}
	r, dir, n := newRunner(t, idx, &fakeDocs{}, 2)

	sum, err := r.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Docs != 8 {
		t.Fatalf("exported %d docs, want 8 (90..97)", sum.Docs)
	}
	if got := listDir(t, dir); strings.Join(got, ",") != "90-97.sql,last_id.txt" {
		t.Fatalf("files = %v", got)
	}
	if err := r.StallCheck(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The runner reports on every check; the real notifier dedupes.
	if len(n.stalls) != 2 || n.stalls[0] != 98 {
		t.Fatalf("stalls = %v", n.stalls)
	}

	idx.blocker = nil
	if _, err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(n.resolved) != 1 || n.resolved[0] != 98 {
		t.Fatalf("resolved = %v", n.resolved)
	}
	if _, err := os.Stat(filepath.Join(dir, "98-110.sql")); err != nil {
		t.Fatal(err)
	}
}

func TestYoungBlockerIsNotAStall(t *testing.T) {
	idx := &fakeIndex{ids: seq(1, 3), blocker: &source.Blocker{ID: 2, AgeHours: 5}}
	r, _, n := newRunner(t, idx, &fakeDocs{}, 1)
	if _, err := r.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(n.stalls) != 0 {
		t.Fatalf("stalls = %v", n.stalls)
	}
}

func TestNothingReadyIsANoop(t *testing.T) {
	r, dir, _ := newRunner(t, &fakeIndex{}, &fakeDocs{}, 2)
	sum, err := r.Run(context.Background())
	if err != nil || sum.Files != 0 {
		t.Fatalf("sum=%+v err=%v", sum, err)
	}
	if got := listDir(t, dir); len(got) != 0 {
		t.Fatalf("files = %v", got)
	}
}

func TestAlertKey(t *testing.T) {
	cases := map[string]error{
		"missing_table": fmt.Errorf("x: %w", source.ErrNoSuchTable),
		"db":            fmt.Errorf("x: %w", driver.ErrBadConn),
		"io":            &os.PathError{Op: "write", Path: "f", Err: errors.New("disk full")},
		"canceled":      context.Canceled,
		"run":           errors.New("other"),
	}
	for want, err := range cases {
		if got := AlertKey(err); got != want {
			t.Errorf("AlertKey(%v) = %q, want %q", err, got, want)
		}
	}
}
