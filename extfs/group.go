package extfs

import (
	"encoding/binary"
	"fmt"
)

// GroupDescriptor holds the parsed block group descriptor.
type GroupDescriptor struct {
	BlockBitmap     uint32
	InodeBitmap     uint32
	InodeTable      uint32
	FreeBlocksCount uint16
	FreeInodesCount uint16
	UsedDirsCount   uint16
}

// groupDescriptorSize is the on-disk size of a group descriptor entry.
const groupDescriptorSize = 32

// parseGroupDescriptor decodes one descriptor. buf must hold at least the 18
// bytes the parsed fields occupy.
func parseGroupDescriptor(buf []byte) (GroupDescriptor, error) {
	const parsedBytes = 18
	if len(buf) < parsedBytes {
		return GroupDescriptor{}, fmt.Errorf("extfs: group descriptor buffer too small (%d bytes, need %d)", len(buf), parsedBytes)
	}
	return GroupDescriptor{
		BlockBitmap:     binary.LittleEndian.Uint32(buf[0:4]),
		InodeBitmap:     binary.LittleEndian.Uint32(buf[4:8]),
		InodeTable:      binary.LittleEndian.Uint32(buf[8:12]),
		FreeBlocksCount: binary.LittleEndian.Uint16(buf[12:14]),
		FreeInodesCount: binary.LittleEndian.Uint16(buf[14:16]),
		UsedDirsCount:   binary.LittleEndian.Uint16(buf[16:18]),
	}, nil
}

// GroupHasSuperBackup reports whether block group g holds a copy of the
// superblock and group descriptor table. With SPARSE_SUPER only groups 0, 1 and
// the powers of 3, 5 and 7 do; without it, every group does.
func GroupHasSuperBackup(g uint32, sparseSuperEnabled bool) bool {
	if g == 0 || g == 1 {
		return true
	}
	if !sparseSuperEnabled {
		return true
	}
	return isPowerOf(g, 3) || isPowerOf(g, 5) || isPowerOf(g, 7)
}

// isPowerOf returns true if n equals base^k for some integer k ≥ 1.
func isPowerOf(n, base uint32) bool {
	if base < 2 || n == 0 {
		return false
	}
	for n%base == 0 {
		n /= base
	}
	return n == 1
}
