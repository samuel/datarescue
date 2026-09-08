package cdrom

import "testing"

// FuzzParseFullTOC feeds arbitrary bytes through the Full TOC parser. The
// buffer comes straight off a drive that may be lying about its own data
// length, so parseFullTOC must return an error rather than read out of bounds,
// and the two assemblers must cope with whatever it does return.
func FuzzParseFullTOC(f *testing.F) {
	f.Add(fullTOCResponse(leadInDesc(1, 0x04, 1, 0, 2, 0)))
	f.Add(fullTOCResponse(
		leadInDesc(1, 0x04, 1, 0, 2, 0),
		leadInDesc(1, 0x00, 2, 0, 12, 0),
		inTrackDesc(1, 0x00, 2, 0, 0, 10, 0),
		leadInDesc(1, 0x00, 0xA2, 0, 32, 0),
	))
	f.Add([]byte{0x00, 0x02, 0x01, 0x01})
	f.Add([]byte(nil))

	f.Fuzz(func(t *testing.T, data []byte) {
		descs, err := parseFullTOC(data)
		if err != nil {
			return
		}
		toc := tocFromFullTOC(descs)
		// Whatever the descriptors said, the assembled TOC must be internally
		// consistent enough for the callers that walk it.
		for _, tr := range toc.Tracks {
			if tr.Number < 1 || tr.Number > maxTrackNumber {
				t.Fatalf("track number %d out of range", tr.Number)
			}
			if tr.Number < toc.FirstTrack || tr.Number > toc.LastTrack {
				t.Fatalf("track %d outside the reported range %d-%d",
					tr.Number, toc.FirstTrack, toc.LastTrack)
			}
		}
		toc.HasAudio()
		toc.LastDataSession()
		toc.InPregap(0)
		toc.TrackTypeAt(0)
		for tno := range fullTOCEntries(descs) {
			if tno == 0 {
				t.Fatal("entry for track 0")
			}
		}
	})
}
