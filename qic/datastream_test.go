package qic

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// gappedIndex is two stored segments with a stretch between them that no
// segment accounts for: the first covers 0..100 of the uncompressed stream and
// the second resumes at 300.
//
// buildSegIndex tiles the segments it reads, so the synthetic archives never
// reach this defensive path, and this test does. Reporting io.EOF for a range
// no segment covers would let io.Copy treat it as a clean end and return a
// silently short file. The distance to the segment that resumes is known
// exactly, so the gap is reported instead.
func gappedIndex() (*segIndex, io.ReaderAt) {
	payload := make([]byte, 200)
	for i := range payload {
		payload[i] = byte(i%251 + 1) // never zero, so a filled hole is distinguishable
	}
	x := &segIndex{
		segs: []Segment{
			{Index: 0, FileOffset: 0, Payload: 100, Raw: true, Start: 0, Length: 100},
			{Index: 1, FileOffset: 100, Payload: 100, Raw: true, Start: 300, Length: 100},
		},
		dataSegs:   2,
		dataLength: 400,
	}
	return x, bytes.NewReader(payload)
}

func TestReadAtOverAnIndexGapReports(t *testing.T) {
	t.Parallel()
	x, r := gappedIndex()
	s := newDataStream(r, x, func(DiagnosticKind, string, ...any) {})

	p := make([]byte, 250)
	n, err := s.ReadAt(p, 0)
	if n != 100 {
		t.Errorf("delivered %d bytes before the gap; want the first segment's 100", n)
	}
	if errors.Is(err, io.EOF) {
		t.Fatal("a range no segment covers came back as io.EOF, which io.Copy reads as a clean end")
	}
	var sre *SegmentReadError
	if !errors.As(err, &sre) {
		t.Fatalf("error = %v; want a *SegmentReadError", err)
	}
	if sre.Offset != 100 || sre.Length != 200 {
		t.Errorf("the gap is reported as %d+%d; want 100+200, the distance to where the stream resumes",
			sre.Offset, sre.Length)
	}
}

// Reading from inside the gap reports it too, measured from where the caller
// stands rather than from where the missing range began.
func TestReadAtInsideAnIndexGap(t *testing.T) {
	t.Parallel()
	x, r := gappedIndex()
	s := newDataStream(r, x, func(DiagnosticKind, string, ...any) {})

	p := make([]byte, 50)
	n, err := s.ReadAt(p, 200)
	if n != 0 {
		t.Errorf("delivered %d bytes from inside the gap; want none", n)
	}
	var sre *SegmentReadError
	if !errors.As(err, &sre) {
		t.Fatalf("error = %v; want a *SegmentReadError", err)
	}
	if sre.Offset != 200 || sre.Length != 100 {
		t.Errorf("the gap is reported as %d+%d; want 200+100", sre.Offset, sre.Length)
	}
}

// Past the last segment there is no more data and no gap to measure, so io.EOF
// is the right answer and the one a reader has to be able to tell from damage.
func TestReadAtPastTheEndIsEOF(t *testing.T) {
	t.Parallel()
	x, r := gappedIndex()
	s := newDataStream(r, x, func(DiagnosticKind, string, ...any) {})

	if _, err := s.ReadAt(make([]byte, 10), 400); !errors.Is(err, io.EOF) {
		t.Errorf("reading past the data region gave %v; want io.EOF", err)
	}
}

// A segment whose length is unknown, because no later header survived to bound
// it, gives no measurement, so the reader must latch rather than invent a
// hole. This is where the fill has to refuse, under the rule everything else
// rests on: a hole is filled only when its length is arithmetic.
func TestFillRefusesAnUnboundedSegment(t *testing.T) {
	t.Parallel()
	r := &Reader{file: &File{Path: `C:\X.BIN`, Size: 100}}
	if _, ok := r.holeEnd(&SegmentReadError{Segment: 1, Offset: 0, Length: -1}); ok {
		t.Error("a segment of unknown length was filled; its length is not arithmetic, so it must latch")
	}
	if _, ok := r.holeEnd(&SegmentReadError{Segment: 1, Offset: 0, Length: 100}); !ok {
		t.Error("a segment of known length was not filled")
	}
	if _, ok := r.holeEnd(errors.New("something else entirely")); ok {
		t.Error("an error that is not a segment's was filled")
	}
}

// A failure met a second time reports the segment's own start, which is behind
// the reader by then. The hole has to run from where the reader stands, or it
// would re-report bytes already delivered and stall instead of advancing.
func TestFillClampsARepeatedFailureToTheReaderPosition(t *testing.T) {
	t.Parallel()
	// The segment covers data-region bytes 1000..1400, which is file offsets
	// 0..400; the reader is already at 200.
	r := &Reader{file: &File{Path: `C:\X.BIN`, Size: 500, dataOff: 1000}, off: 200}
	end, ok := r.holeEnd(&SegmentReadError{Segment: 1, Offset: 1000, Length: 400})
	if !ok {
		t.Fatal("the hole was refused")
	}
	if end-r.off != 200 {
		t.Errorf("hole of %d bytes; want 200, from where the reader stands to where the segment ends", end-r.off)
	}

	// One entirely behind the reader leaves nothing to fill and must latch,
	// since a reader that neither advances nor stops is a hang.
	r2 := &Reader{file: &File{Path: `C:\X.BIN`, Size: 500, dataOff: 1000}, off: 400}
	if _, ok := r2.holeEnd(&SegmentReadError{Segment: 1, Offset: 1000, Length: 400}); ok {
		t.Error("a hole entirely behind the reader was accepted, which would not advance the read")
	}

	// A segment reaching past the end of the file is clipped to it: the bytes
	// past the file are not the file's to lose.
	r3 := &Reader{file: &File{Path: `C:\X.BIN`, Size: 100, dataOff: 1000}}
	end, ok = r3.holeEnd(&SegmentReadError{Segment: 1, Offset: 1000, Length: 400})
	if !ok || end != 100 {
		t.Errorf("hole ends at %d (ok=%v); want the file's own end at 100", end, ok)
	}
}
