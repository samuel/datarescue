package hfs

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"testing"
)

// testfixtureBytes returns the fixture image, or nil when it is absent. A fuzz
// corpus seed is optional, so this must not fail the target.
func testfixtureBytes(f *testing.F) []byte {
	f.Helper()
	file, err := os.Open(testImagePath)
	if err != nil {
		return nil
	}
	defer file.Close() //nolint:errcheck // read-only
	gz, err := gzip.NewReader(file)
	if err != nil {
		return nil
	}
	data, err := io.ReadAll(gz)
	if err != nil {
		return nil
	}
	return data
}

// FuzzVolume drives the whole read path — MDB, catalog B-tree, path
// reconstruction — from raw bytes. Every offset the node walker derives comes
// off the disc, so every panic it can reach is reachable this way.
func FuzzVolume(f *testing.F) {
	f.Add(testfixtureBytes(f))
	f.Add(make([]byte, 4096))
	f.Add([]byte{})
	// A volume whose primary master directory block is gone, which sends Open
	// to the spare at the far end of the image.
	if fixture := testfixtureBytes(f); fixture != nil {
		broken := append([]byte(nil), fixture...)
		clear(broken[mdbOffset : mdbOffset+8])
		f.Add(broken)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		v, err := Open(bytes.NewReader(data), 0, int64(len(data)))
		if err != nil {
			return
		}
		_ = v.MDB.Label()
		entries, _, err := v.List()
		if err != nil {
			return
		}
		for _, e := range entries {
			// A recovered path must never climb out of the volume root; the
			// extract commands join it to an output directory.
			if len(e.Path) > maxPathDepth+1 {
				t.Fatalf("path of %d components", len(e.Path))
			}
			for _, c := range e.Path {
				if len(c) > 1<<20 {
					t.Fatalf("path component of %d bytes", len(c))
				}
			}
			// The root folder is the volume, not something in it.
			if e.CNID == RootFolderID {
				t.Fatal("the root folder was listed as an entry")
			}
			for _, ref := range []ForkRef{e.DataFork(), e.ResourceFork()} {
				ranges, err := v.ForkRanges(ref)
				if err != nil {
					continue
				}
				var covered, want int64
				for _, rg := range ranges {
					if rg.Length <= 0 {
						t.Fatalf("range of %d bytes", rg.Length)
					}
					if rg.ImageOffset < 0 {
						t.Fatalf("range at image offset %d", rg.ImageOffset)
					}
					if rg.FileOffset != want {
						t.Fatalf("range at file offset %d; the ranges must be contiguous from %d",
							rg.FileOffset, want)
					}
					covered += rg.Length
					want += rg.Length
				}
				// Clipped to the fork's recorded length: a reader that
				// delivered the slack in the last allocation block would hand
				// back bytes the file does not own.
				if covered > ref.Size {
					t.Fatalf("ranges cover %d bytes of a %d-byte fork", covered, ref.Size)
				}
				r, err := v.OpenFork(ref)
				if err != nil {
					continue
				}
				// Bound the read: the size is a disk field.
				buf := make([]byte, 4096)
				_, _ = r.Read(buf)
			}
			_ = e.FinderInfo()
			_ = e.Type.String()
			_ = e.Modified.Time(nil)
		}
	})
}
