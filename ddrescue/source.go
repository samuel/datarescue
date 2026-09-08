package ddrescue

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// Options is how a tool asks for an image to be read through its map.
type Options struct {
	// Map is the map path given on the command line. When empty, a sidecar
	// beside the image is used.
	Map string
	// NoMap reads the image as it is, even when a sidecar sits beside it.
	NoMap bool
}

// Resolve returns the path of the map to read an image through: the one asked
// for, the sidecar beside the image, or none. The policy lives only here, so
// every tool finds a map the same way.
func Resolve(image string, opts Options) (string, error) {
	if opts.NoMap {
		if opts.Map != "" {
			return "", errors.New("ddrescue: a map was named and also asked to be ignored")
		}
		return "", nil
	}
	if opts.Map != "" {
		if _, err := os.Stat(opts.Map); err != nil {
			return "", fmt.Errorf("ddrescue: map file: %w", err)
		}
		return opts.Map, nil
	}
	return FindMap(image), nil
}

// Source is an image opened under that policy. It offers the two views a
// damaged image needs.
//
// Checked fails on any range the map does not mark recovered, so a parser never
// decodes unrecovered bytes as structure. Raw is the file's bytes as they are,
// for verification: a check that compares the medium against a checksum has to
// read the bytes the map calls bad, or it can never find corruption the map
// missed or detect a stale map. Without a map the two are the same reader.
type Source struct {
	Path    string
	MapPath string // empty when the image is read as it is
	Blocks  []Block
	Size    int64

	f       *os.File
	checked *Reader
}

// OpenSource opens the image at path, through its map where the options and the
// files on disk call for one.
func OpenSource(path string, opts Options) (*Source, error) {
	mapPath, err := Resolve(path, opts)
	if err != nil {
		return nil, err
	}
	var blocks []Block
	if mapPath != "" {
		if blocks, err = ParseMap(mapPath); err != nil {
			return nil, err
		}
	}
	f, err := os.Open(path) //nolint:gosec // the image path is the tool's argument
	if err != nil {
		return nil, fmt.Errorf("ddrescue: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close() //nolint:errcheck // best-effort cleanup on the error path
		return nil, fmt.Errorf("ddrescue: %w", err)
	}
	s := &Source{Path: path, MapPath: mapPath, Blocks: blocks, Size: info.Size(), f: f}
	if mapPath != "" {
		s.checked = NewReader(f, blocks)
	}
	return s, nil
}

// Raw is the image's bytes as they are.
func (s *Source) Raw() io.ReaderAt { return s.f }

// Checked is the image read through its map, or Raw when there is no map.
func (s *Source) Checked() io.ReaderAt {
	if s.checked == nil {
		return s.f
	}
	return s.checked
}

// Reader is the map-checked view as an io.ReadSeeker for a parser that seeks,
// or the file itself when there is no map.
func (s *Source) Reader() io.ReadSeeker {
	if s.checked == nil {
		return s.f
	}
	return s.checked
}

// Unrecovered returns the parts of an image byte range the map does not mark
// finished. Without a map nothing is known to be missing, so it returns nil and
// only a read can find damage. Handling that here means callers do not each
// have to remember that an absent map is not an empty one.
func (s *Source) Unrecovered(off, size int64) []Block {
	if len(s.Blocks) == 0 {
		return nil
	}
	return Unrecovered(s.Blocks, off, size)
}

// UnrecoveredBytes is how much of a range the map does not mark finished.
func (s *Source) UnrecoveredBytes(off, size int64) int64 {
	var n int64
	for _, b := range s.Unrecovered(off, size) {
		n += b.Size
	}
	return n
}

func (s *Source) Close() error { return s.f.Close() }
