package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"samuelks.com/datarescue/cdrom"
	"samuelks.com/datarescue/cuebin"
	"samuelks.com/datarescue/internal/subcmd"
	"samuelks.com/datarescue/internal/testcd"
	"samuelks.com/datarescue/internal/testout"
)

// writeDisc renders a small MODE1/2352 BIN with a matching cue sheet and
// returns the cue path.
func writeDisc(t *testing.T, sectors int) string {
	t.Helper()
	dir := t.TempDir()
	binPath := filepath.Join(dir, "disc.bin")

	var data []byte
	sync := [12]byte{0x00, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0x00}
	for lba := range int64(sectors) {
		sec := make([]byte, cdrom.RawSectorSize)
		copy(sec[:12], sync[:])
		m, s, f := cdrom.LBAToMSF(lba)
		sec[12], sec[13], sec[14] = (m/10)<<4|m%10, (s/10)<<4|s%10, (f/10)<<4|f%10
		sec[15] = 0x01
		for j := 16; j < 2064; j++ {
			sec[j] = byte(lba)
		}
		binary.LittleEndian.PutUint32(sec[2064:2068], 0) // EDC left wrong on purpose
		data = append(data, sec...)
	}
	if err := os.WriteFile(binPath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	cuePath := filepath.Join(dir, "disc.cue")
	cue := "FILE \"disc.bin\" BINARY\n  TRACK 01 MODE1/2352\n    INDEX 01 00:00:00\n"
	if err := os.WriteFile(cuePath, []byte(cue), 0o600); err != nil {
		t.Fatal(err)
	}
	return cuePath
}

func TestRunExport(t *testing.T) {
	cuePath := writeDisc(t, 4)
	out := t.TempDir()
	if code := runExport([]string{"-o", out, cuePath}); code != subcmd.ExitOK {
		t.Fatalf("export = %d", code)
	}
	iso := filepath.Join(out, "track01.iso")
	info, err := os.Stat(iso)
	if err != nil {
		t.Fatalf("exported track missing: %v", err)
	}
	if info.Size() != 4*2048 {
		t.Errorf("track01.iso = %d bytes; want %d", info.Size(), 4*2048)
	}

	// A second export must refuse rather than truncate, and report it.
	if code := runExport([]string{"-o", out, cuePath}); code != subcmd.ExitFailure {
		t.Errorf("re-export = %d; want %d (existing tracks must not be overwritten)", code, subcmd.ExitFailure)
	}
	if code := runExport([]string{"-f", "-o", out, cuePath}); code != subcmd.ExitOK {
		t.Errorf("re-export with -f = %d", code)
	}
}

// The output directory is created rather than failing once per track.
func TestRunExportCreatesOutputDir(t *testing.T) {
	cuePath := writeDisc(t, 2)
	out := filepath.Join(t.TempDir(), "does", "not", "exist")
	if code := runExport([]string{"-o", out, cuePath}); code != subcmd.ExitOK {
		t.Fatalf("export into a missing directory = %d", code)
	}
	if _, err := os.Stat(filepath.Join(out, "track01.iso")); err != nil {
		t.Errorf("track not written: %v", err)
	}
}

// Selecting a track that is not on the disc exports nothing, which is a
// failure rather than a silent success.
func TestRunExportNoTracksMatched(t *testing.T) {
	cuePath := writeDisc(t, 2)
	if code := runExport([]string{"-tracks", "7", "-o", t.TempDir(), cuePath}); code != subcmd.ExitFailure {
		t.Errorf("export of an absent track = %d; want %d", code, subcmd.ExitFailure)
	}
}

func TestRunVerify(t *testing.T) {
	cuePath := writeDisc(t, 4)
	// The fixture's EDC is deliberately wrong, so verify must report it.
	if code := runVerify([]string{cuePath}); code == subcmd.ExitOK {
		t.Error("verify reported a clean result on a disc with bad EDC")
	}
}

func TestUsageErrors(t *testing.T) {
	cuePath := writeDisc(t, 2)
	if code := runExport(nil); code != subcmd.ExitUsage {
		t.Errorf("export without a cue = %d; want %d", code, subcmd.ExitUsage)
	}
	if code := runVerify(nil); code != subcmd.ExitUsage {
		t.Errorf("verify without a cue = %d; want %d", code, subcmd.ExitUsage)
	}
	if code := runExport([]string{"-tracks", "abc", "-o", t.TempDir(), cuePath}); code != subcmd.ExitUsage {
		t.Errorf("export with a bad track list = %d; want %d", code, subcmd.ExitUsage)
	}
	missing := filepath.Join(t.TempDir(), "missing.cue")
	if code := runVerify([]string{missing}); code != subcmd.ExitFailure {
		t.Errorf("verify on a missing cue = %d; want %d", code, subcmd.ExitFailure)
	}
}

// A BIN cut mid-sector is a short rip and must affect the exit code, not just
// appear at the top of the report.
func TestVerifySizeUnalignedIsDegraded(t *testing.T) {
	// A sound disc, EDC and all, so the unaligned size is the only finding —
	// otherwise a failing EDC would carry the exit code on its own.
	payload := make([]byte, 4*2048)
	for i := range payload {
		payload[i] = byte(i)
	}
	bin := testcd.MODE1(payload, 0)
	dir := t.TempDir()
	cuePath, err := testcd.Write(dir, "disc", "MODE1/2352", bin)
	if err != nil {
		t.Fatal(err)
	}
	sound := testout.CaptureStdout(t, func() {
		if code := runVerify([]string{cuePath}); code != subcmd.ExitOK {
			t.Errorf("verify of a sound disc = %d; want %d", code, subcmd.ExitOK)
		}
	})
	if !strings.Contains(sound, "Result: CLEAN") {
		t.Fatalf("the fixture is not a clean disc, so the unaligned case proves nothing:\n%s", sound)
	}

	// Lop off half a sector: nothing else about the disc has changed.
	if err := os.WriteFile(filepath.Join(dir, "disc.bin"), bin[:len(bin)-cdrom.RawSectorSize/2], 0o600); err != nil {
		t.Fatal(err)
	}
	out := testout.CaptureStdout(t, func() {
		if code := runVerify([]string{cuePath}); code != subcmd.ExitDegraded {
			t.Errorf("verify of a BIN cut mid-sector = %d; want %d", code, subcmd.ExitDegraded)
		}
	})
	if !strings.Contains(out, "not a multiple") {
		t.Errorf("the report does not name the unaligned size:\n%s", out)
	}
}

// The header line describes what the BIN holds, based on the track modes. An
// audio-only disc has no MODE1 sectors even though its stride is 2352 bytes.
func TestDiscModeString(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		tracks []cuebin.CueTrack
		want   string
	}{
		{"data", []cuebin.CueTrack{{Mode: "MODE1/2352"}}, "MODE1/2352"},
		{"cooked", []cuebin.CueTrack{{Mode: "MODE1/2048"}}, "MODE1/2048"},
		{"mode 2", []cuebin.CueTrack{{Mode: "MODE2/2352"}}, "MODE2/2352"},
		{"audio only", []cuebin.CueTrack{{Mode: "AUDIO"}, {Mode: "AUDIO"}}, "AUDIO"},
		{"mixed", []cuebin.CueTrack{{Mode: "AUDIO"}, {Mode: "MODE2/2352"}}, "MODE2/2352"},
		{"no tracks", nil, "no tracks"},
	}
	for _, tc := range tests {
		if got := discModeString(tc.tracks); got != tc.want {
			t.Errorf("%s: discModeString = %q; want %q", tc.name, got, tc.want)
		}
	}
}
