//go:build integration

package integration

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"autodump/internal/dump"
	"autodump/internal/mirror"
	"autodump/internal/output"
	"autodump/internal/source"
)

func TestMirrorSync(t *testing.T) {
	src := freshDB(t, "autodump_it_msrc")
	dst := freshDB(t, "autodump_it_mirror")
	buildFixture(t, src)
	// 1 is 40 days old, 2-4 are recent, 5 (not ready) is never exported.
	mustExec(t, src, "UPDATE parsing_documents SET created_at = NOW() - INTERVAL 40 DAY WHERE id = 1")

	// The mirror already holds an old document, one of unknown age, a
	// half-imported table and an unrelated table.
	mustExec(t, dst, resultDDL("document_1"))
	mustExec(t, dst, "CREATE TABLE document_99 (id INT)")
	mustExec(t, dst, "CREATE TABLE document_2__part (id INT)")
	mustExec(t, dst, "CREATE TABLE keep_me (id INT)")

	out, share := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(out, "1-1.sql"), []byte("old"), 0o644)
	os.WriteFile(filepath.Join(out, "2-4.sql"), []byte("recent"), 0o644)
	os.WriteFile(filepath.Join(out, output.StateFile), []byte("4\n"), 0o644)
	os.WriteFile(filepath.Join(share, "1-1.sql"), []byte("old"), 0o644)

	idxEng, docEng, dstEng := openEngine(t, "autodump_it_msrc"), openEngine(t, "autodump_it_msrc"), openEngine(t, "autodump_it_mirror")
	s := mirror.New(mirror.Config{
		RetentionDays: 30, OutputDir: out, FilesDir: share, Workers: 2, RetryAttempts: 1,
		Dump: dump.Options{InsertBatchRows: 1000, InsertBatchBytes: 64 << 10},
	}, source.NewIndex(idxEng, "parsing_documents"), source.NewDocs(docEng), mirror.NewDBTarget(dstEng),
		output.NewDir(out).Watermark, log.New(io.Discard, "", 0))

	sum, err := s.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sum.Added != 3 || sum.Dropped != 1 || sum.Rows != 52+0+20000 || sum.FilesCopied != 1 || sum.FilesDeleted != 1 {
		t.Fatalf("summary %+v", sum)
	}

	var names []string
	rows, err := dst.Query("SELECT table_name FROM information_schema.tables WHERE table_schema = DATABASE() ORDER BY table_name")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var n string
		rows.Scan(&n)
		names = append(names, n)
	}
	rows.Close()
	if want := []string{"document_2", "document_3", "document_4", "document_99", "keep_me"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("mirror tables %v, want %v", names, want)
	}
	for _, id := range []uint64{2, 3, 4} {
		tbl := source.TableName(id)
		if a, b := showCreate(t, src, tbl), showCreate(t, dst, tbl); a != b {
			t.Errorf("%s DDL differs:\nsrc: %s\ndst: %s", tbl, a, b)
		}
		if a, b := checksum(t, src, tbl), checksum(t, dst, tbl); a != b {
			t.Errorf("%s checksum differs: src %d dst %d", tbl, a, b)
		}
	}

	entries, _ := os.ReadDir(share)
	var files []string
	for _, e := range entries {
		files = append(files, e.Name())
	}
	if !slices.Equal(files, []string{"2-4.sql"}) {
		t.Fatalf("share has %v", files)
	}

	// A second run has nothing to do.
	sum, err = s.Run(context.Background())
	if err != nil || sum.Added != 0 || sum.Dropped != 0 || sum.Kept != 3 || sum.FilesCopied != 0 || sum.FilesDeleted != 0 {
		t.Fatalf("second run: %v %+v", err, sum)
	}

	// Once document 2 ages out it is dropped; its file stays while 3 and 4 are recent.
	mustExec(t, src, "UPDATE parsing_documents SET created_at = NOW() - INTERVAL 31 DAY WHERE id = 2")
	sum, err = s.Run(context.Background())
	if err != nil || sum.Dropped != 1 || sum.FilesDeleted != 0 {
		t.Fatalf("third run: %v %+v", err, sum)
	}
	// All of 2-4 old: the file goes too.
	mustExec(t, src, "UPDATE parsing_documents SET created_at = NOW() - INTERVAL 31 DAY WHERE id IN (3, 4)")
	sum, err = s.Run(context.Background())
	if err != nil || sum.Dropped != 2 || sum.FilesDeleted != 1 {
		t.Fatalf("fourth run: %v %+v", err, sum)
	}
}
