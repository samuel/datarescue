package qic

import (
	"bytes"
	"io"
	"testing"

	"samuelks.com/datarescue/ddrescue"
	"samuelks.com/datarescue/internal/testqic"
)

// The invariant across all of these: read anything, panic on nothing. Every
// length, offset and count these parsers use to address something comes off
// damaged media, so a corrupt one has to be turned away rather than trusted.
// The assertions past no-panic are the ones a corrupt archive must not be able
// to violate either.

// FuzzDecompress hammers the bit stream, which is where the length escape
// chain and the match offset bound live. Nothing in the encoding caps a match
// length, so an all-ones payload is exactly the input to worry about.
func FuzzDecompress(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0x41})
	f.Add(bytes.Repeat([]byte{0xFF}, 64))
	f.Add(bytes.Repeat([]byte{0x00}, 64))
	out, _ := testqic.Compress([]byte("the quick brown fox jumps over the lazy dog"), 4096)
	f.Add(out)

	f.Fuzz(func(t *testing.T, src []byte) {
		for _, want := range []int{0, 1, 64, 4096} {
			dst := make([]byte, want)
			n, err := decompress(dst, src)
			if n < 0 || n > want {
				t.Fatalf("decompress produced %d bytes into a %d-byte buffer (err %v)", n, want, err)
			}
		}
	})
}

// FuzzRecord feeds arbitrary bytes to the record parser and its screening
// test, which are what the catalog resync and the data region sweep rely on to
// reject a chance run of bytes.
func FuzzRecord(f *testing.F) {
	f.Add(make([]byte, fix1Size))
	f.Add(make([]byte, fix1Size+fix2Size))
	f.Add(testqic.Build(testqic.Options{})[VTBLSize:][:4096])

	f.Fuzz(func(t *testing.T, data []byte) {
		size, ok := plausibleRecord(data)
		if ok && (size < fix1Size+fix2Size || size > 0x10000+2) {
			t.Fatalf("plausibleRecord accepted a size of %d", size)
		}
		if size, ok := recordSize(data); ok && size > len(data) {
			t.Fatalf("recordSize returned %d for %d bytes", size, len(data))
		}
		for _, inData := range []bool{false, true} {
			r, err := parseRecord(data, inData)
			if err != nil {
				continue
			}
			if r.size > len(data) {
				t.Fatalf("a record of %d bytes was parsed out of %d", r.size, len(data))
			}
			if r.fileLen < 0 {
				t.Fatalf("a record declared a negative size of %d", r.fileLen)
			}
		}
	})
}

// FuzzCatalog feeds arbitrary bytes to the catalog decoder, whose resync is
// the part that has to be watched: one that fails to advance spins on the same
// window forever, which is a hang and not a slowdown.
func FuzzCatalog(f *testing.F) {
	f.Add([]byte{})
	f.Add(make([]byte, 512))
	f.Add(testqic.Build(testqic.Options{}))

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<20 {
			return
		}
		cr := &catalogRegion{data: data}
		files, _, end := decodeCatalog(cr, VariantWin95, "C:", func(DiagnosticKind, string, ...any) {})
		if end < 0 {
			t.Fatalf("the records account for a negative %d bytes", end)
		}
		for _, x := range files {
			if x.Depth > maxDepth+1 {
				t.Fatalf("%q was placed %d levels deep", x.Path, x.Depth)
			}
			if x.Size < 0 {
				t.Fatalf("%q declared a negative size of %d", x.Path, x.Size)
			}
			if bytes.Contains([]byte(x.Path), []byte("..")) {
				continue // a name off the medium may hold anything; safepath handles it
			}
		}
		// resync must always move forward, or parsing never terminates.
		for off := 0; off < len(data) && off < 4096; off += 97 {
			if next, ok := resync(data, off); ok && next <= off {
				t.Fatalf("resync went from %d to %d", off, next)
			}
		}
	})
}

// FuzzArchive drives the whole reader. Source carries an io.ReaderAt for this:
// the fuzzer needs no temporary file.
func FuzzArchive(f *testing.F) {
	for _, opts := range []testqic.Options{
		{},
		{Compress: true},
		{Compress: true, Variant: testqic.Win98},
		{Compress: true, BadSegment: 1},
		{Compress: true, BadCumSz: 1},
		{Compress: true, BadRecLen: 2},
		{Compress: true, DropCatalog: true},
		{Compress: true, Terminator: true},
	} {
		opts.Files = []testqic.File{
			{Name: "A.TXT", Data: []byte("a")},
			{Dir: "D", Name: "B.TXT", Data: bytes.Repeat([]byte("b"), 300)},
		}
		f.Add(testqic.Build(opts))
	}
	f.Add(make([]byte, VTBLSize))
	f.Add([]byte("VTBL"))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 4<<20 {
			return
		}
		for _, mode := range []Mode{ModeAuto, ModeCatalog, ModeScan} {
			exerciseArchive(t, data, mode)
		}
	})
}

func exerciseArchive(t *testing.T, data []byte, mode Mode) {
	a, err := OpenSource(Source{
		Path: "fuzz.qic", Reader: bytes.NewReader(data), Size: int64(len(data)),
	}, Options{Mode: mode})
	if err != nil {
		return
	}
	defer a.Close() //nolint:errcheck // read-only

	_, _ = a.Volumes(), a.MediumID()
	var pos int64
	for i, s := range a.Segments() {
		if s.Start < pos {
			t.Fatalf("segment %d starts at %d, behind %d", i, s.Start, pos)
		}
		pos = s.Start
	}

	files := a.Files()
	if len(files) > 64 {
		files = files[:64]
	}
	// One file is read again through an armed gap log, which is the path that
	// turns damage into zeros and the arithmetic that has to hold whatever the
	// segment offsets say.
	var filled bool
	for i := range files {
		f := &files[i]
		if got, ok := a.Find(f.Path); !ok || got == nil {
			t.Fatalf("Files lists %q but Find does not have it", f.Path)
		}
		if f.Dir {
			continue
		}
		n, err := io.Copy(io.Discard, io.LimitReader(a.OpenFile(f), 1<<20))
		if err == nil && n > f.Size {
			t.Fatalf("%q delivered %d bytes where it declares %d", f.Path, n, f.Size)
		}
		if !filled {
			fillOne(t, a, f)
			filled = true
		}
	}
	_ = a.Diagnostics()
}

// fillOne drives the zero-filling path, where a hole's length is worked out
// from the segment offsets rather than read off the medium. Only ddrescue.Fill
// arms a log, so this is the only way to reach it, and a corrupt offset must
// not be able to turn that arithmetic into a negative length, a hole outside
// the file, or an unbounded allocation.
func fillOne(t *testing.T, a *Archive, f *File) {
	const limit = 1 << 20
	var data, mapped fuzzSink
	got, err := ddrescue.Fill(&data,
		func() (io.WriteCloser, string, error) { return &mapped, "out.map", nil },
		func(l *ddrescue.GapLog) (io.Reader, error) {
			r := a.OpenFile(f)
			r.Fill(l)
			return io.LimitReader(r, limit), nil
		})
	if err != nil {
		return
	}
	for _, g := range got.Gaps {
		if g.FileOffset < 0 || g.Length < 0 || g.FileOffset+g.Length > f.Size {
			t.Fatalf("%q recorded a hole at %d of %d bytes, outside the %d it declares",
				f.Path, g.FileOffset, g.Length, f.Size)
		}
	}
}

// fuzzSink takes the filled bytes and the map beside them without touching the
// filesystem.
type fuzzSink struct{ bytes.Buffer }

func (*fuzzSink) Close() error { return nil }
