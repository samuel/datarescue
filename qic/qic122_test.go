package qic

import (
	"errors"
	"strings"
	"testing"
)

// bitsOf assembles a stream from a string of '0' and '1', ignoring spaces, so
// a test case can be read against the token layout in QICDCOMP.C rather than
// against a hex dump. Trailing bits are zero-padded, which is what a real
// segment does after its terminator.
func bitsOf(t *testing.T, s string) []byte {
	t.Helper()
	s = strings.ReplaceAll(s, " ", "")
	out := make([]byte, (len(s)+7)/8)
	for i, c := range s {
		switch c {
		case '1':
			out[i/8] |= 1 << (7 - i%8)
		case '0':
		default:
			t.Fatalf("bit string contains %q", c)
		}
	}
	return out
}

// TestDecompressTokens checks each token form against a hand-assembled bit
// string, so the decoder is pinned down by something other than this package's
// own compressor agreeing with it.
func TestDecompressTokens(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		bits string
		want string
	}{
		{
			name: "literal",
			bits: "0 01000001",
			want: "A",
		},
		{
			// A match with the short-offset selector, a displacement of 1 and
			// the 2-bit length 01 meaning 3. Repeating one byte three times is
			// only correct if the offset is a displacement and the copy
			// overlaps a byte at a time.
			name: "overlapping match",
			bits: "0 01000001 1 1 0000001 01",
			want: "AAAA",
		},
		{
			name: "length two",
			bits: "0 01000001 0 01000010 1 1 0000010 00",
			want: "ABAB",
		},
		{
			// Both 2-bit groups escape, then a 4-bit group of 0011: 2+3+3+3.
			name: "length eleven via one nibble",
			bits: "0 01000001 1 1 0000001 11 11 0011",
			want: "AAAAAAAAAAAA",
		},
		{
			// The 4-bit chain escapes once: 2+3+3+15+0.
			name: "length via two nibbles",
			bits: "0 01000001 1 1 0000001 11 11 1111 0000",
			want: strings.Repeat("A", 24),
		},
		{
			// The long-offset selector with a displacement of 1 must decode
			// the same as the short form.
			name: "long offset",
			bits: "0 01000001 1 0 00000000001 01",
			want: "AAAA",
		},
		{
			name: "terminator ends the segment",
			bits: "0 01000001 1 1 0000000 0 01000010",
			want: "A",
		},
		{
			name: "exhausted input ends the segment",
			bits: "0 01000001",
			want: "A",
		},
		{
			// Fewer bits left over than a token can be are the padding a
			// segment ends with, not a truncated stream.
			name: "trailing pad is not a token",
			bits: "0 01000001 1 1 0000000 0000",
			want: "A",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dst := make([]byte, 64)
			n, err := decompress(dst, bitsOf(t, tt.bits))
			if err != nil {
				t.Fatalf("decompress: %v", err)
			}
			if got := string(dst[:n]); got != tt.want {
				t.Errorf("decompress = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestDecompressErrors checks that a stream asking for something impossible is
// reported with the bytes recovered before it, rather than reading whatever
// the previous segment left behind the way the reference does.
func TestDecompressErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		bits    string
		dst     int
		want    error
		wantOut string
	}{
		{
			name: "match before the segment start",
			bits: "1 1 0000010 00",
			dst:  64,
			want: ErrOffsetOutOfRange,
		},
		{
			name:    "match reaching past what was decoded",
			bits:    "0 01000001 1 1 0000010 00",
			dst:     64,
			want:    ErrOffsetOutOfRange,
			wantOut: "A",
		},
		{
			name:    "more output than the segment declares",
			bits:    "0 01000001 0 01000010",
			dst:     1,
			want:    ErrCorruptStream,
			wantOut: "A",
		},
		{
			name:    "match longer than the segment declares",
			bits:    "0 01000001 1 1 0000001 11 11 0011",
			dst:     4,
			want:    ErrCorruptStream,
			wantOut: "A",
		},
		{
			// The two length escapes leave only two bits where the next 4-bit
			// group needs four, which is a token the payload does not hold.
			name:    "match length cut off",
			bits:    "0 01000001 1 1 0000001 11 11",
			dst:     64,
			want:    ErrTruncatedStream,
			wantOut: "A",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dst := make([]byte, tt.dst)
			n, err := decompress(dst, bitsOf(t, tt.bits))
			if !errors.Is(err, tt.want) {
				t.Fatalf("decompress error = %v, want %v", err, tt.want)
			}
			if got := string(dst[:n]); got != tt.wantOut {
				t.Errorf("recovered %q, want %q", got, tt.wantOut)
			}
		})
	}
}

// TestMatchLengthBands walks the boundary of each length encoding, since an
// off-by-one in the escape chain desynchronizes the whole segment rather than
// producing a locally wrong byte.
func TestMatchLengthBands(t *testing.T) {
	t.Parallel()

	tests := []struct {
		bits string
		want int
	}{
		{"00", 2},
		{"01", 3},
		{"10", 4},
		{"11 00", 5},
		{"11 01", 6},
		{"11 10", 7},
		{"11 11 0000", 8},
		{"11 11 1110", 22},
		{"11 11 1111 0000", 23},
		{"11 11 1111 1110", 37},
		{"11 11 1111 1111 0000", 38},
	}

	for _, tt := range tests {
		t.Run(tt.bits, func(t *testing.T) {
			t.Parallel()
			r := bitReader{src: bitsOf(t, tt.bits)}
			got, err := r.matchLength(1 << 20)
			if err != nil {
				t.Fatalf("matchLength: %v", err)
			}
			if got != tt.want {
				t.Errorf("matchLength(%s) = %d, want %d", tt.bits, got, tt.want)
			}
		})
	}
}
