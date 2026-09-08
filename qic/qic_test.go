package qic_test

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"samuelks.com/datarescue/internal/testqic"
	"samuelks.com/datarescue/qic"
)

// build opens a synthetic archive from memory, so no test needs a temporary
// file to read one.
func build(t *testing.T, opts testqic.Options) *qic.Archive {
	t.Helper()
	data := testqic.Build(opts)
	a, err := qic.OpenSource(qic.Source{
		Path:   "backup.qic",
		Reader: bytes.NewReader(data),
		Size:   int64(len(data)),
	}, qic.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { a.Close() }) //nolint:errcheck // read-only
	return a
}

func noDiagnostics(t *testing.T, a *qic.Archive) {
	t.Helper()
	for _, d := range a.Diagnostics() {
		t.Errorf("unexpected diagnostic %s: %s", d.Kind, d.Message)
	}
}

func readAll(t *testing.T, a *qic.Archive, f *qic.File) ([]byte, error) {
	t.Helper()
	return io.ReadAll(a.OpenFile(f))
}

func fileNamed(t *testing.T, a *qic.Archive, path string) *qic.File {
	t.Helper()
	f, ok := a.Find(path)
	if !ok {
		var have []string
		for _, x := range a.Files() {
			have = append(have, x.Path)
		}
		t.Fatalf("%s is not in the archive; it holds %v", path, have)
	}
	return f
}

// TestRoundTrip checks that every file comes back byte for byte, in both
// dialects and whether or not the archive is compressed.
func TestRoundTrip(t *testing.T) {
	t.Parallel()
	for _, compress := range []bool{false, true} {
		for _, variant := range []testqic.Variant{testqic.Win95, testqic.Win98} {
			t.Run(name(compress, variant), func(t *testing.T) {
				t.Parallel()
				files := testqic.DefaultFiles()
				a := build(t, testqic.Options{Compress: compress, Variant: variant, Files: files})
				noDiagnostics(t, a)

				for _, want := range files {
					if want.IsDir {
						continue
					}
					f := fileNamed(t, a, want.Path())
					if f.Size != int64(len(want.Data)) {
						t.Errorf("%s: size %d, want %d", want.Path(), f.Size, len(want.Data))
					}
					got, err := readAll(t, a, f)
					if err != nil {
						t.Errorf("%s: %v", want.Path(), err)
						continue
					}
					if !bytes.Equal(got, want.Data) {
						t.Errorf("%s: %d bytes back, want %d, contents differ", want.Path(), len(got), len(want.Data))
					}
				}
			})
		}
	}
}

// TestCompressedMatchesStored is the differential test that keeps the builder
// and the reader from agreeing on a shared mistake. An uncompressed archive
// carries no segment headers and no compressed stream at all, so the record,
// tree, path and offset models are exercised without the decompressor, and the
// decompressor is then checked against a path that does not contain them.
func TestCompressedMatchesStored(t *testing.T) {
	t.Parallel()
	files := testqic.DefaultFiles()
	plain := build(t, testqic.Options{Files: files})
	packed := build(t, testqic.Options{Compress: true, Files: files})
	noDiagnostics(t, plain)
	noDiagnostics(t, packed)

	if len(plain.Files()) != len(packed.Files()) {
		t.Fatalf("stored holds %d entries, compressed %d", len(plain.Files()), len(packed.Files()))
	}
	for i := range plain.Files() {
		p, c := plain.Files()[i], packed.Files()[i]
		if p.Path != c.Path || p.Size != c.Size || p.Dir != c.Dir {
			t.Errorf("entry %d differs: stored %+v, compressed %+v", i, p, c)
			continue
		}
		if p.DataOffset() != c.DataOffset() {
			t.Errorf("%s sits at %d stored and %d compressed", p.Path, p.DataOffset(), c.DataOffset())
		}
		if p.Dir {
			continue
		}
		pd, perr := readAll(t, plain, &plain.Files()[i])
		cd, cerr := readAll(t, packed, &packed.Files()[i])
		if perr != nil || cerr != nil {
			t.Errorf("%s: stored %v, compressed %v", p.Path, perr, cerr)
			continue
		}
		if !bytes.Equal(pd, cd) {
			t.Errorf("%s: stored and compressed contents differ", p.Path)
		}
	}
}

// TestTree checks the paths the depth-first traversal produces, including the
// directories the builder created only because a file's path included them.
func TestTree(t *testing.T) {
	t.Parallel()
	a := build(t, testqic.Options{Compress: true})
	noDiagnostics(t, a)

	for _, want := range []string{
		`C:`, `C:\DOS`, `C:\SRC`, `C:\SRC\LIB`, `C:\SRC\LIB\NET`, `C:\TEMP`,
		`C:\TEMP\SCRATCH`, `C:\README.TXT`, `C:\SRC\LIB\NET\socket.c`,
	} {
		f := fileNamed(t, a, want)
		if f.Path != want {
			t.Errorf("found %q looking for %q", f.Path, want)
		}
	}
	if d := fileNamed(t, a, `C:\TEMP\SCRATCH`); !d.Dir || !d.Empty {
		t.Errorf(`C:\TEMP\SCRATCH is dir=%v empty=%v, want both`, d.Dir, d.Empty)
	}
	if d := fileNamed(t, a, `C:\SRC\LIB\NET`); d.Depth != 3 {
		t.Errorf(`C:\SRC\LIB\NET has depth %d, want 3`, d.Depth)
	}
}

// TestEmptyFile pins down the zero-length case, where the reference
// implementation branches on the recorded size and so steps over an empty file
// as though it were a directory.
func TestEmptyFile(t *testing.T) {
	t.Parallel()
	for _, variant := range []testqic.Variant{testqic.Win95, testqic.Win98} {
		t.Run(variant.String(), func(t *testing.T) {
			t.Parallel()
			a := build(t, testqic.Options{Compress: true, Variant: variant})
			noDiagnostics(t, a)

			f := fileNamed(t, a, `C:\EMPTY.DAT`)
			if f.Size != 0 || f.Dir {
				t.Fatalf("EMPTY.DAT is size=%d dir=%v, want an empty file", f.Size, f.Dir)
			}
			got, err := readAll(t, a, f)
			if err != nil || len(got) != 0 {
				t.Fatalf("reading EMPTY.DAT gave %d bytes and %v", len(got), err)
			}
			// Every file after it has to still land in the right place, which
			// is what the size-based branch gets wrong.
			after := fileNamed(t, a, `C:\DOS\ATTRIB.EXE`)
			data, err := readAll(t, a, after)
			if err != nil {
				t.Fatalf("ATTRIB.EXE: %v", err)
			}
			if !bytes.HasPrefix(data, []byte("MZ")) {
				t.Errorf("ATTRIB.EXE starts %q, so the offsets drifted past the empty file", data[:min(4, len(data))])
			}
		})
	}
}

// TestVolume checks the header fields a report shows.
func TestVolume(t *testing.T) {
	t.Parallel()
	for _, variant := range []testqic.Variant{testqic.Win95, testqic.Win98} {
		t.Run(variant.String(), func(t *testing.T) {
			t.Parallel()
			a := build(t, testqic.Options{Compress: true, Variant: variant})
			noDiagnostics(t, a)

			v := a.Volume()
			if v.Label != testqic.Label {
				t.Errorf("label %q, want %q", v.Label, testqic.Label)
			}
			if !v.Compressed || v.Algorithm != 1 {
				t.Errorf("compression is %v with algorithm %d, want QIC-122", v.Compressed, v.Algorithm)
			}
			if a.Drive() != testqic.Drive {
				t.Errorf("drive %q, want %q", a.Drive(), testqic.Drive)
			}
			if !v.Created.Equal(testqic.Created) {
				t.Errorf("created %s, want %s", v.Created, testqic.Created)
			}
			wantVariant := qic.VariantWin95
			if variant == testqic.Win98 {
				wantVariant = qic.VariantWin98
			}
			if v.Variant != wantVariant {
				t.Errorf("variant %s, want %s", v.Variant, wantVariant)
			}
			if variant == testqic.Win98 && a.MediumID() == nil {
				t.Error("no MDID record was decoded")
			}
		})
	}
}

// TestSegmentIndex checks that the index covers the data region without a gap,
// since a gap is what a lookup by offset falls into.
func TestSegmentIndex(t *testing.T) {
	t.Parallel()
	a := build(t, testqic.Options{Compress: true})
	noDiagnostics(t, a)

	segs := a.Segments()
	if len(segs) == 0 {
		t.Fatal("the index holds no segment")
	}
	var pos int64
	for i, s := range segs[:a.DataSegments()] {
		if s.Start != pos {
			t.Errorf("segment %d starts at %d, want %d", i, s.Start, pos)
		}
		pos = s.End()
	}
	if pos != a.DataSize() {
		t.Errorf("the segments account for %d bytes, want %d", pos, a.DataSize())
	}
}

// TestNotQIC checks that a file which is not an archive is told apart from one
// that could not be read.
func TestNotQIC(t *testing.T) {
	t.Parallel()
	data := make([]byte, 4096)
	_, err := qic.OpenSource(qic.Source{Path: "x", Reader: bytes.NewReader(data), Size: int64(len(data))}, qic.Options{})
	if err == nil {
		t.Fatal("a file of zeros opened as an archive")
	}
	if !errors.Is(err, qic.ErrNotQIC) {
		t.Errorf("error is %v, want it to report that the file is not an archive", err)
	}
}

func name(compress bool, v testqic.Variant) string {
	s := "stored"
	if compress {
		s = "compressed"
	}
	return s + "/" + v.String()
}

// TestCompressedQIC checks that a compressed archive is identified as such
// rather than only refused.
func TestCompressedQIC(t *testing.T) {
	t.Parallel()
	data := append([]byte{0x1f, 0x8b, 0x08, 0x00}, make([]byte, 4092)...)
	_, err := qic.OpenSource(qic.Source{Path: "x", Reader: bytes.NewReader(data), Size: int64(len(data))}, qic.Options{})
	if !errors.Is(err, qic.ErrNotQIC) {
		t.Fatalf("error is %v, want ErrNotQIC", err)
	}
	if !strings.Contains(err.Error(), "gzip-compressed") {
		t.Errorf("error %q does not say the file is gzip-compressed", err)
	}
}
