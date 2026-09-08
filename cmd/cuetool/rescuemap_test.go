package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"samuelks.com/datarescue/ddrescue"
	"samuelks.com/datarescue/internal/subcmd"
	"samuelks.com/datarescue/internal/testcd"
	"samuelks.com/datarescue/internal/testiso"
	"samuelks.com/datarescue/internal/testout"
)

// rescueDisc writes an ISO 9660 image framed as a MODE1/2352 BIN with its cue
// sheet, damages the given sectors and writes a rescue map marking them. It
// returns the cue path.
func rescueDisc(t *testing.T, lbas ...int64) string {
	t.Helper()
	dir := t.TempDir()
	bin := testcd.MODE1(testiso.Build(), 0)
	size := int64(len(bin))

	var mapText string
	if len(lbas) > 0 {
		for _, lba := range lbas {
			// The pattern is neither zero nor a byte the image holds, so a test
			// can tell "honored the map and zero-filled" from "read the bytes
			// anyway" from "this is the disc's own data".
			for i := lba * testcd.RawSectorSize; i < (lba+1)*testcd.RawSectorSize; i++ {
				bin[i] = 0xDB
			}
		}
		mapText = testcd.MapText(size, testcd.RawSectorSize, lbas...)
	}

	cuePath, err := testcd.Write(dir, "disc", "MODE1/2352", bin)
	if err != nil {
		t.Fatal(err)
	}
	if mapText != "" {
		if err := os.WriteFile(filepath.Join(dir, "disc.bin.map"), []byte(mapText), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return cuePath
}

// A damaged disc must produce the whole report and a DEGRADED exit, not an
// error. A verifier for damaged media must not abort on the first sector that
// would not read.
func TestVerifyDamagedReportsRatherThanAborting(t *testing.T) {
	cuePath := rescueDisc(t, 21)
	var code int
	out := testout.CaptureStdout(t, func() { code = runVerify([]string{cuePath}) })

	if code != subcmd.ExitDegraded {
		t.Errorf("verify of a damaged disc = %d; want %d (DEGRADED)", code, subcmd.ExitDegraded)
	}
	// The whole report, not a truncated one: structural checks, the filesystem
	// and a verdict.
	for _, want := range []string{"Structural", "sync headers", "Filesystem", "Result: DEGRADED"} {
		if !strings.Contains(out, want) {
			t.Errorf("report is missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "disc.bin.map") {
		t.Errorf("report does not name the sidecar it read:\n%s", out)
	}
	// The damaged sector belongs to BIG.DAT, which the report must list.
	if !strings.Contains(out, testiso.BigName) {
		t.Errorf("report does not name the file the bad sector cost:\n%s", out)
	}
}

// A sound disc reads clean, which gives the damaged case its baseline.
func TestVerifyCleanDisc(t *testing.T) {
	cuePath := rescueDisc(t)
	var code int
	out := testout.CaptureStdout(t, func() { code = runVerify([]string{cuePath}) })
	if code != subcmd.ExitOK {
		t.Errorf("verify of a sound disc = %d; want %d\n%s", code, subcmd.ExitOK, out)
	}
	if !strings.Contains(out, "Result: CLEAN") {
		t.Errorf("report:\n%s", out)
	}
}

// -no-map reads the image as it is, so an operator can see what the bytes hold
// when the map may be stale.
func TestVerifyNoMap(t *testing.T) {
	cuePath := rescueDisc(t, 21)
	out := testout.CaptureStdout(t, func() { runVerify([]string{"-no-map", cuePath}) })
	if strings.Contains(out, "disc.bin.map") {
		t.Errorf("-no-map still read the sidecar:\n%s", out)
	}
	// Without the map the damaged sector is read, and its EDC no longer matches.
	if !strings.Contains(out, "EDC") {
		t.Errorf("report:\n%s", out)
	}
}

// -map and -no-map together are contradictory and are rejected before the
// image is touched.
func TestVerifyMapFlagsConflict(t *testing.T) {
	cuePath := rescueDisc(t, 21)
	mapPath := filepath.Join(filepath.Dir(cuePath), "disc.bin.map")
	if code := runVerify([]string{"-no-map", "-map", mapPath, cuePath}); code != subcmd.ExitUsage {
		t.Errorf("verify with both -map and -no-map = %d; want %d", code, subcmd.ExitUsage)
	}
}

// A damaged BIN exports a finished ISO plus a map, in the exported file's own
// offsets, marking which of its bytes are zeros for unrecovered data. This lets
// fstool read the artifact through the damage rather than decoding it as data.
func TestExportWritesTranslatedMap(t *testing.T) {
	t.Parallel()
	const damagedLBA = testiso.BigLBA + 1
	cuePath := rescueDisc(t, damagedLBA)
	out := t.TempDir()

	// The artifact and its map are the correct output, so every track was
	// exported. The disc was damaged, not the run, and the status says so.
	if code := runExport([]string{"-o", out, cuePath}); code != subcmd.ExitDegraded {
		t.Fatalf("export of a damaged disc = %d; want %d", code, subcmd.ExitDegraded)
	}

	isoPath := filepath.Join(out, "track01.iso")
	data, err := os.ReadFile(isoPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != testiso.Sectors*testiso.SectorSize {
		t.Fatalf("track01.iso = %d bytes; want %d", len(data), testiso.Sectors*testiso.SectorSize)
	}

	// The lost sector is zeros, and only that sector: the disc's own bytes
	// either side of it are untouched, and 0xDB reaches the artifact nowhere.
	hole := data[damagedLBA*testiso.SectorSize : (damagedLBA+1)*testiso.SectorSize]
	for i, b := range hole {
		if b != 0 {
			t.Fatalf("exported byte %d of the lost sector = 0x%02x; want 0", i, b)
		}
	}
	if data[damagedLBA*testiso.SectorSize-1] == 0 {
		t.Error("the byte before the hole was zeroed too")
	}

	blocks, err := ddrescue.ParseMap(isoPath + ".map")
	if err != nil {
		t.Fatalf("the exported map does not parse: %v", err)
	}
	lost := ddrescue.Unrecovered(blocks, 0, int64(len(data)))
	if len(lost) != 1 || lost[0].Offset != damagedLBA*testiso.SectorSize || lost[0].Size != testiso.SectorSize {
		t.Fatalf("exported map reports %v; want one lost sector at %d", lost, damagedLBA*testiso.SectorSize)
	}
}

// A disc with no map beside it produces no map beside the export either: an
// all-recovered map would assert a recovery that was never measured.
func TestExportWithoutSourceMapWritesNone(t *testing.T) {
	t.Parallel()
	cuePath := rescueDisc(t)
	out := t.TempDir()
	if code := runExport([]string{"-o", out, cuePath}); code != subcmd.ExitOK {
		t.Fatalf("export = %d", code)
	}
	if _, err := os.Stat(filepath.Join(out, "track01.iso.map")); err == nil {
		t.Error("a source with no rescue map produced one for the export")
	}
}

// A map that cannot be written is a failure, not a warning: unrecorded zeros
// are worse than a short file.
func TestExportMapWriteFailure(t *testing.T) {
	t.Parallel()
	cuePath := rescueDisc(t, testiso.BigLBA+1)
	out := t.TempDir()
	// Stand something in the map's way that -f would be needed to replace.
	if err := os.WriteFile(filepath.Join(out, "track01.iso.map"), []byte("in the way\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := runExport([]string{"-o", out, cuePath}); code != subcmd.ExitFailure {
		t.Errorf("export that could not write its map = %d; want %d", code, subcmd.ExitFailure)
	}
	if code := runExport([]string{"-f", "-o", out, cuePath}); code != subcmd.ExitDegraded {
		t.Errorf("export with -f = %d; want %d, the damage still being there", code, subcmd.ExitDegraded)
	}
}
