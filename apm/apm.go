package apm

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
)

// ErrNotAPM reports that no Apple partition map was found, as opposed to an I/O
// failure reaching one. A caller probing an image has to tell those apart: on
// damaged media "this disk has no map" and "the block holding the map was never
// recovered" are different answers.
var ErrNotAPM = errors.New("apm: no Apple partition map found")

// Signatures. The driver descriptor record opens block 0 and every map entry
// opens with PM; TS is the pre-1987 spelling a few early third-party and A/UX
// disks carry, with the same entry layout.
var (
	DriverSignature    = [2]byte{'E', 'R'}
	PartitionSignature = [2]byte{'P', 'M'}
	OldSignature       = [2]byte{'T', 'S'}
)

// EntrySize is the size of one partition map entry. The map is a run of these
// beginning at block 1, one per device block.
const EntrySize = 512

// maxDrivers is how many 8-byte driver descriptors fit in block 0 after its
// 18-byte header: (512-18)/8, with six bytes left over.
const maxDrivers = 61

// maxEntries bounds the walk. No real map comes near it: the largest met here
// reserves 80 blocks and uses six.
const maxEntries = 256

// maxProbeAfterLoss is how far past an unreadable block the walk keeps looking
// when no entry has read yet and so nothing has declared a count.
const maxProbeAfterLoss = 8

// blockSizeCandidates are the strides a map is found at, in probe order.
var blockSizeCandidates = []int{512, 1024, 2048, 4096}

// DriverDescriptor is one entry of the driver table in block 0.
type DriverDescriptor struct {
	Block uint32 // ddBlock, in device blocks
	Size  uint16 // ddSize, in 512-byte blocks rather than device blocks
	Type  uint16 // ddType: 1 for a Mac driver, 0xFFFF for A/UX
}

// DDR is the driver descriptor record in block 0. It is defined to live in the
// first 512 bytes whatever BlockSize says, so it is read at that width.
type DDR struct {
	Signature   [2]byte // sbSig, 'ER'
	BlockSize   uint16  // sbBlkSize
	BlockCount  uint32  // sbBlkCount, the device length in BlockSize blocks
	DeviceType  uint16  // sbDevType
	DeviceID    uint16  // sbDevId
	Data        uint32  // sbData
	DriverCount uint16  // sbDrvrCount
	Drivers     [maxDrivers]DriverDescriptor
	_           [6]byte
}

// DriverList returns the descriptors the record declares, clamped to the table.
// Reading past DriverCount invents drivers out of whatever follows: one disk
// here carries a well-formed-looking descriptor past its declared count of one.
func (d *DDR) DriverList() []DriverDescriptor {
	n := min(int(d.DriverCount), len(d.Drivers))
	return d.Drivers[:n]
}

// Partition is one 512-byte partition map entry.
type Partition struct {
	Signature      [2]byte // pmSig
	SigPad         uint16  // pmSigPad
	MapBlockCount  uint32  // pmMapBlkCnt, how many entries the map holds
	PyPartStart    uint32  // pmPyPartStart, in device blocks from block 0
	PartBlockCount uint32  // pmPartBlkCnt
	Name           [32]byte
	Type           [32]byte
	LgDataStart    uint32
	DataCount      uint32
	Status         uint32
	LgBootStart    uint32
	BootSize       uint32
	BootAddr       uint32
	BootAddr2      uint32
	BootEntry      uint32
	BootEntry2     uint32
	BootChecksum   uint32
	Processor      [16]byte
	Pad            [188]uint16
}

// PartName is the partition's name, trimmed at the first NUL.
func (p *Partition) PartName() string { return trimNUL(p.Name[:]) }

// TypeName is pmParType, trimmed at the first NUL.
func (p *Partition) TypeName() string { return trimNUL(p.Type[:]) }

func trimNUL(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

// Partition type strings. Apple's own note spells the ProDOS one in capitals
// and third parties wrote it mixed, so every comparison here folds case.
const (
	TypeFree      = "Apple_Free"
	TypeMap       = "Apple_partition_map"
	TypeDriver    = "Apple_Driver"
	TypeDriver43  = "Apple_Driver43"
	TypeDriverATA = "Apple_Driver_ATA"
	TypePatches   = "Apple_Patches"
	TypeLoader    = "Apple_Loader"
	TypeHFS       = "Apple_HFS"
	TypeHFSX      = "Apple_HFSX"
	TypeMFS       = "Apple_MFS"
	TypeProDOS    = "Apple_PRODOS"
	TypeUNIX      = "Apple_UNIX_SVR2"
	TypeScratch   = "Apple_Scratch"
	TypeVoid      = "Apple_Void"
)

var typeNames = map[string]string{
	TypeFree:      "free space",
	TypeMap:       "the partition map",
	TypeDriver:    "a device driver",
	TypeDriver43:  "a SCSI Manager 4.3 driver",
	TypeDriverATA: "an ATA driver",
	TypePatches:   "a patch partition",
	TypeLoader:    "a loader partition",
	TypeHFS:       "Mac OS Standard (HFS)",
	TypeHFSX:      "Mac OS Extended (HFSX)",
	TypeMFS:       "Macintosh File System",
	TypeProDOS:    "ProDOS",
	TypeUNIX:      "A/UX",
	TypeScratch:   "scratch space",
	TypeVoid:      "unused",
}

// TypeDescription returns a readable name for a partition type. An
// unrecognized one is returned as itself: the string is the disk's own answer
// and says more than "unknown" would.
func TypeDescription(t string) string {
	for k, v := range typeNames {
		if strings.EqualFold(k, t) {
			return v
		}
	}
	if t == "" {
		return "unnamed"
	}
	return t
}

// IsHFS reports whether the type is an HFS volume type. Apple_HFS+ is not in
// Apple's list and appears on some hybrid CD masterings, which is why it is
// here; an HFS wrapper around an HFS+ volume is signalled in the volume itself
// and not by the partition type.
func IsHFS(t string) bool {
	return strings.EqualFold(t, TypeHFS) || strings.EqualFold(t, TypeHFSX) ||
		strings.EqualFold(t, "Apple_HFS+")
}

// IsFree reports whether the entry describes unallocated space.
func IsFree(t string) bool { return strings.EqualFold(t, TypeFree) }

// IsMap reports whether the entry describes the partition map itself.
func IsMap(t string) bool { return strings.EqualFold(t, TypeMap) }

// IsDriver reports whether the entry holds driver or patch code rather than a
// filesystem.
func IsDriver(t string) bool {
	return strings.HasPrefix(strings.ToLower(t), strings.ToLower(TypeDriver)) ||
		strings.EqualFold(t, TypePatches) || strings.EqualFold(t, TypeLoader)
}

// IsFilesystem reports whether a partition of this type could hold a
// filesystem.
//
// It is a negative test: only the types known to hold something else are
// excluded, and everything else — including a type this package has never heard
// of — is a candidate. The caller's own probe is the real authority, so a wrong
// guess here costs one probe while a wrong exclusion costs the volume. One disk
// in this archive carries Iomega_DiskInfo and Iomega_backup entries, and
// neither is in anybody's list.
func IsFilesystem(t string) bool {
	switch {
	case IsFree(t), IsMap(t), IsDriver(t):
		return false
	case strings.EqualFold(t, TypeScratch), strings.EqualFold(t, TypeVoid):
		return false
	case t == "":
		return false
	}
	return true
}

// Issue is a non-fatal finding about the map, returned alongside the entries
// that were recovered.
type Issue struct {
	// Block is the map block it concerns, 0 for one about the record in block 0
	// or about the map as a whole.
	Block int
	Err   error
}

func (i Issue) String() string {
	if i.Block == 0 {
		return i.Err.Error()
	}
	return fmt.Sprintf("map block %d: %s", i.Block, i.Err)
}

// Entry is one partition found on the disk.
type Entry struct {
	Partition
	// Index is the entry's 1-based position in the map, which is also the block
	// it lives in. It is the number -partition selects by, so what a listing
	// prints and what an operator types are the same.
	Index int
	// BlockSize is the stride the map was read at, so Offset and Size need
	// nothing else.
	BlockSize int
}

// Offset is the partition's byte offset from the front of the disk.
func (e *Entry) Offset() int64 { return int64(e.PyPartStart) * int64(e.BlockSize) }

// Size is the partition's length in bytes.
func (e *Entry) Size() int64 { return int64(e.PartBlockCount) * int64(e.BlockSize) }

// Unused reports whether the entry describes nothing worth looking inside.
func (e *Entry) Unused() bool { return e.PartBlockCount == 0 || IsFree(e.TypeName()) }

// Map is everything block 0 and the map blocks said.
type Map struct {
	DDR    DDR
	HasDDR bool
	// BlockSize is the stride the entries were found at.
	BlockSize int
	// Declared is pmMapBlkCnt as the entries give it, 0 when none read.
	Declared uint32
	Entries  []Entry
	Issues   []Issue
}

// Read parses the driver descriptor record in block 0, returning ErrNotAPM when
// the signature is absent. It mirrors mbr.Read.
func Read(r io.Reader) (*DDR, error) {
	var d DDR
	if err := binary.Read(r, binary.BigEndian, &d); err != nil {
		return nil, err
	}
	if d.Signature != DriverSignature {
		return &d, ErrNotAPM
	}
	return &d, nil
}

// ReadEntry parses one partition map entry, returning ErrNotAPM when neither
// signature is present.
func ReadEntry(r io.Reader) (*Partition, error) {
	var p Partition
	if err := binary.Read(r, binary.BigEndian, &p); err != nil {
		return nil, err
	}
	if p.Signature != PartitionSignature && p.Signature != OldSignature {
		return &p, ErrNotAPM
	}
	return &p, nil
}

// ReadMap parses the whole map. size is the image's length in bytes and bounds
// every offset derived from it; pass 0 when it is not known.
func ReadMap(r io.ReaderAt, size int64) (*Map, error) {
	m := &Map{}

	// Block 0 is read at 512 bytes whatever it then says the device block size
	// is, because that is where the record is defined to live.
	head := make([]byte, EntrySize)
	if _, err := r.ReadAt(head, 0); err != nil {
		// Block 0 was not recovered. That is different from "this disk has no
		// map", and it is the case a rescue map exists to report.
		return nil, fmt.Errorf("apm: reading block 0: %w", err)
	}
	if d, err := Read(bytes.NewReader(head)); err == nil {
		m.DDR, m.HasDDR = *d, true
	} else if errors.Is(err, ErrNotAPM) {
		// A map is readable without its driver descriptor record: every entry
		// carries its own start and count, and only the stride is unknown,
		// which the probe below recovers. A disk whose very first sector is the
		// one the drive could not read is exactly this case.
		m.addIssue(0, errors.New("no driver descriptor record in block 0; the block size was found by probing"))
	} else {
		return nil, err
	}

	bs, probeErr := m.findBlockSize(r, size)
	if bs == 0 {
		return nil, probeErr
	}
	m.BlockSize = bs
	m.walk(r, size)
	return m, nil
}

// findBlockSize settles the stride the map is written at.
//
// sbBlkSize is only a hint; the PM signature confirms the stride. The two
// disagree in the field: Apple's own CD formatter wrote a 2048-byte device block
// size with one entry per 2048-byte block, while several third-party hybrid
// masterings wrote a 512-strided map inside the same 2048-byte sectors. Probing
// for the signature settles it, and also finds the map on a disk whose block 0
// was lost.
func (m *Map) findBlockSize(r io.ReaderAt, size int64) (int, error) {
	candidates := make([]int, 0, len(blockSizeCandidates)+1)
	if m.HasDDR && plausibleBlockSize(int(m.DDR.BlockSize)) {
		candidates = append(candidates, int(m.DDR.BlockSize))
	} else if m.HasDDR {
		m.addIssue(0, fmt.Errorf("the driver descriptor record declares a block size of %d, which no device uses; probing instead",
			m.DDR.BlockSize))
	}
	for _, c := range blockSizeCandidates {
		if len(candidates) == 0 || c != candidates[0] {
			candidates = append(candidates, c)
		}
	}

	buf := make([]byte, EntrySize)
	var firstIOErr error
	for _, bs := range candidates {
		// Entry 1 has to fit inside the image before it is worth a read.
		if size > 0 && int64(2)*int64(bs) > size {
			continue
		}
		if _, err := r.ReadAt(buf, int64(bs)); err != nil {
			if firstIOErr == nil {
				firstIOErr = err
			}
			continue
		}
		if sig := [2]byte{buf[0], buf[1]}; sig != PartitionSignature && sig != OldSignature {
			continue
		}
		if m.HasDDR && bs != int(m.DDR.BlockSize) {
			m.addIssue(0, fmt.Errorf("the driver descriptor record declares a block size of %d but the map is written at %d",
				m.DDR.BlockSize, bs))
		}
		return bs, nil
	}
	if firstIOErr != nil {
		if !m.HasDDR {
			// Nothing has said this is an Apple disk: block 0 read and carried
			// no driver descriptor record, and the blocks that might have held
			// a map would not read. Returning ErrNotAPM lets a caller try the
			// other partition scheme. The I/O error is wrapped with it, so a
			// disk that really is Apple-formatted is not silently reported as
			// having no map.
			return 0, fmt.Errorf("%w: %w", ErrNotAPM, firstIOErr)
		}
		// The record says this is an Apple disk, so a map that will not read is
		// a finding about the medium rather than a disk of another kind.
		return 0, fmt.Errorf("apm: reading the partition map: %w", firstIOErr)
	}
	return 0, ErrNotAPM
}

func plausibleBlockSize(n int) bool {
	return slices.Contains(blockSizeCandidates, n)
}

// walk reads the run of entries beginning at block 1.
//
// A block that will not read loses only the partition it described: an entry is
// addressed by its position and carries its own start and count, so nothing in
// it is needed to find or read the next one. In an MBR extended chain, by
// contrast, a lost record hides everything after it.
func (m *Map) walk(r io.ReaderAt, size int64) {
	buf := make([]byte, EntrySize)
	lost := 0
	for i := 1; i <= maxEntries; i++ {
		off := int64(i) * int64(m.BlockSize)
		if size > 0 && off+EntrySize > size {
			break
		}
		if _, err := r.ReadAt(buf, off); err != nil {
			m.addIssue(i, err)
			lost++
			// Keep going while an entry that did read says more follow, or —
			// when entry 1 itself was lost and nothing has declared a count —
			// for a few blocks before concluding there is no map.
			if m.Declared > 0 && uint32(i) < m.Declared {
				continue
			}
			if m.Declared == 0 && lost <= maxProbeAfterLoss {
				continue
			}
			break
		}
		p, err := ReadEntry(bytes.NewReader(buf))
		if err != nil {
			// Not an entry: the map ends here. The reserved tail of the map
			// partition is whatever the medium's erase pattern is — zeros on
			// one disk in this archive, 0xFA on another — and neither is a
			// partition.
			break
		}
		if m.Declared == 0 {
			m.Declared = p.MapBlockCount
		}
		m.place(i, p, size)
	}
	if m.Declared > 0 && int(m.Declared) != len(m.Entries)+lost {
		m.addIssue(0, fmt.Errorf("the map declares %d entries and %d were read",
			m.Declared, len(m.Entries)))
	}
}

// place range-checks one decoded entry and keeps it.
func (m *Map) place(index int, p *Partition, size int64) {
	e := Entry{Partition: *p, Index: index, BlockSize: m.BlockSize}
	// uint32 times a block size of at most 4096 tops out around 17.6 TB, well
	// inside int64, so neither product can wrap; the sum still needs checking.
	off, span := e.Offset(), e.Size()
	switch {
	case p.PyPartStart == 0:
		// Block 0 is the driver descriptor record, so a partition claiming it
		// is corrupt.
		m.addIssue(index, errors.New("the entry starts at block 0, where the driver descriptor record is"))
		return
	case off+span < off:
		m.addIssue(index, fmt.Errorf("the entry describes %d bytes at offset %d, which overflows", span, off))
		return
	case size > 0 && off >= size:
		m.addIssue(index, fmt.Errorf("the entry starts at byte %d, past the end of the %d-byte image", off, size))
		return
	case size > 0 && off+span > size:
		// A partition running past the end of a truncated image is what a
		// partial rescue looks like, and its head is still readable, so the
		// entry is kept and the shortfall reported.
		m.addIssue(index, fmt.Errorf("the entry describes %d bytes at offset %d, %d past the end of the image",
			span, off, off+span-size))
	}
	m.Entries = append(m.Entries, e)
}

func (m *Map) addIssue(block int, err error) {
	m.Issues = append(m.Issues, Issue{Block: block, Err: err})
}

// Partitions returns every usable partition on the disk, the convenience
// wrapper matching mbr.Partitions. Entries describing free space or no blocks
// at all are left out; the map partition and the driver partitions are kept, so
// an entry's index is its position in the map rather than a number this package
// invented.
func Partitions(r io.ReaderAt, size int64) ([]Entry, error) {
	m, err := ReadMap(r, size)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(m.Entries))
	for _, e := range m.Entries {
		if e.Unused() {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}
