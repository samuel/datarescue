package ddrescue

import (
	"fmt"
	"io"
)

// Gap is a range of a file that would not read. A gap-filling reader delivers
// zeros in its place, and the Gap records where those zeros are.
type Gap struct {
	FileOffset int64
	Length     int64
	Err        error
}

func (g Gap) String() string {
	return fmt.Sprintf("%d bytes at offset %d: %s", g.Length, g.FileOffset, g.Err)
}

// GapLog is where a gap-filling reader writes down the holes it zero-filled.
//
// A reader fills only when it is handed an armed log, and only Fill arms one,
// writing the map as it does. A GapLog a caller makes for itself is inert:
// Filling reports false and the reader latches at the damage. This is the
// safety guarantee: zeros are produced only where a map recording them is being
// written. The type enforces it, so extract commands do not have to remember it
// as a rule.
type GapLog struct {
	armed bool
	limit int64 // the file's declared length; 0 when it is not known
	gaps  []Gap
}

// Filling reports whether a reader handed this log should zero-fill rather than
// latch. The nil log and the zero value both mean latch.
func (l *GapLog) Filling() bool { return l != nil && l.armed }

// Limit tells the log how long the file is, so a hole in the unused tail of a
// cluster or block is not reported as lost data. A reader sets it when it is
// handed the log.
func (l *GapLog) Limit(size int64) {
	if l != nil {
		l.limit = size
	}
}

// Add records a hole, clipped to the declared length and coalesced with the
// previous one when they meet.
func (l *GapLog) Add(off, length int64, err error) {
	if !l.Filling() {
		return
	}
	if l.limit > 0 {
		if off >= l.limit {
			return
		}
		length = min(length, l.limit-off)
	}
	if length <= 0 {
		return
	}
	if n := len(l.gaps); n > 0 && l.gaps[n-1].FileOffset+l.gaps[n-1].Length == off {
		l.gaps[n-1].Length += length
		return
	}
	l.gaps = append(l.gaps, Gap{FileOffset: off, Length: length, Err: err})
}

// Gaps returns the holes in file order.
func (l *GapLog) Gaps() []Gap {
	if l == nil {
		return nil
	}
	return l.gaps
}

// Bytes is how much of the file was filled with zeros.
func (l *GapLog) Bytes() int64 {
	var n int64
	for _, g := range l.Gaps() {
		n += g.Length
	}
	return n
}

// ReadFunc reads into buf from the medium at off and reports how much came back
// before any failure. A short read is not an error by itself: through a rescue
// map the recovered prefix arrives and the failure comes after it, and those
// bytes are real data.
type ReadFunc func(off int64, buf []byte) (int, error)

// Chunk is the unit a reader takes the medium in — a cluster, a filesystem
// block, a sector — as Recover needs to see it.
type Chunk struct {
	// FileOffset is where the chunk begins within the file, and ImageOffset
	// where it begins on the medium.
	FileOffset  int64
	ImageOffset int64
	// Buf is the chunk's buffer; its length is the chunk size.
	Buf []byte
	// Unit is the retry granularity, normally the medium's sector size. The
	// whole chunk is one unit when this is 0 or larger than the buffer.
	Unit int
	Read ReadFunc
}

// Recover fills the rest of a chunk whose read failed after have bytes,
// retrying Unit bytes at a time so the hole is the units that will not read
// rather than the whole chunk. Everything not recovered is zeroed and recorded.
//
// have is clamped to the buffer. A caller reporting more bytes than the chunk
// holds is wrong about the read, and trusting it would panic here rather than
// in the caller.
func (l *GapLog) Recover(c Chunk, have int, cause error) {
	size := len(c.Buf)
	unit := c.Unit
	if unit <= 0 || unit > size {
		unit = size
	}
	have = min(max(have, 0), size)
	clear(c.Buf[have:])
	if unit == 0 {
		return
	}

	gapStart := int64(-1)
	closeGap := func(end int64) {
		if gapStart >= 0 {
			l.Add(gapStart, end-gapStart, cause)
			gapStart = -1
		}
	}
	// The unit holding the first byte that did not come back is the one that
	// failed, so the retry starts at the next boundary and the remainder of
	// that unit is lost with it.
	resume := (have + unit - 1) / unit * unit
	if resume > have {
		gapStart = c.FileOffset + int64(have)
	}
	for s := resume; s < size; s += unit {
		end := min(s+unit, size)
		n, err := c.Read(c.ImageOffset+int64(s), c.Buf[s:end])
		n = min(max(n, 0), end-s)
		if err == nil && n < end-s {
			// A ReadFunc is allowed to come back short without an error, and
			// the bytes it did not deliver are zeros. Treating that as success
			// would leave unrecorded zeros in the file, which this type exists
			// to prevent.
			err = io.ErrUnexpectedEOF
		}
		if n > 0 {
			closeGap(c.FileOffset + int64(s))
		}
		if err != nil {
			clear(c.Buf[s+n : end])
			if gapStart < 0 {
				gapStart = c.FileOffset + int64(s) + int64(n)
			}
			continue
		}
		closeGap(c.FileOffset + int64(end))
	}
	closeGap(c.FileOffset + int64(size))
}
