// Package output writes dump files atomically and tracks export progress.
package output

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const (
	// PartialSuffix marks files still being written.
	PartialSuffix = ".partial"
	// StateFile holds the last exported ID.
	StateFile = "last_id.txt"
)

// FileName returns the dump file name for an ID range.
func FileName(first, last uint64) string { return fmt.Sprintf("%d-%d.sql", first, last) }

// Sink receives finished dump files.
type Sink interface {
	Begin(name string) (io.WriteCloser, error)
	Commit(name string, lastID uint64) error
	Abort(name string) error
}

// Dir is a Sink backed by a local directory.
type Dir struct{ path string }

// NewDir returns a Dir sink rooted at path.
func NewDir(path string) *Dir { return &Dir{path: path} }

var _ Sink = (*Dir)(nil)

type syncCloser struct{ f *os.File }

func (s syncCloser) Write(p []byte) (int, error) { return s.f.Write(p) }

// Close fsyncs then closes the file.
func (s syncCloser) Close() error {
	serr := s.f.Sync()
	if cerr := s.f.Close(); serr == nil {
		serr = cerr
	}
	return serr
}

// Begin creates name+".partial" exclusively and returns a writer for it.
func (d *Dir) Begin(name string) (io.WriteCloser, error) {
	f, err := os.OpenFile(filepath.Join(d.path, name+PartialSuffix), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return syncCloser{f}, nil
}

// Commit publishes the partial file as name (never overwriting) and records lastID.
func (d *Dir) Commit(name string, lastID uint64) error {
	final := filepath.Join(d.path, name)
	if _, err := os.Stat(final); err == nil {
		return fmt.Errorf("output file %s already exists", name)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.Rename(final+PartialSuffix, final); err != nil {
		return err
	}
	if err := d.writeState(lastID); err != nil {
		return err
	}
	syncDir(d.path)
	return nil
}

func (d *Dir) writeState(lastID uint64) error {
	state := filepath.Join(d.path, StateFile)
	tmp := state + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, err = f.WriteString(strconv.FormatUint(lastID, 10) + "\n")
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, state)
}

// syncDir is a best-effort directory fsync; unsupported on Windows.
func syncDir(path string) {
	if f, err := os.Open(path); err == nil {
		f.Sync()
		f.Close()
	}
}

// Abort removes the partial file; a missing file is not an error.
func (d *Dir) Abort(name string) error {
	err := os.Remove(filepath.Join(d.path, name+PartialSuffix))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// CleanPartials removes leftover *.partial files and the state temp file.
func (d *Dir) CleanPartials() (int, error) {
	entries, err := os.ReadDir(d.path)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !(strings.HasSuffix(name, PartialSuffix) || name == StateFile+".tmp") {
			continue
		}
		if err := os.Remove(filepath.Join(d.path, name)); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

var dumpRe = regexp.MustCompile(`^(\d+)-(\d+)\.sql$`)

// Watermark returns the highest exported ID known from dump file names and
// the state file, or 0 if there is none. A malformed state file is an error.
func (d *Dir) Watermark() (uint64, error) {
	entries, err := os.ReadDir(d.path)
	if err != nil {
		return 0, err
	}
	var w uint64
	for _, e := range entries {
		m := dumpRe.FindStringSubmatch(e.Name())
		if m == nil || e.IsDir() {
			continue
		}
		first, err1 := strconv.ParseUint(m[1], 10, 64)
		last, err2 := strconv.ParseUint(m[2], 10, 64)
		if err1 != nil || err2 != nil || first > last {
			continue
		}
		w = max(w, last)
	}
	b, err := os.ReadFile(filepath.Join(d.path, StateFile))
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return 0, err
	default:
		v, perr := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
		if perr != nil {
			return 0, fmt.Errorf("malformed %s: %w", StateFile, perr)
		}
		w = max(w, v)
	}
	return w, nil
}
