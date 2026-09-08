package extfs

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// ErrNotExt2 reports that no ext2 superblock was found, as opposed to an I/O
// failure reaching one. A caller probing an image has to tell those apart: on
// damaged media "this is not ext2" and "the superblock was never recovered" are
// different answers, and reporting the second as the first sends an operator
// looking for the wrong problem.
var ErrNotExt2 = errors.New("extfs: no ext2 superblock found")

const (
	SuperblockMagic  = 0xEF53
	superblockOffset = 1024 // always at byte 1024 from FS start

	RevOriginal = 0
	RevDynamic  = 1 // variable inode size, feature flags

	StateClean  = 1
	StateErrors = 2

	// Compatible feature flags — safe to ignore for read-only access.
	FeatureCompatDirPrealloc  = 0x0001
	FeatureCompatImagicInodes = 0x0002
	FeatureCompatHasJournal   = 0x0004
	FeatureCompatExtAttr      = 0x0008
	FeatureCompatResizeIno    = 0x0010
	FeatureCompatDirIndex     = 0x0020 // htree directory index

	// Incompatible feature flags — must support to read correctly.
	FeatureIncompatCompression = 0x0001
	FeatureIncompatFiletype    = 0x0002 // dir entries carry file_type byte
	FeatureIncompatRecover     = 0x0004 // journal needs recovery
	FeatureIncompatJournalDev  = 0x0008 // external journal device
	FeatureIncompatMetaBG      = 0x0010 // meta block groups
	FeatureIncompatExtents     = 0x0040 // ext4 extent trees
	FeatureIncompat64Bit       = 0x0080 // ext4 64-bit block numbers
	FeatureIncompatFlexBG      = 0x0200 // flexible block groups

	// Read-only compatible feature flags — can mount read-only without support.
	FeatureROCompatSparseSuperblock = 0x0001
	FeatureROCompatLargeFile        = 0x0002
	FeatureROCompatBTreeDir         = 0x0004
	FeatureROCompatHugeFile         = 0x0008
	FeatureROCompatGDTChecksum      = 0x0010
	FeatureROCompatDirNlink         = 0x0020
	FeatureROCompatExtraIsize       = 0x0040
)

// supportedIncompat is the set of INCOMPAT flags this package handles. Any
// other bit fails Open rather than risk misreading the filesystem. Journal
// flags are accepted because journal replay is skipped by design.
const supportedIncompat = FeatureIncompatFiletype |
	FeatureIncompatRecover |
	FeatureIncompatJournalDev

// Superblock holds the ext2/ext3 superblock fields used for read-only access.
type Superblock struct {
	InodesCount     uint32
	BlocksCount     uint32
	FreeBlocksCount uint32
	FreeInodesCount uint32
	FirstDataBlock  uint32 // 0 for blockSize≥2KiB, 1 for 1KiB blocks
	LogBlockSize    uint32 // blockSize = 1024 << LogBlockSize
	BlocksPerGroup  uint32
	InodesPerGroup  uint32
	Mtime           uint32 // last mount time (unix)
	Wtime           uint32 // last write time (unix)
	State           uint16
	RevLevel        uint32
	InodeSize       uint16 // 128 for RevOriginal; s_inode_size for RevDynamic
	FeatureCompat   uint32
	FeatureIncompat uint32
	FeatureROCompat uint32
	UUID            [16]byte
	VolumeName      string
	BackupGroup     uint32 // which group this copy came from; 0 = primary
}

// unsupportedIncompatFlags returns the INCOMPAT bits that are set but not
// handled by this implementation.
func (sb *Superblock) unsupportedIncompatFlags() uint32 {
	return sb.FeatureIncompat &^ supportedIncompat
}

// parseSuperblock parses a 1024-byte superblock from buf.
func parseSuperblock(buf []byte) (*Superblock, error) {
	if len(buf) < 264 {
		return nil, fmt.Errorf("extfs: superblock buffer too small (%d bytes)", len(buf))
	}
	magic := binary.LittleEndian.Uint16(buf[56:58])
	if magic != SuperblockMagic {
		return nil, fmt.Errorf("%w: bad superblock magic 0x%04X (want 0x%04X)", ErrNotExt2, magic, SuperblockMagic)
	}
	sb := &Superblock{
		InodesCount:     binary.LittleEndian.Uint32(buf[0:4]),
		BlocksCount:     binary.LittleEndian.Uint32(buf[4:8]),
		FreeBlocksCount: binary.LittleEndian.Uint32(buf[12:16]),
		FreeInodesCount: binary.LittleEndian.Uint32(buf[16:20]),
		FirstDataBlock:  binary.LittleEndian.Uint32(buf[20:24]),
		LogBlockSize:    binary.LittleEndian.Uint32(buf[24:28]),
		BlocksPerGroup:  binary.LittleEndian.Uint32(buf[32:36]),
		InodesPerGroup:  binary.LittleEndian.Uint32(buf[40:44]),
		Mtime:           binary.LittleEndian.Uint32(buf[44:48]),
		Wtime:           binary.LittleEndian.Uint32(buf[48:52]),
		State:           binary.LittleEndian.Uint16(buf[58:60]),
		RevLevel:        binary.LittleEndian.Uint32(buf[76:80]),
		InodeSize:       128,
	}
	if sb.RevLevel >= RevDynamic {
		sb.InodeSize = binary.LittleEndian.Uint16(buf[88:90])
		sb.FeatureCompat = binary.LittleEndian.Uint32(buf[92:96])
		sb.FeatureIncompat = binary.LittleEndian.Uint32(buf[96:100])
		sb.FeatureROCompat = binary.LittleEndian.Uint32(buf[100:104])
		copy(sb.UUID[:], buf[104:120])
		nameEnd := 136
		for nameEnd > 120 && buf[nameEnd-1] == 0 {
			nameEnd--
		}
		sb.VolumeName = string(buf[120:nameEnd])
	}
	// blkSize is 1024 << LogBlockSize; ext2/3/4 cap the block size at 64 KiB.
	// Without this bound the shift overflows and every derived size is nonsense.
	if sb.LogBlockSize > 6 {
		return nil, fmt.Errorf("extfs: superblock has invalid LogBlockSize %d", sb.LogBlockSize)
	}
	if sb.BlocksPerGroup == 0 {
		return nil, errors.New("extfs: superblock has zero BlocksPerGroup")
	}
	if sb.InodesPerGroup == 0 {
		return nil, errors.New("extfs: superblock has zero InodesPerGroup")
	}
	if sb.InodeSize == 0 || sb.InodeSize > 1024 {
		return nil, fmt.Errorf("extfs: superblock has invalid InodeSize %d", sb.InodeSize)
	}
	return sb, nil
}
