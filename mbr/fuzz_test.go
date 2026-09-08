package mbr

import (
	"bytes"
	"testing"
)

// The partition table is the first structure every image tool reads, and every
// offset it returns comes straight off the disk. As with the other parsers, any
// input must parse without a panic.

func FuzzPartitions(f *testing.F) {
	f.Add(buildMBR(
		Partition{PartitionType: 0x06, FirstSectorLBA: 64, NumberOfSectors: 100},
		Partition{PartitionType: TypeExtendedLBA, FirstSectorLBA: 2048, NumberOfSectors: 4000},
	))
	f.Add(buildMBR(Partition{PartitionType: TypeGPTProtective, FirstSectorLBA: 1, NumberOfSectors: 0xFFFFFFFF}))
	// An extended entry pointing at itself: the loop guard's case.
	f.Add(buildMBR(Partition{PartitionType: TypeExtendedCHS, FirstSectorLBA: 0, NumberOfSectors: 1}))
	// A logical partition whose LBA is relative to an EBR near the addressable
	// end, where the sum overflows the uint32 field.
	f.Add(append(buildMBR(Partition{PartitionType: TypeExtendedLBA, FirstSectorLBA: 0xFFFFFF00, NumberOfSectors: 8}),
		buildMBR(Partition{PartitionType: 0x83, FirstSectorLBA: 0xFFFF, NumberOfSectors: 1})...))
	f.Add(make([]byte, SectorSize))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		parts, err := Partitions(bytes.NewReader(data))
		// A failure still returns what was found before it, so the entries are
		// checked either way.
		_ = err
		for _, p := range parts {
			if off := p.Offset(); off < 0 {
				t.Fatalf("partition %+v has a negative offset %d", p, off)
			}
			if p.Unused() {
				t.Fatalf("an unused entry was returned as a partition: %+v", p)
			}
			if IsExtended(p.PartitionType) {
				t.Fatalf("an extended partition was returned as a filesystem: %+v", p)
			}
			if p.Index < 1 {
				t.Fatalf("partition with index %d: %+v", p.Index, p)
			}
		}
		if len(parts) > 4+maxEBRChain {
			t.Fatalf("returned %d partitions; the table and the chain cap allow at most %d",
				len(parts), 4+maxEBRChain)
		}
	})
}
