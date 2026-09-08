package testqic

// QIC-122 compression, enough of it to write an archive the reader has to
// decode for real. A synthetic archive built only from literals would leave
// the match path untested, so this matches greedily against the 2048-byte
// window the format allows.

const (
	historySize = 2048
	minMatch    = 2
	maxChain    = 64 // candidates examined per position
)

// bitWriter writes the most significant bit of each byte first, which is the
// order QIC-122 reads them in.
type bitWriter struct {
	buf []byte
	n   int // bits written
}

func (w *bitWriter) bit(v uint32) {
	if w.n%8 == 0 {
		w.buf = append(w.buf, 0)
	}
	if v&1 != 0 {
		w.buf[w.n/8] |= 1 << (7 - w.n%8)
	}
	w.n++
}

func (w *bitWriter) bits(v uint32, n int) {
	for i := n - 1; i >= 0; i-- {
		w.bit(v >> uint(i))
	}
}

// bytes returns the stream padded to a whole byte, which is how a segment's
// payload ends.
func (w *bitWriter) bytes() []byte { return w.buf }

// matchBits is how many bits a match token takes, so the encoder can tell
// whether one still fits in the segment before committing to it.
func matchBits(off, length int) int {
	n := 2 // the token flag and the offset selector
	if off < 128 {
		n += 7
	} else {
		n += 11
	}
	switch {
	case length <= 4:
		return n + 2
	case length <= 7:
		return n + 4
	default:
		n += 4
		for l := length - 8; l >= 15; l -= 15 {
			n += 4
		}
		return n + 4
	}
}

func (w *bitWriter) literal(b byte) {
	w.bit(0)
	w.bits(uint32(b), 8)
}

func (w *bitWriter) match(off, length int) {
	w.bit(1)
	if off < 128 {
		w.bit(1)
		w.bits(uint32(off&0x7f), 7)
	} else {
		w.bit(0)
		w.bits(uint32(off&0x7ff), 11)
	}
	// Two 2-bit groups cover 2 to 7, then 4-bit groups extend it, each all-ones
	// group adding fifteen to what follows.
	switch {
	case length <= 4:
		w.bits(uint32((length-2)&3), 2)
	case length <= 7:
		w.bits(3, 2)
		w.bits(uint32((length-5)&3), 2)
	default:
		w.bits(3, 2)
		w.bits(3, 2)
		l := length - 8
		for l >= 15 {
			w.bits(15, 4)
			l -= 15
		}
		w.bits(uint32(l&0xf), 4)
	}
}

// terminator ends a segment's stream: a match token whose offset is zero.
func (w *bitWriter) terminator() {
	w.bit(1)
	w.bit(1)
	w.bits(0, 7)
}

const terminatorBits = 9

// Compress encodes as much of src as fits in maxOut bytes and returns the
// payload with the count it consumed. The real writer does the same, filling
// each segment and stopping where the next token would not fit, which is why a
// segment's recorded payload length varies from one to the next instead of
// being the constant the format documentation reports.
func Compress(src []byte, maxOut int) (out []byte, consumed int) {
	var w bitWriter
	limit := maxOut*8 - terminatorBits

	head := map[uint16]int{}
	prev := make([]int, len(src))
	for i := range prev {
		prev[i] = -1
	}

	for i := 0; i < len(src); {
		off, length := longestMatch(src, i, head, prev)
		var need int
		if length >= minMatch {
			need = matchBits(off, length)
		} else {
			need = 9
		}
		if w.n+need > limit {
			break
		}
		if length >= minMatch {
			w.match(off, length)
		} else {
			w.literal(src[i])
			length = 1
		}
		// Every position the token covered is indexed, so a later match can
		// start inside a run this one produced.
		for j := i; j < i+length && j+1 < len(src); j++ {
			k := key(src, j)
			prev[j] = head[k]
			head[k] = j
		}
		i += length
		consumed = i
	}
	w.terminator()
	return w.bytes(), consumed
}

func key(src []byte, i int) uint16 {
	return uint16(src[i]) | uint16(src[i+1])<<8
}

// longestMatch finds the nearest longest match for the position, searching a
// bounded number of candidates so building a large archive stays quick.
func longestMatch(src []byte, i int, head map[uint16]int, prev []int) (off, length int) {
	if i+1 >= len(src) {
		return 0, 0
	}
	for cand, n := head[key(src, i)], 0; cand >= 0 && n < maxChain; cand, n = prev[cand], n+1 {
		d := i - cand
		if d <= 0 || d >= historySize {
			break
		}
		// The comparison walks src rather than the candidate, so a match is
		// allowed to run past its own start: the decoder copies a byte at a
		// time from the output it is building, and an overlapping match is
		// how a repeated run is meant to be encoded.
		var l int
		for i+l < len(src) && src[i+l-d] == src[i+l] {
			l++
		}
		if l > length {
			off, length = d, l
		}
	}
	return off, length
}
