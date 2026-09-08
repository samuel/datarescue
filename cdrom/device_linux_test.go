//go:build linux

package cdrom

import (
	"bytes"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"samuelks.com/datarescue/internal/testiso"
)

// These tests drive the real ioctls against a real device node, which is the
// only way to check the hand-transcribed linux/cdrom.h and scsi/sg.h structs:
// a wrong field size does not fail to compile, it silently shifts every field
// after it. They are meant to run inside the virtual machine "make qemutest"
// starts, where the attached disc is the image internal/testiso builds.
//
// Everywhere else they skip, so "go test ./..." stays meaningful on a machine
// with no optical drive and does not touch one that happens to be there.
const (
	// deviceEnv holds the path of the device to test against.
	deviceEnv = "DATARESCUE_CDROM_DEVICE"
	// badLBAEnv holds an LBA the host arranged to fail, for the tests that
	// need a read error the emulator cannot otherwise produce.
	badLBAEnv = "DATARESCUE_CDROM_BAD_LBA"
)

// testDevice opens the device given in the environment, skipping the test when
// there is none. The kernel probes ATAPI asynchronously, so the node can appear
// a moment after userspace starts.
func testDevice(t *testing.T) *linuxDevice {
	t.Helper()
	path := os.Getenv(deviceEnv)
	if path == "" {
		t.Skipf("%s is not set; no CD-ROM device to test against", deviceEnv)
	}

	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never appeared", path)
		}
		time.Sleep(100 * time.Millisecond)
	}

	dev, err := Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	t.Cleanup(func() { _ = dev.Close() }) //nolint:errcheck // read-only device
	// Open returns the interface; the tests below reach the file descriptor to
	// exercise readFullTOC directly.
	ld, ok := dev.(*linuxDevice)
	if !ok {
		t.Fatalf("Open returned %T; want *linuxDevice", dev)
	}
	return ld
}

// injectedBadLBA returns the LBA the host made unreadable, or -1 when the disc
// is undamaged.
func injectedBadLBA(t *testing.T) int64 {
	t.Helper()
	v := os.Getenv(badLBAEnv)
	if v == "" {
		return -1
	}
	lba, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		t.Fatalf("%s=%q: %v", badLBAEnv, v, err)
	}
	return lba
}

func decToBCD(v byte) byte { return (v/10)<<4 | v%10 }

func TestDeviceTOC(t *testing.T) {
	dev := testDevice(t)

	toc, err := dev.TOC()
	if err != nil {
		t.Fatalf("TOC: %v", err)
	}
	// One data track covering an ISO image: the only disc shape an emulated
	// drive backed by an image file can present.
	if toc.FirstTrack != 1 || toc.LastTrack != 1 {
		t.Errorf("track range = %d-%d; want 1-1", toc.FirstTrack, toc.LastTrack)
	}
	if len(toc.Tracks) != 1 {
		t.Fatalf("got %d tracks; want 1", len(toc.Tracks))
	}
	tr := toc.Tracks[0]
	if tr.Number != 1 || tr.StartLBA != 0 || tr.Type != TrackTypeData || tr.Session != 1 {
		t.Errorf("track = %+v; want track 1, session 1, data, LBA 0", tr)
	}
	if toc.LeadOutLBA != testiso.Sectors {
		t.Errorf("LeadOutLBA = %d; want %d", toc.LeadOutLBA, testiso.Sectors)
	}
	if toc.HasAudio() {
		t.Error("HasAudio() = true on a data-only disc")
	}
}

func TestDeviceReadFullTOC(t *testing.T) {
	dev := testDevice(t)

	// This is the only check on sgIOHdr: a wrong offset in it shows up as
	// EINVAL from the ioctl or a nonzero SCSI status, not as a compile error.
	entries, err := readFullTOC(dev.f.Fd())
	if err != nil {
		t.Fatalf("readFullTOC: %v", err)
	}
	e, ok := entries[1]
	if !ok {
		t.Fatalf("Full TOC has no entry for track 1; got %v", entries)
	}
	if e.session != 1 {
		t.Errorf("track 1 session = %d; want 1", e.session)
	}
	// An emulated drive reports no INDEX 00 descriptors, so there is no pregap
	// to find here. Real pregaps are covered by the fake device in cmd/disctool.
	if e.index0LBA != 0 {
		t.Errorf("track 1 INDEX 00 = %d; want 0", e.index0LBA)
	}
}

func TestDeviceReadSector(t *testing.T) {
	dev := testDevice(t)
	want := testiso.Build()
	bad := injectedBadLBA(t)

	for lba := range int64(testiso.Sectors) {
		got, err := dev.ReadSector(lba)
		if lba == bad {
			// This sector is unreadable on purpose; the damage itself is
			// TestDeviceInjectedReadError's subject.
			if err == nil {
				t.Errorf("ReadSector(%d) succeeded; the host injected a read error there", lba)
			}
			continue
		}
		if err != nil {
			t.Fatalf("ReadSector(%d): %v", lba, err)
		}
		if len(got) != DataSectorSize {
			t.Fatalf("ReadSector(%d) returned %d bytes; want %d", lba, len(got), DataSectorSize)
		}
		if !bytes.Equal(got, want[lba*testiso.SectorSize:(lba+1)*testiso.SectorSize]) {
			t.Errorf("ReadSector(%d) does not match the image", lba)
		}
	}
}

func TestDeviceReadSectorPastLeadOut(t *testing.T) {
	dev := testDevice(t)

	if _, err := dev.ReadSector(testiso.Sectors + 100); err == nil {
		t.Error("ReadSector past the lead-out succeeded; want an error")
	}
}

func TestDeviceReadRawSector(t *testing.T) {
	dev := testDevice(t)

	const lba = 16 // the primary volume descriptor
	raw, err := dev.ReadRawSector(lba, TrackTypeData)
	if err != nil {
		t.Fatalf("ReadRawSector(%d): %v", lba, err)
	}
	if len(raw) != RawSectorSize {
		t.Fatalf("ReadRawSector returned %d bytes; want %d", len(raw), RawSectorSize)
	}

	wantSync := []byte{0x00, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0x00}
	if !bytes.Equal(raw[:12], wantSync) {
		t.Errorf("sync pattern = % x; want % x", raw[:12], wantSync)
	}
	// Real media stores the header address in BCD, as ECMA-130 requires; an
	// emulated drive synthesizes it in plain binary. Either encoding shows the
	// read landed on the requested sector, which is what this checks.
	m, s, f := LBAToMSF(lba)
	bcd := raw[12] == decToBCD(m) && raw[13] == decToBCD(s) && raw[14] == decToBCD(f)
	plain := raw[12] == m && raw[13] == s && raw[14] == f
	if !bcd && !plain {
		t.Errorf("MSF header = %02x:%02x:%02x; want %d:%d:%d in BCD or binary",
			raw[12], raw[13], raw[14], m, s, f)
	}
	if raw[15] != 0x01 {
		t.Errorf("mode byte = %d; want 1", raw[15])
	}

	cooked, err := dev.ReadSector(lba)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw[16:16+DataSectorSize], cooked) {
		t.Error("raw sector user data differs from the cooked read of the same sector")
	}

	// An emulated drive synthesizes the sync and header but not the error
	// correction, so the tail is zeros. Real media carries an EDC there, which
	// is what cuebin.VerifyBIN checks; asserting it here would only assert the
	// emulator's shortcut.
	if !bytes.Equal(raw[2064:], make([]byte, RawSectorSize-2064)) {
		t.Log("raw sector carries error correction data; the drive is not the emulated one these tests assume")
	}
}

func TestDeviceReadAudioIsWellFormed(t *testing.T) {
	dev := testDevice(t)

	// Whether a drive will read a data track in audio mode is up to the drive:
	// most refuse, an emulated one obliges. Either answer is fine. What must
	// not happen is the kernel rejecting the request itself, which is how a
	// wrong cdromReadAudio layout shows up -- the fields it cares about would
	// hold whatever the neighboring ones do.
	got, err := dev.ReadRawSector(16, TrackTypeAudio)
	switch {
	case err == nil:
		if len(got) != RawSectorSize {
			t.Errorf("audio read returned %d bytes; want %d", len(got), RawSectorSize)
		}
	case errors.Is(err, unix.EINVAL), errors.Is(err, unix.EFAULT), errors.Is(err, unix.ENOTTY):
		t.Errorf("audio read was rejected as malformed: %v", err)
	}
}

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

	dev := &linuxDevice{f: f, retries: 1}
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

func TestDeviceInjectedReadError(t *testing.T) {
	dev := testDevice(t)
	bad := injectedBadLBA(t)
	if bad < 0 {
		t.Skipf("%s is not set; no injected read error to test against", badLBAEnv)
	}

	// A drive error has to reach the caller as an error rather than as a short
	// or zeroed sector: the rip decides what to record from this one verdict.
	if _, err := dev.ReadSector(bad); err == nil {
		t.Errorf("ReadSector(%d) succeeded; the host injected a read error there", bad)
	}
	if _, err := dev.ReadRawSector(bad, TrackTypeData); err == nil {
		t.Errorf("ReadRawSector(%d) succeeded; the host injected a read error there", bad)
	}
	// Damage must not spread. These reads go to the drive one sector at a time,
	// so a neighbor failing would mean the error was attributed to the wrong
	// place, and the rip would write off sectors it could have recovered.
	for lba := range int64(testiso.Sectors) {
		if lba == bad {
			continue
		}
		if _, err := dev.ReadSector(lba); err != nil {
			t.Errorf("ReadSector(%d) failed: %v; only LBA %d should be unreadable", lba, err, bad)
		}
	}
}

// The kernel's struct cdrom_multisession is a 4-byte union cdrom_addr, then
// xa_flag and addr_format, padded to 8. A pad between the address and XAFlag
// would put Format at byte 9, so the kernel would read the format from padding
// and reject every CDROMMULTISESSION with EINVAL. TOC() treats that as a
// single-session disc, so ripping a multisession disc's last session would
// silently rip the first.
func TestCdromMultiSessionLayout(t *testing.T) {
	t.Parallel()
	var ms cdromMultiSession
	if got := unsafe.Sizeof(ms); got != 8 {
		t.Errorf("sizeof cdrom_multisession = %d; the kernel struct is 8 bytes", got)
	}
	for _, tc := range []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"addr", unsafe.Offsetof(ms.AddrLBA), 0},
		{"xa_flag", unsafe.Offsetof(ms.XAFlag), 4},
		{"addr_format", unsafe.Offsetof(ms.Format), 5},
	} {
		if tc.got != tc.want {
			t.Errorf("%s is at byte %d; the kernel puts it at %d", tc.name, tc.got, tc.want)
		}
	}
}

// The other transcribed structs, for the same reason: a wrong field size or a
// stray pad shifts every field after it, and the ioctl then fails or reads
// from somewhere else on the disc.
func TestIoctlStructLayouts(t *testing.T) {
	t.Parallel()
	var e cdromTOCEntry
	if got := unsafe.Sizeof(e); got != 12 {
		t.Errorf("sizeof cdrom_tocentry = %d; want 12", got)
	}
	if got := unsafe.Offsetof(e.AddrLBA); got != 4 {
		t.Errorf("cdrom_tocentry addr is at byte %d; want 4", got)
	}
	var h sgIOHdr
	if got := unsafe.Sizeof(h); got != 88 {
		t.Errorf("sizeof sg_io_hdr_t = %d; want 88 on 64-bit Linux", got)
	}
}
