package ddrescue

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// Fill is the only thing that arms a log, and it always writes the map.
func TestFillWritesTheMap(t *testing.T) {
	t.Parallel()
	const size = 1000
	var data, mapped closableBuffer
	var opened bool

	f, err := Fill(&data, dest(&mapped, "out.map", &opened), func(l *GapLog) (io.Reader, error) {
		if !l.Filling() {
			t.Error("Fill handed the reader an unarmed log")
		}
		l.Limit(size)
		l.Add(100, 50, errUnreadable)
		return bytes.NewReader(make([]byte, size)), nil
	})
	if err != nil {
		t.Fatalf("Fill: %v", err)
	}
	if f.Bytes != size || f.GapBytes != 50 || f.MapName != "out.map" {
		t.Errorf("Filled = %+v; want %d bytes, 50 lost, out.map", f, size)
	}
	if !f.Damaged() {
		t.Error("Damaged() is false for a filled file")
	}

	blocks, err := parseBlocks(mapped.String())
	if err != nil {
		t.Fatalf("the map Fill wrote does not parse: %v", err)
	}
	bad := Unrecovered(blocks, 0, size)
	if len(bad) != 1 || bad[0].Offset != 100 || bad[0].Size != 50 {
		t.Errorf("the map records %v; want one range of 50 bytes at 100", bad)
	}
}

// No holes means no map: a sidecar beside a file says the file is damaged, so
// one must not appear beside a file that came back whole.
func TestFillWritesNoMapWhenWhole(t *testing.T) {
	t.Parallel()
	var data, mapped closableBuffer
	var opened bool
	f, err := Fill(&data, dest(&mapped, "out.map", &opened), func(*GapLog) (io.Reader, error) {
		return bytes.NewReader(make([]byte, 10)), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if opened {
		t.Error("a map was opened for an undamaged file")
	}
	if f.Damaged() || f.MapName != "" {
		t.Errorf("Filled = %+v; want no damage and no map", f)
	}
}

// A map that cannot be written is a failure, not a warning: unrecorded zeros
// are worse than a short file. The data is still written, since it is evidence
// either way.
func TestFillReportsAMapThatCannotBeWritten(t *testing.T) {
	t.Parallel()
	var data closableBuffer
	wantErr := errors.New("no room")
	f, err := Fill(&data, func() (io.WriteCloser, string, error) { return nil, "out.map", wantErr },
		func(l *GapLog) (io.Reader, error) {
			l.Limit(10)
			l.Add(0, 4, errUnreadable)
			return bytes.NewReader(make([]byte, 10)), nil
		})
	if !errors.Is(err, wantErr) {
		t.Errorf("Fill = %v; want the map's error", err)
	}
	if !f.Unrecorded {
		t.Error("Unrecorded is false; the caller has no way to know the zeros went unrecorded")
	}
	if f.MapName != "" {
		t.Errorf("MapName = %q; want none, no map having been written", f.MapName)
	}
	if data.Len() != 10 {
		t.Errorf("the recovered data was not written: %d bytes", data.Len())
	}
}

// A copy that fills a hole and then stops must still get its map, clipped to
// what actually came back. Otherwise the file on disk holds zeros that nothing
// records, which the design forbids.
func TestFillWritesTheMapAfterAFailedCopy(t *testing.T) {
	t.Parallel()
	var data, mapped closableBuffer
	var opened bool
	stop := errors.New("stopped")
	f, err := Fill(&data, dest(&mapped, "out.map", &opened), func(l *GapLog) (io.Reader, error) {
		l.Limit(1000)
		l.Add(10, 20, errUnreadable)   // filled, and delivered
		l.Add(500, 100, errUnreadable) // recorded, but the copy never got there
		return io.MultiReader(bytes.NewReader(make([]byte, 100)), &errReader{stop}), nil
	})
	if err != nil {
		t.Fatalf("Fill returned the copy's error as the map's: %v", err)
	}
	if !errors.Is(f.Err, stop) {
		t.Errorf("Filled.Err = %v; want the copy's error", f.Err)
	}
	if !opened {
		t.Error("no map was written for a file that filled a hole and then failed")
	}
	if len(f.Gaps) != 1 || f.Gaps[0].FileOffset != 10 || f.Gaps[0].Length != 20 {
		t.Errorf("Gaps = %v; want only the hole inside the bytes delivered", f.Gaps)
	}
}

type closableBuffer struct {
	bytes.Buffer
	closed bool
}

func (b *closableBuffer) Close() error { b.closed = true; return nil }

func dest(w *closableBuffer, name string, opened *bool) MapDest {
	return func() (io.WriteCloser, string, error) {
		*opened = true
		return w, name, nil
	}
}

type errReader struct{ err error }

func (r *errReader) Read([]byte) (int, error) { return 0, r.err }

// failingWriter refuses to take the file, the way a full output disk does.
type failingWriter struct {
	err      error
	closeErr error
	n        int64
}

func (w *failingWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	w.n += int64(len(p))
	return len(p), nil
}

func (w *failingWriter) Close() error { return w.closeErr }

// Medium and destination failures are different findings and must be reported
// separately. io.Copy returns one error for both, which would report a full
// output disk as a disc that lost sectors (a damaged medium the operator might
// accept) rather than as a truncated output.
func TestFillKeepsTheDestinationsErrorsOffTheMedium(t *testing.T) {
	t.Parallel()
	full := errors.New("no space left on device")
	var mapped closableBuffer
	var opened bool
	f, err := Fill(&failingWriter{err: full}, dest(&mapped, "out.map", &opened),
		func(l *GapLog) (io.Reader, error) {
			l.Limit(100)
			l.Add(0, 10, errUnreadable)
			return bytes.NewReader(make([]byte, 100)), nil
		})
	if !errors.Is(err, full) {
		t.Errorf("Fill = %v; want the destination's error", err)
	}
	if f.Err != nil {
		t.Errorf("Filled.Err = %v; a write failure is not damage to the medium", f.Err)
	}
	if f.Bytes != 0 || f.Damaged() {
		t.Errorf("Filled = %+v; nothing was written, so there is nothing to call damaged", f)
	}
	if opened {
		t.Error("a map was written for a file that never reached the destination")
	}
}

// A failed close is also a destination error, and must not be hidden behind the
// read error.
func TestFillReportsACloseThatFails(t *testing.T) {
	t.Parallel()
	full := errors.New("no space left on device")
	stop := errors.New("stopped")
	w := &failingWriter{closeErr: full}
	f, err := Fill(w, func() (io.WriteCloser, string, error) {
		t.Error("a map was opened for a file with no holes")
		return nil, "", nil
	}, func(*GapLog) (io.Reader, error) {
		return io.MultiReader(bytes.NewReader(make([]byte, 40)), &errReader{stop}), nil
	})
	if !errors.Is(err, full) {
		t.Errorf("Fill = %v; want the close's error", err)
	}
	if !errors.Is(f.Err, stop) {
		t.Errorf("Filled.Err = %v; want the medium's error", f.Err)
	}
	if f.Bytes != 40 {
		t.Errorf("Filled.Bytes = %d; want the 40 bytes that were written", f.Bytes)
	}
}
