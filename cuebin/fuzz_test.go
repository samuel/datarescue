package cuebin

import (
	"strings"
	"testing"
)

// A cue sheet is a text file an operator or another tool wrote, and every
// address in it is the arithmetic each track boundary rests on: a negative or
// non-monotonic bound indexes the per-sector slices verify builds. The
// invariant is parse anything, panic on nothing, and return bounds that are in
// order and inside the file.

func FuzzSheet(f *testing.F) {
	f.Add(`FILE "disc.bin" BINARY
  TRACK 01 MODE1/2352
    INDEX 01 00:00:00
  TRACK 02 AUDIO
    PREGAP 00:02:00
    INDEX 00 00:09:00
    INDEX 01 00:10:00
  TRACK 03 AUDIO
    POSTGAP 00:01:00
    INDEX 01 00:22:00
`, int64(2000*2352))
	f.Add("FILE \"d.bin\" BINARY\n  TRACK 01 MODE2/2336\n    INDEX 01 00:00:00\n", int64(2336))
	f.Add("FILE \"d.bin\" BINARY\n  TRACK 01 AUDIO\n", int64(0))
	f.Add("", int64(0))
	f.Add("TRACK 01 AUDIO", int64(-1))

	f.Fuzz(func(t *testing.T, text string, binSize int64) {
		sheet, err := ParseSheet(strings.NewReader(text))
		if err != nil {
			return
		}
		for _, cf := range sheet.Files {
			for _, tr := range cf.Tracks {
				for _, idx := range tr.Indices {
					if lba := idx.Position.LBA(); lba < 0 || lba > maxCueSector {
						t.Fatalf("track %d INDEX %02d is at LBA %d", tr.Number, idx.Number, lba)
					}
				}
			}
		}
		if binSize < 0 {
			return
		}
		bounds, err := TrackBounds(sheet, binSize)
		if err != nil {
			return
		}
		size, err := BinSectorSize(sheet.Files[0].Tracks)
		if err != nil {
			t.Fatalf("TrackBounds succeeded where BinSectorSize failed: %v", err)
		}
		total := binSize / int64(size)
		for i, b := range bounds {
			if b[0] < 0 || b[1] < b[0] {
				t.Fatalf("track %d bounds = %v; a track cannot start before zero or end before it starts", i+1, b)
			}
			if i > 0 && b[0] != bounds[i-1][1] {
				t.Fatalf("track %d starts at %d where track %d ends at %d; the bounds must be contiguous",
					i+1, b[0], i, bounds[i-1][1])
			}
			// Only the last track is bounded by the file; the others come from
			// the sheet's own addresses and may point to sectors past its end.
			if i == len(bounds)-1 && b[1] > max(total, b[0]) {
				t.Fatalf("the last track ends at %d, past the %d sectors the file holds", b[1], total)
			}
		}
	})
}
