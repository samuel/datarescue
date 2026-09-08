package fat

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"samuelks.com/datarescue/ddrescue"
)

// MediaType describes the physical medium the volume was formatted for.
type MediaType byte

const (
	MediaType8InchSS       MediaType = 0xE5 // 8-inch single-sided
	MediaType525DS         MediaType = 0xED // 5.25-inch double-sided
	MediaTypeNonStandardA  MediaType = 0xEE // non-standard custom partitions
	MediaTypeNonStandardB  MediaType = 0xEF // non-standard superfloppy
	MediaType35DS          MediaType = 0xF0 // 3.5-inch double-sided
	MediaTypeDoubleDensity MediaType = 0xF4 // double-density
	MediaTypeFixedDiskA    MediaType = 0xF5 // fixed disk
	MediaTypeFixedDisk     MediaType = 0xF8 // fixed disk / hard disk
	MediaType35Or525       MediaType = 0xF9 // 3.5-inch or 5.25-inch
	MediaType35And525SS    MediaType = 0xFA // 3.5-inch & 5.25-inch single-sided
	MediaType35And525DS    MediaType = 0xFB // 3.5-inch & 5.25-inch double-sided
	MediaType525SS         MediaType = 0xFC // 5.25-inch single-sided
	MediaType525Or8DS      MediaType = 0xFD // 5.25-inch or 8-inch double-sided
	MediaType525Or8SS      MediaType = 0xFE // 5.25-inch or 8-inch single-sided
	MediaType525OrHardDisk MediaType = 0xFF // 5.25-inch or hard disk
)

var mediaTypeNames = map[MediaType]string{
	MediaType8InchSS:       "8-inch single-sided floppy",
	MediaType525DS:         "5.25-inch double-sided floppy",
	MediaTypeNonStandardA:  "non-standard custom partitions",
	MediaTypeNonStandardB:  "non-standard superfloppy",
	MediaType35DS:          "3.5-inch double-sided floppy",
	MediaTypeDoubleDensity: "double-density floppy",
	MediaTypeFixedDiskA:    "fixed disk",
	MediaTypeFixedDisk:     "fixed disk / hard disk",
	MediaType35Or525:       "3.5-inch or 5.25-inch floppy",
	MediaType35And525SS:    "3.5-inch & 5.25-inch single-sided floppy",
	MediaType35And525DS:    "3.5-inch & 5.25-inch double-sided floppy",
	MediaType525SS:         "5.25-inch single-sided floppy",
	MediaType525Or8DS:      "5.25-inch or 8-inch double-sided floppy",
	MediaType525Or8SS:      "5.25-inch or 8-inch single-sided floppy",
	MediaType525OrHardDisk: "5.25-inch or hard disk",
}

func (m MediaType) String() string {
	if name, ok := mediaTypeNames[m]; ok {
		return fmt.Sprintf("0x%02X (%s)", byte(m), name)
	}
	return fmt.Sprintf("0x%02X (unknown)", byte(m))
}

type Type string

const (
	TypeFAT12 Type = "FAT12"
	TypeFAT16 Type = "FAT16"
	TypeFAT32 Type = "FAT32"
)

// FSInfo holds the FAT32 FS Information Sector. FreeCount and NextFree are
// 0xFFFFFFFF when unknown, and are advisory even when set.
type FSInfo struct {
	FreeCount uint32
	NextFree  uint32
}

type FAT struct {
	r io.ReadSeeker

	BootSectorOffset    int64
	FATTableOffsets     []int64
	RootDirectoryOffset int64
	DataRegionOffset    int64
	SectorSize          int
	ClusterSize         int
	FATTableSize        int
	TotalSectors        uint32
	RootCluster         uint32  // FAT32 only
	FSInfo              *FSInfo // FAT32 only; nil if not present or signatures invalid
	BootSector          *BootSector
	Type                Type
	// Tables holds the FAT copies that were read successfully; TableCopies gives
	// the on-disk copy index of each, since an unreadable copy is skipped.
	Tables      []Table
	TableCopies []int
	// ChainPolicy selects which copy to trust when the copies disagree.
	ChainPolicy ChainPolicy

	// imageEnd is the byte length of the underlying image, or 0 when it cannot
	// be measured. volumeEnd clamps the declared volume to it, so a boot sector
	// overstating the volume cannot make a chain look longer than the bytes
	// the image actually holds.
	imageEnd int64

	// mu serializes access to r, which is shared mutable state, and to the
	// fields below it.
	mu sync.Mutex
	// merged caches the best-effort reconciliation of the copies.
	merged *Table
	// diags collects non-fatal problems noticed while reading the volume.
	diags []Diagnostic
}

// DiagnosticKind classifies a non-fatal problem found while reading a volume.
type DiagnosticKind string

const (
	// DiagFATCopyUnreadable means a whole FAT copy could not be read.
	DiagFATCopyUnreadable DiagnosticKind = "fat_copy_unreadable"
	// DiagFATCopyDamaged means some of a copy's entries came from bad sectors.
	DiagFATCopyDamaged DiagnosticKind = "fat_copy_damaged"
	// DiagFATCopyInvalid means a copy failed structural validation.
	DiagFATCopyInvalid DiagnosticKind = "fat_copy_invalid"
	// DiagMediaByteMismatch means FAT[0] disagrees with the BPB media type.
	DiagMediaByteMismatch DiagnosticKind = "media_byte_mismatch"
	// DiagFSInfoUnreadable means the FAT32 FSInfo sector could not be read.
	DiagFSInfoUnreadable DiagnosticKind = "fsinfo_unreadable"
	// DiagChainCopyFallback means one copy's chain was used in place of the other.
	DiagChainCopyFallback DiagnosticKind = "chain_copy_fallback"
	// DiagVFATName means a long filename entry was malformed.
	DiagVFATName DiagnosticKind = "vfat_name"
	// DiagVolumeSizeMismatch means the volume declares more sectors than the
	// image holds.
	DiagVolumeSizeMismatch DiagnosticKind = "volume_size_mismatch"
	// DiagRootClusterZero means a FAT32 boot sector declares no root cluster.
	DiagRootClusterZero DiagnosticKind = "root_cluster_zero"
)

// Diagnostic is a non-fatal problem noticed while reading the volume. They are
// returned as data rather than logged, so a caller can render or serialize them
// instead of scraping a log stream.
type Diagnostic struct {
	Kind    DiagnosticKind
	Message string
}

func (d Diagnostic) String() string { return d.Message }

func (f *FAT) diag(kind DiagnosticKind, format string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.diags = append(f.diags, Diagnostic{Kind: kind, Message: fmt.Sprintf(format, args...)})
}

// Diagnostics returns the problems noticed so far and clears the list, so a
// caller can attribute them to the operation that produced them.
func (f *FAT) Diagnostics() []Diagnostic {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := f.diags
	f.diags = nil
	return d
}

// readAt reads exactly len(buf) bytes at offset, returning the number of bytes
// actually read so a caller can keep a short read's data. mu serializes access
// to the shared reader.
func (f *FAT) readAt(offset int64, buf []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, err := f.r.Seek(offset, io.SeekStart); err != nil {
		return 0, err
	}
	return io.ReadFull(f.r, buf)
}

// ChainPolicy selects which FAT copy a cluster chain is resolved from when the
// copies disagree.
type ChainPolicy int

const (
	// ChainPolicyStrict cross-checks the copies and fails when they disagree.
	ChainPolicyStrict ChainPolicy = iota
	// ChainPolicyCopy0 and ChainPolicyCopy1 take the named copy verbatim.
	ChainPolicyCopy0
	ChainPolicyCopy1
	// ChainPolicyBestEffort merges the copies: where one side is free or was
	// lost to a bad sector the other side wins, and copy 0 wins the rest.
	ChainPolicyBestEffort
)

func (p ChainPolicy) String() string {
	switch p {
	case ChainPolicyStrict:
		return "strict"
	case ChainPolicyCopy0:
		return "copy0"
	case ChainPolicyCopy1:
		return "copy1"
	case ChainPolicyBestEffort:
		return "best-effort"
	}
	return fmt.Sprintf("ChainPolicy(%d)", int(p))
}

// ParseChainPolicy converts a policy name to a ChainPolicy.
func ParseChainPolicy(s string) (ChainPolicy, error) {
	switch s {
	case "strict":
		return ChainPolicyStrict, nil
	case "copy0":
		return ChainPolicyCopy0, nil
	case "copy1":
		return ChainPolicyCopy1, nil
	case "best-effort":
		return ChainPolicyBestEffort, nil
	}
	return 0, fmt.Errorf("fat: unknown FAT policy %q (want strict, copy0, copy1 or best-effort)", s)
}

type BootSector struct {
	Jump               [3]byte
	OEMName            [8]byte
	BIOSParameterBlock BIOSParameterBlock
	BootstrapCode      [448]byte
	EndOfSectorMarker  [2]byte // 0x55 0xaa
}

type BIOSParameterBlock struct {
	// BytesPerSector is the size of a hardware sector. For most disks in use
	// in the United States, the value of this field is 512.
	BytesPerSector uint16 // offset 0x0B
	// SectorsPerCluster is the number of sectors in a cluster. The default
	// cluster size for a volume depends on the volume size and the file system.
	SectorsPerCluster byte // offset 0x0D
	// ReservedSectors is the number of sectors from the Partition Boot Sector
	// to the start of the first file allocation table, including the Partition
	// Boot Sector. The minimum value is 1. If the value is greater than 1, it
	// means that the bootstrap code is too long to fit completely in the
	// Partition Boot Sector.
	ReservedSectors uint16 // Offset 0x0E
	// NumberOfFATs is the number of copies of the file allocation table on the
	// volume. Typically, the value of this field is 2.
	NumberOfFATs byte // Offset 0x10
	// RootEntries is the total number of file name entries that can be stored in
	// the root folder of the volume. One entry is always used as a Volume Label.
	// Files with long filenames use up multiple entries per file. Therefore, the
	// largest number of files in the root folder is typically 511, but you will
	// run out of entries sooner if you use long filenames. Zero on
	// FAT32, where the root is an ordinary cluster chain.
	RootEntries uint16 // offset 0x11
	// SmallSectors is the number of sectors on the volume if the number fits in
	// 16 bits (65535). For volumes larger than 65536 sectors, this field has a value
	// of 0 and the Large Sectors field is used instead.
	SmallSectors uint16 // offset 0x13
	// MediaType provides information about the media being used. A value of
	// 0xF8 indicates a hard disk.
	MediaType MediaType // offset 0x15
	// SectorsPerFAT is the sectors per file allocation table (FAT). Number of sectors
	// occupied by each of the file allocation tables on the volume. By using this
	// information, together with the Number of FATs and Reserved Sectors, you can
	// compute where the root folder begins. By using the number of entries in the
	// root folder, you can also compute where the user data area of the volume begins.
	// Zero on FAT32
	SectorsPerFAT uint16 // offset 0x16
	// SectorsPerTrack is the number of sectors per track. The apparent disk geometry
	// in use when the disk was low-level formatted.
	SectorsPerTrack uint16
	// NumberOfHeads is the number of read heads. The apparent disk geometry in use
	// when the disk was low-level formatted.
	NumberOfHeads uint16 // offset 0x1a
	// HiddenSectors is the same as the Relative Sector field in the Partition Table.
	HiddenSectors uint32 // offset 0x1c
	// LargeSectors is the volume size in sectors when SmallSectors is 0.
	LargeSectors uint32 // offset 0x20

	// Extended BIOS Parameter Block
	// PhysicalDiskNumber is related to the BIOS physical disk number. Floppy drives are numbered
	// starting with 0x00 for the A disk. Physical hard disks are numbered starting with 0x80. The
	// value is typically 0x80 for hard disks, regardless of how many physical disk drives exist,
	// because the value is only relevant if the device is the startup disk.
	PhysicalDiskNumber byte // offset 0x24
	// CurrentHead is not used by the FAT file system.
	CurrentHead byte // offset 0x25
	// Signature must be either 0x28 or 0x29 in order to be recognized by Windows NT.
	Signature byte // offset 0x26
	// VolumeSerialNumber is a unique number that is created when you format the volume.
	VolumeSerialNumber [4]byte // offset 0x27
	// NoNameVolumeLabel is the BPB's copy of the label. The volume label proper
	// is a directory entry in the root, which is the one a tool should read.
	NoNameVolumeLabel [11]byte // offset 0x2B
	// SystemID is either FAT12, FAT16 or FAT32, depending on the format of the disk.
	SystemID [8]byte // offset 0x36
}

type RawDirectoryEntry struct {
	Name      [8]byte // Offset 0x00
	Extension [3]byte // Offset 0x08
	Attribute byte    // Offset 0x0B
	Reserved  byte    // Offset 0x0C
	// CreatedTime10MS is in 10 ms units, 0-199. Some DOS variants reuse this
	// byte to store a deleted file's original first name character.
	CreatedTime10MS  byte   // Offset 0x0D
	CreatedTime      uint16 // Offset 0x0E
	CreatedDate      uint16 // Offset 0x10
	LastAccessDate   uint16 // Offset 0x12
	FirstClusterHigh uint16 // Offset 0x14 -- Not used by FAT12 and FAT16
	LastModifiedTime uint16 // Offset 0x16
	LastModifiedDate uint16 // Offset 0x18
	FirstCluster     uint16 // Offset 0x1A
	FileSize         uint32 // Offset 0x1C
}

type DirectoryEntry struct {
	Name         string
	ShortName    string
	Deleted      bool
	ReadOnly     bool
	Hidden       bool
	System       bool
	VolumeLabel  bool
	Subdirectory bool
	Archive      bool
	Created      time.Time
	LastAccessed time.Time
	LastModified time.Time
	FirstCluster uint32
	FileSize     uint32
}

// Directory entry attribute bits. The combination 0x0F (read-only, hidden,
// system and volume label set together) designates a VFAT long filename entry
// rather than a file.
const (
	// DirectoryEntryAttributeReadOnly (Since DOS 2.0) if set, the operating system
	// will not allow a file to be opened for modification. Deliberately setting this
	// bit for files which will not be written to (executables, shared libraries and
	// data files) may help avoid problems with concurrent file access in multi-tasking,
	// multi-user or network environments with applications not specifically designed to
	// work in such environments (i.e. non-SHARE-enabled programs).
	//
	// The DCF digital camera file system standard utilizes the Read Only attribute to
	// allow directories or individual files (DCF objects) to be marked as
	// "protected" from deletion by the user.
	DirectoryEntryAttributeReadOnly = 0x01
	// DirectoryEntryAttributeHidden hides files or directories from normal directory views.
	DirectoryEntryAttributeHidden = 0x02
	// DirectoryEntryAttributeSystem marks a file that must not be physically relocated,
	// because it may be referenced by absolute address rather than through the filesystem.
	DirectoryEntryAttributeSystem = 0x04
	// DirectoryEntryAttributeVolumeLabel entries hold the volume name and have no file data.
	DirectoryEntryAttributeVolumeLabel = 0x08
	// DirectoryEntryAttributeSubdirectory means the cluster chain holds directory entries; FileSize is 0.
	DirectoryEntryAttributeSubdirectory = 0x10
	// DirectoryEntryAttributeArchive marks the file as modified since the last backup.
	DirectoryEntryAttributeArchive = 0x20
	// DirectoryEntryAttributeDevice is set only in memory for character device names, never on disk.
	DirectoryEntryAttributeDevice   = 0x40
	DirectoryEntryAttributeReserved = 0x80
)

const DeletedDirectoryEntryMarker = 0xE5

type TableEntry uint32

const (
	TableEntryFree         TableEntry = 0x00000000
	TableEntryReserved1    TableEntry = 0x00000001
	TableEntryNonStandard0 TableEntry = 0xfffffff0
	TableEntryNonStandard1 TableEntry = 0xfffffff1
	TableEntryNonStandard2 TableEntry = 0xfffffff2
	TableEntryNonStandard3 TableEntry = 0xfffffff3
	TableEntryNonStandard4 TableEntry = 0xfffffff4
	TableEntryNonStandard5 TableEntry = 0xfffffff5
	TableEntryReserved2    TableEntry = 0xfffffff6
	TableEntryBadSector    TableEntry = 0xfffffff7
	TableEntryEOC8         TableEntry = 0xfffffff8
	TableEntryEOC9         TableEntry = 0xfffffff9
	TableEntryEOCA         TableEntry = 0xfffffffa
	TableEntryEOCB         TableEntry = 0xfffffffb
	TableEntryEOCC         TableEntry = 0xfffffffc
	TableEntryEOCD         TableEntry = 0xfffffffd
	TableEntryEOCE         TableEntry = 0xfffffffe
	TableEntryEOCF         TableEntry = 0xffffffff
)

// EntryRange is a half-open range of FAT entry indexes, [Start, End).
type EntryRange struct {
	Start uint32
	End   uint32
}

// Table is one copy of the file allocation table.
type Table struct {
	Entries []TableEntry
	// Unreadable lists the entry ranges backed by sectors that could not be
	// read. Those entries decode as zero, which is indistinguishable from
	// "free", so a chain reaching one is unknown rather than ended.
	Unreadable []EntryRange
}

// Len returns the number of entries in the table.
func (t Table) Len() int { return len(t.Entries) }

// Readable reports whether entry i came from a sector that was read.
func (t Table) Readable(i uint32) bool {
	for _, r := range t.Unreadable {
		if i >= r.Start && i < r.End {
			return false
		}
	}
	return true
}

// UnreadableEntries returns how many entries were lost to unreadable sectors.
func (t Table) UnreadableEntries() int {
	var n int
	for _, r := range t.Unreadable {
		n += int(r.End - r.Start)
	}
	return n
}

func (t Table) Cluster(firstCluster uint32) ([]uint32, TableEntry, error) {
	if firstCluster < 2 {
		return nil, 0, fmt.Errorf("fat: cluster %d is reserved", firstCluster)
	}
	var ents []uint32
	seen := make(map[uint32]struct{})
	c := firstCluster
	for {
		if _, sn := seen[c]; sn {
			return nil, 0, fmt.Errorf("fat: cluster %d in chain starting at %d loops", c, firstCluster)
		}
		seen[c] = struct{}{}
		if int(c) >= len(t.Entries) {
			// Not counted as one of the chain's clusters: a number past the
			// table is not a cluster of this volume, and appending it makes
			// every size derived from the chain one cluster too large.
			return ents, TableEntry(c), fmt.Errorf("fat: cluster %d in chain starting at %d past end of table", c, firstCluster)
		}
		ents = append(ents, c)
		if !t.Readable(c) {
			return ents, 0, fmt.Errorf("fat: cluster %d in chain starting at %d lies in an unreadable region of the FAT", c, firstCluster)
		}
		e := t.Entries[c]
		if e < 0x00000002 || e > 0x0FFFFFEF {
			return ents, e, nil
		}
		c = uint32(e)
	}
}

// Validate checks the table for structural problems. Entries 0 and 1 are
// skipped: they are reserved and hold the media descriptor and the volume
// flags, not cluster pointers. Entry 1 in particular carries the clean-shutdown
// bit, so a volume that was pulled without unmounting leaves 0x7FFF there —
// reading that as a chain link claims a real cluster a second time and reports
// a sound table as corrupt.
func (t Table) Validate() error {
	// claimedBy[c] is 1 + the index of the entry pointing at cluster c, so zero
	// means unclaimed and a conflict can report both claimants.
	claimedBy := make([]uint32, len(t.Entries))
	for i, e := range t.Entries {
		if i < 2 || !t.Readable(uint32(i)) {
			continue
		}
		if e == TableEntryReserved1 {
			return fmt.Errorf("entry %d has reserved value 0x00000001", i)
		}
		if e >= 0x00000002 && e <= 0x0FFFFFEF {
			if int(e) >= len(t.Entries) {
				return fmt.Errorf("entry %d with value %d past end of table", i, e)
			}
			if prev := claimedBy[e]; prev != 0 {
				return fmt.Errorf("cluster %d is claimed by both entry %d and entry %d", e, prev-1, i)
			}
			//nolint:gosec // e is bounded above by len(t.Entries), which is an int
			claimedBy[e] = uint32(i) + 1
		}
	}
	return nil
}

func (de *RawDirectoryEntry) NameChecksum() byte {
	var sum byte
	for _, b := range de.Name {
		sum = ((sum & 1) << 7) + (sum >> 1) + b
	}
	for _, b := range de.Extension {
		sum = ((sum & 1) << 7) + (sum >> 1) + b
	}
	return sum
}

// maxFATTableSize caps one FAT copy. FAT32 stores SectorsPerFAT as a uint32,
// which times a 4096-byte sector would permit a multi-terabyte allocation; the
// largest table a real FAT32 volume needs is well under 256 MiB.
const maxFATTableSize = 256 << 20

// setFATTableSize computes the byte size of one FAT copy and rejects a value
// too large to be real, since the table is read in one allocation.
func (f *FAT) setFATTableSize(sectorsPerFAT int64) error {
	size := sectorsPerFAT * int64(f.SectorSize)
	if size <= 0 || size > maxFATTableSize {
		return fmt.Errorf("fat: implausible FAT size: %d sectors × %d bytes", sectorsPerFAT, f.SectorSize)
	}
	f.FATTableSize = int(size)
	return nil
}

func Open(r io.ReadSeeker, offset int64) (*FAT, error) {
	// The image's own length bounds everything the boot sector declares: a
	// volume claiming more sectors than the image holds must not turn a chain
	// into an allocation nothing bounds.
	imageEnd, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		imageEnd = 0
	}
	if _, err := r.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}
	f := &FAT{
		r:                r,
		BootSectorOffset: offset,
		imageEnd:         imageEnd,
	}
	bs, rawBS, err := readBootSector(r)
	if err != nil {
		return nil, err
	}
	f.BootSector = bs
	switch bs.BIOSParameterBlock.BytesPerSector {
	case 128, 256, 512, 1024, 2048, 4096:
	default:
		return nil, fmt.Errorf("%w: unsupported sector size %d", ErrNotFAT, bs.BIOSParameterBlock.BytesPerSector)
	}
	f.SectorSize = int(bs.BIOSParameterBlock.BytesPerSector)
	if bs.BIOSParameterBlock.ReservedSectors < 1 {
		return nil, fmt.Errorf("%w: invalid reserved sectors count %d", ErrNotFAT, bs.BIOSParameterBlock.ReservedSectors)
	}
	// Every table lookup starts at Tables[0]. Without a copy there is nothing to
	// resolve a cluster chain against.
	if bs.BIOSParameterBlock.NumberOfFATs < 1 {
		return nil, fmt.Errorf("%w: boot sector declares no FAT copies", ErrNotFAT)
	}
	if bs.BIOSParameterBlock.SectorsPerCluster == 0 {
		return nil, fmt.Errorf("%w: SectorsPerCluster is zero", ErrNotFAT)
	}
	if bs.BIOSParameterBlock.SectorsPerFAT == 0 {
		// FAT32: extended BPB has SectorsPerFAT at boot sector offset 0x24 and RootClus at 0x2C.
		fat32SPF := binary.LittleEndian.Uint32(rawBS[0x24:0x28])
		f.RootCluster = binary.LittleEndian.Uint32(rawBS[0x2C:0x30])
		if f.RootCluster == 0 {
			f.diag(DiagRootClusterZero, "fat: FAT32 boot sector declares no root cluster (RootCluster = 0)")
		}
		sysID := strings.TrimSpace(string(rawBS[0x52:0x5A]))
		if sysID != "FAT32" {
			return nil, fmt.Errorf("%w: unexpected system ID %q for FAT32", ErrNotFAT, sysID)
		}
		f.Type = TypeFAT32
		if err := f.setFATTableSize(int64(fat32SPF)); err != nil {
			return nil, err
		}
		// FS Information Sector: sector number at extended BPB offset 0x30 (relative to partition).
		fsInfoSector := binary.LittleEndian.Uint16(rawBS[0x30:0x32])
		if fsInfoSector != 0 && fsInfoSector != 0xFFFF {
			fsInfoOffset := offset + int64(fsInfoSector)*int64(f.SectorSize)
			fsInfo, err := readFSInfo(r, fsInfoOffset)
			if err != nil {
				f.diag(DiagFSInfoUnreadable, "fat: failed to read FSInfo sector: %s", err)
			} else {
				f.FSInfo = fsInfo
			}
		}
	} else {
		bpb := bs.BIOSParameterBlock
		if err := f.setFATTableSize(int64(bpb.SectorsPerFAT)); err != nil {
			return nil, err
		}
		totalSectors := int64(bpb.SmallSectors)
		if totalSectors == 0 {
			totalSectors = int64(bpb.LargeSectors)
		}
		rootDirSectors := (int64(bpb.RootEntries)*32 + int64(bpb.BytesPerSector) - 1) / int64(bpb.BytesPerSector)
		firstDataSector := int64(bpb.ReservedSectors) + int64(bpb.NumberOfFATs)*int64(bpb.SectorsPerFAT) + rootDirSectors
		// Signed arithmetic throughout: a truncated or corrupt BPB can put
		// firstDataSector past totalSectors, and an unsigned subtraction wraps to
		// a huge count that misdetects FAT12 as FAT16. The wrong entry width then
		// makes the whole table parse as garbage.
		if firstDataSector > totalSectors {
			return nil, fmt.Errorf("fat: data region starts at sector %d, past the declared volume size of %d sectors",
				firstDataSector, totalSectors)
		}
		dataClusters := (totalSectors - firstDataSector) / int64(bpb.SectorsPerCluster)
		if dataClusters < 4085 {
			f.Type = TypeFAT12
		} else {
			f.Type = TypeFAT16
		}
	}
	fatOffset := offset + int64(bs.BIOSParameterBlock.ReservedSectors)*int64(f.SectorSize)
	for range int(bs.BIOSParameterBlock.NumberOfFATs) {
		f.FATTableOffsets = append(f.FATTableOffsets, fatOffset)
		fatOffset += int64(f.FATTableSize)
	}
	f.RootDirectoryOffset = fatOffset
	rootDirSectors := ((int64(bs.BIOSParameterBlock.RootEntries) * 32) + int64(f.SectorSize-1)) / int64(f.SectorSize)
	f.DataRegionOffset = fatOffset + rootDirSectors*int64(f.SectorSize)
	f.TotalSectors = uint32(bs.BIOSParameterBlock.SmallSectors)
	if f.TotalSectors == 0 {
		f.TotalSectors = bs.BIOSParameterBlock.LargeSectors
	}
	f.ClusterSize = int(f.BootSector.BIOSParameterBlock.SectorsPerCluster) * f.SectorSize
	if f.TotalSectors != 0 && f.imageEnd > f.BootSectorOffset {
		declaredEnd := f.BootSectorOffset + int64(f.TotalSectors)*int64(f.SectorSize)
		if declaredEnd > f.imageEnd {
			f.diag(DiagVolumeSizeMismatch,
				"fat: volume declares %d sectors beyond the end of the image (%d bytes available)",
				(declaredEnd-f.imageEnd)/int64(f.SectorSize), f.imageEnd-f.BootSectorOffset)
		}
	}
	// Each copy is read independently: a bad sector in one copy must not make
	// the volume unopenable when another copy is intact.
	for i := range int(f.BootSector.BIOSParameterBlock.NumberOfFATs) {
		t, err := f.ReadFATTable(i)
		if err != nil {
			f.diag(DiagFATCopyUnreadable, "fat: FAT copy %d is unreadable: %s", i, err)
			continue
		}
		if n := t.UnreadableEntries(); n > 0 {
			f.diag(DiagFATCopyDamaged, "fat: FAT copy %d has %d entries in unreadable sectors", i, n)
		}
		if err := t.Validate(); err != nil {
			f.diag(DiagFATCopyInvalid, "fat: invalid FAT table %d: %s", i, err)
		}
		f.Tables = append(f.Tables, t)
		f.TableCopies = append(f.TableCopies, i)
	}
	if len(f.Tables) == 0 {
		return nil, errors.New("fat: no readable FAT copy")
	}
	// FAT spec: low byte of Table[0] must match BPB MediaType.
	if len(f.Tables[0].Entries) > 0 {
		//nolint:gosec // the media descriptor is by definition the low byte of FAT[0]
		if got := byte(f.Tables[0].Entries[0]); got != byte(f.BootSector.BIOSParameterBlock.MediaType) {
			f.diag(DiagMediaByteMismatch, "fat: FAT[0] media byte 0x%02X does not match BPB MediaType %s", got, f.BootSector.BIOSParameterBlock.MediaType)
		}
	}
	return f, nil
}

// ByteRange is a half-open range of byte offsets within a buffer, such as a FAT
// copy or a directory's data.
type ByteRange struct {
	Start int
	End   int
}

// appendRange adds [start, end) to ranges, coalescing with the previous one.
// Ranges are added in increasing order.
func appendRange(ranges []ByteRange, start, end int) []ByteRange {
	if end <= start {
		return ranges
	}
	if n := len(ranges); n > 0 && ranges[n-1].End == start {
		ranges[n-1].End = end
		return ranges
	}
	return append(ranges, ByteRange{Start: start, End: end})
}

// readSectors fills buf from image offset off one sector at a time, zeroing
// every sector that will not read and recording its range in the buffer's own
// offsets. Both the FAT copies and the directories are read this way: one
// io.ReadFull over the whole span would lose everything after the first bad
// sector, including the sectors that read fine. The head of a sector that
// failed part way is kept, since those bytes did come back.
//
// readable counts the sectors that came back whole, so a caller can tell "some
// of this is missing" from "none of it was there".
func (f *FAT) readSectors(off int64, buf []byte) (bad []ByteRange, readable int, firstErr error) {
	unit := f.SectorSize
	if unit <= 0 || unit > len(buf) {
		unit = len(buf)
	}
	if unit == 0 {
		return nil, 0, nil
	}
	for s := 0; s < len(buf); s += unit {
		end := min(s+unit, len(buf))
		n, err := f.readAt(off+int64(s), buf[s:end])
		if err != nil {
			clear(buf[s+n : end])
			bad = appendRange(bad, s+n, end)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		readable++
	}
	return bad, readable, firstErr
}

// readFATBytes reads one FAT copy a sector at a time so that an unreadable
// sector costs only the entries it holds instead of the whole table. Failed
// sectors are left zeroed and their byte ranges returned. The error is non-nil
// only when no sector of the copy could be read.
func (f *FAT) readFATBytes(index int) ([]byte, []ByteRange, error) {
	if index < 0 || index >= len(f.FATTableOffsets) {
		return nil, nil, fmt.Errorf("fat: FAT copy %d does not exist", index)
	}
	data := make([]byte, f.FATTableSize)
	bad, readable, firstErr := f.readSectors(f.FATTableOffsets[index], data)
	if readable == 0 {
		return nil, nil, fmt.Errorf("fat: no readable sector in FAT copy %d: %w", index, firstErr)
	}
	return data, bad, nil
}

// entryRanges converts unreadable byte ranges to the entry indexes they cover,
// widening to whole entries so an entry straddling the edge counts as lost.
func (f *FAT) entryRanges(bad []ByteRange, nEntries int) []EntryRange {
	var out []EntryRange
	for _, br := range bad {
		var start, end int
		switch f.Type {
		case TypeFAT12:
			// Entry n occupies the two bytes at n*3/2, so entries overlap.
			start = (br.Start - 2) * 2 / 3
			end = br.End*2/3 + 2
		case TypeFAT16:
			start = br.Start / 2
			end = (br.End + 1) / 2
		case TypeFAT32:
			start = br.Start / 4
			end = (br.End + 3) / 4
		}
		start = max(start, 0)
		end = min(end, nEntries)
		if start >= end {
			continue
		}
		//nolint:gosec // start and end are clamped to [0, nEntries] on the lines above
		s, e := uint32(start), uint32(end)
		// Coalesce with the previous range; sectors are visited in order.
		if n := len(out); n > 0 && s <= out[n-1].End {
			out[n-1].End = e
			continue
		}
		out = append(out, EntryRange{Start: s, End: e})
	}
	return out
}

// ReadFATTable reads one copy of the FAT. The copy is read sector by sector, so
// a bad sector costs only the entries it holds: those are recorded in
// Table.Unreadable and every chain avoiding them still resolves.
func (f *FAT) ReadFATTable(index int) (Table, error) {
	data, bad, err := f.readFATBytes(index)
	if err != nil {
		return Table{}, err
	}
	var table Table
	switch f.Type {
	case TypeFAT12:
		nEntries := len(data) * 2 / 3
		table.Entries = make([]TableEntry, nEntries)
		for n := range nEntries {
			byteOff := n * 3 / 2
			word := uint16(data[byteOff]) | uint16(data[byteOff+1])<<8
			var val uint16
			if n%2 == 0 {
				val = word & 0x0FFF
			} else {
				val = word >> 4
			}
			if val >= 0xFF0 {
				table.Entries[n] = TableEntry(uint32(val) | 0xFFFFF000)
			} else {
				table.Entries[n] = TableEntry(val)
			}
		}
	case TypeFAT16:
		nEntries := len(data) / 2
		table.Entries = make([]TableEntry, nEntries)
		for n := range nEntries {
			e := binary.LittleEndian.Uint16(data[n*2 : n*2+2])
			if e <= 0xffef {
				table.Entries[n] = TableEntry(e)
			} else {
				table.Entries[n] = TableEntry(e) | 0xffff0000
			}
		}
	case TypeFAT32:
		nEntries := len(data) / 4
		table.Entries = make([]TableEntry, nEntries)
		for n := range nEntries {
			e := binary.LittleEndian.Uint32(data[n*4:n*4+4]) & 0x0FFFFFFF
			if e >= 0x0FFFFFF0 {
				table.Entries[n] = TableEntry(e | 0xF0000000)
			} else {
				table.Entries[n] = TableEntry(e)
			}
		}
	default:
		return Table{}, fmt.Errorf("unsupported fat type %q", f.Type)
	}
	table.Unreadable = f.entryRanges(bad, table.Len())
	return table, nil
}

// ClusterReadError reports a cluster that could not be read in full. N is the
// number of leading bytes recovered before the failure; those bytes are
// returned alongside this error rather than discarded.
type ClusterReadError struct {
	Cluster uint32
	N       int
	Err     error
}

func (e *ClusterReadError) Error() string {
	return fmt.Sprintf("fat: read cluster %d: %d bytes recovered before %s", e.Cluster, e.N, e.Err)
}

func (e *ClusterReadError) Unwrap() error { return e.Err }

// volumeEnd returns the byte offset just past the volume, or 0 when there is
// no usable size to report. The declared size is clamped to the image's actual
// end, so a boot sector overstating the volume (e.g. a truncated or corrupt rescue)
// cannot make the data region look bigger than the bytes behind it.
func (f *FAT) volumeEnd() int64 {
	if f.SectorSize == 0 {
		return 0
	}
	var end int64
	if f.TotalSectors != 0 {
		end = f.BootSectorOffset + int64(f.TotalSectors)*int64(f.SectorSize)
	}
	if f.imageEnd > f.BootSectorOffset && (end == 0 || end > f.imageEnd) {
		end = f.imageEnd
	}
	return end
}

// dataRegionBytes is how many bytes the data region holds, or 0 when the boot
// sector did not declare a usable volume size. It is not volumeEnd: that is an
// absolute offset and includes everything before the data region, so comparing
// a chain's length against it lets a chain run past the volume.
func (f *FAT) dataRegionBytes() int64 {
	end := f.volumeEnd()
	if end <= f.DataRegionOffset {
		return 0
	}
	return end - f.DataRegionOffset
}

// DataClusters is how many clusters the data region holds: the volume's
// clusters are 2 through DataClusters+1. It is not the number of FAT entries —
// the FAT is a whole number of sectors and so has room for entries past the
// last real cluster, and counting those reports phantom free clusters.
func (f *FAT) DataClusters() int64 {
	if f.ClusterSize == 0 {
		return 0
	}
	return f.dataRegionBytes() / int64(f.ClusterSize)
}

// lastDataCluster is the highest cluster number the data region holds, or
// MaxInt64 when nothing bounds the table's own extent. Once the data region's
// start is known to lie past the image's end there are no data clusters at all,
// which is a different answer from unknown: counting the table then would
// report clusters whose bytes do not exist.
func (f *FAT) lastDataCluster() int64 {
	if n := f.DataClusters(); n > 0 {
		return n + 1
	}
	if f.imageEnd > 0 {
		return 1
	}
	return math.MaxInt64
}

// checkChainFits rejects a chain longer than the data region, which a corrupt
// table can produce and which would otherwise be read into one huge allocation.
// The region's extent is clamped to the image's actual end in volumeEnd, so a
// boot sector overstating the volume cannot let an oversized chain through.
// When the region holds no bytes at all (the volume declared none usable, or
// its start lies past the image's end), any chain of positive length is
// rejected rather than read.
func (f *FAT) checkChainFits(cluster uint32, n int) error {
	if n <= 0 {
		return nil
	}
	if room := f.dataRegionBytes(); room > 0 {
		if int64(n)*int64(f.ClusterSize) > room {
			return fmt.Errorf("fat: chain starting at %d spans %d clusters, more than the whole data region",
				cluster, n)
		}
		return nil
	}
	if f.imageEnd > 0 {
		return fmt.Errorf("fat: chain starting at %d spans %d clusters, past the end of the image",
			cluster, n)
	}
	return nil
}

func (f *FAT) ClusterOffset(cluster uint32) (int64, error) {
	if cluster < 2 {
		return 0, fmt.Errorf("fat: cluster %d is reserved and cannot be read", cluster)
	}
	off := f.DataRegionOffset + int64(f.ClusterSize)*int64(cluster-2)
	// Without this an out-of-range cluster yields an offset past the end of the
	// image and the caller sees an opaque EOF instead of the real problem.
	// The check is against the declared volume, not the clamped volumeEnd: an
	// image shorter than its declared volume is a truncated rescue, and the
	// tail of a cluster straddling the cut must still come back before the
	// read reports the failure.
	if end := f.declaredVolumeEnd(); end > 0 && off+int64(f.ClusterSize) > end {
		return 0, fmt.Errorf("fat: cluster %d starts at offset %d, past the end of the volume at %d", cluster, off, end)
	}
	return off, nil
}

// declaredVolumeEnd is the end the boot sector declares, ignoring any clamp to
// the image's actual length.
func (f *FAT) declaredVolumeEnd() int64 {
	if f.TotalSectors == 0 || f.SectorSize == 0 {
		return 0
	}
	return f.BootSectorOffset + int64(f.TotalSectors)*int64(f.SectorSize)
}

// ReadClusterDirect reads one cluster's raw data without consulting the FAT,
// for clusters whose FAT entries have been freed or corrupted.
func (f *FAT) ReadClusterDirect(cluster uint32) ([]byte, error) {
	off, err := f.ClusterOffset(cluster)
	if err != nil {
		return nil, err
	}
	buf := make([]byte, f.ClusterSize)
	n, err := f.readAt(off, buf)
	if err != nil {
		return buf[:n], &ClusterReadError{Cluster: cluster, N: n, Err: err}
	}
	return buf, nil
}

// MergedTable reconciles the FAT copies into one best-effort table: where the
// copies disagree and one side is free (which includes an entry lost to a bad
// sector, since those decode as zero) the other side wins, and copy 0 wins the
// irreconcilable rest. An entry is unknown only when every copy lost it. The
// result is cached.
func (f *FAT) MergedTable() (Table, error) {
	// merged is declared under mu; nothing this function calls takes it.
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.merged != nil {
		return *f.merged, nil
	}
	if len(f.Tables) == 0 {
		return Table{}, errors.New("fat: no FAT tables loaded")
	}
	if len(f.Tables) < 2 {
		t := f.Tables[0]
		f.merged = &t
		return t, nil
	}
	conflicts, err := f.AnalyzeFATConflicts()
	if err != nil {
		return Table{}, err
	}
	t0, t1 := f.Tables[0], f.Tables[1]
	merged := Table{Entries: slices.Clone(t0.Entries)}
	for _, c := range conflicts {
		if c.Mergeable {
			merged.Entries[c.Cluster] = c.Resolved
		}
	}
	for _, r := range t0.Unreadable {
		for i := r.Start; i < r.End && int(i) < len(merged.Entries); i++ {
			if t1.Readable(i) {
				continue // copy 1 supplied a value for this entry
			}
			if n := len(merged.Unreadable); n > 0 && merged.Unreadable[n-1].End == i {
				merged.Unreadable[n-1].End = i + 1
				continue
			}
			merged.Unreadable = append(merged.Unreadable, EntryRange{Start: i, End: i + 1})
		}
	}
	f.merged = &merged
	return merged, nil
}

// resolveClusterChain returns the ordered cluster list for a chain, using the
// FAT copy that ChainPolicy selects.
func (f *FAT) resolveClusterChain(cluster uint32) ([]uint32, error) {
	if len(f.Tables) == 0 {
		return nil, errors.New("fat: no FAT tables loaded")
	}
	clusters, finalEntry, err := f.chainByPolicy(cluster)
	if err != nil {
		return nil, err
	}
	switch {
	case finalEntry == TableEntryBadSector:
		return nil, fmt.Errorf("fat: cluster chain starting at %d has bad sectors", cluster)
	case finalEntry >= TableEntryEOC8:
		// valid end of chain
	default:
		return nil, fmt.Errorf("fat: cluster chain starting at %d has invalid endpoint %08x", cluster, finalEntry)
	}
	return clusters, nil
}

func (f *FAT) chainByPolicy(cluster uint32) ([]uint32, TableEntry, error) {
	switch f.ChainPolicy {
	case ChainPolicyCopy0:
		return f.Tables[0].Cluster(cluster)
	case ChainPolicyCopy1:
		if len(f.Tables) < 2 {
			return nil, 0, errors.New("fat: FAT copy 1 was not loaded")
		}
		return f.Tables[1].Cluster(cluster)
	case ChainPolicyBestEffort:
		t, err := f.MergedTable()
		if err != nil {
			return nil, 0, err
		}
		return t.Cluster(cluster)
	default:
		return f.strictChain(cluster)
	}
}

// strictChain walks the chain in both copies and requires them to agree. One
// copy holding a structurally invalid chain, or no end-of-chain marker, is
// resolved in favor of the other; a genuine disagreement is an error, which
// another ChainPolicy can override.
func (f *FAT) strictChain(cluster uint32) ([]uint32, TableEntry, error) {
	clusters, finalEntry, err := f.Tables[0].Cluster(cluster)
	var clusters1 []uint32
	var finalEntry1 TableEntry
	var err1 error
	if len(f.Tables) > 1 {
		clusters1, finalEntry1, err1 = f.Tables[1].Cluster(cluster)
	} else {
		clusters1, finalEntry1, err1 = clusters, finalEntry, err
	}
	if err != nil && err1 != nil {
		return nil, 0, err
	}
	if err == nil && err1 != nil {
		f.diag(DiagChainCopyFallback, "fat: table 1 has an invalid chain for cluster %d, using table 0: %s", cluster, err1)
		clusters1 = clusters
		finalEntry1 = finalEntry
	} else if err != nil && err1 == nil {
		f.diag(DiagChainCopyFallback, "fat: table 0 has an invalid chain for cluster %d, using table 1: %s", cluster, err)
		clusters = clusters1
		finalEntry = finalEntry1
	}
	if finalEntry == 0 && finalEntry1 >= TableEntryEOC8 {
		f.diag(DiagChainCopyFallback, "fat: table 1 has the end-of-chain marker for cluster %d (%02x), using table 1", cluster, finalEntry1)
		finalEntry = finalEntry1
		clusters = clusters1
	} else if finalEntry >= TableEntryEOC8 && finalEntry1 == 0 {
		f.diag(DiagChainCopyFallback, "fat: table 0 has the end-of-chain marker for cluster %d (%02x), using table 0", cluster, finalEntry)
		finalEntry1 = finalEntry
		clusters1 = clusters
	}
	if len(clusters1) != len(clusters) || finalEntry1 != finalEntry {
		return nil, 0, fmt.Errorf("mismatched fat at cluster %d (%d != %d || %x != %x); a --fat-policy override can pick a copy",
			cluster, len(clusters), len(clusters1), finalEntry, finalEntry1)
	}
	for i, e := range clusters {
		if clusters1[i] != e {
			return nil, 0, fmt.Errorf("mismatched fat at cluster %d offset %d; a --fat-policy override can pick a copy", cluster, i)
		}
	}
	return clusters, finalEntry, nil
}

// ReadClusterChain reads the whole chain starting at cluster into one buffer.
// A positive expectedSize stops the read once enough clusters to cover it have
// been read, which bounds the allocation when a corrupt chain runs long; pass a
// negative value for the full chain.
func (f *FAT) ReadClusterChain(cluster uint32, expectedSize int64) ([]byte, error) {
	clusters, err := f.resolveClusterChain(cluster)
	if err != nil {
		return nil, err
	}
	if expectedSize > 0 {
		need := (expectedSize + int64(f.ClusterSize) - 1) / int64(f.ClusterSize)
		if need < int64(len(clusters)) {
			clusters = clusters[:need]
		}
	}
	// A chain cannot hold more than the data region it lives in.
	if err := f.checkChainFits(cluster, len(clusters)); err != nil {
		return nil, err
	}
	buf := make([]byte, f.ClusterSize*len(clusters))
	for i, c := range clusters {
		off, err := f.ClusterOffset(c)
		if err != nil {
			return buf[:i*f.ClusterSize], err
		}
		n, err := f.readAt(off, buf[i*f.ClusterSize:(i+1)*f.ClusterSize])
		if err != nil {
			// Keep the clusters already read plus the readable head of this one.
			return buf[:i*f.ClusterSize+n], &ClusterReadError{Cluster: c, N: n, Err: err}
		}
	}
	return buf, nil
}

type Reader struct {
	f            *FAT
	size         int64
	total        int64
	clusters     []uint32
	buf          []byte
	left         []byte
	currentIndex int
	// fill, when armed, makes a failed read deliver zeros for the bytes it
	// could not recover and continue to the next sector instead of latching.
	// This keeps an extracted container parseable: a hole at the right offset
	// loses only the files over it, while a truncated copy loses every file
	// after it. It is safe only because the log records exactly where the zeros
	// are and is written out beside the recovered data.
	fill *ddrescue.GapLog
	// err is latched once a cluster fails. The reader does not advance past a
	// failed cluster, so without this a partial cluster would be delivered and
	// then re-read on every subsequent call.
	err error
}

// Fill gives the reader a log to record its holes in, switching it from
// latching at the damage to zero-filling past it. Call it before the first
// Read. Only ddrescue.Fill arms a log, and it writes the map in the same call,
// so a reader cannot deliver zeros that nothing records.
func (r *Reader) Fill(l *ddrescue.GapLog) {
	if !l.Filling() {
		return
	}
	l.Limit(r.total)
	r.fill = l
}

func (r *Reader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for r.size != 0 {
		if len(r.left) != 0 {
			if int64(len(p)) > r.size {
				p = p[:r.size]
			}
			n := copy(p, r.left)
			r.left = r.left[n:]
			r.size -= int64(n)
			return n, nil
		}
		if r.err != nil {
			return 0, r.err
		}
		if r.currentIndex >= len(r.clusters) {
			return 0, io.EOF
		}
		cluster := r.clusters[r.currentIndex]
		off, err := r.f.ClusterOffset(cluster)
		if err != nil {
			if !r.fill.Filling() {
				return 0, err
			}
			// The cluster has no readable position at all, so the whole of it
			// is a hole.
			clear(r.buf)
			r.fill.Add(r.clusterBase(), int64(r.f.ClusterSize), err)
			r.currentIndex++
			r.left = r.buf
			continue
		}
		n, err := r.f.readAt(off, r.buf)
		if err != nil {
			if r.fill.Filling() {
				// Retry a sector at a time, so a bad sector costs its sector
				// and not the whole cluster.
				r.fill.Recover(ddrescue.Chunk{
					FileOffset:  r.clusterBase(),
					ImageOffset: off,
					Buf:         r.buf,
					Unit:        r.f.SectorSize,
					Read:        r.f.readAt,
				}, n, err)
				r.currentIndex++
				r.left = r.buf
				continue
			}
			// Hand back the readable head of the cluster before reporting.
			r.err = &ClusterReadError{Cluster: cluster, N: n, Err: err}
			if n == 0 {
				return 0, r.err
			}
			r.left = r.buf[:n]
			continue
		}
		r.currentIndex++
		r.left = r.buf
	}
	return 0, io.EOF
}

// OpenClusterChain returns a Reader over the chain starting at cluster, bounded
// by expectedSize; pass a negative value for the whole chain.
//
// A zero expectedSize is an empty file and yields an empty reader whatever the
// entry's first cluster says. A sound volume leaves that pointer at 0, which
// resolves to no chain at all ("cluster 0 is reserved"), and one that carries a
// stale pointer would otherwise stream a cluster of whatever now lives there as
// if it were the file's data.
func (f *FAT) OpenClusterChain(cluster uint32, expectedSize int64) (*Reader, error) {
	if expectedSize == 0 {
		return &Reader{f: f, buf: make([]byte, f.ClusterSize)}, nil
	}
	clusters, err := f.resolveClusterChain(cluster)
	if err != nil {
		return nil, err
	}
	size := int64(len(clusters) * f.ClusterSize)
	if expectedSize > 0 && size < expectedSize {
		return nil, fmt.Errorf("fat: expected cluster chain start at %d to be of size %d but got %d", cluster, expectedSize, size)
	}
	if expectedSize > 0 {
		size = expectedSize
	}
	return &Reader{
		f:        f,
		size:     size,
		total:    size,
		clusters: clusters,
		buf:      make([]byte, f.ClusterSize),
	}, nil
}

// DirEntrySize is the on-disk size of one directory entry. Entries never span a
// sector, so a bad sector loses only the entries it holds, not the rest of the
// directory.
const DirEntrySize = 32

// DirReadError reports the sectors of a directory that would not read. The
// entries they held are gone; every entry in every other sector is recovered
// and returned alongside this error.
//
// The parser needs Lost: an entry that was never read decodes as all zeros,
// which FAT uses to mark the end of a directory. An unmarked hole would
// truncate the listing at the damage, losing every entry after it, and every
// subdirectory beneath them, for one bad sector.
type DirReadError struct {
	// Lost holds the byte ranges of the directory's data that came back as
	// zeros, in the directory's own offsets.
	Lost []ByteRange
	// Err is the first read failure.
	Err error
}

func (e *DirReadError) Error() string {
	var n int
	for _, r := range e.Lost {
		n += r.End - r.Start
	}
	return fmt.Sprintf("fat: %d unreadable range(s) in the directory: %d bytes lost, up to %d entries: %s",
		len(e.Lost), n, n/DirEntrySize, e.Err)
}

func (e *DirReadError) Unwrap() error { return e.Err }

// ReadRawDirectory returns the raw directory entries starting at firstCluster,
// or the root directory when firstCluster is 0.
//
// The directory is read sector by sector, so one bad sector loses the sixteen
// entries it holds and the parse resumes at the next sector boundary. One
// io.ReadFull per cluster would lose everything after the first failure,
// including clusters that read fine: a bad sector in cluster 2 of a
// four-cluster directory would also lose clusters 3 and 4. The ranges the
// failed sectors covered are returned in a *DirReadError.
func (f *FAT) ReadRawDirectory(firstCluster uint32) ([]RawDirectoryEntry, error) {
	dir, _, err := f.readRawDirectory(firstCluster)
	return dir, err
}

// readRawDirectory is ReadRawDirectory with the lost ranges handed back
// separately, since the entry parser needs them to tell a hole from the end of
// the directory.
func (f *FAT) readRawDirectory(firstCluster uint32) ([]RawDirectoryEntry, []ByteRange, error) {
	raw, lost, readErr := f.readDirectoryData(firstCluster)
	if raw == nil {
		return nil, nil, readErr
	}
	dir := make([]RawDirectoryEntry, len(raw)/DirEntrySize)
	if err := binary.Read(bytes.NewReader(raw), binary.LittleEndian, &dir); err != nil {
		return dir, nil, err
	}
	if readErr != nil {
		return dir, lost, &DirReadError{Lost: lost, Err: readErr}
	}
	return dir, nil, nil
}

// readDirectoryData reads a directory's bytes sector by sector, zeroing what
// would not read and reporting where. A nil buffer means nothing of the
// directory was readable at all, which is a different answer from an empty one.
func (f *FAT) readDirectoryData(firstCluster uint32) ([]byte, []ByteRange, error) {
	if firstCluster == 0 && f.Type == TypeFAT32 {
		firstCluster = f.RootCluster
	}
	if firstCluster == 0 {
		if f.Type == TypeFAT32 {
			// A FAT32 root is a cluster chain. A boot sector with no root
			// cluster leaves nothing to walk, which is damage, not an empty
			// volume. Without this check the read falls into the FAT12/16
			// fixed-root branch sized by RootEntries (0 on FAT32) and silently
			// parses as an empty volume.
			return nil, nil, errors.New("fat: FAT32 root cluster is zero; the root directory has no cluster chain")
		}
		// FAT12/16 root: a fixed region of the volume rather than a chain.
		raw := make([]byte, int(f.BootSector.BIOSParameterBlock.RootEntries)*DirEntrySize)
		lost, readable, err := f.readSectors(f.RootDirectoryOffset, raw)
		if readable == 0 && err != nil {
			return nil, nil, err
		}
		return raw, lost, err
	}
	clusters, err := f.resolveClusterChain(firstCluster)
	if err != nil {
		return nil, nil, err
	}
	if err := f.checkChainFits(firstCluster, len(clusters)); err != nil {
		return nil, nil, err
	}
	raw := make([]byte, len(clusters)*f.ClusterSize)
	var lost []ByteRange
	var firstErr error
	var readable int
	for i, c := range clusters {
		base := i * f.ClusterSize
		off, err := f.ClusterOffset(c)
		if err != nil {
			// The cluster has no readable position at all, so all of it is
			// lost, but the clusters after it are still at their own offsets.
			lost = appendRange(lost, base, base+f.ClusterSize)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		bad, n, err := f.readSectors(off, raw[base:base+f.ClusterSize])
		for _, r := range bad {
			lost = appendRange(lost, base+r.Start, base+r.End)
		}
		readable += n
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if readable == 0 && firstErr != nil {
		return nil, nil, firstErr
	}
	return raw, lost, firstErr
}

// ReadDirectory returns the directory's entries. On a read failure it returns
// the entries recovered from every sector that did read, alongside the error.
func (f *FAT) ReadDirectory(firstCluster uint32, filter bool) ([]*DirectoryEntry, error) {
	rawDir, lost, err := f.readRawDirectory(firstCluster)
	return f.processRawDirectory(rawDir, lost, filter), err
}

// ReadDirectoryDirect reads directory entries from one cluster without
// consulting the FAT, for deleted directories whose FAT entries have been freed.
func (f *FAT) ReadDirectoryDirect(cluster uint32, filter bool) ([]*DirectoryEntry, error) {
	// A short read still yields the head of the cluster; parse what arrived.
	data, readErr := f.ReadClusterDirect(cluster)
	nEntries := len(data) / DirEntrySize
	rawDir := make([]RawDirectoryEntry, nEntries)
	if err := binary.Read(bytes.NewReader(data), binary.LittleEndian, &rawDir); err != nil {
		return nil, err
	}
	return f.processRawDirectory(rawDir, nil, filter), readErr
}

// processRawDirectory decodes the raw entries. lost lists the byte ranges that
// were never read: those entries are all zeros, which would otherwise read as
// the end of the directory and stop the parse at the damage.
func (f *FAT) processRawDirectory(rawDir []RawDirectoryEntry, lost []ByteRange, filter bool) []*DirectoryEntry {
	var vfatName string
	var vfatChecksum byte
	var vfatSeq byte
	var dir []*DirectoryEntry
	for i, e := range rawDir {
		if inRanges(lost, i*DirEntrySize) {
			// Never read, so it carries no information and is not an
			// end-of-directory marker. Any long-name sequence it was part of is
			// abandoned rather than joined across the hole into a name the
			// volume never held.
			vfatName = ""
			vfatSeq = 0
			continue
		}
		if e.Name[0] == 0 {
			break
		}
		// Deleted
		if filter && e.Name[0] == DeletedDirectoryEntryMarker {
			continue
		}
		// On FAT12/FAT16 a zero start cluster separates a real LFN entry from a
		// pending-delete file, which also carries the 0x0F attribute.
		if e.Attribute == 0x0f && e.FirstCluster == 0 {
			// VFAT
			switch seq := e.Name[0] & 0x3F; {
			case e.Name[0]&0x40 != 0:
				// Last logical / first physical LFN entry: start a new sequence.
				vfatName = ""
				vfatSeq = seq
			case vfatSeq == 0 || seq != vfatSeq-1:
				f.diag(DiagVFATName, "fat: unexpected VFAT sequence number %d (expected %d)", seq, vfatSeq-1)
				vfatName = ""
				vfatSeq = 0
				continue
			default:
				vfatSeq = seq
			}
			name := []rune{
				rune(e.Name[1]) | (rune(e.Name[2]) << 8),
				rune(e.Name[3]) | (rune(e.Name[4]) << 8),
				rune(e.Name[5]) | (rune(e.Name[6]) << 8),
				rune(e.Name[7]) | (rune(e.Extension[0]) << 8),
				rune(e.Extension[1]) | (rune(e.Extension[2]) << 8),
				rune(e.CreatedTime),
				rune(e.CreatedDate),
				rune(e.LastAccessDate),
				rune(e.FirstClusterHigh),
				rune(e.LastModifiedTime),
				rune(e.LastModifiedDate),
				rune(e.FileSize & 0xffff),
				rune(e.FileSize >> 16),
			}
			// 0xFFFF is the LFN spec's padding; a truncated chain or a
			// formatter that does not zero its tail leaves it as garbage.
			for i, r := range name {
				if r == 0 || r == 0xFFFF {
					name = name[:i]
					break
				}
			}
			vfatName = string(name) + vfatName
			vfatChecksum = e.CreatedTime10MS
		} else {
			// A deleted entry's first name byte is overwritten with 0xE5, which
			// invalidates the LFN checksum, so skip the check for those.
			if vfatName != "" && e.Name[0] != DeletedDirectoryEntryMarker && vfatChecksum != e.NameChecksum() {
				f.diag(DiagVFATName, "fat: invalid VFAT checksum for short name %q", strings.TrimSpace(string(e.Name[:])))
				vfatName = ""
			}
			shortName := strings.TrimSpace(string(e.Name[:]))
			ext := strings.TrimSpace(string(e.Extension[:]))
			if ext != "" {
				shortName += "." + ext
			}
			name := shortName
			if vfatName != "" {
				name = vfatName
			}
			firstCluster := uint32(e.FirstCluster)
			if f.Type == TypeFAT32 {
				firstCluster |= uint32(e.FirstClusterHigh) << 16
			}
			// CreatedTime10MS counts 10ms units up to 199, so values over 99 carry
			// an extra whole second.
			ct10msNanos := (int(e.CreatedTime10MS) % 100) * int(10*time.Millisecond)
			ct10msSec := int(e.CreatedTime10MS) / 100
			dir = append(dir, &DirectoryEntry{
				Name:         name,
				ShortName:    shortName,
				Deleted:      e.Name[0] == DeletedDirectoryEntryMarker,
				ReadOnly:     e.Attribute&DirectoryEntryAttributeReadOnly != 0,
				Hidden:       e.Attribute&DirectoryEntryAttributeHidden != 0,
				System:       e.Attribute&DirectoryEntryAttributeSystem != 0,
				VolumeLabel:  e.Attribute&DirectoryEntryAttributeVolumeLabel != 0,
				Subdirectory: e.Attribute&DirectoryEntryAttributeSubdirectory != 0,
				Archive:      e.Attribute&DirectoryEntryAttributeArchive != 0,
				FileSize:     e.FileSize,
				FirstCluster: firstCluster,
				Created:      decodeFATDateTime(e.CreatedDate, e.CreatedTime, ct10msSec, ct10msNanos),
				LastModified: decodeFATDateTime(e.LastModifiedDate, e.LastModifiedTime, 0, 0),
				LastAccessed: decodeFATDate(e.LastAccessDate),
			})
			vfatName = ""
		}
	}
	return dir
}

// inRanges reports whether off falls in one of the half-open ranges, which are
// in increasing order and do not overlap.
func inRanges(ranges []ByteRange, off int) bool {
	for _, r := range ranges {
		if off < r.Start {
			return false
		}
		if off < r.End {
			return true
		}
	}
	return false
}

// decodeFATDateTime decodes a FAT date+time pair, returning the zero time when
// the date is absent or has a zero month.
func decodeFATDateTime(date, t uint16, extraSec, nanos int) time.Time {
	month := time.Month((date >> 5) & 0xf)
	if month == 0 {
		return time.Time{}
	}
	return time.Date(
		int(date>>9)+1980,
		month,
		int(date)&0x1f,
		int(t>>11),
		int(t>>5)&0x3f,
		(int(t)&0x1f)*2+extraSec,
		nanos,
		time.Local)
}

// decodeFATDate decodes a FAT date-only field, returning the zero time when the
// date is absent or has a zero month.
func decodeFATDate(date uint16) time.Time {
	return decodeFATDateTime(date, 0, 0, 0)
}

// ErrNotFound reports that the volume holds no readable file at the requested
// path: nothing by that name, or a directory where a file was wanted. The
// caller asked for something that is not there, which is a different finding,
// with a different exit code, from a directory or cluster chain that would not
// read.
var ErrNotFound = errors.New("fat: not found")

// ErrNotFAT reports that the bytes at the offset are not a FAT boot sector, as
// opposed to an I/O failure reaching them. A caller probing an image has to
// tell those apart: on damaged media "this is not FAT" and "the boot sector was
// never recovered" are different answers, and reporting the second as the first
// sends an operator looking for the wrong problem.
var ErrNotFAT = errors.New("fat: not a FAT boot sector")

func (f *FAT) Open(path string) (*Reader, error) {
	trimmed := strings.Trim(path, "/")
	if trimmed == "" {
		return nil, errors.New("fat: empty path to open")
	}
	p := strings.Split(trimmed, "/")
	return f.open(p, 0)
}

func (f *FAT) open(path []string, cluster uint32) (*Reader, error) {
	// A partially readable directory still lists the entries that were
	// recovered; only report the read failure if the name is not among them.
	dir, readErr := f.ReadDirectory(cluster, true)
	if readErr != nil && len(dir) == 0 {
		return nil, fmt.Errorf("fat: failed to read directory %q: %w", path[0], readErr)
	}
	for _, e := range dir {
		if !e.VolumeLabel && strings.EqualFold(e.Name, path[0]) {
			if len(path) == 1 {
				if e.Subdirectory {
					return nil, fmt.Errorf("fat: %q is a subdirectory: %w", path[0], ErrNotFound)
				}
				return f.OpenClusterChain(e.FirstCluster, int64(e.FileSize))
			} else {
				if !e.Subdirectory {
					return nil, fmt.Errorf("fat: %q is not a directory: %w", path[0], ErrNotFound)
				}
				return f.open(path[1:], e.FirstCluster)
			}
		}
	}
	if readErr != nil {
		return nil, fmt.Errorf("fat: path %q not found in the readable part of the directory: %w", path[0], readErr)
	}
	return nil, fmt.Errorf("fat: path %q: %w", path[0], ErrNotFound)
}

// ValidateDirectoryTree reads every file in the volume and returns one issue
// per file or directory that could not be read. The whole tree is walked: a
// failure is recorded and traversal continues.
func (f *FAT) ValidateDirectoryTree() ([]WalkIssue, error) {
	var fileIssues []WalkIssue
	issues, err := f.Walk(func(path string, e *DirectoryEntry) error {
		if e.Subdirectory || e.FirstCluster == 0 {
			return nil
		}
		r, err := f.OpenClusterChain(e.FirstCluster, int64(e.FileSize))
		if err != nil {
			fileIssues = append(fileIssues, WalkIssue{Path: path, Err: err})
			return nil //nolint:nilerr // collect-and-continue: the failure is recorded, the walk goes on
		}
		if _, err := io.Copy(io.Discard, r); err != nil {
			fileIssues = append(fileIssues, WalkIssue{Path: path, Err: err})
		}
		return nil
	})
	return append(issues, fileIssues...), err
}

// WalkFunc is called by Walk for each file and directory. path is slash-separated
// from the volume root. Returning a non-nil error stops the walk.
type WalkFunc func(path string, entry *DirectoryEntry) error

// WalkIssue records something the walk found in a directory: a failure to read
// or traverse it, or — with Diag set — a non-fatal problem noticed while
// reading it. The rest of the tree is still walked either way.
type WalkIssue struct {
	Path string
	Err  error
	// Diag is true for a diagnostic rather than a failure. The walk drains
	// them per directory because Diagnostics() clears the list: left pending,
	// a directory's problems surface under whichever file is read next and are
	// reported against the wrong path.
	Diag bool
}

func (w WalkIssue) String() string {
	path := w.Path
	if path == "" {
		path = "/"
	}
	return fmt.Sprintf("%s: %s", path, w.Err)
}

// Walk traverses the directory tree, calling fn for every file and directory
// before descending into it. Entries named ".", ".." and volume labels are
// skipped. A directory that cannot be read, or that loops back on itself, is
// recorded in the returned issues and the walk continues with the rest of the
// tree; a partially readable directory still yields the entries recovered from
// it. The error is non-nil only when fn asked to stop.
func (f *FAT) Walk(fn WalkFunc) ([]WalkIssue, error) {
	w := &walker{f: f, fn: fn, visited: make(map[uint32]struct{})}
	err := w.walk("", 0)
	return w.issues, err
}

type walker struct {
	f       *FAT
	fn      WalkFunc
	visited map[uint32]struct{}
	issues  []WalkIssue
}

func (w *walker) walk(path string, cluster uint32) error {
	if _, ok := w.visited[cluster]; ok {
		w.issues = append(w.issues, WalkIssue{
			Path: path,
			Err:  fmt.Errorf("fat: directory loop detected at cluster %d", cluster),
		})
		return nil
	}
	w.visited[cluster] = struct{}{}
	dir, err := w.f.ReadDirectory(cluster, true)
	// Drained here, against the directory that produced them, and before fn is
	// called for the first entry.
	for _, d := range w.f.Diagnostics() {
		w.issues = append(w.issues, WalkIssue{Path: path, Err: errors.New(d.Message), Diag: true})
	}
	if err != nil {
		w.issues = append(w.issues, WalkIssue{Path: path, Err: err})
		if len(dir) == 0 {
			return nil
		}
		// Keep going with the entries recovered from the readable part.
	}
	for _, e := range dir {
		if e.VolumeLabel || e.Name == "." || e.Name == ".." {
			continue
		}
		entryPath := e.Name
		if path != "" {
			entryPath = path + "/" + e.Name
		}
		if err := w.fn(entryPath, e); err != nil {
			return err
		}
		if e.Subdirectory {
			if err := w.walk(entryPath, e.FirstCluster); err != nil {
				return err
			}
		}
	}
	return nil
}

// OpenContiguousChain returns a Reader over count clusters starting at
// startCluster, read sequentially without consulting the FAT, for files whose
// chain is broken but whose data is still contiguous on disk. size limits the
// bytes returned; pass 0 for the full count×ClusterSize.
func (f *FAT) OpenContiguousChain(startCluster, count uint32, size int64) (*Reader, error) {
	if startCluster < 2 {
		return nil, fmt.Errorf("fat: cluster %d is reserved", startCluster)
	}
	if count == 0 {
		return nil, errors.New("fat: count must be greater than zero")
	}
	clusters := make([]uint32, count)
	for i := range count {
		clusters[i] = startCluster + i
	}
	totalSize := int64(count) * int64(f.ClusterSize)
	if size > 0 && totalSize < size {
		return nil, fmt.Errorf("fat: %d contiguous clusters from %d is only %d bytes, less than requested %d",
			count, startCluster, totalSize, size)
	}
	if size <= 0 || size > totalSize {
		size = totalSize
	}
	return &Reader{
		f:        f,
		size:     size,
		total:    size,
		clusters: clusters,
		buf:      make([]byte, f.ClusterSize),
	}, nil
}

// AllocationSummary describes how the data clusters in the volume are used.
type AllocationSummary struct {
	Total     uint32 // all data clusters (cluster 2 and above)
	Free      uint32 // FAT entry == TableEntryFree
	Bad       uint32 // FAT entry == TableEntryBadSector
	Reachable uint32 // allocated and reachable from the directory tree
	Orphaned  uint32 // allocated but not reachable from the directory tree
}

// AllocationSummary scans the FAT table and directory tree to report how every
// data cluster is used.
func (f *FAT) AllocationSummary() (AllocationSummary, error) {
	if len(f.Tables) == 0 {
		return AllocationSummary{}, errors.New("fat: no FAT tables loaded")
	}
	reachable, err := f.buildReachableSet()
	if err != nil {
		return AllocationSummary{}, err
	}
	// The FAT is a whole number of sectors, so it has entries past the last
	// real cluster. Counting those reports up to SectorSize/2-1 free clusters
	// the volume does not have.
	last := f.lastDataCluster()
	var s AllocationSummary
	for i, e := range f.Tables[0].Entries {
		if i < 2 {
			continue
		}
		if int64(i) > last {
			break
		}
		s.Total++
		switch e {
		case TableEntryFree:
			s.Free++
		case TableEntryBadSector:
			s.Bad++
		default:
			if _, ok := reachable[uint32(i)]; ok {
				s.Reachable++
			} else {
				s.Orphaned++
			}
		}
	}
	return s, nil
}

// FATConflict describes a single cluster where the two FAT table copies disagree.
type FATConflict struct {
	Cluster uint32
	Table0  TableEntry
	Table1  TableEntry
	// Mergeable is true when one side is free (zero), leaving the other as the
	// reliable value in Resolved.
	Mergeable bool
	Resolved  TableEntry
}

// AnalyzeFATConflicts compares the two FAT table copies and returns one
// FATConflict per differing cluster. It fails if only one copy was loaded.
func (f *FAT) AnalyzeFATConflicts() ([]FATConflict, error) {
	if len(f.Tables) < 2 {
		return nil, errors.New("fat: fewer than 2 FAT tables loaded")
	}
	t0, t1 := f.Tables[0], f.Tables[1]
	n := min(t1.Len(), t0.Len())
	var conflicts []FATConflict
	for i := range n {
		e0, e1 := t0.Entries[i], t1.Entries[i]
		if e0 == e1 {
			continue
		}
		c := FATConflict{Cluster: uint32(i), Table0: e0, Table1: e1}
		switch {
		case e0 == TableEntryFree:
			c.Mergeable = true
			c.Resolved = e1
		case e1 == TableEntryFree:
			c.Mergeable = true
			c.Resolved = e0
		}
		conflicts = append(conflicts, c)
	}
	return conflicts, nil
}

// EntryIssueKind classifies a problem found in a directory entry.
type EntryIssueKind string

const (
	// IssueClusterFreed means the entry's first cluster is marked free in the FAT.
	IssueClusterFreed EntryIssueKind = "cluster_freed"
	// IssueChainCorrupt means the cluster chain loops or leaves the volume.
	IssueChainCorrupt EntryIssueKind = "chain_corrupt"
	// IssueSizeMismatch means the chain length disagrees with the declared file size.
	IssueSizeMismatch EntryIssueKind = "size_mismatch"
)

// EntryIssue describes a directory entry whose cluster allocation disagrees
// with the FAT.
type EntryIssue struct {
	Path      string
	Entry     *DirectoryEntry
	IssueKind EntryIssueKind
	Detail    string
}

// CheckDirectoryEntries walks the reachable directory tree and returns an
// EntryIssue for every file entry whose cluster allocation disagrees with the FAT.
func (f *FAT) CheckDirectoryEntries() ([]EntryIssue, error) {
	if len(f.Tables) == 0 {
		return nil, errors.New("fat: no FAT tables loaded")
	}
	var issues []EntryIssue
	// Walk collects its own directory issues; the caller reports those once, so
	// they are not folded into the entry issues here.
	_, err := f.Walk(func(path string, e *DirectoryEntry) error {
		if e.Subdirectory || e.FirstCluster == 0 {
			return nil
		}
		if int(e.FirstCluster) >= f.Tables[0].Len() {
			issues = append(issues, EntryIssue{
				Path:      path,
				Entry:     e,
				IssueKind: IssueChainCorrupt,
				Detail:    fmt.Sprintf("first cluster %d past end of FAT (%d entries)", e.FirstCluster, f.Tables[0].Len()),
			})
			return nil
		}
		if f.Tables[0].Entries[e.FirstCluster] == TableEntryFree {
			issues = append(issues, EntryIssue{
				Path:      path,
				Entry:     e,
				IssueKind: IssueClusterFreed,
				Detail:    fmt.Sprintf("cluster %d is marked free in FAT", e.FirstCluster),
			})
			return nil
		}
		clusters, _, chainErr := f.Tables[0].Cluster(e.FirstCluster)
		if chainErr != nil {
			issues = append(issues, EntryIssue{
				Path:      path,
				Entry:     e,
				IssueKind: IssueChainCorrupt,
				Detail:    chainErr.Error(),
			})
			return nil //nolint:nilerr // collect-and-continue: the chain failure is recorded as an issue
		}
		if e.FileSize > 0 {
			expected := (int64(e.FileSize) + int64(f.ClusterSize) - 1) / int64(f.ClusterSize)
			if int64(len(clusters)) != expected {
				issues = append(issues, EntryIssue{
					Path:      path,
					Entry:     e,
					IssueKind: IssueSizeMismatch,
					Detail: fmt.Sprintf("size %d bytes needs %d clusters but chain has %d",
						e.FileSize, expected, len(clusters)),
				})
			}
		}
		return nil
	})
	return issues, err
}

// OrphanChain describes a cluster chain allocated in the FAT but not reachable
// from any directory entry in the directory tree.
type OrphanChain struct {
	StartCluster uint32
	Clusters     []uint32
	// Size is the raw on-disk byte count, len(Clusters) × ClusterSize.
	Size int64
	// LooksLikeDir is true when the chain parsed as a directory containing ".";
	// Entries is then populated.
	LooksLikeDir bool
	Entries      []*DirectoryEntry
}

// FindOrphans returns the cluster chains that are allocated in the FAT but not
// reachable from the root directory. Orphans that parse as directories are
// followed, so chains reachable only from another orphan are also returned.
func (f *FAT) FindOrphans() ([]*OrphanChain, error) {
	if len(f.Tables) == 0 {
		return nil, errors.New("fat: no FAT tables loaded")
	}

	// Step 1: mark all clusters reachable from the directory tree.
	reachable, err := f.buildReachableSet()
	if err != nil {
		return nil, err
	}

	t := f.Tables[0]

	// Build set of clusters pointed to by some FAT entry (they're mid-chain,
	// not heads, so we skip them when looking for orphan starts).
	// Entries 0 and 1 are reserved and hold the media descriptor and volume
	// flags, so their values are not cluster pointers.
	pointed := make(map[uint32]struct{}, t.Len())
	for i, e := range t.Entries {
		if i >= 2 && e >= 2 && e <= 0x0FFFFFEF {
			pointed[uint32(e)] = struct{}{}
		}
	}

	// Step 2: collect orphan chain heads.
	orphanClusters := make(map[uint32]struct{})
	var orphans []*OrphanChain

	// The entries past the last real cluster are the table's own slack, not
	// clusters: a chain running into them has no data region to read, and
	// counting them inflates the orphan's size with bytes the volume does not
	// have. AllocationSummary is bounded the same way, so the two agree on
	// what "orphaned" adds up to.
	last := f.lastDataCluster()

	addOrphan := func(startCluster uint32) {
		clusters, _, _ := t.Cluster(startCluster)
		for i, cl := range clusters {
			if int64(cl) > last {
				clusters = clusters[:i]
				break
			}
		}
		if len(clusters) == 0 {
			return
		}
		for _, cl := range clusters {
			orphanClusters[cl] = struct{}{}
		}
		orphans = append(orphans, &OrphanChain{
			StartCluster: startCluster,
			Clusters:     clusters,
			Size:         int64(len(clusters)) * int64(f.ClusterSize),
		})
	}

	for i := 2; i < t.Len() && int64(i) <= last; i++ {
		c := uint32(i)
		if _, ok := reachable[c]; ok {
			continue
		}
		if _, ok := pointed[c]; ok {
			continue
		}
		if !t.Readable(c) {
			continue // unknown entry: not evidence of an orphan chain
		}
		e := t.Entries[c]
		if e == TableEntryFree || e == TableEntryBadSector || e == TableEntryReserved1 {
			continue
		}
		addOrphan(c)
	}

	// Step 3: explore orphan chains that look like directories, discovering
	// further orphans they reference. Iterate until no new orphan is added.
	//
	// examined keeps this from being O(passes × chains). Without it, every pass
	// re-reads and re-parses every chain that has not turned out to be a
	// directory, which on damaged media spends the drive's retry budget again
	// each time and appends the parse's diagnostics again.
	examined := make(map[uint32]struct{}, len(orphans))
	for changed := true; changed; {
		changed = false
		for _, o := range orphans {
			if o.LooksLikeDir {
				continue
			}
			if _, done := examined[o.StartCluster]; done {
				continue
			}
			examined[o.StartCluster] = struct{}{}
			data, err := f.readChainData(o.Clusters)
			if err != nil {
				continue
			}
			entries := f.tryParseAsDirectory(data)
			if len(entries) == 0 {
				continue
			}
			var hasDot bool
			for _, e := range entries {
				if e.ShortName == "." {
					hasDot = true
					break
				}
			}
			if !hasDot {
				continue
			}
			o.LooksLikeDir = true
			o.Entries = entries

			for _, e := range entries {
				if e.FirstCluster < 2 || e.Name == "." || e.Name == ".." {
					continue
				}
				if _, ok := reachable[e.FirstCluster]; ok {
					continue
				}
				if _, ok := orphanClusters[e.FirstCluster]; ok {
					continue
				}
				addOrphan(e.FirstCluster)
				changed = true
			}
		}
	}

	return orphans, nil
}

// buildReachableSet returns every cluster reachable from the root directory.
func (f *FAT) buildReachableSet() (map[uint32]struct{}, error) {
	reachable := make(map[uint32]struct{})
	if f.Type == TypeFAT32 && f.RootCluster >= 2 {
		f.markChainReachable(reachable, f.RootCluster)
	}
	_, err := f.Walk(func(_ string, e *DirectoryEntry) error {
		if e.FirstCluster >= 2 {
			f.markChainReachable(reachable, e.FirstCluster)
		}
		return nil
	})
	return reachable, err
}

// markChainReachable adds every cluster of startCluster's chain to reachable.
func (f *FAT) markChainReachable(reachable map[uint32]struct{}, startCluster uint32) {
	if len(f.Tables) == 0 || startCluster < 2 {
		return
	}
	t := f.Tables[0]
	for c := startCluster; ; {
		if _, ok := reachable[c]; ok {
			return
		}
		reachable[c] = struct{}{}
		if int(c) >= t.Len() || !t.Readable(c) {
			return
		}
		e := t.Entries[c]
		if e < 2 || e > 0x0FFFFFEF {
			return
		}
		c = uint32(e)
	}
}

// readChainData reads the listed clusters into one buffer.
func (f *FAT) readChainData(clusters []uint32) ([]byte, error) {
	if len(clusters) == 0 {
		return nil, nil
	}
	// readDirectoryData and ReadClusterChain bound their chains already; this
	// path has no other guard, and the buffer is one allocation of the whole
	// chain, so it cannot be allowed to skip the same check.
	if err := f.checkChainFits(clusters[0], len(clusters)); err != nil {
		return nil, err
	}
	buf := make([]byte, len(clusters)*f.ClusterSize)
	for i, c := range clusters {
		off, err := f.ClusterOffset(c)
		if err != nil {
			return nil, err
		}
		if _, err := f.readAt(off, buf[i*f.ClusterSize:(i+1)*f.ClusterSize]); err != nil {
			return nil, err
		}
	}
	return buf, nil
}

// tryParseAsDirectory interprets raw bytes as a FAT directory, returning nil if
// they do not look like one.
func (f *FAT) tryParseAsDirectory(data []byte) []*DirectoryEntry {
	if len(data) < DirEntrySize || data[0] == 0 {
		return nil
	}
	nEntries := len(data) / DirEntrySize
	rawDir := make([]RawDirectoryEntry, nEntries)
	if err := binary.Read(bytes.NewReader(data), binary.LittleEndian, &rawDir); err != nil {
		return nil
	}
	return f.processRawDirectory(rawDir, nil, true)
}

func readBootSector(r io.Reader) (*BootSector, [512]byte, error) {
	var raw [512]byte
	if _, err := io.ReadFull(r, raw[:]); err != nil {
		return nil, raw, err
	}
	var bs BootSector
	if err := binary.Read(bytes.NewReader(raw[:]), binary.LittleEndian, &bs); err != nil {
		return nil, raw, err
	}
	if bs.EndOfSectorMarker[0] != 0x55 || bs.EndOfSectorMarker[1] != 0xaa {
		return &bs, raw, fmt.Errorf("%w: invalid end of sector marker in boot sector", ErrNotFAT)
	}
	// FAT32 puts the signature at offset 0x42 rather than 0x26, so skip this
	// check when SectorsPerFAT is 0.
	if bs.BIOSParameterBlock.SectorsPerFAT != 0 && bs.BIOSParameterBlock.Signature != 0x28 && bs.BIOSParameterBlock.Signature != 0x29 {
		return &bs, raw, fmt.Errorf("%w: invalid signature 0x%02x in parameter block", ErrNotFAT, bs.BIOSParameterBlock.Signature)
	}
	return &bs, raw, nil
}

// readFSInfo reads and validates the FAT32 FS Information Sector.
func readFSInfo(r io.ReadSeeker, offset int64) (*FSInfo, error) {
	if _, err := r.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}
	var raw [512]byte
	if _, err := io.ReadFull(r, raw[:]); err != nil {
		return nil, err
	}
	sig1 := binary.LittleEndian.Uint32(raw[0:4])
	sig2 := binary.LittleEndian.Uint32(raw[484:488])
	if sig1 != 0x41615252 || sig2 != 0x61417272 {
		return nil, fmt.Errorf("fat: FSInfo sector has invalid signatures (0x%08X, 0x%08X)", sig1, sig2)
	}
	return &FSInfo{
		FreeCount: binary.LittleEndian.Uint32(raw[488:492]),
		NextFree:  binary.LittleEndian.Uint32(raw[492:496]),
	}, nil
}
