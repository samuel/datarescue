package cuebin

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"samuelks.com/datarescue/cdrom"
	"samuelks.com/datarescue/ddrescue"
)

func decToBCD(v byte) byte { return (v/10)<<4 | v%10 }

// openSource opens a BIN the way a tool does. mapText, when non-empty, is
// written as the sidecar beside it and picked up the same way.
func openSource(t *testing.T, binPath, mapText string) *Source {
	t.Helper()
	if mapText != "" {
		if err := os.WriteFile(binPath+".map", []byte(mapText), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	src, err := OpenSource(binPath, ddrescue.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { src.Close() }) //nolint:errcheck // read-only
	return src
}

// mapText renders a ddrescue map over a file of size bytes marking the given
// sectors unrecovered.
func mapText(size int64, sectorSize int64, lbas ...int64) string {
	var b strings.Builder
	b.WriteString("# Mapfile.\n0x00000000     +               1\n")
	var pos int64
	for _, lba := range lbas {
		start, end := lba*sectorSize, (lba+1)*sectorSize
		if start > pos {
			fmt.Fprintf(&b, "0x%08x  0x%08x  +\n", pos, start-pos)
		}
		fmt.Fprintf(&b, "0x%08x  0x%08x  -\n", start, end-start)
		pos = end
	}
	if pos < size {
		fmt.Fprintf(&b, "0x%08x  0x%08x  +\n", pos, size-pos)
	}
	return b.String()
}

// buildDisc writes a raw BIN whose data sectors carry a valid sync pattern,
// correct MSF and correct EDC. Audio sectors are filled with noise.
func buildDisc(t *testing.T, modes []string, sectorsPerTrack int) (string, []CueTrack) {
	t.Helper()
	var tracks []CueTrack
	var lba int64
	var data []byte
	for ti, mode := range modes {
		tracks = append(tracks, CueTrack{
			Number: ti + 1,
			Mode:   mode,
			Indices: []Index{{Number: 1, Position: MSF{
				M: int(lba / (60 * 75)), S: int(lba / 75 % 60), F: int(lba % 75),
			}}},
		})
		for range sectorsPerTrack {
			sec := make([]byte, cdrom.RawSectorSize)
			if mode == "AUDIO" {
				for j := range sec {
					sec[j] = byte(lba + int64(j))
				}
			} else {
				copy(sec[:12], syncPattern[:])
				m, s, f := cdrom.LBAToMSF(lba)
				sec[12], sec[13], sec[14] = decToBCD(m), decToBCD(s), decToBCD(f)
				sec[15] = 0x01
				for j := 16; j < 2064; j++ {
					sec[j] = byte(lba)
				}
				binary.LittleEndian.PutUint32(sec[2064:2068], computeEDC(sec))
			}
			data = append(data, sec...)
			lba++
		}
	}
	path := filepath.Join(t.TempDir(), "disc.bin")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, tracks
}

func TestVerifyClean(t *testing.T) {
	t.Parallel()
	path, tracks := buildDisc(t, []string{"MODE1/2352"}, 8)
	vr, err := VerifyBIN(openSource(t, path, ""), tracks)
	if err != nil {
		t.Fatal(err)
	}
	if vr.BadSync != 0 || vr.BadMSF != 0 || vr.BadEDC != 0 || vr.Unreadable != 0 {
		t.Errorf("clean disc reported sync=%d msf=%d edc=%d unreadable=%d",
			vr.BadSync, vr.BadMSF, vr.BadEDC, vr.Unreadable)
	}
	if vr.TotalSectors != 8 {
		t.Errorf("TotalSectors = %d; want 8", vr.TotalSectors)
	}
}

// An audio track carries no MSF, so the first data sector after one must not be
// flagged, and a single corrupt MSF must be reported once rather than twice.
func TestVerifyMSFContinuity(t *testing.T) {
	t.Parallel()
	path, tracks := buildDisc(t, []string{"MODE1/2352", "AUDIO", "MODE1/2352"}, 4)
	vr, err := VerifyBIN(openSource(t, path, ""), tracks)
	if err != nil {
		t.Fatal(err)
	}
	if vr.BadMSF != 0 {
		var flagged []int
		for i := range vr.Sectors {
			if vr.Sectors[i].BadMSF {
				flagged = append(flagged, i)
			}
		}
		t.Errorf("BadMSF = %d on a clean mixed-mode disc (sectors %v); want 0", vr.BadMSF, flagged)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw[9*cdrom.RawSectorSize+13] = 0x59 // corrupt one sector's seconds field
	corrupt := filepath.Join(t.TempDir(), "corrupt.bin")
	if err := os.WriteFile(corrupt, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	vr, err = VerifyBIN(openSource(t, corrupt, ""), tracks)
	if err != nil {
		t.Fatal(err)
	}
	if vr.BadMSF != 1 {
		t.Errorf("BadMSF = %d for one corrupt MSF; want 1", vr.BadMSF)
	}
}

// Failing to resolve track bounds must be an error: leaving every sector mode
// empty would silently skip EDC checking for the whole image.
func TestVerifyTrackBoundsFailure(t *testing.T) {
	t.Parallel()
	path, tracks := buildDisc(t, []string{"MODE1/2352"}, 2)
	unbounded := []CueTrack{
		{Number: 1, Mode: "MODE1/2352", Indices: []Index{{Number: 1, Position: MSF{}}}},
		{Number: 2, Mode: "MODE1/2352"}, // no INDEX 01
	}
	if _, err := VerifyBIN(openSource(t, path, ""), unbounded); err == nil {
		t.Error("VerifyBIN succeeded with unresolvable track bounds")
	}
	if _, err := VerifyBIN(openSource(t, path, ""), tracks); err != nil {
		t.Errorf("VerifyBIN on a good sheet = %v", err)
	}
}

// EDC and the map disagreeing means two opposite things, counted apart.
func TestVerifyEDCMapDisagreement(t *testing.T) {
	t.Parallel()
	path, tracks := buildDisc(t, []string{"MODE1/2352"}, 4)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw[1*cdrom.RawSectorSize+100] ^= 0xFF // corrupt sector 1's payload
	corrupt := filepath.Join(t.TempDir(), "c.bin")
	if err := os.WriteFile(corrupt, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	ss := int64(cdrom.RawSectorSize)
	vr, err := VerifyBIN(openSource(t, corrupt, mapText(4*ss, ss, 3)), tracks)
	if err != nil {
		t.Fatal(err)
	}
	if vr.EDCBadMapGood != 1 {
		t.Errorf("EDCBadMapGood = %d; want 1 (corruption the map missed)", vr.EDCBadMapGood)
	}
	if vr.EDCGoodMapBad != 1 {
		t.Errorf("EDCGoodMapBad = %d; want 1 (stale map)", vr.EDCGoodMapBad)
	}
	if vr.BadMap != 1 {
		t.Errorf("BadMap = %d; want 1", vr.BadMap)
	}
}

// The cursor-based map lookup must agree with a naive rescan of the block list.
func TestMapScannerMatchesLinearScan(t *testing.T) {
	t.Parallel()
	ss := int64(cdrom.RawSectorSize)
	blocks := []ddrescue.Block{
		{Offset: 0, Size: 5 * ss, Status: ddrescue.BlockStatusFinished},
		{Offset: 5 * ss, Size: 2 * ss, Status: ddrescue.BlockStatusFailedBlockBadSectors},
		{Offset: 7 * ss, Size: 3 * ss, Status: ddrescue.BlockStatusFinished},
		{Offset: 10 * ss, Size: ss, Status: ddrescue.BlockStatusNonTried},
		{Offset: 11 * ss, Size: 9 * ss, Status: ddrescue.BlockStatusFinished},
	}
	naive := func(sector int64) bool {
		start, end := sector*ss, sector*ss+ss
		for _, b := range blocks {
			if b.Status == ddrescue.BlockStatusFinished {
				continue
			}
			if b.Offset < end && b.Offset+b.Size > start {
				return true
			}
		}
		return false
	}
	m := &mapScanner{blocks: blocks}
	for s := range int64(20) {
		if got, want := m.bad(s*ss, ss), naive(s); got != want {
			t.Errorf("sector %d: scanner=%v naive=%v", s, got, want)
		}
	}
}

func TestComputeEDCShortSector(t *testing.T) {
	t.Parallel()
	// A short sector has no EDC to compute; indexing it must not panic.
	for _, n := range []int{0, 100, 2048, 2063} {
		if got := computeEDC(make([]byte, n)); got != 0 {
			t.Errorf("computeEDC(%d bytes) = %d; want 0", n, got)
		}
	}
	full := make([]byte, cdrom.RawSectorSize)
	for i := range full {
		full[i] = byte(i)
	}
	if computeEDC(full) == 0 {
		t.Error("computeEDC on a full sector returned 0")
	}
}

func TestBCDToDec(t *testing.T) {
	t.Parallel()
	tests := []struct{ in, want byte }{{0x00, 0}, {0x09, 9}, {0x10, 10}, {0x59, 59}, {0x99, 99}}
	for _, tc := range tests {
		if got := bcdToDec(tc.in); got != tc.want {
			t.Errorf("bcdToDec(0x%02X) = %d; want %d", tc.in, got, tc.want)
		}
	}
}

// A sector the map never recovered has no sync pattern and no address, because
// nothing was read there. Counting that as a structural fault of the BIN says
// something much worse than what happened, and would turn every damaged disc
// into a failed one.
func TestVerifyMapBadIsNotStructural(t *testing.T) {
	t.Parallel()
	path, tracks := buildDisc(t, []string{"MODE1/2352"}, 6)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ss := int64(cdrom.RawSectorSize)
	// What a rescue leaves behind is not the disc's bytes: garbage here, zeros
	// under GNU ddrescue. Either way the sync pattern is gone.
	for i := 2 * ss; i < 3*ss; i++ {
		raw[i] = 0xDB
	}
	damaged := filepath.Join(t.TempDir(), "damaged.bin")
	if err := os.WriteFile(damaged, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	vr, err := VerifyBIN(openSource(t, damaged, mapText(6*ss, ss, 2)), tracks)
	if err != nil {
		t.Fatal(err)
	}
	if vr.BadSync != 0 || vr.BadMSF != 0 {
		t.Errorf("sync=%d msf=%d on a sector the map already calls bad; want 0 and 0", vr.BadSync, vr.BadMSF)
	}
	if vr.BadMap != 1 {
		t.Errorf("BadMap = %d; want 1", vr.BadMap)
	}
	// The EDC is still compared, since that is what separates a stale map from
	// a real loss. Here it fails, agreeing with the map.
	if vr.BadEDC != 1 || vr.EDCGoodMapBad != 0 || vr.EDCBadMapGood != 0 {
		t.Errorf("edc=%d goodMapBad=%d badMapGood=%d; want the failure to agree with the map",
			vr.BadEDC, vr.EDCGoodMapBad, vr.EDCBadMapGood)
	}
}

// The MSF anchor must come from a sector that was actually read. Taking it from
// a lost one puts every sector of the track at the wrong expected address.
func TestVerifyMSFAnchorSkipsLostSector(t *testing.T) {
	t.Parallel()
	path, tracks := buildDisc(t, []string{"MODE1/2352"}, 6)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ss := int64(cdrom.RawSectorSize)
	for i := range ss { // the track's first sector, where the anchor is taken
		raw[i] = 0xDB
	}
	damaged := filepath.Join(t.TempDir(), "damaged.bin")
	if err := os.WriteFile(damaged, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	vr, err := VerifyBIN(openSource(t, damaged, mapText(6*ss, ss, 0)), tracks)
	if err != nil {
		t.Fatal(err)
	}
	if vr.BadMSF != 0 {
		t.Errorf("BadMSF = %d; a lost first sector made the rest of the track look misaddressed", vr.BadMSF)
	}
}

// A range the map does not cover is not data. ddrescue.Unrecovered calls it
// non-tried, and reporting it good made its missing sync pattern and failed EDC
// count as structural faults of the BIN and as EDCBadMapGood — corruption the
// map missed — which is a much worse finding than what happened, and the
// opposite of what ExtractDataTrack does with the same bytes.
func TestMapScannerUncoveredIsBad(t *testing.T) {
	t.Parallel()
	const sec = 2352
	// A map covering sectors 0 and 2 only: sector 1 is a hole and sector 3 is
	// past the last block.
	blocks := []ddrescue.Block{
		{Offset: 0, Size: sec, Status: ddrescue.BlockStatusFinished},
		{Offset: 2 * sec, Size: sec, Status: ddrescue.BlockStatusFinished},
	}
	want := []bool{false, true, false, true}
	m := &mapScanner{blocks: blocks}
	for i, w := range want {
		if got := m.bad(int64(i)*sec, sec); got != w {
			t.Errorf("sector %d: bad = %v; want %v", i, got, w)
		}
	}

	// A block that only partly covers a sector leaves the rest uncovered.
	partial := &mapScanner{blocks: []ddrescue.Block{
		{Offset: 0, Size: sec / 2, Status: ddrescue.BlockStatusFinished},
	}}
	if !partial.bad(0, sec) {
		t.Error("a sector the map covers only half of reported good")
	}

	// No map at all is not an empty map: nothing is known to be missing.
	none := &mapScanner{}
	for i := range 4 {
		if none.bad(int64(i)*sec, sec) {
			t.Fatalf("sector %d reported bad with no map at all", i)
		}
	}
}
