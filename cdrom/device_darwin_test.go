//go:build darwin

package cdrom

import (
	"bytes"
	"math"
	"os"
	"testing"
)

// TestReadSectorCookedReadsThePlainBlockPath needs no drive: ReadSectorCooked
// is a plain ReadAt over whatever *os.File the Device holds, the same path
// mount and dd use, so a regular file stands in for the device node.
func TestReadSectorCookedReadsThePlainBlockPath(t *testing.T) {
	const sectors = 4
	buf := make([]byte, sectors*DataSectorSize)
	for lba := range int64(sectors) {
		sec := buf[lba*DataSectorSize : (lba+1)*DataSectorSize]
		for i := range sec {
			sec[i] = byte(lba + 1)
		}
	}
	f, err := os.CreateTemp(t.TempDir(), "cooked-*.img")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck // read-only test file
	if _, err := f.Write(buf); err != nil {
		t.Fatal(err)
	}

	dev := &darwinDevice{f: f, retries: 1}
	for lba := range int64(sectors) {
		got, err := dev.ReadSectorCooked(lba)
		if err != nil {
			t.Fatalf("ReadSectorCooked(%d): %v", lba, err)
		}
		want := buf[lba*DataSectorSize : (lba+1)*DataSectorSize]
		if !bytes.Equal(got, want) {
			t.Errorf("ReadSectorCooked(%d) did not match the underlying bytes", lba)
		}
	}

	// A DVD's LBAs routinely exceed MaxLBA -- unlike ReadSector, that must not
	// be rejected here.
	if _, err := dev.ReadSectorCooked(MaxLBA + 1000); err == nil {
		t.Error("ReadSectorCooked(MaxLBA+1000) succeeded past EOF; want an error")
	}
	if _, err := dev.ReadSectorCooked(-1); err == nil {
		t.Error("ReadSectorCooked(-1) succeeded; want an error")
	}
}

func TestTOCFromBlockGeometry(t *testing.T) {
	t.Parallel()
	// The geometry a 5.1 GB DVD-ROM reports: 2048-byte blocks, one per sector.
	toc, err := tocFromBlockGeometry(2048, 2481232)
	if err != nil {
		t.Fatal(err)
	}
	if !toc.NotCD {
		t.Error("NotCD = false; a TOC synthesized from block geometry is not a CD's")
	}
	if !toc.NeedsCookedReads() {
		t.Error("NeedsCookedReads() = false for a synthesized TOC")
	}
	if toc.LeadOutLBA != 2481232 {
		t.Errorf("LeadOutLBA = %d; want 2481232", toc.LeadOutLBA)
	}
	if len(toc.Tracks) != 1 || toc.Tracks[0].Type != TrackTypeData || toc.Tracks[0].StartLBA != 0 {
		t.Errorf("tracks = %+v; want one data track starting at LBA 0", toc.Tracks)
	}
	// The rip loop takes its bounds from the TOC, so a synthesized one has to
	// answer the same queries a read one does.
	if _, idx := toc.LastDataSession(); idx != 0 {
		t.Errorf("LastDataSession() track index = %d; want 0", idx)
	}
	if toc.HasAudio() {
		t.Error("HasAudio() = true for a synthesized data-only TOC")
	}

	// A block size other than 2048 is converted, not assumed: the lead-out is
	// counted in the 2048-byte sectors every LBA in this package means.
	toc, err = tocFromBlockGeometry(512, 9924928)
	if err != nil {
		t.Fatal(err)
	}
	if toc.LeadOutLBA != 2481232 {
		t.Errorf("512-byte blocks: LeadOutLBA = %d; want 2481232", toc.LeadOutLBA)
	}

	// An absent medium, a medium too small to hold one sector, and a geometry
	// whose byte count will not fit an int64 are all reported rather than
	// yielding a TOC covering nothing.
	for _, tc := range []struct {
		name       string
		blockSize  uint32
		blockCount uint64
	}{
		{"no blocks", 2048, 0},
		{"no block size", 0, 100},
		{"less than one sector", 512, 3},
		{"byte count past an int64", math.MaxUint32, math.MaxUint64 / 2},
	} {
		if _, err := tocFromBlockGeometry(tc.blockSize, tc.blockCount); err == nil {
			t.Errorf("%s: tocFromBlockGeometry(%d, %d) succeeded; want an error",
				tc.name, tc.blockSize, tc.blockCount)
		}
	}
}
