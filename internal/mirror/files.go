package mirror

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"autodump/internal/output"
)

type dumpFile struct {
	name        string
	first, last uint64
	size        int64
}

// syncFiles copies every exported file that holds a document in the window
// and deletes mirrored files whose documents are all proven old.
func (s *Syncer) syncFiles(ctx context.Context, cutoff string, young []uint64) (copied, deleted int, err error) {
	if _, err := output.NewDir(s.cfg.FilesDir).CleanPartials(); err != nil {
		return 0, 0, fmt.Errorf("clean %s: %w", s.cfg.FilesDir, err)
	}
	local, err := listDumps(s.cfg.OutputDir)
	if err != nil {
		return 0, 0, err
	}
	remote, err := listDumps(s.cfg.FilesDir)
	if err != nil {
		return 0, 0, err
	}
	have := make(map[string]int64, len(remote))
	for _, f := range remote {
		have[f.name] = f.size
	}

	for _, f := range local {
		if err := ctx.Err(); err != nil {
			return copied, deleted, err
		}
		if size, ok := have[f.name]; (ok && size == f.size) || !hasAny(young, f.first, f.last) {
			continue
		}
		if err := copyFile(filepath.Join(s.cfg.OutputDir, f.name), filepath.Join(s.cfg.FilesDir, f.name)); err != nil {
			return copied, deleted, fmt.Errorf("copy %s: %w", f.name, err)
		}
		copied++
	}

	for _, f := range remote {
		if hasAny(young, f.first, f.last) {
			continue
		}
		var old, notOld int64
		if err := s.retry(ctx, "read index", func() (e error) {
			old, notOld, e = s.idx.RangeAges(ctx, cutoff, f.first, f.last)
			return e
		}); err != nil {
			return copied, deleted, err
		}
		if notOld > 0 || old == 0 {
			continue // a document is still young, or nothing proves the file old
		}
		if err := os.Remove(filepath.Join(s.cfg.FilesDir, f.name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return copied, deleted, fmt.Errorf("delete %s: %w", f.name, err)
		}
		deleted++
	}
	return copied, deleted, nil
}

func listDumps(dir string) ([]dumpFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []dumpFile
	for _, e := range entries {
		first, last, ok := output.ParseFileName(e.Name())
		if !ok || !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return nil, err
		}
		out = append(out, dumpFile{name: e.Name(), first: first, last: last, size: info.Size()})
	}
	return out, nil
}

// hasAny reports whether sorted ids has an element in [first, last].
func hasAny(ids []uint64, first, last uint64) bool {
	i, _ := slices.BinarySearch(ids, first)
	return i < len(ids) && ids[i] <= last
}

// copyFile copies via a .partial file so a half-copied file never looks complete.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + output.PartialSuffix
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, err = io.CopyBuffer(out, in, make([]byte, 4<<20))
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, dst)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}
