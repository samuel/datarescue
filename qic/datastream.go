package qic

import (
	"errors"
	"fmt"
	"io"
	"sync"
)

// errGapInIndex is a stretch of the uncompressed stream that no segment
// accounts for, because the segment headers that would have placed it were
// lost.
var errGapInIndex = errors.New("qic: no segment covers this range")

// SegmentReadError is a segment whose bytes could not be recovered or whose
// stream would not expand. N is what the read delivered before the failure,
// which is kept rather than discarded: a segment that decodes part way still
// holds the data for whatever starts in its readable part.
type SegmentReadError struct {
	Segment int
	Offset  int64 // uncompressed offset of the first byte not recovered
	Length  int64 // uncompressed bytes the segment should have held
	N       int64
	Err     error
}

func (e *SegmentReadError) Error() string {
	return fmt.Sprintf("qic: segment %d: bytes %d..%d were not recovered: %s",
		e.Segment, e.Offset, e.Offset+e.Length, e.Err)
}

func (e *SegmentReadError) Unwrap() error { return e.Err }

// segCacheMax is how many expanded segments are kept. A file's body spans at
// most two at a time, the record check ahead of it one more, and extracting
// many small files returns to the same one; past that a walk of the whole
// archive would hold everything. The cache is dropped whole when it fills, as
// extfs does with its pointer blocks.
const segCacheMax = 4

// dataStream is random access to the uncompressed data region.
//
// It is an io.ReaderAt and holds no read position, because three callers need
// it at once: the check that a file's record really sits where the catalog's
// arithmetic says, the file bodies themselves, and the forward sweep for a
// record signature. A single cursor that latched its own errors would have
// them interfere. Latching belongs to the per-file Reader, alongside the file
// offset it protects.
type dataStream struct {
	r    io.ReaderAt
	x    *segIndex
	diag diagFunc

	mu    sync.Mutex
	cache map[int]*segBuf
	// reported remembers which segments have already been diagnosed, since
	// the cache is dropped whole when it fills and a walk over a large archive
	// would otherwise report the same bad segment once per eviction.
	reported map[int]bool
}

// segBuf is one expanded segment. A failure is cached alongside the data:
// without that, walking a thousand files re-reads and re-reports the same bad
// segment once per file, and through a ddrescue image that is a thousand seeks
// for an answer already known.
type segBuf struct {
	data []byte
	err  error
}

func newDataStream(r io.ReaderAt, x *segIndex, diag diagFunc) *dataStream {
	return &dataStream{r: r, x: x, diag: diag, cache: map[int]*segBuf{}, reported: map[int]bool{}}
}

// Size is the length of the uncompressed data region.
func (s *dataStream) Size() int64 { return s.x.dataLength }

// ReadAt fills p from the uncompressed data region, expanding only the
// segments the range touches.
func (s *dataStream) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("qic: negative offset %d", off)
	}
	var n int
	for n < len(p) {
		pos := off + int64(n)
		if pos >= s.x.dataLength {
			return n, io.EOF
		}
		i, ok := s.x.find(pos)
		if !ok {
			// A range no segment covers is not the end of the data. io.Copy
			// would take io.EOF as a clean end and silently return a short
			// file, while the distance to the segment that resumes is known
			// exactly.
			if seg, length, gap := s.x.gapAt(pos); gap {
				return n, &SegmentReadError{
					Segment: seg, Offset: pos, Length: length, N: int64(n),
					Err: errGapInIndex,
				}
			}
			return n, io.EOF
		}
		seg := s.x.segs[i]
		buf, err := s.segment(i)
		if err != nil {
			return n, &SegmentReadError{
				Segment: i, Offset: seg.Start, Length: seg.Length,
				N: int64(n), Err: err,
			}
		}
		start := pos - seg.Start
		if start >= int64(len(buf.data)) {
			// The segment expanded short of the length its own offset chain
			// claims, so the bytes from here on were never recovered.
			return n, &SegmentReadError{
				Segment: i, Offset: pos, Length: seg.End() - pos,
				N: int64(n), Err: buf.err,
			}
		}
		n += copy(p[n:], buf.data[start:])
	}
	return n, nil
}

// segment expands one segment, or returns what a previous attempt made of it.
func (s *dataStream) segment(i int) (*segBuf, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if buf, ok := s.cache[i]; ok {
		if len(buf.data) == 0 && buf.err != nil {
			return nil, buf.err
		}
		return buf, nil
	}

	buf, err := s.expand(i)
	if err != nil {
		buf = &segBuf{err: err}
	}
	if len(s.cache) >= segCacheMax {
		s.cache = map[int]*segBuf{}
	}
	s.cache[i] = buf
	if err != nil {
		return nil, err
	}
	return buf, nil
}

// expand reads a segment's payload and decompresses it. A segment that yields
// fewer bytes than its offset chain says is reported and kept: the prefix is
// real data, and what follows it in that segment is what was lost.
func (s *dataStream) expand(i int) (*segBuf, error) {
	seg := s.x.segs[i]
	switch seg.State {
	case SegmentHeaderLost:
		return nil, fmt.Errorf("qic: the header of segment %d was not recovered", i)
	case SegmentTerminator:
		return &segBuf{}, nil
	}

	want := seg.Length
	if want < 0 || want > maxSegmentOutput {
		want = maxSegmentOutput
	}
	if seg.Payload <= 0 {
		return &segBuf{}, nil
	}

	src := make([]byte, seg.Payload)
	payloadOff := seg.FileOffset
	if s.x.compressed {
		payloadOff += segHeadSize
	}
	if _, err := readFull(s.r, src, payloadOff); err != nil {
		return nil, fmt.Errorf("qic: reading segment %d at 0x%x: %w", i, payloadOff, err)
	}

	if seg.Raw {
		if int64(len(src)) > want {
			src = src[:want]
		}
		return &segBuf{data: src}, nil
	}

	dst := make([]byte, want)
	n, err := decompress(dst, src)
	buf := &segBuf{data: dst[:n]}
	first := !s.reported[i]
	s.reported[i] = true
	switch {
	case err != nil:
		if first {
			s.diag(DiagSegmentDecompress,
				"segment %d covering %d..%d yielded %d of %d bytes before %v",
				i, seg.Start, seg.End(), n, seg.Length, err)
		}
		buf.err = err
	case seg.Length >= 0 && int64(n) != seg.Length:
		if first {
			s.diag(DiagSegmentShort,
				"segment %d covering %d..%d yielded %d bytes where its offsets account for %d",
				i, seg.Start, seg.End(), n, seg.Length)
		}
		buf.err = fmt.Errorf("qic: the segment's stream ended after %d of %d bytes", n, seg.Length)
	}
	return buf, nil
}

// readFull reads len(p) bytes, turning the EOF that a short ReadAt reports
// into the unexpected EOF it is for a caller that asked for a whole record.
func readFull(r io.ReaderAt, p []byte, off int64) (int, error) {
	n, err := r.ReadAt(p, off)
	if err == io.EOF && n > 0 {
		err = io.ErrUnexpectedEOF
	}
	if n == len(p) {
		return n, nil
	}
	if err == nil {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}
