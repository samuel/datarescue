package ddrescue

import "testing"

func FuzzParseBlocks(f *testing.F) {
	f.Add("# Mapfile\n0x00000000     ?               1\n0x00000000  0x00001000  +\n")
	f.Add("0x00000000     ?               1\n   0x0  0x10  -\n")
	f.Add("0x00000000     ?               1\n0x0  0x10  x\n") // unknown status
	f.Add("")
	f.Add("#")
	f.Add("0x0 0x0 +\r\n")

	f.Fuzz(func(t *testing.T, raw string) {
		blocks, err := parseBlocks(raw)
		if err != nil {
			return
		}
		var prev, prevEnd int64 = -1, 0
		for _, b := range blocks {
			if !b.Status.Valid() {
				t.Fatalf("accepted status %q", string(b.Status))
			}
			if b.Offset < 0 || b.Size < 0 {
				t.Fatalf("negative block %+v", b)
			}
			// Read locates blocks by binary search, so the list must be sorted.
			if b.Offset < prev {
				t.Fatalf("blocks are not sorted: %d after %d", b.Offset, prev)
			}
			// Coverage is walked forward from one block to the next, so an
			// overlap would let a finished block admit a failed one's bytes.
			if b.Offset < prevEnd {
				t.Fatalf("block %+v overlaps the one ending at %d", b, prevEnd)
			}
			prev, prevEnd = b.Offset, b.Offset+b.Size
		}
	})
}
