package ddrescue

import (
	"errors"
	"io"
)

// copyChunk is how much of a file moves per write.
const copyChunk = 64 << 10

// MapDest opens the destination for one file's map. It is separate from the
// data writer because the map goes beside the file — a sidecar, or a member of
// its own inside an archive — and is opened only once there is something to put
// in it.
type MapDest func() (w io.WriteCloser, name string, err error)

// Filled is the result of one gap-filled copy.
type Filled struct {
	// Bytes is what came back and was written, holes included.
	Bytes int64
	Gaps  []Gap
	// GapBytes is how much of Bytes is zeros standing in for what would not
	// read.
	GapBytes int64
	// MapName is where the map was written, empty when there were no holes or
	// when the map could not be written.
	MapName string
	// Err is the medium error that stopped the copy, nil when the file came
	// back whole. The bytes recovered before it are still written. A
	// destination failure is never reported here; it is Fill's returned error.
	Err error
	// Unrecorded reports that holes were filled but the map recording them was
	// not written. The file then holds unexplained zeros, which is worse than a
	// short file, so a caller that can discard the file must do so.
	Unrecorded bool
}

// Damaged reports whether anything was zero-filled.
func (f Filled) Damaged() bool { return len(f.Gaps) > 0 }

// Fill copies a gap-filling reader into dst and writes a map recording where
// the zeros are.
//
// open is handed the armed log to give the reader. Nothing else arms a log, so
// a reader can only zero-fill inside this call, and this call always writes the
// map. That includes a copy that failed part way: a file that filled some holes
// and then stopped is where the map matters most. dst is closed before the map
// is opened, because an archive can only have one member open at a time.
//
// Errors from the two sides of the copy are kept apart. A read failure is a
// finding about the medium: it goes in Filled.Err, and the bytes recovered
// before it are kept. A destination failure (a write, its close, or the map) is
// a failure of the run and is the returned error. A full output disk is not a
// damaged disc, and one exit code cannot report both.
func Fill(dst io.WriteCloser, m MapDest, open func(*GapLog) (io.Reader, error)) (Filled, error) {
	log := &GapLog{armed: true}
	var f Filled

	r, err := open(log)
	if err != nil {
		f.Err = err
		return f, dst.Close()
	}

	n, readErr, writeErr := copyFilled(dst, r)
	f.Bytes = n
	f.Err = readErr
	runErr := errors.Join(writeErr, dst.Close())

	// Clip to what was actually delivered: a fill followed by a hard failure
	// must not leave the map describing bytes the file does not have.
	for _, g := range log.Gaps() {
		if g.FileOffset >= n {
			break
		}
		g.Length = min(g.Length, n-g.FileOffset)
		f.Gaps = append(f.Gaps, g)
		f.GapBytes += g.Length
	}
	if len(f.Gaps) == 0 {
		return f, runErr
	}

	w, name, err := m()
	if err != nil {
		f.Unrecorded = true
		return f, errors.Join(runErr, err)
	}
	// Only RecordRange is used, so the builder needs no sector size: a file's
	// loss ends where its bytes stopped coming back, not at a sector boundary
	// of the medium.
	b := NewMapBuilder(0)
	var pos int64
	for _, g := range f.Gaps {
		b.RecordRange(pos, g.FileOffset-pos, false)
		b.RecordRange(g.FileOffset, g.Length, true)
		pos = g.FileOffset + g.Length
	}
	b.RecordRange(pos, n-pos, false)
	_, werr := b.WriteTo(w)
	if mapErr := errors.Join(werr, w.Close()); mapErr != nil {
		f.Unrecorded = true
		return f, errors.Join(runErr, mapErr)
	}
	f.MapName = name
	return f, runErr
}

// copyFilled copies r into dst, reporting read and write failures separately.
// io.Copy returns one error for both, which would report a full output disk as
// a medium that lost data.
func copyFilled(dst io.Writer, r io.Reader) (n int64, readErr, writeErr error) {
	buf := make([]byte, copyChunk)
	for {
		m, rerr := r.Read(buf)
		if !errors.Is(rerr, io.EOF) {
			readErr = rerr
		}
		if m > 0 {
			w, werr := dst.Write(buf[:m])
			n += int64(w)
			if werr == nil && w < m {
				werr = io.ErrShortWrite
			}
			if werr != nil {
				return n, readErr, werr
			}
		}
		if rerr != nil {
			return n, readErr, nil
		}
	}
}
