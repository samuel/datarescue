package onestep_test

import (
	"bytes"
	"io"
	"testing"

	"samuelks.com/datarescue/ddrescue"
	"samuelks.com/datarescue/internal/test1step"
	"samuelks.com/datarescue/onestep"
)

// fill reads r through ddrescue.Fill, which is the only thing that arms a gap
// log — a reader cannot zero-fill anywhere else, so this is also the only way
// to test that it does. It returns what came back and the holes recorded.
func fill(t *testing.T, r *onestep.Reader) ([]byte, ddrescue.Filled) {
	t.Helper()
	var data, mapped nopCloser
	f, err := ddrescue.Fill(&data,
		func() (io.WriteCloser, string, error) { return &mapped, "out.map", nil },
		func(l *ddrescue.GapLog) (io.Reader, error) {
			r.Fill(l)
			return r, nil
		})
	if err != nil {
		t.Fatalf("writing the map beside the data: %s", err)
	}
	return data.Bytes(), f
}

type nopCloser struct{ bytes.Buffer }

func (*nopCloser) Close() error { return nil }

// zeroed returns want with each of the gaps cleared, which is what a filled
// read of it should be.
func zeroed(want []byte, gaps []ddrescue.Gap) []byte {
	out := append([]byte(nil), want...)
	for _, g := range gaps {
		clear(out[g.FileOffset : g.FileOffset+g.Length])
	}
	return out
}

// A disk cut short costs the chunks stored past the cut. The catalog records
// each chunk's original size, so the hole is that size exactly: the file comes
// back at its declared length with the missing chunks marked, and — the point —
// every chunk after the damage still lands at the offset the file gives it.
func TestFillTruncatedDisk(t *testing.T) {
	t.Parallel()
	want := test1step.DefaultFiles()
	paths := writeDisks(t, test1step.Options{Disks: 3, Truncate: map[int]int64{2: 4096}}, nil)
	a := openPaths(t, paths...)

	var filled, clean int
	for i := range a.Files() {
		f := &a.Files()[i]
		got, res := fill(t, a.OpenFile(f))
		if res.Err != nil {
			t.Errorf("%s: reading with a gap log: %s", f.Path, res.Err)
			continue
		}
		if int64(len(got)) != f.Size {
			t.Errorf("%s: recovered %d bytes; want the file's full %d", f.Path, len(got), f.Size)
			continue
		}
		if !res.Damaged() {
			clean++
		} else {
			filled++
		}
		// Whether it was filled or not, the bytes that came back are the
		// file's own everywhere the map does not say otherwise. A hole of the
		// wrong length would shift the chunks after it and show up here.
		if !bytes.Equal(got, zeroed(want[i].Data, res.Gaps)) {
			t.Errorf("%s: the recovered bytes are not the file's with zeros only where the gaps are", f.Path)
		}
	}
	if filled == 0 {
		t.Error("the truncated disk cost nothing, so the test proves nothing")
	}
	if clean == 0 {
		t.Error("every file was filled; a truncated disk must not reach the files stored elsewhere")
	}
}

// A disk of the set that was never supplied is a hole of the chunks' whole
// recorded length, so a backup missing one of its disks restores at full length
// with the missing chunks marked in every affected file's map, rather than
// stopping dead at the first file that needs it.
//
// The withheld disk is a middle one because the catalog is written at the end
// of the last: without that disk there is no catalog and nothing to open.
func TestFillMissingDisk(t *testing.T) {
	t.Parallel()
	want := test1step.DefaultFiles()
	paths := writeDisks(t, test1step.Options{Disks: 3}, nil)
	a := openPaths(t, paths[0], paths[2]) // disk 2 withheld

	var filled, clean int
	for i := range a.Files() {
		f := &a.Files()[i]
		got, res := fill(t, a.OpenFile(f))
		if res.Err != nil {
			t.Errorf("%s: reading with a gap log: %s", f.Path, res.Err)
			continue
		}
		if int64(len(got)) != f.Size {
			t.Errorf("%s: recovered %d of %d bytes; a missing disk is a measured hole, not a short file",
				f.Path, len(got), f.Size)
			continue
		}
		if res.Damaged() {
			filled++
		} else {
			clean++
		}
		if !bytes.Equal(got, zeroed(want[i].Data, res.Gaps)) {
			t.Errorf("%s: the recovered bytes are not the file's with zeros only where the gaps are", f.Path)
		}
	}
	if filled == 0 {
		t.Error("the withheld disk cost nothing, so the test proves nothing")
	}
	if clean == 0 {
		t.Error("every file was filled; the files wholly on disk 1 must still come back")
	}
}

// A file whose chunks were zero-filled must not also be reported as the wrong
// length or as failing its checksum: the gap already says what happened, and a
// CRC over zeros is noise.
func TestFilledChunkIsNotAlsoCalledCorrupt(t *testing.T) {
	t.Parallel()
	paths := writeDisks(t, test1step.Options{Disks: 3}, nil)
	a := openPaths(t, paths[0], paths[2])

	var checked int
	for i := range a.Files() {
		f := &a.Files()[i]
		r := a.OpenFile(f)
		_, res := fill(t, r)
		if !res.Damaged() {
			continue
		}
		checked++
		if errs := r.Errors(); len(errs) > 0 {
			t.Errorf("%s: filled and then also reported %v", f.Path, errs)
		}
	}
	if checked == 0 {
		t.Error("no file was filled, so the test proves nothing")
	}
}

// Without an armed log the read still latches at the damage, so a caller with
// nowhere to record a hole cannot be handed one.
func TestWithoutAGapLogTheReadLatches(t *testing.T) {
	t.Parallel()
	paths := writeDisks(t, test1step.Options{Disks: 3}, nil)
	a := openPaths(t, paths[0], paths[2])

	for i := range a.Files() {
		f := &a.Files()[i]
		r := a.OpenFile(f)
		// A log the caller builds for itself is inert: only ddrescue.Fill arms
		// one, and it writes the map in the same call.
		r.Fill(&ddrescue.GapLog{})
		n, err := io.Copy(io.Discard, r)
		if err == nil {
			continue
		}
		if n >= f.Size {
			t.Errorf("%s: an unarmed log still delivered the whole file", f.Path)
		}
		return
	}
	t.Error("no file was stopped by the withheld disk")
}

// A chunk whose recorded original size is longer than the file itself must not
// put zeros past the end of the file: the map beside it describes the file's
// own bytes, and zeros nothing accounts for are what a later tool decodes as
// data. The file's declared size is the authority on what the reader delivers,
// and the chunk claiming more is reported.
func TestFillIsBoundedByTheFileSize(t *testing.T) {
	t.Parallel()
	const over = 4096
	a := build(t, test1step.Options{LongOrigSize: map[int]int64{4: over}})

	f := fileNamed(t, a, "BIG.BIN")
	got, filled := fill(t, a.OpenFile(f))
	if filled.Err != nil {
		t.Fatalf("filling: %s", filled.Err)
	}
	if int64(len(got)) != f.Size {
		t.Errorf("recovered %d bytes, want the declared %d", len(got), f.Size)
	}
	for _, g := range filled.Gaps {
		if end := g.FileOffset + g.Length; end > f.Size {
			t.Errorf("a gap runs to %d, past the file's %d bytes", end, f.Size)
		}
	}
}
