package qic_test

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"samuelks.com/datarescue/ddrescue"
	"samuelks.com/datarescue/internal/testqic"
	"samuelks.com/datarescue/qic"
)

// fill reads r through ddrescue.Fill, which is the only thing that arms a gap
// log — a reader cannot zero-fill anywhere else, so this is also the only way
// to test that it does. It returns what came back and the holes recorded.
func fill(t *testing.T, r *qic.Reader) ([]byte, ddrescue.Filled) {
	t.Helper()
	var data, mapped nopCloser
	f, err := ddrescue.Fill(&data,
		func() (io.WriteCloser, string, error) { return &mapped, "out.map", nil },
		func(l *ddrescue.GapLog) (io.Reader, error) {
			r.Fill(l)
			return r, nil
		})
	if err != nil {
		t.Fatalf("writing the map beside the data: %v", err)
	}
	return data.Bytes(), f
}

type nopCloser struct{ bytes.Buffer }

func (*nopCloser) Close() error { return nil }

// want is the file set's content by path, for comparing what came back against
// what the archive was built from.
func want(files []testqic.File) map[string][]byte {
	out := map[string][]byte{}
	for _, f := range files {
		if !f.IsDir {
			out[strings.ToUpper(f.Path())] = f.Data
		}
	}
	return out
}

// A segment that will not expand costs the bytes it held and nothing else. With
// a log to write the holes down in, the file comes back at its declared length
// with zeros exactly over the lost segment — the length being the distance to
// where the next segment says the stream resumes, which is measured rather than
// guessed.
func TestFillBadSegment(t *testing.T) {
	t.Parallel()
	files := testqic.DefaultFiles()
	a, err := open(testqic.Options{Compress: true, Files: files, BadSegment: 1})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer a.Close() //nolint:errcheck // read-only

	byPath := want(files)
	var filledFiles, cleanFiles int
	for i := range a.Files() {
		f := &a.Files()[i]
		if f.Dir {
			continue
		}
		got, res := fill(t, a.OpenFile(f))
		if res.Err != nil {
			t.Errorf("%s: reading with a gap log: %v", f.Path, res.Err)
			continue
		}
		if int64(len(got)) != f.Size {
			t.Errorf("%s: recovered %d bytes; want the file's full %d", f.Path, len(got), f.Size)
			continue
		}

		orig := byPath[strings.ToUpper(f.Path)]
		if !res.Damaged() {
			cleanFiles++
			if !bytes.Equal(got, orig) {
				t.Errorf("%s: came back changed although nothing was filled", f.Path)
			}
			continue
		}
		filledFiles++

		// Every hole is zeros, and it lies inside the file.
		var lastGapEnd int64
		for _, g := range res.Gaps {
			if g.FileOffset < 0 || g.FileOffset+g.Length > f.Size {
				t.Fatalf("%s: gap %d+%d falls outside the file's %d bytes", f.Path, g.FileOffset, g.Length, f.Size)
			}
			hole := got[g.FileOffset : g.FileOffset+g.Length]
			if !bytes.Equal(hole, make([]byte, len(hole))) {
				t.Errorf("%s: the gap at %d is not zeros", f.Path, g.FileOffset)
			}
			lastGapEnd = max(lastGapEnd, g.FileOffset+g.Length)
		}
		// Past the last hole the bytes are the file's own, at the offsets the
		// file gives them: that is the alignment claim, and it is what a hole
		// of the wrong length would break.
		//
		// Before a hole they need not be. A corrupted QIC-122 stream decodes
		// to wrong bytes for a while and then fails, and nothing in the format
		// can tell those from good ones — which is the reason the map beside
		// the file has to say where the damage was.
		if !bytes.Equal(got[lastGapEnd:], orig[lastGapEnd:]) {
			t.Errorf("%s: the bytes past the last hole are not the file's; the hole was the wrong length", f.Path)
		}
	}
	if filledFiles == 0 {
		t.Error("no file was filled, so the test proves nothing")
	}
	if cleanFiles == 0 {
		t.Error("every file was filled; a segment's damage must not reach the files after it")
	}
}

// Filling one file must not disturb the next. Each segment carries its own
// count of the bytes before it, so a hole is bounded by the segment that
// follows and the file after it is unaffected.
func TestFillLeavesTheNextFileAlone(t *testing.T) {
	t.Parallel()
	files := testqic.DefaultFiles()
	a, err := open(testqic.Options{Compress: true, Files: files, BadSegment: 1})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer a.Close() //nolint:errcheck // read-only

	// Fill every file, then read them all again plainly: a reader that had
	// shifted anything would show it the second time round.
	for i := range a.Files() {
		if f := &a.Files()[i]; !f.Dir {
			fill(t, a.OpenFile(f))
		}
	}
	whole, lost := survivors(t, a, files)
	if whole == 0 {
		t.Error("no file read back whole after the archive had been filled")
	}
	if lost == 0 {
		t.Error("the corrupt segment cost nothing, so the test proves nothing")
	}
}

// Without an armed log the read still latches at the damage, so a caller with
// nowhere to record a hole cannot be handed one.
func TestWithoutAGapLogTheReadLatches(t *testing.T) {
	t.Parallel()
	files := testqic.DefaultFiles()
	a, err := open(testqic.Options{Compress: true, Files: files, BadSegment: 1})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer a.Close() //nolint:errcheck // read-only

	for i := range a.Files() {
		f := &a.Files()[i]
		if f.Dir {
			continue
		}
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
	t.Error("no file was stopped by the corrupt segment")
}
