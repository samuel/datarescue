package outsink

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"samuelks.com/datarescue/internal/safepath"
	"samuelks.com/datarescue/internal/xio"
)

// Dir writes recovered files as loose files under a directory.
type Dir struct {
	root  string
	force bool
	times bool
}

// NewDir prepares root as an output directory.
func NewDir(root string, force, times bool) (*Dir, error) {
	if err := xio.EnsureDir(root); err != nil {
		return nil, err
	}
	return &Dir{root: root, force: force, times: times}, nil
}

// Create makes the file for one entry, creating parent directories as needed.
func (d *Dir) Create(e Entry) (io.WriteCloser, string, error) {
	path, err := safepath.Join(d.root, e.Path)
	if err != nil {
		return nil, "", err
	}
	f, err := xio.Create(path, d.force)
	if err != nil {
		return nil, "", err
	}
	mod := e.ModTime
	if !d.times {
		mod = time.Time{}
	}
	return &dirFile{File: f, path: path, mod: mod}, d.rel(path), nil
}

// Discard removes the file written for an entry. A file is the one destination
// that can be taken back, so here the "no zeros without a map" guarantee is
// enforced rather than only reported.
func (d *Dir) Discard(e Entry) error {
	path, err := safepath.Join(d.root, e.Path)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Manifest keeps nothing: a directory has nowhere to put a report that would
// not look like another recovered file.
func (d *Dir) Manifest(string) error { return nil }

// Close has nothing to finish; each file was closed as it was written.
func (d *Dir) Close() error { return nil }

// rel renders an output path relative to the output directory, for messages.
func (d *Dir) rel(path string) string {
	if rel, err := filepath.Rel(d.root, path); err == nil {
		return rel
	}
	return path
}

// dirFile applies the recorded modification time once the data is written. A
// file that stopped partway keeps its time too: it is still what the medium
// recorded, and the partial data is still evidence.
type dirFile struct {
	*os.File
	path string
	mod  time.Time
}

func (w *dirFile) Close() error {
	if err := w.File.Close(); err != nil {
		return err
	}
	if w.mod.IsZero() {
		return nil
	}
	return os.Chtimes(w.path, w.mod, w.mod)
}
