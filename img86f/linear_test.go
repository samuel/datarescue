package img86f

import (
	"errors"
	"io"
	"testing"
)

func TestDetectGeometry(t *testing.T) {
	d := openTestDisk(t)
	spt, heads, sectorSize, err := DetectGeometry(d)
	if err != nil {
		t.Fatalf("DetectGeometry: %v", err)
	}
	if spt != 18 || heads != 2 || sectorSize != 512 {
		t.Errorf("got spt=%d heads=%d sectorSize=%d, want 18/2/512", spt, heads, sectorSize)
	}
}

func TestNewLinearReaderSize(t *testing.T) {
	d := openTestDisk(t)
	r := newLinearReaderT(t, d, 18, 2, 512)
	// Seek to end to verify total size = 80 tracks × 2 heads × 18 sectors × 512 bytes
	n, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		t.Fatalf("Seek to end: %v", err)
	}
	const want = 80 * 2 * 18 * 512
	if n != want {
		t.Errorf("size = %d, want %d", n, want)
	}
}

func TestLinearReaderReadBootSector(t *testing.T) {
	d := openTestDisk(t)
	r := newLinearReaderT(t, d, 18, 2, 512)

	buf := make([]byte, 512)
	n, err := r.Read(buf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if n != 512 {
		t.Fatalf("read %d bytes, want 512", n)
	}
	// Same boot sector as ReadSector(0,0,1): jump + "FRDOS4.1"
	if buf[0] != 0xEB {
		t.Errorf("buf[0] = 0x%02X, want 0xEB", buf[0])
	}
	if oem := string(buf[3:11]); oem != "FRDOS4.1" {
		t.Errorf("OEM = %q, want \"FRDOS4.1\"", oem)
	}
}

func TestLinearReaderCrossTrackRead(t *testing.T) {
	d := openTestDisk(t)
	r := newLinearReaderT(t, d, 18, 2, 512)

	// Read across the end of track 0 head 0 into track 0 head 1.
	// Track 0 head 0: sectors 1–18 = 18×512 = 9216 bytes.
	// Seek to 512 bytes before end of head 0, then read 1024 bytes.
	if _, err := r.Seek(int64(17*512), io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	buf := make([]byte, 1024)
	n, err := r.Read(buf)
	if err != nil {
		t.Fatalf("cross-track Read: %v", err)
	}
	if n != 1024 {
		t.Errorf("read %d bytes, want 1024", n)
	}
}

func TestLinearReaderSeekAndRead(t *testing.T) {
	d := openTestDisk(t)
	r := newLinearReaderT(t, d, 18, 2, 512)

	// LBA 1 = track 0 head 0 sector 2; should match ReadSector(0,0,2).
	if _, err := r.Seek(512, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	linearBuf := make([]byte, 512)
	if _, err := r.Read(linearBuf); err != nil {
		t.Fatalf("Read: %v", err)
	}

	directBuf, err := d.ReadSector(0, 0, 2)
	if err != nil {
		t.Fatalf("ReadSector: %v", err)
	}
	for i, b := range directBuf {
		if linearBuf[i] != b {
			t.Errorf("byte %d: linear=0x%02X direct=0x%02X", i, linearBuf[i], b)
			break
		}
	}
}

func TestLinearReaderEOF(t *testing.T) {
	d := openTestDisk(t)
	r := newLinearReaderT(t, d, 18, 2, 512)

	if _, err := r.Seek(0, io.SeekEnd); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	n, err := r.Read(make([]byte, 1))
	if !errors.Is(err, io.EOF) {
		t.Errorf("expected io.EOF at end, got err=%v n=%d", err, n)
	}
}

func TestLinearReaderSeekNegative(t *testing.T) {
	d := openTestDisk(t)
	r := newLinearReaderT(t, d, 18, 2, 512)

	_, err := r.Seek(-1, io.SeekStart)
	if err == nil {
		t.Error("expected error for negative seek")
	}
}

// newLinearReaderT is NewLinearReader with the geometry check already known to
// pass, so each test reads as one statement.
func newLinearReaderT(t *testing.T, d *Disk, spt, heads, sectorSize int) io.ReadSeeker {
	t.Helper()
	r, err := NewLinearReader(d, spt, heads, sectorSize)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// Every LBA is divided by sectorsPerTrack*heads, so a zero in the geometry
// would panic and must be refused up front.
func TestNewLinearReaderRejectsZeroGeometry(t *testing.T) {
	d := openTestDisk(t)
	for _, g := range [][3]int{{0, 2, 512}, {18, 0, 512}, {18, 2, 0}, {-1, 2, 512}} {
		if _, err := NewLinearReader(d, g[0], g[1], g[2]); err == nil {
			t.Errorf("NewLinearReader(%d, %d, %d) = nil error; want a refusal", g[0], g[1], g[2])
		}
	}
}
