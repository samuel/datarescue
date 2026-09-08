package onestep

import (
	"bytes"
	"encoding/binary"
	"testing"

	"samuelks.com/datarescue/internal/dbf"
	"samuelks.com/datarescue/internal/test1step"
)

// fakeDeadHeader is a plausible dBase table whose declared record count is
// dead: no real catalog can hold 2.1 billion rows, but the header passes
// PlausibleHeader and Open so the scan stops to consider it.
func fakeDeadHeader() []byte {
	h := make([]byte, 32+2*32+1)
	h[0] = 0x03
	h[1] = 1
	h[2] = 1
	h[3] = 1
	binary.LittleEndian.PutUint32(h[4:], 0x7FFFFFFF)
	binary.LittleEndian.PutUint16(h[8:], uint16(len(h)))
	binary.LittleEndian.PutUint16(h[10:], 21)
	off := 1
	for i, name := range []string{"A", "B"} {
		d := h[32+i*32 : 32+i*32+32]
		copy(d[:11], name)
		d[11] = 'C'
		binary.LittleEndian.PutUint32(d[12:], uint32(off))
		d[16] = 10
		off += 10
	}
	h[len(h)-1] = 0x0D
	return h
}

// TestScanTablesBoundedByClaimedTable reproduces the damage the missing bound
// used to miss: a dead-count header early in the scan region inflates its
// Size(), and with the whole rest of the image as its limit it jumps the scan
// cursor past every real table after it. Bounding the region by the next table
// already claimed keeps the jump inside the gap, so the tables beyond are
// still reached.
func TestScanTablesBoundedByClaimedTable(t *testing.T) {
	disk := test1step.Build(test1step.Options{Disks: 1})[0]
	catOff := int64(binary.LittleEndian.Uint32(disk[0x1c:]))
	members, err := readContainer(bytes.NewReader(disk), catOff, int64(len(disk)))
	if err != nil {
		t.Fatalf("read the synthetic catalog: %v", err)
	}
	var iojob, iosess []byte
	for _, m := range members {
		switch m.Name {
		case "IOJOB.DBF":
			iojob = disk[m.Offset : m.Offset+m.Size]
		case "IOSESS.DBF":
			iosess = disk[m.Offset : m.Offset+m.Size]
		}
	}
	if iojob == nil || iosess == nil {
		t.Fatal("the synthetic catalog lacks the real tables the test needs")
	}

	// The fake sits exactly at the scan start; the real tables follow it at
	// where the scan has to be still alive to reach them.
	gapBeforeJob := 1024
	var img bytes.Buffer
	img.Grow(HeaderSize + len(fakeDeadHeader()) + gapBeforeJob + len(iojob) + 512 + len(iosess))
	hdr := make([]byte, HeaderSize)
	binary.LittleEndian.PutUint32(hdr[0x1c:], uint32(HeaderSize))
	img.Write(hdr)
	img.Write(fakeDeadHeader())
	img.Write(bytes.Repeat([]byte{0xE5}, gapBeforeJob))
	iojobAt := int64(img.Len())
	img.Write(iojob)
	img.Write(bytes.Repeat([]byte{0xE5}, 512))
	iosessAt := int64(img.Len())
	img.Write(iosess)

	d := &diskFile{
		r:      bytes.NewReader(img.Bytes()),
		size:   int64(img.Len()),
		header: Header{CatalogOffset: HeaderSize},
	}
	// IOJOB came from the directory and is already claimed when the scan is
	// asked for the rest; its position is the bound the fix leans on.
	job, err := dbf.Open(bytes.NewReader(img.Bytes()), iojobAt, int64(len(iojob)))
	if err != nil {
		t.Fatalf("open the claimed IOJOB: %v", err)
	}
	tables := map[tableID]*dbf.Table{tblJob: job}

	a := &Archive{}
	if err := a.scanTables(d, tables); err != nil {
		t.Fatalf("scan failed: %v", err)
	}

	got := tables[tblSession]
	if got == nil {
		t.Fatalf("scan never reached %s past the dead-count header", tblSession)
	}
	if got.Start() != iosessAt {
		t.Errorf("%s found at %d, want %d", tblSession, got.Start(), iosessAt)
	}
	var named bool
	for _, diag := range a.diags.drain() {
		if diag.Kind == DiagDirectoryScan && bytes.Contains([]byte(diag.Message), []byte("IOSESS")) {
			named = true
		}
	}
	if !named {
		t.Errorf("no scan diagnostic names %s", tblSession)
	}
}
