package mbr

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

// MBR is the 512-byte master boot record at the start of a partitioned disk.
type MBR struct {
	Bootstrap     [446]byte
	Partitions    [4]Partition
	BootSignature [2]byte // 0x55 0xAA
}

type Partition struct {
	// Status is 0x80 for the active partition and 0x00 for an inactive one;
	// other values are invalid.
	Status byte
	// FirstSectorCHS and LastSectorCHS hold head, then sector in bits 5-0 with
	// the high cylinder bits in 7-6, then the low cylinder bits.
	FirstSectorCHS  [3]byte
	PartitionType   byte
	LastSectorCHS   [3]byte
	FirstSectorLBA  uint32
	NumberOfSectors uint32
}

// ErrNotMBR reports that the sector read did not carry the boot signature, as
// opposed to an I/O failure reaching it. Callers probing an image for a
// partition table need to tell those apart.
var ErrNotMBR = errors.New("mbr: boot signature not found")

// SectorSize is the sector size LBA values in a partition entry are counted in.
const SectorSize = 512

// Partition type codes this package recognizes.
const (
	// TypeEmpty marks an unused partition table slot.
	TypeEmpty byte = 0x00
	// TypeExtendedCHS, TypeExtendedLBA and TypeExtendedLinux head a chain of
	// extended boot records rather than holding a filesystem.
	TypeExtendedCHS   byte = 0x05
	TypeExtendedLBA   byte = 0x0F
	TypeExtendedLinux byte = 0x85
	// TypeGPTProtective covers the whole disk on a GPT-partitioned drive. Read
	// as an ordinary entry it looks like one large partition.
	TypeGPTProtective byte = 0xEE
)

// partitionTypeNames maps the type codes a recovery tool is likely to meet to a
// human-readable name.
var partitionTypeNames = map[byte]string{
	0x01: "FAT12",
	0x04: "FAT16 <32MB",
	0x05: "Extended CHS",
	0x06: "FAT16 >=32MB",
	0x07: "NTFS/exFAT/HPFS",
	0x0B: "FAT32 CHS",
	0x0C: "FAT32 LBA",
	0x0E: "FAT16 LBA",
	0x0F: "Extended LBA",
	0x82: "Linux swap",
	0x83: "Linux",
	0x85: "Extended Linux",
	0x8E: "Linux LVM",
	0xA5: "FreeBSD",
	0xA8: "Apple UFS",
	0xAF: "Apple HFS/HFS+",
	0xEE: "GPT protective",
	0xEF: "EFI system",
}

// fatPartitionTypes is the subset holding a FAT filesystem.
var fatPartitionTypes = map[byte]struct{}{
	0x01: {}, 0x04: {}, 0x06: {}, 0x0B: {}, 0x0C: {}, 0x0E: {},
}

// TypeName returns a human-readable name for a partition type code.
func TypeName(t byte) string {
	if n, ok := partitionTypeNames[t]; ok {
		return n
	}
	return fmt.Sprintf("unknown (0x%02X)", t)
}

// IsFAT reports whether the type code is a FAT filesystem type.
func IsFAT(t byte) bool {
	_, ok := fatPartitionTypes[t]
	return ok
}

// IsExtended reports whether the type code heads an extended partition chain.
func IsExtended(t byte) bool {
	return t == TypeExtendedCHS || t == TypeExtendedLBA || t == TypeExtendedLinux
}

// Unused reports whether the slot holds no partition. Entries are identified by
// PartitionType, not by Status: the bootable flag is optional and a disk with
// no active partition has it clear on every entry.
func (p *Partition) Unused() bool {
	return p.PartitionType == TypeEmpty || p.NumberOfSectors == 0
}

// Offset returns the partition's byte offset from the start of the disk.
func (p *Partition) Offset() int64 {
	return int64(p.FirstSectorLBA) * SectorSize
}

// IsGPTProtective reports whether the table is a GPT protective MBR. Without
// this check a GPT disk reads as one partition spanning the whole drive.
func (m *MBR) IsGPTProtective() bool {
	for i := range m.Partitions {
		if m.Partitions[i].PartitionType == TypeGPTProtective {
			return true
		}
	}
	return false
}

// Read parses an MBR from r, returning ErrNotMBR if the boot signature is absent.
func Read(r io.Reader) (*MBR, error) {
	mbr := &MBR{}
	if err := binary.Read(r, binary.LittleEndian, mbr); err != nil {
		return nil, err
	}
	if mbr.BootSignature[0] != 0x55 || mbr.BootSignature[1] != 0xaa {
		return nil, ErrNotMBR
	}
	return mbr, nil
}

// Entry is one partition found on the disk, primary or logical.
type Entry struct {
	Partition
	// Index is the 1-based primary slot, or the 1-based position in the
	// extended chain for a logical partition.
	Index int
	// Logical is true for a partition found inside an extended partition.
	Logical bool
}

// maxEBRChain bounds the extended boot record walk. A corrupt or looping chain
// would otherwise never terminate. Reaching it is reported rather than treated
// as the end of the chain: the partitions past it were not found, and a walk
// that stops silently claims there are none.
const maxEBRChain = 128

// Partitions returns every usable partition on the disk: the primary entries
// plus the logical partitions inside any extended partition. A GPT protective
// MBR yields no entries, since its single entry describes no filesystem.
// Errors walking the extended chain stop the walk; the partitions found before
// the failure are still returned.
func Partitions(r io.ReaderAt) ([]Entry, error) {
	m, err := Read(io.NewSectionReader(r, 0, SectorSize))
	if err != nil {
		return nil, err
	}
	if m.IsGPTProtective() {
		return nil, nil
	}

	var out []Entry
	var extStart int64 = -1
	for i := range m.Partitions {
		p := m.Partitions[i]
		if p.Unused() {
			continue
		}
		if IsExtended(p.PartitionType) {
			if extStart < 0 {
				extStart = p.Offset()
			}
			continue
		}
		out = append(out, Entry{Partition: p, Index: i + 1})
	}
	if extStart < 0 {
		return out, nil
	}

	// Each EBR holds up to two entries: the logical partition, at an offset
	// relative to that EBR, and a link to the next EBR, relative to the start
	// of the extended partition.
	seen := make(map[int64]struct{})
	next := extStart
	for n := 1; ; n++ {
		if n > maxEBRChain {
			return out, fmt.Errorf("mbr: extended partition chain is longer than %d records; stopped at offset %d",
				maxEBRChain, next)
		}
		if _, ok := seen[next]; ok {
			return out, fmt.Errorf("mbr: extended partition chain loops at offset %d", next)
		}
		seen[next] = struct{}{}

		ebr, err := Read(io.NewSectionReader(r, next, SectorSize))
		if err != nil {
			return out, fmt.Errorf("mbr: read extended boot record at offset %d: %w", next, err)
		}
		// Entry 0 is the logical partition and entry 1 is the link to the next
		// record. An entry 0 that is itself an extended type describes no
		// filesystem, so it is skipped here as the primary loop above skips
		// extended entries; returning it would give a caller a partition that
		// cannot hold a filesystem.
		if p := ebr.Partitions[0]; !p.Unused() && !IsExtended(p.PartitionType) {
			// The logical partition's LBA is relative to its own EBR. It is
			// summed in int64 and checked before it goes back into the uint32
			// field: on a disk past 2 TiB the sum would wrap, and the partition
			// would point to a small offset near the front of the disk instead
			// of failing.
			lba := next/SectorSize + int64(p.FirstSectorLBA)
			if lba > math.MaxUint32 {
				return out, fmt.Errorf("mbr: logical partition %d starts at LBA %d, past what a partition entry can address",
					n, lba)
			}
			p.FirstSectorLBA = uint32(lba) //nolint:gosec // bounded against math.MaxUint32 just above
			out = append(out, Entry{Partition: p, Index: n, Logical: true})
		}
		link := ebr.Partitions[1]
		if link.Unused() || !IsExtended(link.PartitionType) {
			break
		}
		next = extStart + link.Offset()
	}
	return out, nil
}
