package cuebin

import (
	"os"
	"strings"
	"testing"

	"samuelks.com/datarescue/cdrom"
	"samuelks.com/datarescue/ddrescue"
)

func TestWriterDataOnly(t *testing.T) {
	dir := t.TempDir()
	stem := dir + "/disc"

	toc := cdrom.TOC{
		FirstTrack: 1,
		LastTrack:  1,
		LeadOutLBA: 100,
		Tracks: []cdrom.Track{
			{Number: 1, Session: 1, Type: cdrom.TrackTypeData, StartLBA: 0},
		},
	}

	w, err := NewWriter(stem, toc, false)
	if err != nil {
		t.Fatal(err)
	}

	sector := make([]byte, cdrom.RawSectorSize)
	if err := w.WriteRawSector(0, sector); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	cueData, err := os.ReadFile(stem + ".cue")
	if err != nil {
		t.Fatal(err)
	}
	cue := string(cueData)
	if !strings.Contains(cue, "MODE1/2352") {
		t.Errorf("CUE missing MODE1/2352:\n%s", cue)
	}
	if !strings.Contains(cue, "TRACK 01") {
		t.Errorf("CUE missing TRACK 01:\n%s", cue)
	}

	binInfo, err := os.Stat(stem + ".bin")
	if err != nil {
		t.Fatal(err)
	}
	if binInfo.Size() != cdrom.RawSectorSize {
		t.Errorf("BIN size = %d; want %d", binInfo.Size(), cdrom.RawSectorSize)
	}
}

func TestWriterMixedMode(t *testing.T) {
	dir := t.TempDir()
	stem := dir + "/disc"

	toc := cdrom.TOC{
		FirstTrack: 1,
		LastTrack:  3,
		LeadOutLBA: 300,
		Tracks: []cdrom.Track{
			{Number: 1, Session: 1, Type: cdrom.TrackTypeData, StartLBA: 0},
			{Number: 2, Session: 1, Type: cdrom.TrackTypeAudio, StartLBA: 100},
			{Number: 3, Session: 1, Type: cdrom.TrackTypeAudio, StartLBA: 200},
		},
	}

	w, err := NewWriter(stem, toc, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	cueData, err := os.ReadFile(stem + ".cue")
	if err != nil {
		t.Fatal(err)
	}
	cue := string(cueData)
	if !strings.Contains(cue, "MODE1/2352") {
		t.Errorf("CUE missing MODE1/2352:\n%s", cue)
	}
	if !strings.Contains(cue, "AUDIO") {
		t.Errorf("CUE missing AUDIO:\n%s", cue)
	}
	// No Index0LBA in TOC and no sectors written → no pregap detected.
	if strings.Contains(cue, "PREGAP") {
		t.Errorf("CUE unexpectedly contains PREGAP:\n%s", cue)
	}
	if strings.Contains(cue, "INDEX 00") {
		t.Errorf("CUE unexpectedly contains INDEX 00:\n%s", cue)
	}
}

// TestWriterMixedModeDetectedPregap verifies that the writer detects the
// pregap boundary from the sync-pattern transition when Index0LBA is absent
// from the TOC — the common case when drives omit full-TOC in-track entries.
func TestWriterMixedModeDetectedPregap(t *testing.T) {
	dir := t.TempDir()
	stem := dir + "/disc"

	// Index0LBA intentionally absent; the writer must detect it from sector data.
	toc := cdrom.TOC{
		FirstTrack: 1,
		LastTrack:  2,
		LeadOutLBA: 200,
		Tracks: []cdrom.Track{
			{Number: 1, Session: 1, Type: cdrom.TrackTypeData, StartLBA: 0},
			{Number: 2, Session: 1, Type: cdrom.TrackTypeAudio, StartLBA: 100},
		},
	}

	w, err := NewWriter(stem, toc, false)
	if err != nil {
		t.Fatal(err)
	}

	// Sectors 0–89: MODE1 data (sync pattern present).
	dataSector := make([]byte, cdrom.RawSectorSize)
	copy(dataSector[:12], syncPattern[:])
	for lba := range int64(90) {
		if err := w.WriteRawSector(lba, dataSector); err != nil {
			t.Fatal(err)
		}
	}
	// Sectors 90–99: audio pregap (no sync pattern; all zeros).
	audioSector := make([]byte, cdrom.RawSectorSize)
	for lba := range int64(10) {
		if err := w.WriteRawSector(90+lba, audioSector); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	cueData, err := os.ReadFile(stem + ".cue")
	if err != nil {
		t.Fatal(err)
	}
	cue := string(cueData)
	// BIN sector 90 → 00:01:15 (90/75=1s, 90%75=15f)
	if !strings.Contains(cue, "INDEX 00 00:01:15") {
		t.Errorf("CUE missing detected INDEX 00:\n%s", cue)
	}
	// BIN sector 100 → 00:01:25
	if !strings.Contains(cue, "INDEX 01 00:01:25") {
		t.Errorf("CUE missing INDEX 01 for track 2:\n%s", cue)
	}
}

func TestWriterMixedModeWithPregap(t *testing.T) {
	dir := t.TempDir()
	stem := dir + "/disc"

	// Track 2 starts at BIN sector 100, pregap starts at sector 90.
	toc := cdrom.TOC{
		FirstTrack: 1,
		LastTrack:  2,
		LeadOutLBA: 200,
		Tracks: []cdrom.Track{
			{Number: 1, Session: 1, Type: cdrom.TrackTypeData, StartLBA: 0},
			{Number: 2, Session: 1, Type: cdrom.TrackTypeAudio, StartLBA: 100, Index0LBA: 90},
		},
	}

	w, err := NewWriter(stem, toc, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	cueData, err := os.ReadFile(stem + ".cue")
	if err != nil {
		t.Fatal(err)
	}
	cue := string(cueData)
	// Track 2 pregap: BIN sector 90, BIN sector 100.
	if !strings.Contains(cue, "INDEX 00 00:01:15") {
		t.Errorf("CUE missing INDEX 00 for track 2 pregap:\n%s", cue)
	}
	if !strings.Contains(cue, "INDEX 01 00:01:25") {
		t.Errorf("CUE missing INDEX 01 for track 2:\n%s", cue)
	}
}

// TestWriterCUEPositionsRelativeToBIN verifies that INDEX positions in the
// generated CUE are relative to the BIN file start, not absolute disc LBAs.
// On a real CD track 1 starts at disc LBA 150 (00:02:00) but the BIN starts
// there too, so the CUE must say INDEX 01 00:00:00, not 00:02:00.
func TestWriterCUEPositionsRelativeToBIN(t *testing.T) {
	dir := t.TempDir()
	stem := dir + "/disc"

	toc := cdrom.TOC{
		FirstTrack: 1,
		LastTrack:  1,
		LeadOutLBA: 250,
		Tracks: []cdrom.Track{
			{Number: 1, Session: 1, Type: cdrom.TrackTypeData, StartLBA: 150},
		},
	}

	w, err := NewWriter(stem, toc, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	cueData, err := os.ReadFile(stem + ".cue")
	if err != nil {
		t.Fatal(err)
	}
	cue := string(cueData)
	if !strings.Contains(cue, "INDEX 01 00:00:00") {
		t.Errorf("CUE INDEX position should be 00:00:00 (BIN-relative), got:\n%s", cue)
	}
	if strings.Contains(cue, "00:02:00") {
		t.Errorf("CUE contains absolute disc LBA 00:02:00; should use BIN-relative position:\n%s", cue)
	}
}

func TestStem(t *testing.T) {
	tests := []struct{ in, want string }{
		{"/tmp/disc.bin", "/tmp/disc"},
		{"/tmp/disc.cue", "/tmp/disc"},
		{"/tmp/disc", "/tmp/disc"},
		{"/tmp/disc.iso", "/tmp/disc.iso"},
	}
	for _, tt := range tests {
		if got := Stem(tt.in); got != tt.want {
			t.Errorf("Stem(%q) = %q; want %q", tt.in, got, tt.want)
		}
	}
}

// rawSector frames a MODE1 or MODE2 sector: the sync pattern, a placeholder
// address, and the mode byte the cue's track mode is read from.
func rawSector(mode byte) []byte {
	sec := make([]byte, cdrom.RawSectorSize)
	copy(sec, syncPattern[:])
	sec[15] = mode
	return sec
}

// The map records positions in the BIN, not on the disc. The BIN begins at the
// first track's LBA and the cue's INDEX values already subtract it, so a map
// left in disc offsets is shifted by StartLBA*2352 and every tool downstream
// attributes the damage to the wrong sectors.
func TestWriterMapIsBinRelative(t *testing.T) {
	dir := t.TempDir()
	stem := dir + "/disc"
	const start = 150
	toc := cdrom.TOC{
		FirstTrack: 1, LastTrack: 1, LeadOutLBA: start + 4,
		Tracks: []cdrom.Track{{Number: 1, Session: 1, Type: cdrom.TrackTypeData, StartLBA: start}},
	}
	w, err := NewWriter(stem, toc, false)
	if err != nil {
		t.Fatal(err)
	}
	// Two good sectors, one bad, one good — at disc LBAs 150 to 153.
	for i, bad := range []bool{false, false, true, false} {
		lba := int64(start + i)
		if bad {
			err = w.WriteZeroSector(lba)
		} else {
			err = w.WriteRawSector(lba, rawSector(1))
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := w.WriteMap(stem + ".bin.map"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	blocks, err := ddrescue.ParseMap(stem + ".bin.map")
	if err != nil {
		t.Fatal(err)
	}
	bin, err := os.ReadFile(stem + ".bin")
	if err != nil {
		t.Fatal(err)
	}
	bad := ddrescue.Unrecovered(blocks, 0, int64(len(bin)))
	// The bad sector is the third in the file: byte 2*2352.
	wantOff, wantSize := int64(2*cdrom.RawSectorSize), int64(cdrom.RawSectorSize)
	if len(bad) != 1 || bad[0].Offset != wantOff || bad[0].Size != wantSize {
		t.Fatalf("the map records %v; want one range of %d bytes at %d — the BIN's own offsets",
			bad, wantSize, wantOff)
	}
}

// A data track's cue mode comes from byte 15 of its own first sector. Labelling
// every data track MODE1/2352 gives a MODE2/XA disc a cue that makes verify
// fail the EDC of every sector and export drop 288 bytes of each.
func TestWriterTrackModeFromSectorHeader(t *testing.T) {
	tests := []struct {
		name string
		mode byte
		want string
	}{
		{"mode 1", 1, "MODE1/2352"},
		{"mode 2", 2, "MODE2/2352"},
		{"mode byte never read", 0, "MODE1/2352"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			stem := dir + "/disc"
			toc := cdrom.TOC{
				FirstTrack: 1, LastTrack: 1, LeadOutLBA: 2,
				Tracks: []cdrom.Track{{Number: 1, Session: 1, Type: cdrom.TrackTypeData, StartLBA: 0}},
			}
			w, err := NewWriter(stem, toc, false)
			if err != nil {
				t.Fatal(err)
			}
			if tc.mode == 0 {
				// Nothing readable came back, so nothing said what mode it is.
				if err := w.WriteZeroSector(0); err != nil {
					t.Fatal(err)
				}
			} else if err := w.WriteRawSector(0, rawSector(tc.mode)); err != nil {
				t.Fatal(err)
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			cue, err := os.ReadFile(stem + ".cue")
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(cue), "TRACK 01 "+tc.want) {
				t.Errorf("cue does not name the track %s:\n%s", tc.want, cue)
			}
		})
	}
}
