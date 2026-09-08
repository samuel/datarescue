package img86f

import (
	"testing"

	"samuelks.com/datarescue/internal/testfixture"
)

func TestCRC16(t *testing.T) {
	// Known IDAM CRC from floppyout.86f track 0: A1 A1 A1 FE 00 00 01 02 → 0xCA6F
	got := crc16([]byte{0xA1, 0xA1, 0xA1, 0xFE, 0x00, 0x00, 0x01, 0x02})
	if got != 0xCA6F {
		t.Errorf("crc16 = 0x%04X, want 0xCA6F", got)
	}
}

func TestDecodeMFMByte(t *testing.T) {
	// 0x4E (gap fill) MFM-encodes to 0x92 0x54
	got := decodeMFMByte(0x92, 0x54)
	if got != 0x4E {
		t.Errorf("decodeMFMByte(0x92, 0x54) = 0x%02X, want 0x4E", got)
	}
	// 0xA1 sync with clock violation encodes to 0x44 0x89
	got = decodeMFMByte(0x44, 0x89)
	if got != 0xA1 {
		t.Errorf("decodeMFMByte(0x44, 0x89) = 0x%02X, want 0xA1", got)
	}
}

const testFile = "testdata/floppyout.86f.gz"

func openTestDisk(t *testing.T) *Disk {
	t.Helper()
	d, err := Open(testfixture.File(t, testFile))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { d.Close() }) //nolint:errcheck // test cleanup
	return d
}

func TestGeometry(t *testing.T) {
	d := openTestDisk(t)
	tracks, sides := d.Geometry()
	if tracks != 80 || sides != 2 {
		t.Errorf("Geometry() = (%d, %d), want (80, 2)", tracks, sides)
	}
}

func TestReadTrack(t *testing.T) {
	d := openTestDisk(t)
	sectors, err := d.ReadTrack(0, 0)
	if err != nil {
		t.Fatalf("ReadTrack(0, 0): %v", err)
	}
	if len(sectors) != 18 {
		t.Errorf("got %d sectors, want 18", len(sectors))
	}
	for _, s := range sectors {
		if s.CRCError {
			t.Errorf("sector %d has CRC error", s.SectorNum)
		}
		if s.SizeCode != 2 {
			t.Errorf("sector %d: size_code = %d, want 2 (512 bytes)", s.SectorNum, s.SizeCode)
		}
	}
}

func TestReadSector(t *testing.T) {
	d := openTestDisk(t)
	data, err := d.ReadSector(0, 0, 1)
	if err != nil {
		t.Fatalf("ReadSector(0, 0, 1): %v", err)
	}
	if len(data) != 512 {
		t.Fatalf("sector length = %d, want 512", len(data))
	}
	// Boot sector: x86 jump + OEM name "FRDOS4.1"
	if data[0] != 0xEB {
		t.Errorf("sector[0] = 0x%02X, want 0xEB (x86 short jump)", data[0])
	}
	oem := string(data[3:11])
	if oem != "FRDOS4.1" {
		t.Errorf("OEM name = %q, want \"FRDOS4.1\"", oem)
	}
}

func TestReadSectorNotFound(t *testing.T) {
	d := openTestDisk(t)
	_, err := d.ReadSector(0, 0, 99)
	if err == nil {
		t.Fatal("expected error for non-existent sector")
	}
}

func TestAbsentTrack(t *testing.T) {
	d := openTestDisk(t)
	// Tracks 80–255 are absent (zero offset): should return nil sectors, no error.
	sectors, err := d.ReadTrack(200, 0)
	if err != nil {
		t.Errorf("ReadTrack(200, 0): unexpected error: %v", err)
	}
	if sectors != nil {
		t.Errorf("expected nil sectors for absent track, got %d", len(sectors))
	}
	// Track index ≥ maxTracksPerSide (256) is genuinely out of range.
	_, err = d.ReadTrack(300, 0)
	if err == nil {
		t.Error("expected error for out-of-range track 300")
	}
}
