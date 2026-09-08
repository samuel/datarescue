package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"samuelks.com/datarescue/ddrescue"
	"samuelks.com/datarescue/internal/subcmd"
	"samuelks.com/datarescue/internal/testiso"
)

// These tests rip a real device node, which the tests above deliberately do not
// touch: they cover the rip decisions with a fake drive, and this covers the
// one thing a fake cannot, that the command works against a kernel. They are
// meant to run inside the virtual machine "make qemutest" starts, and skip
// everywhere else.
const (
	deviceEnv = "DATARESCUE_CDROM_DEVICE"
	badLBAEnv = "DATARESCUE_CDROM_BAD_LBA"
)

func testDevicePath(t *testing.T) string {
	t.Helper()
	path := os.Getenv(deviceEnv)
	if path == "" {
		t.Skipf("%s is not set; no CD-ROM device to rip", deviceEnv)
	}
	return path
}

func TestRipARealDevice(t *testing.T) {
	device := testDevicePath(t)
	if os.Getenv(badLBAEnv) != "" {
		t.Skipf("%s is set; the disc is damaged on purpose", badLBAEnv)
	}
	out := filepath.Join(t.TempDir(), "disc.iso")

	if code := runRip([]string{"-map", "-verbose", device, out}); code != subcmd.ExitOK {
		t.Fatalf("runRip = %d; want %d", code, subcmd.ExitOK)
	}

	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, testiso.Build()) {
		t.Errorf("ripped image differs from the disc (%d bytes vs %d)",
			len(got), testiso.Sectors*testiso.SectorSize)
	}

	blocks, err := ddrescue.ParseMap(out + ".map")
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 1 || blocks[0].Status != ddrescue.BlockStatusFinished ||
		blocks[0].Size != testiso.Sectors*testiso.SectorSize {
		t.Errorf("map = %+v; want one finished block covering the whole image", blocks)
	}
}

func TestRipRecordsAnInjectedBadSector(t *testing.T) {
	device := testDevicePath(t)
	v := os.Getenv(badLBAEnv)
	if v == "" {
		t.Skipf("%s is not set; no injected read error to rip over", badLBAEnv)
	}
	bad, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		t.Fatalf("%s=%q: %v", badLBAEnv, v, err)
	}
	out := filepath.Join(t.TempDir(), "disc.iso")

	// A rip over damage produces a usable image and says the disc was not
	// sound: the point of the tool is to recover what it can and record the
	// rest, and the status distinguishes that from a rip that failed.
	if code := runRip([]string{"-map", "-verbose", device, out}); code != subcmd.ExitDegraded {
		t.Fatalf("runRip over injected damage = %d; want %d", code, subcmd.ExitDegraded)
	}

	img, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(img)) != testiso.Sectors*testiso.SectorSize {
		t.Fatalf("image is %d bytes; want %d", len(img), testiso.Sectors*testiso.SectorSize)
	}
	sec := img[bad*testiso.SectorSize : (bad+1)*testiso.SectorSize]
	if !bytes.Equal(sec, make([]byte, testiso.SectorSize)) {
		t.Errorf("sector %d is not zero-filled; the injected error was not detected", bad)
	}

	blocks, err := ddrescue.ParseMap(out + ".map")
	if err != nil {
		t.Fatal(err)
	}
	// A drive that reads ahead can fail more than the one injected sector, so
	// the map has to cover that sector rather than exactly it.
	var covered bool
	for _, b := range blocks {
		if b.Status == ddrescue.BlockStatusFinished {
			continue
		}
		start := b.Offset / testiso.SectorSize
		if bad >= start && bad < start+b.Size/testiso.SectorSize {
			covered = true
		}
	}
	if !covered {
		t.Errorf("map does not mark LBA %d as bad: %+v", bad, blocks)
	}
}
