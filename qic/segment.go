package qic

import (
	"encoding/binary"
	"io"
	"sort"
)

// SegmentState says what the segment index made of a segment.
type SegmentState uint8

const (
	SegmentOK SegmentState = iota
	// SegmentHeaderLost means the segment's ten-byte header is not usable: it
	// would not read, or what it holds cannot be one. Nothing then places the
	// segment in the uncompressed stream, so it bounds neither itself nor the
	// segment before it.
	SegmentHeaderLost
	// SegmentCumBroken means the recorded offset does not continue the chain
	// and was replaced by the running total.
	SegmentCumBroken
	// SegmentTerminator is a segment header declaring no payload, which ends
	// the data region.
	SegmentTerminator
	// SegmentLengthUnknown means no later header survived to bound it.
	SegmentLengthUnknown
)

func (s SegmentState) String() string {
	switch s {
	case SegmentOK:
		return "ok"
	case SegmentHeaderLost:
		return "header lost"
	case SegmentCumBroken:
		return "offset broken"
	case SegmentTerminator:
		return "terminator"
	case SegmentLengthUnknown:
		return "length unknown"
	}
	return "unknown"
}

// Segment is one 29696-byte slot of the grid.
type Segment struct {
	Index      int
	FileOffset int64 // where its header sits, or its payload when uncompressed
	Payload    int64 // compressed bytes it holds
	Raw        bool  // its payload is stored, not compressed
	Catalog    bool  // it belongs to the catalog rather than the data region
	Start      int64 // offset of its first byte in the uncompressed stream
	Length     int64 // uncompressed bytes it yields, -1 when unknown
	State      SegmentState
}

// End is the uncompressed offset one past the segment's last byte.
func (s Segment) End() int64 {
	if s.Length < 0 {
		return s.Start
	}
	return s.Start + s.Length
}

// segIndex maps uncompressed-stream offsets to the segments holding them.
type segIndex struct {
	compressed bool // segments carry a header and a QIC-122 payload
	segs       []Segment
	dataSegs   int   // how many of segs precede the catalog
	dataLength int64 // uncompressed bytes the data region yields
	catalogLen int64 // payload bytes the catalog region holds
}

// buildSegIndex reads one segment header per 29696 bytes and turns them into
// uncompressed offset ranges.
//
// A segment's Start is its own recorded offset, never a running total. That is
// what makes each segment an independent anchor: a corrupt header, or a
// segment that will not expand, then costs only itself instead of shifting
// everything after it. The running total is kept as a cross-check.
//
// A segment's Length comes from the next segment's Start. It is the only way
// to know it without expanding everything in between, and it makes the
// decompressor's output something checked against the index rather than the
// input the index is built from.
func buildSegIndex(r io.ReaderAt, lay layout, v Volume, diag diagFunc) *segIndex {
	if !lay.compressed {
		return plainSegIndex(lay, v)
	}
	x := &segIndex{compressed: true}

	head := make([]byte, segHeadSize)
	// prevStart is the last offset the index trusted. A segment is judged
	// against that rather than against an expected size, since a compressed
	// segment can yield either side of a segment's worth.
	var prevStart int64
	var haveStart, terminated bool

	// limit is every byte the volume claims to hold, data and directory both,
	// since a catalog segment's offset continues the same count.
	var limit int64
	if v.DataSize > 0 && v.DirSize > 0 {
		limit = v.DataSize + v.DirSize + SegmentSize
	}
	for i := range lay.segments {
		off := lay.headerBytes + int64(i)*SegmentSize
		seg := Segment{
			Index:      i,
			FileOffset: off,
			Catalog:    off >= lay.catalogStart,
			Start:      prevStart,
			Length:     -1,
		}

		if _, err := r.ReadAt(head, off); err != nil {
			seg.State = SegmentHeaderLost
			diag(DiagSegmentUnreadable, "the header of segment %d at 0x%x could not be read: %v", i, off, err)
			x.segs = append(x.segs, seg)
			continue
		}

		cum := int64(binary.LittleEndian.Uint32(head[0:4]))
		if hi := binary.LittleEndian.Uint32(head[4:8]); hi != 0 {
			diag(DiagSegmentInconsistent, "segment %d records an offset above 4 GiB, which no archive here uses", i)
		}
		size := binary.LittleEndian.Uint16(head[8:10])
		seg.Raw = size&rawSegFlag != 0
		seg.Payload = int64(size & ^uint16(rawSegFlag))

		if seg.Payload == 0 && !seg.Catalog {
			// The documented end of the data region. Neither real archive
			// carries one, so it is honored where it appears and not relied
			// on where it does not — but only where the rest of the header
			// agrees that the data is complete. A header damaged to zeros
			// declares no payload too, and taking that for a terminator hands
			// the zeroed segment the whole remaining span and discards the
			// readable segment before it.
			if endsData(cum, prevStart, haveStart, v) {
				seg.State = SegmentTerminator
				seg.Length = 0
				x.segs = append(x.segs, seg)
				terminated = true
				continue
			}
			seg.State = SegmentHeaderLost
			diag(DiagSegmentUnreadable,
				"segment %d at 0x%x declares no payload at offset %d, short of the %d bytes of data the volume table records, so its header is taken as damaged rather than as the end of the data",
				i, off, cum, v.DataSize)
			x.segs = append(x.segs, seg)
			continue
		}
		if seg.Payload > segPayload {
			diag(DiagSegmentInconsistent,
				"segment %d claims a %d-byte payload where a segment holds %d, so it is read to the segment's end",
				i, seg.Payload, int64(segPayload))
			seg.Payload = segPayload
		}

		// An offset going backwards would break the binary search that finds a
		// segment by offset, and one past the end of everything the volume
		// records cannot be a position in it. A forward gap is left alone: it
		// leaves a range no segment covers, which is reported when something
		// reads it rather than being papered over here. Nothing is inferred
		// from a segment's size, since a segment expands by anything from a
		// little under one to one to better than six to one.
		switch {
		case i == 0 && cum != 0:
			seg.State = SegmentCumBroken
			diag(DiagSegmentInconsistent,
				"the first segment records offset %d where the data region begins at 0", cum)
		case haveStart && cum < prevStart:
			seg.State = SegmentCumBroken
			diag(DiagSegmentInconsistent,
				"segment %d records offset %d, behind segment %d at %d, so its own count is discarded",
				i, cum, i-1, prevStart)
		case limit > 0 && cum > limit:
			seg.State = SegmentCumBroken
			diag(DiagSegmentInconsistent,
				"segment %d records offset %d, past the %d bytes the volume table accounts for, so its own count is discarded",
				i, cum, limit)
		default:
			seg.Start = cum
			prevStart, haveStart = cum, true
		}

		x.segs = append(x.segs, seg)
	}

	x.fixLengths(lay, v, terminated, diag)
	return x
}

// endsData decides whether a header declaring no payload really ends the data
// region. A header damaged to zeros declares no payload too, so the rest of it
// has to agree: the offset it records must continue the chain rather than run
// backwards, and must have reached the byte count the volume table gives the
// data.
func endsData(cum, prevStart int64, haveStart bool, v Volume) bool {
	if haveStart && cum < prevStart {
		return false
	}
	if v.DataSize > 0 {
		return cum >= v.DataSize
	}
	return true
}

// placed reports whether the segment's own header gave it a position in the
// uncompressed stream. One without it bounds nothing: it neither ends the
// segment before it nor opens a range of its own.
func (s Segment) placed() bool {
	return s.State != SegmentHeaderLost && s.State != SegmentCumBroken
}

// nextPlaced is the first segment at or after from whose header placed it.
func (x *segIndex) nextPlaced(from int) int {
	for j := from; j < len(x.segs); j++ {
		if x.segs[j].placed() {
			return j
		}
	}
	return -1
}

// lastPlaced is the last segment before end whose header placed it.
func (x *segIndex) lastPlaced(end int) int {
	for j := min(end, len(x.segs)) - 1; j >= 0; j-- {
		if x.segs[j].placed() {
			return j
		}
	}
	return -1
}

// fixLengths turns the recorded offsets into lengths, which is the next
// recorded offset less this one. The data region's total comes from the first
// catalog segment, whose count continues past the end of the data.
//
// A segment whose header is lost records no offset, so nothing measures where
// it begins or how much it holds: it keeps Length -1 and a read that reaches it
// latches. What *is* measured is the span between the two recorded offsets
// surrounding such a run, and that span belongs to the readable segment that
// opens it — its own stream fills as much of the span as survived and the rest
// is a hole of exactly the distance the two offsets put between them. Cutting
// the readable segment to a segment's worth instead, and giving the lost one
// the remainder, puts the hole at a boundary nobody recorded.
func (x *segIndex) fixLengths(lay layout, v Volume, terminated bool, diag diagFunc) {
	for i := range x.segs {
		seg := &x.segs[i]
		if !seg.placed() {
			continue
		}
		switch next := x.nextPlaced(i + 1); {
		case next >= 0:
			seg.Length = x.segs[next].Start - seg.Start
		case v.DataSize > seg.Start:
			seg.Length = v.DataSize - seg.Start
		case seg.Raw:
			// A stored segment's payload is its output, so it bounds itself.
			seg.Length = seg.Payload
		default:
			// Nothing bounds it. A compressed segment expands by anything from
			// a little under one to better than six to one, so its payload
			// does not measure its output, and the length is not guessed.
			seg.Length = -1
			seg.State = SegmentLengthUnknown
		}
	}

	// Park the segments no header placed at the end of the span they sit in, so
	// they cover nothing and the index stays ascending for the binary search
	// that finds a segment by offset.
	var end int64
	for i := range x.segs {
		if x.segs[i].placed() {
			end = x.segs[i].End()
			continue
		}
		if next := x.nextPlaced(i + 1); next >= 0 {
			x.segs[i].Start = x.segs[next].Start
		} else {
			x.segs[i].Start = end
		}
	}

	x.dataSegs = min(lay.dataSegments, len(x.segs))
	// The catalog is stored, so its own payload lengths are what it yields and
	// the running data total ends where the catalog's count begins.
	for i := x.dataSegs; i < len(x.segs); i++ {
		x.segs[i].Length = x.segs[i].Payload
		x.catalogLen += x.segs[i].Payload
	}
	if x.dataSegs < len(x.segs) {
		x.dataLength = x.segs[x.dataSegs].Start
		// Every catalog segment's offset continues the same count, so the last
		// placed data segment's length follows from the first catalog
		// segment's, and with it the span of any lost segments after it.
		if last := x.lastPlaced(x.dataSegs); last >= 0 {
			s := &x.segs[last]
			if s.Length < 0 || s.Start+s.Length != x.dataLength {
				s.Length = max(x.dataLength-s.Start, 0)
			}
		}
	} else if n := len(x.segs); n > 0 {
		x.dataLength = x.segs[n-1].End()
	}

	if v.DataSize > 0 && x.dataLength != v.DataSize {
		diag(DiagDataRegionShort,
			"the volume table records %d bytes of data but the segment offsets account for %d",
			v.DataSize, x.dataLength)
	}
	if v.DirSize > 0 && x.catalogLen < v.DirSize {
		diag(DiagCatalogTruncated,
			"the volume table records a %d-byte directory but the catalog segments hold %d",
			v.DirSize, x.catalogLen)
	}
	if terminated && x.dataLength != v.DataSize {
		diag(DiagSegmentInconsistent,
			"a segment terminator ends the data at %d where the volume table records %d",
			x.dataLength, v.DataSize)
	}
}

// plainSegIndex synthesizes the index for an uncompressed archive, which has
// no segment headers at all. Every later stage then works the same way, which
// also means an uncompressed archive exercises the record, tree and offset
// code with the decompressor taken out of the path.
func plainSegIndex(lay layout, v Volume) *segIndex {
	x := &segIndex{dataSegs: lay.dataSegments}
	for i := range lay.segments {
		off := lay.headerBytes + int64(i)*SegmentSize
		catalog := off >= lay.catalogStart
		x.segs = append(x.segs, Segment{
			Index:      i,
			FileOffset: off,
			Payload:    SegmentSize,
			Raw:        true,
			Catalog:    catalog,
			Start:      int64(i) * SegmentSize,
			Length:     SegmentSize,
		})
		if catalog {
			x.catalogLen += SegmentSize
		}
	}
	x.dataLength = int64(lay.dataSegments) * SegmentSize
	if v.DataSize > 0 && v.DataSize < x.dataLength {
		x.dataLength = v.DataSize
	}
	return x
}

// find returns the index of the segment holding the given uncompressed offset.
func (x *segIndex) find(off int64) (int, bool) {
	i := sort.Search(x.dataSegs, func(i int) bool { return x.segs[i].End() > off })
	if i >= x.dataSegs || x.segs[i].Start > off {
		return 0, false
	}
	return i, true
}

// gapAt measures a forward gap in the index at off: a range no segment covers,
// left when a lost header took its own start with it while a later one still
// says where the stream resumes. buildSegIndex leaves such a gap alone rather
// than closing it, because a segment's start is its own recorded count and not
// a running total, so the distance to the segment that resumes is exact and the
// range can be reported and filled.
func (x *segIndex) gapAt(off int64) (segment int, length int64, ok bool) {
	i := sort.Search(x.dataSegs, func(i int) bool { return x.segs[i].End() > off })
	if i >= x.dataSegs || x.segs[i].Start <= off {
		return 0, 0, false
	}
	return i, x.segs[i].Start - off, true
}

// intact reports whether every segment covering the range yields the length
// its offset claims, so a file can be marked unrecoverable before anything
// tries to read it.
func (x *segIndex) intact(off, n int64) bool {
	if off < 0 || n < 0 || off+n > x.dataLength {
		return false
	}
	for pos := off; pos < off+n; {
		i, ok := x.find(pos)
		if !ok || x.segs[i].State != SegmentOK || x.segs[i].Length <= 0 {
			return false
		}
		pos = x.segs[i].End()
	}
	return true
}
