package qic

import (
	"errors"
	"fmt"
)

// QIC-122 as MS Backup writes it. The stream is read most significant bit
// first: a 0 bit introduces a literal byte, a 1 bit a match whose offset is a
// displacement back from the current output position. The QIC-122B document
// describes the offset as an absolute index into the history buffer, which is
// not what the archives hold.
const (
	shortOffset = 7 // bits in an offset under 128
	longOffset  = 11
	minMatch    = 2

	// minToken is the shortest token, a literal's flag bit plus its byte. A
	// segment's payload ends on a byte boundary, so fewer bits than this left
	// over are the zero padding after the terminator rather than a token.
	minToken = 9

	// maxExpansion is the most bytes one compressed byte can produce. A long
	// match costs four bits per fifteen bytes copied plus a fixed head, so
	// eight bits buy at most thirty bytes. Real segments reach six to one --
	// far past the 32 KiB QIC-113 assumes a segment stays under, the
	// assumption the stored-payload flag was designed around.
	maxExpansion = 30

	// maxSegmentOutput bounds what a segment may expand to when its own
	// offsets do not say. It is above what a full payload could reach, so it
	// never turns away a sound segment, while keeping a corrupt length from
	// asking for an unbounded allocation.
	maxSegmentOutput = maxExpansion * segPayload
)

var (
	// ErrCorruptStream covers a stream that decodes to something impossible.
	ErrCorruptStream = errors.New("qic: corrupt QIC-122 stream")
	// ErrTruncatedStream is a stream that ends part way through a token.
	ErrTruncatedStream = errors.New("qic: QIC-122 stream ends mid-token")
	// ErrOffsetOutOfRange is a match reaching back past the start of its own
	// segment. Each segment is decoded on its own and its output is the whole
	// history, so there is nothing before its first byte to copy from: a match
	// asking for one is damage rather than a wrap into what came before.
	ErrOffsetOutOfRange = errors.New("qic: QIC-122 match offset precedes the segment")
)

// decompress expands one segment into dst, which bounds the output: a segment
// never yields more than the segment index says it does. It returns the bytes
// recovered before any failure, since a partly decoded segment still holds the
// data for whatever files start in its readable part.
//
// dst doubles as the history. Because offsets are displacements rather than
// indexes, a match copies from dst[n-off] a byte at a time, which also makes
// an overlapping match repeat as intended.
func decompress(dst, src []byte) (int, error) {
	r := bitReader{src: src}
	var n int
	for r.remaining() >= minToken {
		bit, err := r.bit()
		if err != nil {
			return n, ErrTruncatedStream
		}
		if bit == 0 {
			b, err := r.bits(8)
			if err != nil {
				return n, ErrTruncatedStream
			}
			if n == len(dst) {
				return n, fmt.Errorf("%w: more output than the segment declares", ErrCorruptStream)
			}
			dst[n] = byte(b & 0xff)
			n++
			continue
		}

		off, err := r.offset()
		if err != nil {
			return n, ErrTruncatedStream
		}
		if off == 0 {
			return n, nil // the end of this segment's stream
		}
		if off > n {
			return n, fmt.Errorf("%w: offset %d with %d bytes decoded", ErrOffsetOutOfRange, off, n)
		}
		length, err := r.matchLength(len(dst) - n)
		if err != nil {
			return n, err
		}
		if n+length > len(dst) {
			return n, fmt.Errorf("%w: more output than the segment declares", ErrCorruptStream)
		}
		for range length {
			dst[n] = dst[n-off]
			n++
		}
	}
	// The payload ran out without a terminator. Some segments are written that
	// way; whether anything was lost is settled by comparing what came back
	// against the length the segment index expects, not here.
	return n, nil
}

// bitReader reads the most significant bit of each byte first.
type bitReader struct {
	src []byte
	pos int // in bits
}

func (r *bitReader) remaining() int { return len(r.src)*8 - r.pos }

func (r *bitReader) bit() (uint32, error) {
	if r.pos >= len(r.src)*8 {
		return 0, ErrTruncatedStream
	}
	b := r.src[r.pos/8] >> (7 - r.pos%8) & 1
	r.pos++
	return uint32(b), nil
}

func (r *bitReader) bits(n int) (uint32, error) {
	var v uint32
	for range n {
		b, err := r.bit()
		if err != nil {
			return 0, err
		}
		v = v<<1 | b
	}
	return v, nil
}

// offset reads a match displacement. One selector bit chooses its width, and
// zero is the segment terminator rather than a displacement.
func (r *bitReader) offset() (int, error) {
	sel, err := r.bit()
	if err != nil {
		return 0, err
	}
	width := longOffset
	if sel == 1 {
		width = shortOffset
	}
	off, err := r.bits(width)
	if err != nil {
		return 0, err
	}
	return int(off), nil
}

// matchLength reads a match length: two 2-bit groups covering 2 to 7, then
// 4-bit groups extending it without limit. limit is what remains of the output
// buffer, which bounds the escape chain -- an all-ones payload would otherwise
// build a length forever, since nothing in the encoding caps it.
func (r *bitReader) matchLength(limit int) (int, error) {
	length := minMatch
	for range 2 {
		v, err := r.bits(2)
		if err != nil {
			return 0, ErrTruncatedStream
		}
		if v < 3 {
			return length + int(v), nil
		}
		length += 3
	}
	for {
		v, err := r.bits(4)
		if err != nil {
			return 0, ErrTruncatedStream
		}
		if v < 15 {
			return length + int(v), nil
		}
		length += 15
		if length > limit {
			return 0, fmt.Errorf("%w: match length exceeds the segment", ErrCorruptStream)
		}
	}
}
