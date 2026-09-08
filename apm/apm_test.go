package apm

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"samuelks.com/datarescue/ddrescue"
	"samuelks.com/datarescue/internal/testapm"
)

// The on-disk records are decoded positionally, so an oversized field silently
// shifts every field after it and an undersized one does the same. Pinning the
// sizes is the cheapest guard against that class of bug.
func TestRecordSizes(t *testing.T) {
	if n := binary.Size(DDR{}); n != EntrySize {
		t.Errorf("binary.Size(DDR) = %d; want %d", n, EntrySize)
	}
	if n := binary.Size(Partition{}); n != EntrySize {
		t.Errorf("binary.Size(Partition) = %d; want %d", n, EntrySize)
	}
}

func build(t *testing.T, opts testapm.Options, parts ...testapm.Partition) ([]byte, []testapm.Placed) {
	t.Helper()
	return testapm.Build(opts, parts...)
}

// damaged marks byte ranges of img unrecovered and returns a reader that
// enforces the map, so a read of a lost block fails the way it would on the
// medium rather than handing back the bytes that are still in the file.
func damaged(t *testing.T, img []byte, ranges ...[2]int) *ddrescue.Reader {
	t.Helper()
	mapText := testapm.DamageRange(img, ranges...)
	path := filepath.Join(t.TempDir(), "disk.img.map")
	if err := os.WriteFile(path, []byte(mapText), 0o600); err != nil {
		t.Fatal(err)
	}
	blocks, err := ddrescue.ParseMap(path)
	if err != nil {
		t.Fatal(err)
	}
	return ddrescue.NewReader(bytes.NewReader(img), blocks)
}

func hfsParts() []testapm.Partition {
	return []testapm.Partition{
		{Type: TypeHFS, Name: "ISIS", Blocks: 40},
		{Type: TypeFree, Blocks: 4},
	}
}

func TestReadsTheMap(t *testing.T) {
	img, placed := build(t, testapm.Options{}, hfsParts()...)
	m, err := ReadMap(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	if !m.HasDDR {
		t.Error("the driver descriptor record was not read")
	}
	if m.BlockSize != 512 {
		t.Errorf("BlockSize = %d; want 512", m.BlockSize)
	}
	if len(m.Entries) != 3 {
		t.Fatalf("read %d entries; want 3", len(m.Entries))
	}
	if got := m.Entries[1].TypeName(); got != TypeHFS {
		t.Errorf("entry 2 type = %q; want %q", got, TypeHFS)
	}
	if got := m.Entries[1].PartName(); got != "ISIS" {
		t.Errorf("entry 2 name = %q; want ISIS", got)
	}
	// The index is the entry's position in the map, which is also its block,
	// so what a listing prints is what -partition takes.
	for i, e := range m.Entries {
		if e.Index != i+1 {
			t.Errorf("entry %d has index %d", i, e.Index)
		}
		if e.Offset() != placed[i].Offset || e.Size() != placed[i].Size {
			t.Errorf("entry %d at %d+%d; want %d+%d",
				e.Index, e.Offset(), e.Size(), placed[i].Offset, placed[i].Size)
		}
	}
}

// The map ends at the first block that is not an entry. The reserved tail of
// the map partition is whatever the medium's erase pattern is, and neither
// zeros nor 0xFA is a partition.
func TestTheSignatureEndsTheMapNotTheCount(t *testing.T) {
	for _, fill := range []byte{0x00, 0xFA} {
		t.Run(string(rune('A'+fill%26)), func(t *testing.T) {
			img, _ := build(t, testapm.Options{MapBlocks: 10, Fill: fill}, hfsParts()...)
			m, err := ReadMap(bytes.NewReader(img), int64(len(img)))
			if err != nil {
				t.Fatal(err)
			}
			if len(m.Entries) != 3 {
				t.Errorf("fill 0x%02X: read %d entries; want 3 — the reserved tail is not a partition",
					fill, len(m.Entries))
			}
		})
	}
}

// pmMapBlkCnt is a cross-check, never a bound. Trusted when it is too high, the
// walk invents partitions out of fill bytes; trusted when it is too low, a
// partition added without updating it is lost.
func TestTheDeclaredCountIsNeverABound(t *testing.T) {
	t.Run("too high", func(t *testing.T) {
		img, _ := build(t, testapm.Options{MapBlocks: 10, DeclaredCount: 10}, hfsParts()...)
		m, err := ReadMap(bytes.NewReader(img), int64(len(img)))
		if err != nil {
			t.Fatal(err)
		}
		if len(m.Entries) != 3 {
			t.Errorf("read %d entries; want 3", len(m.Entries))
		}
		if len(m.Issues) == 0 {
			t.Error("a count that disagrees with the entries was not reported")
		}
	})
	t.Run("too low", func(t *testing.T) {
		img, _ := build(t, testapm.Options{MapBlocks: 10, DeclaredCount: 1}, hfsParts()...)
		m, err := ReadMap(bytes.NewReader(img), int64(len(img)))
		if err != nil {
			t.Fatal(err)
		}
		if len(m.Entries) != 3 {
			t.Errorf("read %d entries; want 3 — a low count must not truncate the walk", len(m.Entries))
		}
	})
}

// An APM entry is addressed by its position and carries its own start and
// count, so nothing in entry N is needed to read entry N+1. A lost block loses
// only the partition it described, unlike an MBR extended chain, where a lost
// record hides everything after it.
func TestALostMapBlockCostsOnlyItself(t *testing.T) {
	img, _ := build(t, testapm.Options{MapBlocks: 10, DeclaredCount: 4},
		testapm.Partition{Type: TypeHFS, Name: "ONE", Blocks: 8},
		testapm.Partition{Type: TypeHFS, Name: "TWO", Blocks: 8},
		testapm.Partition{Type: TypeHFS, Name: "THREE", Blocks: 8},
	)
	// Lose the block holding TWO, which sits between the other two: an entry
	// before it and an entry after it must both survive.
	r := damaged(t, img, [2]int{3 * 512, 512})

	m, err := ReadMap(r, int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range m.Entries {
		names = append(names, e.PartName())
	}
	if slices.Contains(names, "TWO") {
		t.Errorf("the lost entry came back: %v", names)
	}
	for _, want := range []string{"ONE", "THREE"} {
		if !slices.Contains(names, want) {
			t.Errorf("entry %q was lost with block 2; got %v", want, names)
		}
	}
	var named bool
	for _, is := range m.Issues {
		if is.Block == 3 {
			named = true
		}
	}
	if !named {
		t.Errorf("no issue names the unreadable block: %v", m.Issues)
	}
}

// A map is readable without its driver descriptor record: every entry carries
// its own start and count, and only the stride is unknown, which probing
// recovers. A disk whose first sector is the one the drive could not read is
// exactly this case.
func TestAMapIsReadableWithoutTheDriverDescriptorRecord(t *testing.T) {
	img, _ := build(t, testapm.Options{NoDDR: true}, hfsParts()...)
	m, err := ReadMap(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	if m.HasDDR {
		t.Error("HasDDR is true for an image with no record")
	}
	if len(m.Entries) != 3 {
		t.Errorf("read %d entries; want 3", len(m.Entries))
	}
	if len(m.Issues) == 0 {
		t.Error("the missing record was not reported")
	}
}

// sbBlkSize is only a hint; the PM signature confirms the stride. They disagree
// in the field, and a parser that strides the map by the declared size alone
// misses three quarters of a 512-strided map.
func TestTheSignatureSettlesTheStrideNotTheDeclaredBlockSize(t *testing.T) {
	img, _ := build(t, testapm.Options{}, hfsParts()...)
	// Claim 2048 where the map is written at 512.
	binary.BigEndian.PutUint16(img[2:4], 2048)
	m, err := ReadMap(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	if m.BlockSize != 512 {
		t.Errorf("BlockSize = %d; want 512 — the signature is the proof", m.BlockSize)
	}
	if len(m.Entries) != 3 {
		t.Errorf("read %d entries; want 3", len(m.Entries))
	}
	var reported bool
	for _, is := range m.Issues {
		if strings.Contains(is.Err.Error(), "2048") {
			reported = true
		}
	}
	if !reported {
		t.Errorf("the disagreement was not reported: %v", m.Issues)
	}
}

func TestAMapAtA2048ByteStride(t *testing.T) {
	img, placed := build(t, testapm.Options{BlockSize: 2048}, hfsParts()...)
	m, err := ReadMap(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	if m.BlockSize != 2048 {
		t.Fatalf("BlockSize = %d; want 2048", m.BlockSize)
	}
	if m.Entries[1].Offset() != placed[1].Offset {
		t.Errorf("entry 2 at %d; want %d", m.Entries[1].Offset(), placed[1].Offset)
	}
}

// A working Apple_HFS partition in this archive carries pmPartStatus 0 and
// pmDataCnt 0. By the flag definitions that means "not valid, not allocated,
// not readable", on a volume a Mac mounted for twenty years. Nothing may gate
// on either field, though it is a tempting mistake.
func TestAZeroStatusAndDataCountAreStillAPartition(t *testing.T) {
	img, _ := build(t, testapm.Options{},
		testapm.Partition{Type: TypeHFS, Name: "ISIS", Blocks: 40, Status: 0, DataCount: 0})
	m, err := ReadMap(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Entries) != 2 {
		t.Fatalf("read %d entries; want 2", len(m.Entries))
	}
	e := m.Entries[1]
	if e.Status != 0 || e.DataCount != 0 {
		t.Fatalf("the fixture did not write zeros: status=%d dataCnt=%d", e.Status, e.DataCount)
	}
	if e.Unused() {
		t.Error("a partition with a zero status and data count was called unused")
	}
	parts, err := Partitions(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 2 {
		t.Errorf("Partitions returned %d; want 2", len(parts))
	}
}

func TestNotAnAppleDisk(t *testing.T) {
	img := make([]byte, 1<<20)
	if _, err := ReadMap(bytes.NewReader(img), int64(len(img))); !errors.Is(err, ErrNotAPM) {
		t.Errorf("ReadMap over zeros = %v; want ErrNotAPM", err)
	}
}

// "This disk has no map" and "the block holding the map was never recovered"
// are different answers, and only a caller that can tell them apart can act on
// the second.
func TestAnUnreadableBlockZeroIsNotErrNotAPM(t *testing.T) {
	img, _ := build(t, testapm.Options{}, hfsParts()...)
	r := damaged(t, img, [2]int{0, 512 * 2})
	_, err := ReadMap(r, int64(len(img)))
	if err == nil {
		t.Fatal("reading an image whose first blocks are gone returned no error")
	}
	if errors.Is(err, ErrNotAPM) {
		t.Errorf("ReadMap = %v; want an I/O failure, not ErrNotAPM", err)
	}
}

// Block 0 is the driver descriptor record, so a partition claiming it is
// corrupt and must not be handed back as somewhere to look.
func TestAPartitionStartingAtBlockZeroIsRefused(t *testing.T) {
	img, _ := build(t, testapm.Options{}, hfsParts()...)
	binary.BigEndian.PutUint32(img[2*512+8:], 0) // entry 2's pmPyPartStart
	m, err := ReadMap(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range m.Entries {
		if e.PyPartStart == 0 {
			t.Error("an entry starting at block 0 was kept")
		}
	}
	if len(m.Issues) == 0 {
		t.Error("the refusal was not reported")
	}
}

// A partition running past the end of a truncated image is what a partial
// rescue looks like, and its head is still readable, so the entry is kept and
// the shortfall reported.
func TestAPartitionPastTheEndIsKeptAndReported(t *testing.T) {
	img, _ := build(t, testapm.Options{}, hfsParts()...)
	binary.BigEndian.PutUint32(img[2*512+12:], 1<<20) // entry 2's pmPartBlkCnt
	m, err := ReadMap(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Entries) != 3 {
		t.Errorf("read %d entries; want 3 — the head of the partition is still readable", len(m.Entries))
	}
	var reported bool
	for _, is := range m.Issues {
		if strings.Contains(is.Err.Error(), "past the end") {
			reported = true
		}
	}
	if !reported {
		t.Errorf("the shortfall was not reported: %v", m.Issues)
	}
}

// One entry starting past the end of the image describes nothing readable.
func TestAPartitionStartingPastTheEndIsRefused(t *testing.T) {
	img, _ := build(t, testapm.Options{}, hfsParts()...)
	binary.BigEndian.PutUint32(img[2*512+8:], 1<<20) // entry 2's pmPyPartStart
	m, err := ReadMap(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range m.Entries {
		if e.Offset() >= int64(len(img)) {
			t.Errorf("an entry starting at %d was kept in a %d-byte image", e.Offset(), len(img))
		}
	}
}

// Reading past the declared driver count invents drivers out of whatever
// follows; one disk here carries a well-formed-looking descriptor past its
// declared count of one.
func TestDriverListIsClampedToTheDeclaredCount(t *testing.T) {
	img, _ := build(t, testapm.Options{DriverCount: 1}, hfsParts()...)
	// Write a plausible-looking descriptor into the second slot.
	binary.BigEndian.PutUint32(img[18+8:], 95)
	binary.BigEndian.PutUint16(img[18+12:], 0x0088)
	m, err := ReadMap(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	if n := len(m.DDR.DriverList()); n != 1 {
		t.Errorf("DriverList returned %d; want 1", n)
	}
}

func TestTypePredicates(t *testing.T) {
	for _, tc := range []struct {
		t                              string
		hfs, free, isMap, drv, canHold bool
	}{
		{TypeHFS, true, false, false, false, true},
		{"apple_hfs", true, false, false, false, true},
		{TypeHFSX, true, false, false, false, true},
		{TypeFree, false, true, false, false, false},
		{TypeMap, false, false, true, false, false},
		{TypeDriver43, false, false, false, true, false},
		{TypeDriverATA, false, false, false, true, false},
		{TypeUNIX, false, false, false, false, true},
		{"Iomega_DiskInfo", false, false, false, false, true},
		{"", false, false, false, false, false},
	} {
		t.Run(tc.t, func(t *testing.T) {
			if got := IsHFS(tc.t); got != tc.hfs {
				t.Errorf("IsHFS = %v; want %v", got, tc.hfs)
			}
			if got := IsFree(tc.t); got != tc.free {
				t.Errorf("IsFree = %v; want %v", got, tc.free)
			}
			if got := IsMap(tc.t); got != tc.isMap {
				t.Errorf("IsMap = %v; want %v", got, tc.isMap)
			}
			if got := IsDriver(tc.t); got != tc.drv {
				t.Errorf("IsDriver = %v; want %v", got, tc.drv)
			}
			if got := IsFilesystem(tc.t); got != tc.canHold {
				t.Errorf("IsFilesystem = %v; want %v", got, tc.canHold)
			}
		})
	}
}
