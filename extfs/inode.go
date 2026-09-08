package extfs

import (
	"encoding/binary"
	"fmt"
	"math"
	"time"
)

// RootInodeNum is the inode number of the root directory (always 2 in ext2).
const RootInodeNum = 2

// inodeRawMinSize is the minimum on-disk inode size for standard ext2.
const inodeRawMinSize = 128

// File type bits in i_mode.
const (
	ModeIFMT   = 0xF000 // mask for file type
	ModeIFSOCK = 0xC000 // socket
	ModeIFLNK  = 0xA000 // symbolic link
	ModeIFREG  = 0x8000 // regular file
	ModeIFBLK  = 0x6000 // block device
	ModeIFDIR  = 0x4000 // directory
	ModeIFCHR  = 0x2000 // character device
	ModeIFIFO  = 0x1000 // FIFO / named pipe
)

// Inode flag bits (i_flags).
const (
	InodeFlagExtents = 0x00080000 // ext4 extent tree (not supported)
	InodeFlagInline  = 0x10000000 // ext4 inline data (not supported)
	InodeFlagDirIdx  = 0x00001000 // htree directory index
)

// Inode holds the parsed ext2 inode fields.
type Inode struct {
	Mode       uint16
	UID        uint16
	GID        uint16
	Size       int64 // file size in bytes (i_size | i_size_high<<32 when LARGE_FILE)
	ATime      time.Time
	CTime      time.Time
	MTime      time.Time
	DTime      time.Time // non-zero means the inode was deleted
	LinksCount uint16
	Blocks     uint32 // allocated blocks in 512-byte units (not filesystem blocks)
	Flags      uint32
	Block      [15]uint32 // block pointers: [0..11]=direct, [12]=indir, [13]=dindir, [14]=tindir
	Generation uint32
	FileACL    uint32
}

// IsDir reports whether the inode is a directory.
func (ino *Inode) IsDir() bool { return ino.Mode&ModeIFMT == ModeIFDIR }

// IsRegular reports whether the inode is a regular file.
func (ino *Inode) IsRegular() bool { return ino.Mode&ModeIFMT == ModeIFREG }

// IsSymlink reports whether the inode is a symbolic link.
func (ino *Inode) IsSymlink() bool { return ino.Mode&ModeIFMT == ModeIFLNK }

// Deleted reports whether the inode has a deletion time, or no links but a
// non-zero mode showing the slot was once in use.
func (ino *Inode) Deleted() bool {
	return !ino.DTime.IsZero() || (ino.LinksCount == 0 && ino.Mode != 0)
}

func parseInode(buf []byte, largeFile bool) (*Inode, error) {
	if len(buf) < inodeRawMinSize {
		return nil, fmt.Errorf("extfs: inode buffer too small (%d bytes)", len(buf))
	}
	mode := binary.LittleEndian.Uint16(buf[0:2])
	size := int64(binary.LittleEndian.Uint32(buf[4:8]))
	// Offset 108 is i_size_high only for regular files; on a directory the same
	// field is genuinely i_dir_acl, and treating it as a size gives a bogus
	// multi-GB directory.
	if largeFile && mode&ModeIFMT == ModeIFREG {
		// Assembled unsigned: a high word with bit 31 set would otherwise shift
		// into the sign bit and yield a negative size, which every caller would
		// then report or do arithmetic on as if it were real.
		full := uint64(binary.LittleEndian.Uint32(buf[108:112]))<<32 | uint64(binary.LittleEndian.Uint32(buf[4:8]))
		if full > math.MaxInt64 {
			return nil, fmt.Errorf("extfs: inode declares a size of %d bytes, which is not representable", full)
		}
		size = int64(full)
	}
	ino := &Inode{
		Mode:       mode,
		UID:        binary.LittleEndian.Uint16(buf[2:4]),
		Size:       size,
		ATime:      unixSec(binary.LittleEndian.Uint32(buf[8:12])),
		CTime:      unixSec(binary.LittleEndian.Uint32(buf[12:16])),
		MTime:      unixSec(binary.LittleEndian.Uint32(buf[16:20])),
		DTime:      unixSec(binary.LittleEndian.Uint32(buf[20:24])),
		GID:        binary.LittleEndian.Uint16(buf[24:26]),
		LinksCount: binary.LittleEndian.Uint16(buf[26:28]),
		Blocks:     binary.LittleEndian.Uint32(buf[28:32]),
		Flags:      binary.LittleEndian.Uint32(buf[32:36]),
		Generation: binary.LittleEndian.Uint32(buf[100:104]),
		FileACL:    binary.LittleEndian.Uint32(buf[104:108]),
	}
	for i := range 15 {
		ino.Block[i] = binary.LittleEndian.Uint32(buf[40+i*4 : 44+i*4])
	}
	return ino, nil
}

func unixSec(t uint32) time.Time {
	if t == 0 {
		return time.Time{}
	}
	return time.Unix(int64(t), 0)
}
