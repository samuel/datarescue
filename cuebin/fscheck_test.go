package cuebin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"samuelks.com/datarescue/internal/testiso"
)

var isoTracks = []CueTrack{{Number: 1, Mode: "MODE1/2048", Indices: []Index{{Number: 1}}}}

// writeBin puts an image where a Source can open it.
func writeBin(t *testing.T, img []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "disc.bin")
	if err := os.WriteFile(path, img, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func hasIssue(issues []string, substr string) bool {
	for _, is := range issues {
		if strings.Contains(is, substr) {
			return true
		}
	}
	return false
}

func TestCheckFilesystemISO(t *testing.T) {
	t.Parallel()
	src := openSource(t, writeBin(t, testiso.Build()), "")
	fsr := CheckFilesystem(src, isoTracks, nil)
	if fsr.Type != FSISO9660 {
		t.Fatalf("Type = %s; want ISO 9660", fsr.Type)
	}
	if fsr.VolumeLabel != testiso.VolumeID {
		t.Errorf("VolumeLabel = %q; want %q", fsr.VolumeLabel, testiso.VolumeID)
	}
	if len(fsr.Files) != 2 {
		t.Errorf("Files = %d; want 2", len(fsr.Files))
	}
	if len(fsr.Issues) != 0 {
		t.Errorf("a sound image reported issues: %v", fsr.Issues)
	}
}

// A directory the rescue never recovered loses the entries it held, not the
// whole report: the files that were reached are still listed, with the damage
// reported alongside them.
func TestCheckFilesystemDamagedDirectory(t *testing.T) {
	t.Parallel()
	img := testiso.Build()
	const rootLBA = 18
	mapText := testiso.Damage(img, testiso.SectorSize, rootLBA)
	src := openSource(t, writeBin(t, img), mapText)

	fsr := CheckFilesystem(src, isoTracks, nil)
	if len(fsr.Issues) == 0 {
		t.Error("a lost root directory produced no issue, which reads as a clean disc")
	}
	// The volume descriptor is intact, so the disc is still known to be ISO 9660.
	if fsr.Type != FSISO9660 {
		t.Errorf("Type = %s; want ISO 9660", fsr.Type)
	}
}

// A primary volume descriptor that was never recovered is not "this disc has no
// filesystem", and only the map can tell them apart.
func TestCheckFilesystemPVDUnrecovered(t *testing.T) {
	t.Parallel()
	img := testiso.Build()
	mapText := testiso.Damage(img, testiso.SectorSize, 16)
	src := openSource(t, writeBin(t, img), mapText)

	fsr := CheckFilesystem(src, isoTracks, nil)
	if fsr.Type != FSUnknown {
		t.Fatalf("Type = %s; want Unknown", fsr.Type)
	}
	if !hasIssue(fsr.Issues, "never recovered") {
		t.Errorf("issues = %v; want the volume descriptor named as never recovered", fsr.Issues)
	}
}

// Sector 0 not reading is a different answer from "no partition map here", and
// falling through in silence would report a clean disc.
func TestCheckFilesystemSectorZeroUnrecovered(t *testing.T) {
	t.Parallel()
	img := testiso.Build()
	mapText := testiso.Damage(img, testiso.SectorSize, 0, 16)
	src := openSource(t, writeBin(t, img), mapText)

	fsr := CheckFilesystem(src, isoTracks, nil)
	if !hasIssue(fsr.Issues, "sector 0") {
		t.Errorf("issues = %v; want sector 0 named", fsr.Issues)
	}
}

// DeepVerifyFiles re-reads every file sector, so a file over a hole comes back
// with the sectors it lost.
func TestDeepVerifyFiles(t *testing.T) {
	t.Parallel()
	img := testiso.Build()
	mapText := testiso.Damage(img, testiso.SectorSize, testiso.BigLBA+1)
	src := openSource(t, writeBin(t, img), mapText)

	fsr := CheckFilesystem(src, isoTracks, nil)
	DeepVerifyFiles(src, isoTracks, fsr)
	if fsr.AffectedFiles != 1 {
		t.Fatalf("AffectedFiles = %d; want 1", fsr.AffectedFiles)
	}
	for i := range fsr.Files {
		fe := &fsr.Files[i]
		if !strings.Contains(fe.Path, testiso.BigName) {
			continue
		}
		if len(fe.BadSectors) != 1 || fe.BadSectors[0] != testiso.BigLBA+1 {
			t.Errorf("%s bad sectors = %v; want [%d]", fe.Path, fe.BadSectors, testiso.BigLBA+1)
		}
	}
	// A sector the map does not cover is the damage this tool is for, not an
	// I/O failure worth narrating.
	if hasIssue(fsr.Issues, "does not account for") {
		t.Errorf("issues = %v; an unrecovered sector was reported as an unexplained failure", fsr.Issues)
	}
}
