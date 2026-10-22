package iso

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"io/fs"
	"os"
	"testing"
)

// errStopFuzz bounds the walk on a fixture that decodes into a huge tree.
var errStopFuzz = errors.New("stop")

// FuzzReader drives the whole ISO read path from raw bytes: the descriptor
// scan and volume dates, the directory tree and record dates, Joliet names, and
// the Rock Ridge NM, SL, PX, TF, CE, CL and RE entries, multi-extent chains,
// Lookup, and the random-access reader.
//
// It goes through OpenReader rather than Open: that is the path every tool
// takes, since an image is read through its rescue map, and it costs no
// temporary file per iteration.
func FuzzReader(f *testing.F) {
	f.Add(make([]byte, 40960))
	f.Add([]byte{})
	f.Add(flatten(buildPXTFISO(f)))
	f.Add(flatten(buildRelocatedISO(f)))
	multi, _ := multiExtentISO(f, flagMultiExtent, "OTHER.DAT;1")
	f.Add(flatten(multi))
	// A real hdiutil image: Rock Ridge in both trees, and no NM in either.
	if gz, err := os.Open("testdata/hdiutil-hybrid.iso.gz"); err == nil {
		if zr, err := gzip.NewReader(gz); err == nil {
			if img, err := io.ReadAll(zr); err == nil {
				f.Add(img)
			}
		}
		gz.Close() //nolint:errcheck // read-only
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		r, err := OpenReader(bytes.NewReader(data))
		if err != nil {
			return
		}
		_ = r.VolumeLabel()
		_ = r.Volume()
		_ = r.Issues()
		var entries int
		_ = r.Walk(func(e DirEntry) error {
			entries++
			if entries > 10000 {
				return errStopFuzz
			}
			// Lookup and the random-access reader take the same disc-supplied
			// names and extents; a few entries are enough to reach them.
			if entries <= 8 {
				_, _ = r.Lookup(e.Path)
				if !e.IsDir {
					ra := r.OpenFileAt(e)
					var p [64]byte
					if n, err := ra.ReadAt(p[:], ra.Size()/2); n < len(p) && err == nil {
						t.Fatalf("%s: ReadAt returned %d bytes and no error", e.Path, n)
					}
				}
			}
			if len(e.Path) > 1<<20 {
				t.Fatalf("path of %d bytes", len(e.Path))
			}
			// The record's own flag and SL decide how an entry is walked and
			// read, so its mode must never say otherwise.
			if e.Mode.IsDir() != e.IsDir || (e.Mode&fs.ModeSymlink != 0) != (e.Symlink != "") {
				t.Fatalf("%s: mode %v disagrees with IsDir %v, Symlink %q", e.Path, e.Mode, e.IsDir, e.Symlink)
			}
			// A file's size is the sum of its extents, which is what a reader
			// of it delivers.
			var sum int64
			for _, x := range e.Extents {
				sum += x.Size
			}
			if e.Extents != nil && sum != e.Size {
				t.Fatalf("%s: size %d, extents sum to %d", e.Path, e.Size, sum)
			}
			return nil
		})
	})
}
