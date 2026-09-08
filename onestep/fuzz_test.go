package onestep_test

import (
	"bytes"
	"io"
	"testing"

	"samuelks.com/datarescue/ddrescue"
	"samuelks.com/datarescue/internal/test1step"
	"samuelks.com/datarescue/onestep"
)

// Every size and offset here comes off damaged media and is used to address
// something: the catalog offset, the member directory's count and sizes, each
// table's record count, and every chunk's offset and length. The invariant is
// the one the other parsers hold to: read anything, panic on nothing.

func FuzzArchive(f *testing.F) {
	// Small seeds: the fuzzer mutates every byte of these, so a large one buys
	// no coverage and costs a great deal of time.
	small := []test1step.File{
		{Dir: "", Name: "A.TXT", Data: []byte("alpha")},
		{Dir: `D\S`, Name: "B.BIN", Data: []byte("bravo bravo bravo bravo bravo")},
	}
	for _, opts := range []test1step.Options{
		{Files: small},
		{Compress: true, Files: small},
		{Compress: true, Disks: 2, Files: small},
		{Compress: true, BadChecksum: 1, Files: small},
	} {
		f.Add(test1step.Build(opts)[0])
	}
	f.Add(make([]byte, onestep.HeaderSize))
	f.Add([]byte{0xCD, 0xAB, 0xCD, 0xAB})
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		a, err := onestep.OpenSources([]onestep.Source{{
			Path:   "fuzz.1-Step",
			Reader: bytes.NewReader(data),
			Size:   int64(len(data)),
		}})
		if err != nil {
			return
		}
		defer a.Close() //nolint:errcheck // read-only

		_ = a.Job()
		_ = a.Drives()
		_ = a.Sessions()
		_ = a.Selections()
		_ = a.Disks()
		_ = a.Diagnostics()

		for _, m := range a.Members() {
			// A member's declared size cannot make this read the whole file.
			_, _ = io.Copy(io.Discard, io.LimitReader(a.OpenMember(m), 1<<20))
		}

		files := a.Files()
		// One file is read again through an armed gap log, which is the path
		// that turns damage into zeros and the arithmetic that has to hold
		// whatever the catalog says.
		var filled bool
		for i := range files {
			if i >= 64 {
				break
			}
			f := &files[i]
			if _, ok := a.Find(f.Path); !ok {
				t.Fatalf("Find(%q) did not match a file the catalog lists", f.Path)
			}
			// The chunks are the data, so what a file delivers is bounded by
			// their recorded sizes. A catalog that disagrees with itself about
			// File.Size is reported as a diagnostic, not corrected here.
			var want int64
			for _, c := range f.Chunks {
				want += c.OrigSize
			}
			r := a.OpenFile(f)
			n, err := io.Copy(io.Discard, io.LimitReader(r, 1<<20))
			if err == nil && n > want && n < 1<<20 {
				t.Fatalf("%s delivered %d bytes, more than the %d its chunks record", f.Path, n, want)
			}
			_ = r.Errors()
			if !filled {
				fillOne(t, a, f)
				filled = true
			}
		}
	})
}

// fillOne drives the zero-filling path, where a hole's length is the chunk's
// recorded original size less what came back. Only ddrescue.Fill arms a log, so
// this is the only way to reach it, and a corrupt size must not be able to turn
// that arithmetic into a negative length, a hole outside the file, or an
// unbounded allocation.
func fillOne(t *testing.T, a *onestep.Archive, f *onestep.File) {
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
			t.Fatalf("%s recorded a hole at %d of %d bytes, outside the %d it declares",
				f.Path, g.FileOffset, g.Length, f.Size)
		}
	}
}

// fuzzSink takes the filled bytes and the map beside them without touching the
// filesystem.
type fuzzSink struct{ bytes.Buffer }

func (*fuzzSink) Close() error { return nil }
