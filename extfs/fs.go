package extfs

import (
	"errors"
	"fmt"
	"io"
	"sync"
)

// FS is the filesystem handle. All exported methods are safe for concurrent reads.
type FS struct {
	mu      sync.Mutex
	r       io.ReadSeeker
	off     int64 // byte offset of FS start within r (partition start)
	size    int64 // total size of r in bytes, 0 when unknown
	sb      *Superblock
	gds     []GroupDescriptor
	blkSize int
	// diags records non-fatal problems found while opening the filesystem, such
	// as falling back to a backup superblock or descriptor table. It is written
	// only from Open, before the handle has been given to anyone, so
	// Diagnostics is safe to call without taking mu.
	diags []string
}

// Diagnostics returns the non-fatal problems noticed while opening the
// filesystem. They are returned as data rather than logged, so a caller can
// render or serialize them. Nothing appends to them after Open returns.
func (fs *FS) Diagnostics() []string { return fs.diags }

// diag records a non-fatal finding. Only Open and the calls beneath it reach
// it, so the slice needs no lock; see the field comment.
func (fs *FS) diag(format string, args ...any) {
	fs.diags = append(fs.diags, fmt.Sprintf(format, args...))
}

// Open parses the ext2/ext3 filesystem at byte offset off within r; off is the
// partition start, 0 for a raw image. If the primary superblock is unreadable,
// the group-1 backups are tried. Open fails on an incompatible feature flag it
// does not implement; journal flags are accepted, since journal replay is
// skipped for read-only recovery.
func Open(r io.ReadSeeker, off int64) (*FS, error) {
	fs := &FS{r: r, off: off}
	// The image size bounds allocations sized from disk fields. A reader that
	// cannot report it leaves size zero and those checks are skipped.
	if end, err := r.Seek(0, io.SeekEnd); err == nil {
		fs.size = end
	}

	sb, primaryErr := fs.readSuperblockAt(superblockOffset)
	if primaryErr != nil {
		fs.diag("primary superblock unreadable (%s); trying backups", primaryErr)
		var backupErr error
		sb, backupErr = fs.tryBackupSuperblocks()
		if backupErr != nil {
			// Report the primary failure, because it distinguishes "this is not
			// ext2" from "the superblock was never recovered": a superblock
			// that read but held the wrong magic means the first, one that
			// would not read means the second, and a caller probing an image
			// acts on the difference. The backup sweep's summary says only that
			// nothing was found, which would make every offset holding some
			// other filesystem look like damage.
			return nil, primaryErr
		}
	}

	if unsupported := sb.unsupportedIncompatFlags(); unsupported != 0 {
		return nil, fmt.Errorf("extfs: unsupported incompatible feature flags: 0x%08X", unsupported)
	}
	if sb.FeatureIncompat&FeatureIncompatRecover != 0 {
		fs.diag("filesystem has the needs_recovery flag; journal replay is skipped in read-only recovery mode")
	}

	fs.sb = sb
	fs.blkSize = 1024 << sb.LogBlockSize

	if err := fs.readGroupDescriptors(); err != nil {
		return nil, fmt.Errorf("extfs: read block group descriptors: %w", err)
	}
	return fs, nil
}

// readSuperblockAt reads and parses the superblock at byteOffset from the FS start.
func (fs *FS) readSuperblockAt(byteOffset int64) (*Superblock, error) {
	buf := make([]byte, 1024)
	if err := fs.readAt(byteOffset, buf); err != nil {
		return nil, err
	}
	return parseSuperblock(buf)
}

// geometry is one candidate filesystem layout used to locate backup copies
// before the real superblock has been read.
type geometry struct {
	blkSize    int64
	bpg        int64
	firstDataB int64 // s_first_data_block
}

// For 1 KiB blocks s_first_data_block=1 shifts every group start by one block.
// Blocks per group is 8×blockSize, one block being the allocation bitmap.
var geometries = []geometry{
	{blkSize: 1024, bpg: 8192, firstDataB: 1},
	{blkSize: 4096, bpg: 32768, firstDataB: 0},
	{blkSize: 2048, bpg: 16384, firstDataB: 0},
}

// backupGroups lists the groups holding superblock and GDT backups. With
// sparse_super only groups 0, 1 and the powers of 3, 5 and 7 carry a copy;
// without it every group does, and this prefix is still valid.
var backupGroups = []uint32{1, 3, 5, 7, 9, 25, 27, 49, 81, 125, 243, 343, 625, 729}

// blockOf returns the first block of group g under this geometry.
func (g geometry) blockOf(group int64) int64 { return g.firstDataB + group*g.bpg }

// matches reports whether a parsed superblock agrees with the geometry the
// candidate offset assumed. Without this check the first copy with a valid
// magic wins, so stale data left at a wrong-block-size offset is accepted.
func (g geometry) matches(sb *Superblock) bool {
	return int64(1024)<<sb.LogBlockSize == g.blkSize &&
		int64(sb.BlocksPerGroup) == g.bpg &&
		int64(sb.FirstDataBlock) == g.firstDataB
}

// tryBackupSuperblocks scans the backup superblock locations for each plausible
// geometry, accepting only a copy whose own fields match the geometry that
// located it.
func (fs *FS) tryBackupSuperblocks() (*Superblock, error) {
	for _, group := range backupGroups {
		for _, g := range geometries {
			byteOff := g.blockOf(int64(group)) * g.blkSize
			sb, err := fs.readSuperblockAt(byteOff)
			if err != nil {
				continue
			}
			if !g.matches(sb) {
				fs.diag("superblock candidate at byte offset %d has %d-byte blocks, not the %d assumed; ignoring",
					byteOff, 1024<<sb.LogBlockSize, g.blkSize)
				continue
			}
			fs.diag("using backup superblock from group %d at byte offset %d", group, byteOff)
			sb.BackupGroup = group
			return sb, nil
		}
	}
	return nil, errors.New("extfs: no readable superblock found (primary and all backup copies failed)")
}

// readGroupDescriptors reads the block group descriptor table, which starts in
// the block after the one holding the superblock.
func (fs *FS) readGroupDescriptors() error {
	sb := fs.sb
	// Block numbering starts at FirstDataBlock (1 on 1 KiB-block filesystems),
	// so those blocks are not part of any group. Counting them creates a phantom
	// trailing group whose descriptor is read out of the block bitmap.
	dataBlocks := int64(sb.BlocksCount) - int64(sb.FirstDataBlock)
	if dataBlocks <= 0 {
		return fmt.Errorf("extfs: superblock has %d blocks with FirstDataBlock %d", sb.BlocksCount, sb.FirstDataBlock)
	}
	numGroups := int((dataBlocks + int64(sb.BlocksPerGroup) - 1) / int64(sb.BlocksPerGroup))
	// numGroups is derived entirely from disk fields, and the table it sizes is
	// read in one allocation: a corrupt BlocksPerGroup asks for gigabytes. The
	// table cannot be larger than the image that holds it.
	if fs.size > 0 {
		maxGroups := (fs.size - fs.off) / groupDescriptorSize
		if int64(numGroups) > maxGroups {
			return fmt.Errorf("extfs: %d block groups need a %d-byte descriptor table, larger than the %d-byte image",
				numGroups, int64(numGroups)*groupDescriptorSize, fs.size-fs.off)
		}
	}

	// The GDT follows the superblock in each group that carries a copy. A single
	// bad sector in the primary table would otherwise fail Open outright.
	primary := int64(sb.FirstDataBlock) + 1
	err := fs.readGroupDescriptorsAt(primary, numGroups)
	if err == nil {
		return nil
	}
	fs.diag("primary group descriptor table unreadable (%s); trying backups", err)
	for _, group := range backupGroups {
		if int(group) >= numGroups {
			break
		}
		blk := int64(sb.FirstDataBlock) + int64(group)*int64(sb.BlocksPerGroup) + 1
		if bErr := fs.readGroupDescriptorsAt(blk, numGroups); bErr == nil {
			fs.diag("using backup group descriptor table from group %d", group)
			return nil
		}
	}
	return err
}

func (fs *FS) readGroupDescriptorsAt(gdtBlock int64, numGroups int) error {
	buf := make([]byte, numGroups*groupDescriptorSize)
	if err := fs.readAt(gdtBlock*int64(fs.blkSize), buf); err != nil {
		return err
	}
	gds := make([]GroupDescriptor, numGroups)
	for i := range numGroups {
		gd, err := parseGroupDescriptor(buf[i*groupDescriptorSize:])
		if err != nil {
			return fmt.Errorf("extfs: group %d: %w", i, err)
		}
		gds[i] = gd
	}
	fs.gds = gds
	return nil
}

// Superblock returns the parsed superblock. BackupGroup is non-zero when Open
// fell back to a backup copy.
func (fs *FS) Superblock() *Superblock { return fs.sb }

// BlockSize returns the filesystem block size in bytes.
func (fs *FS) BlockSize() int { return fs.blkSize }

// InodeCount returns the total number of inodes in the filesystem.
func (fs *FS) InodeCount() uint32 { return fs.sb.InodesCount }

// ReadInode reads and returns inode inodeNum (1-based).
func (fs *FS) ReadInode(inodeNum uint32) (*Inode, error) {
	if inodeNum == 0 || inodeNum > fs.sb.InodesCount {
		return nil, fmt.Errorf("extfs: inode number %d out of range [1, %d]",
			inodeNum, fs.sb.InodesCount)
	}
	idx := inodeNum - 1
	group := idx / fs.sb.InodesPerGroup
	localIdx := idx % fs.sb.InodesPerGroup
	if int(group) >= len(fs.gds) {
		return nil, fmt.Errorf("extfs: inode %d: block group %d out of range", inodeNum, group)
	}
	gd := fs.gds[group]
	inodeOff := int64(gd.InodeTable)*int64(fs.blkSize) + int64(localIdx)*int64(fs.sb.InodeSize)
	buf := make([]byte, fs.sb.InodeSize)
	if err := fs.readAt(inodeOff, buf); err != nil {
		return nil, fmt.Errorf("extfs: read inode %d: %w", inodeNum, err)
	}
	largeFile := fs.sb.FeatureROCompat&FeatureROCompatLargeFile != 0
	return parseInode(buf, largeFile)
}

// readAt reads exactly len(buf) bytes at offset, relative to the FS start.
func (fs *FS) readAt(offset int64, buf []byte) error {
	_, err := fs.readAtN(offset, buf)
	return err
}

// readAtN reads into buf at offset, relative to the FS start, and reports how
// much came back before any failure. A short read is not an error by itself:
// through a rescue map, the recovered prefix arrives before the failure, and
// those bytes belong to the file. Without them there is nothing to keep and
// nothing to retry.
//
// It has ddrescue.ReadFunc's shape, so a reader recovering a block can pass it
// straight to the gap machinery as the retry.
//
// fs.mu serializes access to the shared io.ReadSeeker.
func (fs *FS) readAtN(offset int64, buf []byte) (int, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if _, err := fs.r.Seek(fs.off+offset, io.SeekStart); err != nil {
		return 0, err
	}
	return io.ReadFull(fs.r, buf)
}
