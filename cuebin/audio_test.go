package cuebin

import (
	"os"
	"path/filepath"
	"testing"

	"samuelks.com/datarescue/cdrom"
)

// audioDisc writes a BIN of audio sectors: every sector holds noise, so nothing
// in it is silent and no two sectors are alike.
func audioDisc(t *testing.T, sectors int) string {
	t.Helper()
	data := make([]byte, sectors*cdrom.RawSectorSize)
	for i := range data {
		// Never zero, so a silent run can only come from a hole or from a
		// sector the analysis skipped and then counted anyway.
		data[i] = byte(i%251 + 1)
	}
	path := filepath.Join(t.TempDir(), "audio.bin")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A damaged audio track must still yield a result. Aborting would fail the
// whole verify run, so a disc with one bad audio sector would get no report at
// all.
func TestAnalyzeAudioTrackDamagedStillReports(t *testing.T) {
	t.Parallel()
	const sectors = 40
	path := audioDisc(t, sectors)
	ss := int64(cdrom.RawSectorSize)
	// Lose a long run of sectors: long enough that, were the hole analyzed as
	// data, it would read as silence and as repeated sectors.
	var lost []int64
	for lba := int64(10); lba < 30; lba++ {
		lost = append(lost, lba)
	}
	src := openSource(t, path, mapText(sectors*ss, ss, lost...))

	ar := AnalyzeAudioTrack(src, 1, 0, sectors, cdrom.RawSectorSize, nil)
	if ar.UnreadableSectors != int64(len(lost)) {
		t.Errorf("UnreadableSectors = %d; want %d", ar.UnreadableSectors, len(lost))
	}
	// A hole is not silence: carrying the run across it would invent ~1.3
	// seconds of quiet the disc never had.
	if len(ar.SilentRuns) != 0 {
		t.Errorf("SilentRuns = %v; a hole is not silence", ar.SilentRuns)
	}
	// Nor is it a repeated sector: every hole reads alike, and hashing them
	// would report 19 repeats that are nothing but the damage.
	if len(ar.RepeatedSectors) != 0 {
		t.Errorf("RepeatedSectors = %v; the holes were hashed", ar.RepeatedSectors)
	}
}

// Real silence either side of a hole is still reported, and each run is its own.
func TestAnalyzeAudioTrackSilenceAroundHole(t *testing.T) {
	t.Parallel()
	const sectors = 24
	data := make([]byte, sectors*cdrom.RawSectorSize)
	for i := range data {
		data[i] = byte(i%251 + 1)
	}
	// A sector is 1176 samples, so a run has to span eight of them to reach the
	// 8800-sample threshold. Sectors 0-9 and 11-23 are true silence either side
	// of the hole at 10.
	clear(data[:10*cdrom.RawSectorSize])
	clear(data[11*cdrom.RawSectorSize:])
	path := filepath.Join(t.TempDir(), "audio.bin")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	ss := int64(cdrom.RawSectorSize)
	src := openSource(t, path, mapText(sectors*ss, ss, 10))

	ar := AnalyzeAudioTrack(src, 1, 0, sectors, cdrom.RawSectorSize, nil)
	if len(ar.SilentRuns) != 2 {
		t.Fatalf("SilentRuns = %v; want the two real runs either side of the hole", ar.SilentRuns)
	}
	if ar.SilentRuns[0].StartLBA != 0 || ar.SilentRuns[1].StartLBA != 11 {
		t.Errorf("silent runs start at %d and %d; want 0 and 11", ar.SilentRuns[0].StartLBA, ar.SilentRuns[1].StartLBA)
	}
}

// BadSectors comes from the map without reading anything; UnreadableSectors
// from the reads that then failed. They are counted apart because they answer
// different questions.
func TestAnalyzeAudioTrackCountsApart(t *testing.T) {
	t.Parallel()
	const sectors = 8
	path := audioDisc(t, sectors)
	ss := int64(cdrom.RawSectorSize)
	src := openSource(t, path, mapText(sectors*ss, ss, 3))

	vr, err := VerifyBIN(src, []CueTrack{{Number: 1, Mode: "AUDIO", Indices: []Index{{Number: 1}}}})
	if err != nil {
		t.Fatal(err)
	}
	ar := AnalyzeAudioTrack(src, 1, 0, sectors, cdrom.RawSectorSize, vr)
	if ar.BadSectors != 1 {
		t.Errorf("BadSectors = %d; want 1 from the map", ar.BadSectors)
	}
	if ar.UnreadableSectors != 1 {
		t.Errorf("UnreadableSectors = %d; want 1 from the read", ar.UnreadableSectors)
	}
}
