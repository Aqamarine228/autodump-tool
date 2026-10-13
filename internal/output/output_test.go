package output

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, d *Dir, name string, last uint64, content string) {
	t.Helper()
	w, err := d.Begin(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := d.Commit(name, last); err != nil {
		t.Fatal(err)
	}
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestFileName(t *testing.T) {
	if FileName(1, 150) != "1-150.sql" {
		t.Fatal(FileName(1, 150))
	}
}

func TestLifecycle(t *testing.T) {
	dir := t.TempDir()
	d := NewDir(dir)
	write(t, d, "1-100.sql", 100, "data")
	b, err := os.ReadFile(filepath.Join(dir, "1-100.sql"))
	if err != nil || string(b) != "data" {
		t.Fatalf("%v %q", err, b)
	}
	if exists(filepath.Join(dir, "1-100.sql"+PartialSuffix)) {
		t.Fatal("partial left")
	}
	st, _ := os.ReadFile(filepath.Join(dir, StateFile))
	if string(st) != "100\n" {
		t.Fatalf("state %q", st)
	}
	write(t, d, "101-200.sql", 200, "x")
	st, _ = os.ReadFile(filepath.Join(dir, StateFile))
	if string(st) != "200\n" {
		t.Fatalf("state %q", st)
	}
	if exists(filepath.Join(dir, StateFile+".tmp")) {
		t.Fatal("tmp left")
	}
}

func TestAbort(t *testing.T) {
	dir := t.TempDir()
	d := NewDir(dir)
	w, err := d.Begin("1-5.sql")
	if err != nil {
		t.Fatal(err)
	}
	w.Close()
	if err := d.Abort("1-5.sql"); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(dir, "1-5.sql"+PartialSuffix)) {
		t.Fatal("partial remains")
	}
	if err := d.Abort("1-5.sql"); err != nil {
		t.Fatalf("missing abort must be ok: %v", err)
	}
}

func TestCommitRefusesOverwrite(t *testing.T) {
	dir := t.TempDir()
	d := NewDir(dir)
	write(t, d, "1-5.sql", 5, "orig")
	w, err := d.Begin("1-5.sql")
	if err != nil {
		t.Fatal(err)
	}
	w.Close()
	if err := d.Commit("1-5.sql", 5); err == nil {
		t.Fatal("expected error")
	}
	b, _ := os.ReadFile(filepath.Join(dir, "1-5.sql"))
	if string(b) != "orig" {
		t.Fatalf("overwritten: %q", b)
	}
}

func TestBeginRefusesExistingPartial(t *testing.T) {
	d := NewDir(t.TempDir())
	w, err := d.Begin("1-5.sql")
	if err != nil {
		t.Fatal(err)
	}
	w.Close()
	if _, err := d.Begin("1-5.sql"); err == nil {
		t.Fatal("expected error")
	}
}

func TestCleanPartials(t *testing.T) {
	dir := t.TempDir()
	d := NewDir(dir)
	for _, n := range []string{"1-5.sql.partial", "6-9.sql.partial", StateFile + ".tmp", "1-2.sql", StateFile} {
		os.WriteFile(filepath.Join(dir, n), []byte("1\n"), 0o644)
	}
	n, err := d.CleanPartials()
	if err != nil || n != 3 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if !exists(filepath.Join(dir, "1-2.sql")) || !exists(filepath.Join(dir, StateFile)) {
		t.Fatal("removed too much")
	}
}

func TestWatermark(t *testing.T) {
	touch := func(dir, name, content string) {
		os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644)
	}
	t.Run("empty", func(t *testing.T) {
		w, err := NewDir(t.TempDir()).Watermark()
		if err != nil || w != 0 {
			t.Fatal(w, err)
		}
	})
	t.Run("files only with junk", func(t *testing.T) {
		dir := t.TempDir()
		for _, n := range []string{"1-100.sql", "101-250.sql", "500-400.sql", "a-b.sql", "1-2.sql.partial", "9-999.txt", "notes.sql"} {
			touch(dir, n, "")
		}
		w, err := NewDir(dir).Watermark()
		if err != nil || w != 250 {
			t.Fatal(w, err)
		}
	})
	t.Run("state only", func(t *testing.T) {
		dir := t.TempDir()
		touch(dir, StateFile, " 777\n")
		w, err := NewDir(dir).Watermark()
		if err != nil || w != 777 {
			t.Fatal(w, err)
		}
	})
	t.Run("max wins", func(t *testing.T) {
		dir := t.TempDir()
		touch(dir, StateFile, "50\n")
		touch(dir, "1-100.sql", "")
		if w, _ := NewDir(dir).Watermark(); w != 100 {
			t.Fatal(w)
		}
		touch(dir, StateFile, "300\n")
		if w, _ := NewDir(dir).Watermark(); w != 300 {
			t.Fatal(w)
		}
	})
	t.Run("malformed state", func(t *testing.T) {
		dir := t.TempDir()
		touch(dir, StateFile, "oops")
		if _, err := NewDir(dir).Watermark(); err == nil {
			t.Fatal("expected error")
		}
	})
}
