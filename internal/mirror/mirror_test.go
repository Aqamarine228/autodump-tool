package mirror

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/go-sql-driver/mysql"

	"autodump/internal/dump"
	"autodump/internal/source"
)

// fakeIndex: age[id] is "young", "old" or missing (unknown).
type fakeIndex struct{ age map[uint64]string }

func (f *fakeIndex) Cutoff(context.Context, int) (string, error) { return "2026-09-13 02:00:00", nil }

func (f *fakeIndex) YoungIDs(_ context.Context, _ string, from, to uint64) ([]uint64, error) {
	var out []uint64
	for id, a := range f.age {
		if a == "young" && id >= from && id <= to {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out, nil
}

func (f *fakeIndex) OldIDs(_ context.Context, _ string, ids []uint64) ([]uint64, error) {
	var out []uint64
	for _, id := range ids {
		if f.age[id] == "old" {
			out = append(out, id)
		}
	}
	return out, nil
}

func (f *fakeIndex) RangeAges(_ context.Context, _ string, first, last uint64) (old, notOld int64, err error) {
	for id, a := range f.age {
		if id >= first && id <= last {
			if a == "old" {
				old++
			} else {
				notOld++
			}
		}
	}
	return old, notOld, nil
}

type fakeDocs struct{}

func (fakeDocs) ShowCreate(context.Context, uint64) (string, error)       { return "CREATE TABLE x", nil }
func (fakeDocs) StreamRows(context.Context, uint64, source.RowFunc) error { return nil }

type fakeTarget struct {
	mu         sync.Mutex
	tables     map[uint64]bool
	staging    []uint64
	dropped    []string
	imported   []uint64
	failOnce   map[uint64]error
	failAlways map[uint64]error
}

func (t *fakeTarget) Tables(context.Context) ([]uint64, []uint64, error) {
	var done []uint64
	for id := range t.tables {
		done = append(done, id)
	}
	slices.Sort(done)
	return done, t.staging, nil
}

func (t *fakeTarget) Drop(_ context.Context, table string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.dropped = append(t.dropped, table)
	return nil
}

func (t *fakeTarget) Import(_ context.Context, id uint64, _ string, _ func(source.RowFunc) error, _ dump.Options) (int64, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if e, ok := t.failOnce[id]; ok {
		delete(t.failOnce, id)
		return 0, e
	}
	if e := t.failAlways[id]; e != nil {
		return 0, e
	}
	t.imported = append(t.imported, id)
	return 10, nil
}

func newSyncer(idx Index, tg Target, watermark uint64, cfg Config) *Syncer {
	cfg.Workers, cfg.RetryAttempts = 3, 3
	return New(cfg, idx, fakeDocs{}, tg, func() (uint64, error) { return watermark, nil }, log.New(io.Discard, "", 0))
}

func TestRunAddsYoungDropsOldKeepsUnknown(t *testing.T) {
	idx := &fakeIndex{age: map[uint64]string{
		1: "old", 2: "old", 3: "young", 4: "young", 5: "young", 6: "young", // 6 is not exported yet
	}}
	// 1 old -> dropped, 3 young -> kept, 99 unknown -> kept; 7 is a leftover staging table.
	tg := &fakeTarget{tables: map[uint64]bool{1: true, 3: true, 99: true}, staging: []uint64{7}}
	sum, err := newSyncer(idx, tg, 5, Config{}).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(tg.imported)
	if !slices.Equal(tg.imported, []uint64{4, 5}) {
		t.Fatalf("imported %v", tg.imported)
	}
	if !slices.Equal(tg.dropped, []string{"document_7__part", "document_1"}) {
		t.Fatalf("dropped %v", tg.dropped)
	}
	if sum.Added != 2 || sum.Dropped != 1 || sum.Kept != 1 || sum.Rows != 20 {
		t.Fatalf("summary %+v", sum)
	}
}

func TestRunRespectsStartID(t *testing.T) {
	idx := &fakeIndex{age: map[uint64]string{1: "young", 2: "young", 3: "young"}}
	tg := &fakeTarget{tables: map[uint64]bool{}}
	if _, err := newSyncer(idx, tg, 3, Config{StartID: 2}).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	slices.Sort(tg.imported)
	if !slices.Equal(tg.imported, []uint64{2, 3}) {
		t.Fatalf("imported %v", tg.imported)
	}
}

func TestRunNothingExported(t *testing.T) {
	idx := &fakeIndex{age: map[uint64]string{1: "young"}}
	tg := &fakeTarget{tables: map[uint64]bool{}}
	if _, err := newSyncer(idx, tg, 0, Config{}).Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(tg.imported) != 0 {
		t.Fatalf("imported %v", tg.imported)
	}
}

func TestImportRetriesTransientAndStopsOnError(t *testing.T) {
	idx := &fakeIndex{age: map[uint64]string{1: "young", 2: "young"}}
	tg := &fakeTarget{tables: map[uint64]bool{}, failOnce: map[uint64]error{1: &mysql.MySQLError{Number: 2013}}}
	if _, err := newSyncer(idx, tg, 2, Config{}).Run(context.Background()); err != nil {
		t.Fatalf("transient error must be retried: %v", err)
	}

	boom := errors.New("boom")
	tg = &fakeTarget{tables: map[uint64]bool{}, failAlways: map[uint64]error{2: boom}}
	_, err := newSyncer(idx, tg, 2, Config{}).Run(context.Background())
	if !errors.Is(err, boom) {
		t.Fatalf("want boom, got %v", err)
	}
}

func TestSyncFiles(t *testing.T) {
	out, share := t.TempDir(), t.TempDir()
	write := func(dir, name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(out, "1-3.sql", "old")        // all old: not copied
	write(out, "4-6.sql", "mixed")      // 5 young: copied
	write(out, "7-9.sql", "young")      // already there, same size: skipped
	write(out, "last_id.txt", "9\n")    // not a dump file
	write(share, "7-9.sql", "young")    // up to date
	write(share, "1-3.sql", "old")      // all old: deleted
	write(share, "10-12.sql", "?")      // unknown age: kept
	write(share, "4-6.sql.partial", "") // leftover: removed

	idx := &fakeIndex{age: map[uint64]string{1: "old", 2: "old", 3: "old", 4: "old", 5: "young", 6: "old", 7: "young", 8: "young", 9: "young"}}
	tg := &fakeTarget{tables: map[uint64]bool{}}
	sum, err := newSyncer(idx, tg, 9, Config{OutputDir: out, FilesDir: share}).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.FilesCopied != 1 || sum.FilesDeleted != 1 {
		t.Fatalf("summary %+v", sum)
	}
	entries, _ := os.ReadDir(share)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if !slices.Equal(names, []string{"10-12.sql", "4-6.sql", "7-9.sql"}) {
		t.Fatalf("share has %v", names)
	}
	if b, _ := os.ReadFile(filepath.Join(share, "4-6.sql")); string(b) != "mixed" {
		t.Fatalf("copied content %q", b)
	}
}

func TestHasAny(t *testing.T) {
	ids := []uint64{5, 10, 20}
	for _, c := range []struct {
		a, b uint64
		want bool
	}{{1, 4, false}, {1, 5, true}, {6, 9, false}, {10, 10, true}, {21, 30, false}, {11, 25, true}} {
		if got := hasAny(ids, c.a, c.b); got != c.want {
			t.Errorf("hasAny(%d,%d)=%v", c.a, c.b, got)
		}
	}
}

func TestDropRefusesOtherTables(t *testing.T) {
	tg := &DBTarget{}
	for _, name := range []string{"parsing_documents", "document_1; DROP DATABASE x", "document_x"} {
		if err := tg.Drop(context.Background(), name); err == nil {
			t.Errorf("Drop(%q) must be refused", name)
		}
	}
}
