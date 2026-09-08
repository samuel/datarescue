package mbr

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"
)

// buildMBR renders a 512-byte boot record carrying the given partition entries.
func buildMBR(entries ...Partition) []byte {
	b := make([]byte, SectorSize)
	for i, p := range entries {
		if i >= 4 {
			break
		}
		off := 446 + i*16
		b[off] = p.Status
		copy(b[off+1:off+4], p.FirstSectorCHS[:])
		b[off+4] = p.PartitionType
		copy(b[off+5:off+8], p.LastSectorCHS[:])
		binary.LittleEndian.PutUint32(b[off+8:], p.FirstSectorLBA)
		binary.LittleEndian.PutUint32(b[off+12:], p.NumberOfSectors)
	}
	b[510], b[511] = 0x55, 0xAA
	return b
}

func TestRead(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		sector  []byte
		wantErr error
	}{
		{
			name:   "valid",
			sector: buildMBR(Partition{PartitionType: 0x0C, FirstSectorLBA: 2048, NumberOfSectors: 1000}),
		},
		{
			name: "bad signature",
			sector: func() []byte {
				b := buildMBR()
				b[511] = 0x00
				return b
			}(),
			wantErr: ErrNotMBR,
		},
		{
			name:    "all zeros",
			sector:  make([]byte, SectorSize),
			wantErr: ErrNotMBR,
		},
		{
			name:    "short read",
			sector:  make([]byte, 100),
			wantErr: io.ErrUnexpectedEOF,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m, err := Read(bytes.NewReader(tc.sector))
			switch {
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Read() = %v; want %v", err, tc.wantErr)
				}
			case err != nil:
				t.Fatalf("Read() = %v; want success", err)
			case m == nil:
				t.Fatal("Read() returned a nil MBR without an error")
			}
		})
	}
}

func TestPartitionsPrimary(t *testing.T) {
	t.Parallel()
	disk := make([]byte, 1<<20)
	copy(disk, buildMBR(
		Partition{Status: 0x80, PartitionType: 0x0C, FirstSectorLBA: 2048, NumberOfSectors: 1000},
		Partition{}, // empty slot between two used ones
		Partition{PartitionType: 0x83, FirstSectorLBA: 8192, NumberOfSectors: 1000},
		Partition{PartitionType: 0x06, FirstSectorLBA: 16384, NumberOfSectors: 0}, // zero length
	))
	parts, err := Partitions(bytes.NewReader(disk))
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 2 {
		t.Fatalf("got %d partitions, want 2: %+v", len(parts), parts)
	}
	if parts[0].Index != 1 || parts[0].Offset() != 2048*SectorSize {
		t.Errorf("first = index %d offset %d; want index 1 offset %d", parts[0].Index, parts[0].Offset(), 2048*SectorSize)
	}
	if parts[1].Index != 3 {
		t.Errorf("second = index %d; want 3 (the empty slot must not shift the numbering)", parts[1].Index)
	}
	if !IsFAT(parts[0].PartitionType) {
		t.Error("type 0x0C should be recognized as FAT")
	}
	if IsFAT(parts[1].PartitionType) {
		t.Error("type 0x83 should not be recognized as FAT")
	}
}

func TestPartitionsGPTProtective(t *testing.T) {
	t.Parallel()
	disk := make([]byte, 1<<20)
	copy(disk, buildMBR(Partition{
		PartitionType:   TypeGPTProtective,
		FirstSectorLBA:  1,
		NumberOfSectors: 0xFFFFFFFF,
	}))
	m, err := Read(bytes.NewReader(disk))
	if err != nil {
		t.Fatal(err)
	}
	if !m.IsGPTProtective() {
		t.Fatal("IsGPTProtective() = false on a protective MBR")
	}
	parts, err := Partitions(bytes.NewReader(disk))
	if err != nil {
		t.Fatal(err)
	}
	// Read as an ordinary entry this looks like one partition spanning the disk,
	// which would send a caller off to parse the whole drive as a filesystem.
	if len(parts) != 0 {
		t.Errorf("got %d partitions on a GPT disk; want 0", len(parts))
	}
}

func TestPartitionsExtendedChain(t *testing.T) {
	t.Parallel()
	disk := make([]byte, 4<<20)
	extStart := int64(2048) // sectors
	copy(disk, buildMBR(
		Partition{PartitionType: 0x06, FirstSectorLBA: 64, NumberOfSectors: 100},
		Partition{PartitionType: TypeExtendedLBA, FirstSectorLBA: uint32(extStart), NumberOfSectors: 4000},
	))
	// Each EBR holds the logical partition at an offset relative to itself and a
	// link to the next EBR relative to the start of the extended partition.
	copy(disk[extStart*SectorSize:], buildMBR(
		Partition{PartitionType: 0x83, FirstSectorLBA: 2, NumberOfSectors: 50},
		Partition{PartitionType: TypeExtendedLBA, FirstSectorLBA: 100, NumberOfSectors: 200},
	))
	copy(disk[(extStart+100)*SectorSize:], buildMBR(
		Partition{PartitionType: 0x07, FirstSectorLBA: 2, NumberOfSectors: 50},
	))

	parts, err := Partitions(bytes.NewReader(disk))
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 3 {
		t.Fatalf("got %d partitions, want 3 (1 primary + 2 logical): %+v", len(parts), parts)
	}
	if parts[0].Logical {
		t.Error("the primary partition was reported as logical")
	}
	want := []uint32{64, uint32(extStart) + 2, uint32(extStart) + 102}
	for i, w := range want {
		if parts[i].FirstSectorLBA != w {
			t.Errorf("partition %d LBA = %d; want %d", i, parts[i].FirstSectorLBA, w)
		}
	}
	for _, p := range parts[1:] {
		if !p.Logical {
			t.Errorf("partition at LBA %d inside the extended partition was not marked logical", p.FirstSectorLBA)
		}
	}
}

func TestPartitionsExtendedChainLoop(t *testing.T) {
	t.Parallel()
	disk := make([]byte, 4<<20)
	extStart := int64(2048)
	copy(disk, buildMBR(Partition{
		PartitionType:   TypeExtendedLBA,
		FirstSectorLBA:  uint32(extStart),
		NumberOfSectors: 4000,
	}))
	// The link points back at the start of the extended partition, so following
	// it without a guard never terminates.
	copy(disk[extStart*SectorSize:], buildMBR(
		Partition{PartitionType: 0x83, FirstSectorLBA: 2, NumberOfSectors: 50},
		Partition{PartitionType: TypeExtendedLBA, FirstSectorLBA: 0, NumberOfSectors: 200},
	))
	parts, err := Partitions(bytes.NewReader(disk))
	if err == nil {
		t.Error("a looping extended chain returned no error")
	}
	if len(parts) != 1 {
		t.Errorf("got %d partitions; want the 1 found before the loop was detected", len(parts))
	}
}

func TestPartitionsNotAnMBR(t *testing.T) {
	t.Parallel()
	_, err := Partitions(bytes.NewReader(make([]byte, 1<<20)))
	if !errors.Is(err, ErrNotMBR) {
		t.Errorf("Partitions on an unpartitioned image = %v; want ErrNotMBR", err)
	}
}

func TestUnused(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		p    Partition
		want bool
	}{
		// The bootable flag is optional; a disk with no active partition has it
		// clear on every entry, so it cannot be what identifies a used slot.
		{"empty type, bootable flag set", Partition{Status: 0x80}, true},
		{"used type, not bootable", Partition{PartitionType: 0x0C, NumberOfSectors: 1}, false},
		{"used type, zero length", Partition{PartitionType: 0x0C}, true},
		{"all zero", Partition{}, true},
	}
	for _, tc := range tests {
		if got := tc.p.Unused(); got != tc.want {
			t.Errorf("%s: Unused() = %v; want %v", tc.name, got, tc.want)
		}
	}
}

func TestTypeHelpers(t *testing.T) {
	t.Parallel()
	if got := TypeName(0x0C); got != "FAT32 LBA" {
		t.Errorf("TypeName(0x0C) = %q", got)
	}
	if got := TypeName(0xAB); got != "unknown (0xAB)" {
		t.Errorf("TypeName(0xAB) = %q", got)
	}
	for _, tp := range []byte{TypeExtendedCHS, TypeExtendedLBA, TypeExtendedLinux} {
		if !IsExtended(tp) {
			t.Errorf("IsExtended(0x%02X) = false", tp)
		}
	}
	if IsExtended(0x0C) {
		t.Error("IsExtended(0x0C) = true")
	}
}

// Reaching the chain cap is reported. Stopping silently would claim the
// partitions past it do not exist, when the walk only stopped looking.
func TestPartitionsExtendedChainTooLong(t *testing.T) {
	t.Parallel()
	const records = maxEBRChain + 40
	extStart := int64(2048) // sectors
	disk := make([]byte, (extStart+records+2)*SectorSize)
	copy(disk, buildMBR(Partition{
		PartitionType:   TypeExtendedLBA,
		FirstSectorLBA:  uint32(extStart),
		NumberOfSectors: 4 * records,
	}))
	// One EBR per sector, each linking to the next. The chain does not loop, so
	// only the cap can end it.
	for k := range records {
		copy(disk[(extStart+int64(k))*SectorSize:], buildMBR(
			Partition{PartitionType: 0x83, FirstSectorLBA: 1, NumberOfSectors: 1},
			Partition{PartitionType: TypeExtendedLBA, FirstSectorLBA: uint32(k) + 1, NumberOfSectors: 2},
		))
	}

	parts, err := Partitions(bytes.NewReader(disk))
	if err == nil {
		t.Fatal("an extended chain longer than the cap returned no error")
	}
	if !strings.Contains(err.Error(), "longer than") {
		t.Errorf("error = %v; want it to name the chain as longer than the cap", err)
	}
	if len(parts) != maxEBRChain {
		t.Errorf("got %d partitions; want the %d found before the cap", len(parts), maxEBRChain)
	}
}

// sparseDisk serves whole sectors from a map, so a test can put a boot record
// past 2 TiB without allocating 2 TiB.
type sparseDisk map[int64][]byte

func (d sparseDisk) ReadAt(p []byte, off int64) (int, error) {
	sector, ok := d[off]
	if !ok {
		return 0, io.EOF
	}
	return copy(p, sector), nil
}

// A logical partition's LBA is relative to its own EBR, and the field it goes
// back into is a uint32. Past 2 TiB the sum wraps, and the partition then
// points to a small offset near the front of the disk. That looks plausible but
// holds some other partition's data.
func TestPartitionsExtendedChainPastTheAddressableEnd(t *testing.T) {
	t.Parallel()
	const extLBA = 0xFFFFFF00
	disk := sparseDisk{
		0: buildMBR(Partition{
			PartitionType:   TypeExtendedLBA,
			FirstSectorLBA:  extLBA,
			NumberOfSectors: 0x1000,
		}),
		int64(extLBA) * SectorSize: buildMBR(
			Partition{PartitionType: 0x83, FirstSectorLBA: 0x200, NumberOfSectors: 0x100},
		),
	}
	parts, err := Partitions(disk)
	if err == nil {
		t.Fatalf("a logical partition past 2 TiB returned no error: %+v", parts)
	}
	if !strings.Contains(err.Error(), "past what a partition entry can address") {
		t.Errorf("error = %v; want it to name the LBA as unaddressable", err)
	}
	if len(parts) != 0 {
		t.Errorf("got %d partitions; want none, the only one being unaddressable: %+v", len(parts), parts)
	}
}
