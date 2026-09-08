package cdrom

import (
	"encoding/binary"
	"testing"
)

// fullTOCResponse builds a Full TOC response around descs, filling in the
// header data length the way a drive would.
func fullTOCResponse(descs ...[fullTOCDescLen]byte) []byte {
	buf := make([]byte, 4, 4+len(descs)*fullTOCDescLen)
	// Data length excludes the two length bytes themselves.
	binary.BigEndian.PutUint16(buf[0:2], uint16(2+len(descs)*fullTOCDescLen)) //nolint:gosec // test data, bounded by the descriptors the caller passes
	buf[2] = 1                                                                // first session
	buf[3] = 1                                                                // last session
	for _, d := range descs {
		buf = append(buf, d[:]...)
	}
	return buf
}

// leadInDesc builds a lead-in descriptor: one for a track (point 1-99) or a
// disc-wide item (point 0xA0-0xA2). adr is 1, position information.
func leadInDesc(session, ctrl, point, m, s, f byte) [fullTOCDescLen]byte {
	return [fullTOCDescLen]byte{session, 0x10 | ctrl, 0x00, point, 0, 0, 0, 0, m, s, f}
}

// inTrackDesc builds an in-track descriptor, where tno is the track and point
// is the index number within it.
func inTrackDesc(session, ctrl, tno, point, m, s, f byte) [fullTOCDescLen]byte {
	return [fullTOCDescLen]byte{session, 0x10 | ctrl, tno, point, 0, 0, 0, 0, m, s, f}
}

func TestParseFullTOCDescriptorFields(t *testing.T) {
	t.Parallel()
	// A data track: ctrl bit 2 set, adr 1, at 00:02:00 which is LBA 0.
	buf := fullTOCResponse(leadInDesc(1, 0x04, 1, 0, 2, 0))
	descs, err := parseFullTOC(buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(descs) != 1 {
		t.Fatalf("got %d descriptors; want 1", len(descs))
	}
	d := descs[0]
	if d.session != 1 || d.adr != 1 || d.ctrl != 0x04 || d.tno != 0 || d.point != 1 {
		t.Errorf("decoded %+v; want session 1, adr 1, ctrl 4, tno 0, point 1", d)
	}
	if d.lba != 0 {
		t.Errorf("lba = %d; want 0 (00:02:00 is the start of the disc)", d.lba)
	}
	if !d.isData() {
		t.Error("isData() = false; want true for ctrl 0x04")
	}
	if !d.carriesPosition() {
		t.Error("carriesPosition() = false; want true for adr 1")
	}
}

func TestParseFullTOCRejectsBadLengths(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		buf  []byte
	}{
		{"empty", nil},
		{"header only truncated", []byte{0x00, 0x0d, 0x01}},
		{"data length below the header", []byte{0x00, 0x01, 0x01, 0x01}},
		{"data length zero", []byte{0x00, 0x00, 0x01, 0x01}},
		// 2 + 5 leaves 5 descriptor bytes, which is not a whole descriptor.
		{"not a multiple of the descriptor size", append([]byte{0x00, 0x07, 0x01, 0x01}, make([]byte, 5)...)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := parseFullTOC(tc.buf); err == nil {
				t.Error("parseFullTOC succeeded; want an error")
			}
		})
	}
}

func TestParseFullTOCClampsLyingDataLength(t *testing.T) {
	t.Parallel()
	// A drive claiming far more data than it returned must not read past the
	// buffer: 2048 descriptor bytes declared, 11 delivered.
	buf := make([]byte, 4+fullTOCDescLen)
	binary.BigEndian.PutUint16(buf[0:2], 2048)
	desc := leadInDesc(1, 0x04, 1, 0, 2, 0)
	copy(buf[4:], desc[:])
	descs, err := parseFullTOC(buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(descs) != 1 {
		t.Fatalf("got %d descriptors; want 1", len(descs))
	}
}

func TestFullTOCEntries(t *testing.T) {
	t.Parallel()
	// Track 1 data in session 1; tracks 2 and 3 audio in session 2, track 2
	// with an INDEX 00 two seconds before its start.
	buf := fullTOCResponse(
		leadInDesc(1, 0x04, 1, 0, 2, 0),      // track 1 @ LBA 0
		leadInDesc(2, 0x00, 2, 0, 12, 0),     // track 2 @ LBA 750
		leadInDesc(2, 0x00, 3, 0, 22, 0),     // track 3 @ LBA 1500
		inTrackDesc(2, 0x00, 2, 0, 0, 10, 0), // track 2 INDEX 00 @ LBA 600
	)
	descs, err := parseFullTOC(buf)
	if err != nil {
		t.Fatal(err)
	}
	entries := fullTOCEntries(descs)
	if len(entries) != 3 {
		t.Fatalf("got %d entries; want 3", len(entries))
	}
	if got := entries[1]; got.session != 1 || got.index0LBA != 0 {
		t.Errorf("track 1 = %+v; want session 1 and no INDEX 00", got)
	}
	if got := entries[2]; got.session != 2 || got.index0LBA != 600 {
		t.Errorf("track 2 = %+v; want session 2, INDEX 00 at 600", got)
	}
	if got := entries[3]; got.session != 2 || got.index0LBA != 0 {
		t.Errorf("track 3 = %+v; want session 2 and no INDEX 00", got)
	}
}

func TestFullTOCEntriesIgnoresCatalogDescriptors(t *testing.T) {
	t.Parallel()
	// ADR 2 puts the media catalog number where the address belongs. Decoding
	// it as an MSF position would place track 1's INDEX 00 at a random LBA.
	mcn := [fullTOCDescLen]byte{1, 0x20, 0x01, 0x00, 0, 0, 0, 0, 0x12, 0x34, 0x56}
	descs, err := parseFullTOC(fullTOCResponse(leadInDesc(1, 0x04, 1, 0, 2, 0), mcn))
	if err != nil {
		t.Fatal(err)
	}
	if got := fullTOCEntries(descs)[1].index0LBA; got != 0 {
		t.Errorf("track 1 INDEX 00 = %d; want 0 (the catalog descriptor is not a position)", got)
	}
}

func TestTOCFromFullTOC(t *testing.T) {
	t.Parallel()
	buf := fullTOCResponse(
		leadInDesc(1, 0x04, 0xA0, 0, 1, 0),   // first track
		leadInDesc(1, 0x04, 0xA1, 0, 3, 0),   // last track
		leadInDesc(1, 0x04, 1, 0, 2, 0),      // track 1 data @ LBA 0
		leadInDesc(2, 0x00, 2, 0, 12, 0),     // track 2 audio @ LBA 750
		leadInDesc(2, 0x00, 3, 0, 22, 0),     // track 3 audio @ LBA 1500
		inTrackDesc(2, 0x00, 2, 0, 0, 10, 0), // track 2 INDEX 00 @ LBA 600
		leadInDesc(2, 0x00, 0xA2, 0, 32, 0),  // lead-out @ LBA 2250
	)
	descs, err := parseFullTOC(buf)
	if err != nil {
		t.Fatal(err)
	}
	toc := tocFromFullTOC(descs)

	if toc.FirstTrack != 1 || toc.LastTrack != 3 {
		t.Errorf("track range = %d-%d; want 1-3", toc.FirstTrack, toc.LastTrack)
	}
	if toc.LeadOutLBA != 2250 {
		t.Errorf("LeadOutLBA = %d; want 2250", toc.LeadOutLBA)
	}
	want := []Track{
		{Number: 1, Session: 1, Type: TrackTypeData, StartLBA: 0},
		{Number: 2, Session: 2, Type: TrackTypeAudio, StartLBA: 750, Index0LBA: 600},
		{Number: 3, Session: 2, Type: TrackTypeAudio, StartLBA: 1500},
	}
	if len(toc.Tracks) != len(want) {
		t.Fatalf("got %d tracks; want %d", len(toc.Tracks), len(want))
	}
	for i, w := range want {
		if toc.Tracks[i] != w {
			t.Errorf("track %d = %+v; want %+v", i, toc.Tracks[i], w)
		}
	}
	// The lead-in items must not be mistaken for tracks.
	if !toc.HasAudio() {
		t.Error("HasAudio() = false; want true")
	}
	// INDEX 00 is what a pregap is measured from, and only the Full TOC carries it.
	if got := toc.Tracks[1].StartLBA - toc.Tracks[1].Index0LBA; got != 150 {
		t.Errorf("track 2 pregap = %d; want 150", got)
	}
}

func TestTOCFromFullTOCIgnoresOutOfRangeTrackNumbers(t *testing.T) {
	t.Parallel()
	// POINT 0xB0-0xC1 appear in the lead-in of recordable discs and are not
	// tracks; neither is POINT 0, which no descriptor uses in the lead-in.
	descs, err := parseFullTOC(fullTOCResponse(
		leadInDesc(1, 0x04, 0xB0, 0, 2, 0),
		leadInDesc(1, 0x04, 0xC0, 0, 2, 0),
		leadInDesc(1, 0x04, 0x00, 0, 2, 0),
	))
	if err != nil {
		t.Fatal(err)
	}
	if toc := tocFromFullTOC(descs); len(toc.Tracks) != 0 {
		t.Errorf("got %d tracks; want 0", len(toc.Tracks))
	}
}

// The drive returns Full TOC descriptors in any order, and every lookup on a
// TOC assumes the track list is ascending: out of order, TrackAt returns the
// wrong track.
func TestTocFromFullTOCSortsTracks(t *testing.T) {
	t.Parallel()
	// Track 3 first, then 1, then 2 — with the lead-out in the middle.
	descs := []fullTOCDescriptor{
		{session: 1, adr: 1, ctrl: 0x04, point: 3, lba: 3000},
		{session: 1, adr: 1, ctrl: 0x00, point: 0xA2, lba: 4000},
		{session: 1, adr: 1, ctrl: 0x04, point: 1, lba: 0},
		{session: 1, adr: 1, ctrl: 0x00, point: 2, lba: 1500},
	}
	toc := tocFromFullTOC(descs)
	if len(toc.Tracks) != 3 {
		t.Fatalf("got %d tracks; want 3", len(toc.Tracks))
	}
	for i := 1; i < len(toc.Tracks); i++ {
		if toc.Tracks[i-1].StartLBA > toc.Tracks[i].StartLBA {
			t.Fatalf("tracks are not in ascending order: %+v", toc.Tracks)
		}
	}
	// And the lookups every caller uses now agree with the disc.
	if tr, ok := toc.TrackAt(1500); !ok || tr.Number != 2 {
		t.Errorf("TrackAt(1500) = %d, %v; want track 2", tr.Number, ok)
	}
	if tr, ok := toc.TrackAt(3500); !ok || tr.Number != 3 {
		t.Errorf("TrackAt(3500) = %d, %v; want track 3", tr.Number, ok)
	}
}
