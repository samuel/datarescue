package extfs

import (
	"errors"
	"fmt"
	"io"
)

// ValidationIssueKind classifies a structural problem found during ValidateFS.
type ValidationIssueKind string

const (
	// IssueUnreadableBlock means a data or indirect pointer block could not be read.
	IssueUnreadableBlock ValidationIssueKind = "unreadable_block"
	// IssueUnreadableInode means an inode's inode table block could not be read.
	IssueUnreadableInode ValidationIssueKind = "unreadable_inode"
	// IssueExtentTree means inode uses ext4 extent trees, which are not supported.
	IssueExtentTree ValidationIssueKind = "extent_tree"
	// IssueUnreadableBitmap means a group's inode bitmap could not be read, so
	// every inode slot in the group was tried instead of skipping the group.
	IssueUnreadableBitmap ValidationIssueKind = "unreadable_bitmap"
	// IssueUnreadableDir means a directory's entry chain was malformed and only
	// part of it could be recovered.
	IssueUnreadableDir ValidationIssueKind = "unreadable_dir"
)

// ValidationIssue describes a single structural problem found by ValidateFS.
type ValidationIssue struct {
	InodeNum uint32
	Kind     ValidationIssueKind
	Detail   string
}

// ValidationResult aggregates the outcome of a ValidateFS run.
type ValidationResult struct {
	Issues         []ValidationIssue
	TotalInodes    uint32
	ReadableInodes uint32 // inodes successfully parsed from the inode table
	ReadableFiles  uint32 // regular files + dirs whose full data could be read
}

func (r *ValidationResult) addIssue(inodeNum uint32, kind ValidationIssueKind, detail string) {
	r.Issues = append(r.Issues, ValidationIssue{InodeNum: inodeNum, Kind: kind, Detail: detail})
}

// WalkInodes calls fn for every inode slot in inode-table order. Setting
// includeUnallocated also yields inodes marked free in the allocation bitmap,
// which is how files no longer listed in any directory are recovered. Inodes
// that cannot be read are collected as issues and the walk continues; an
// unreadable bitmap is treated as all-allocated rather than skipping the group.
func (fs *FS) WalkInodes(includeUnallocated bool, fn func(inodeNum uint32, ino *Inode) error) ([]ValidationIssue, error) {
	var issues []ValidationIssue
	for g, gd := range fs.gds {
		// A corrupt InodesPerGroup makes both this product and the per-slot sum
		// below overflow uint32, wrapping to a small number that then passes the
		// inodeNum > InodesCount guard and reads the wrong inodes. Both are
		// computed wide and compared before narrowing.
		base := int64(g)*int64(fs.sb.InodesPerGroup) + 1
		if base > int64(fs.sb.InodesCount) {
			break
		}

		var bitmap []byte
		if !includeUnallocated {
			bitmap = make([]byte, fs.blkSize)
			var err error
			if gd.InodeBitmap == 0 {
				// Block 0 is a sparse hole to ReadBlock, so a descriptor table
				// that itself read as zeros would return an all-free bitmap and
				// every inode in the group would be skipped silently. Reporting
				// a whole group of files as an empty filesystem is wrong;
				// treating the bitmap as all-allocated is only slower.
				err = errors.New("the group descriptor names block 0, which cannot hold a bitmap")
			} else {
				err = fs.ReadBlock(gd.InodeBitmap, bitmap)
			}
			if err != nil {
				// Skipping the group would lose every one of its 8192+ inodes.
				// Treating the bitmap as all-allocated tries them all instead.
				issues = append(issues, ValidationIssue{
					Kind: IssueUnreadableBitmap,
					Detail: fmt.Sprintf("group %d: unreadable inode bitmap, treating every inode as allocated: %s",
						g, err),
				})
				bitmap = nil
			}
		}

		for i := range fs.sb.InodesPerGroup {
			num := base + int64(i)
			if num > int64(fs.sb.InodesCount) {
				break
			}
			inodeNum := uint32(num) //nolint:gosec // num was just bounded by InodesCount, itself a uint32
			if bitmap != nil {
				byteIdx := int(i / 8)
				bitIdx := i % 8
				if byteIdx < len(bitmap) && bitmap[byteIdx]&(1<<bitIdx) == 0 {
					continue
				}
			}
			ino, err := fs.ReadInode(inodeNum)
			if err != nil {
				issues = append(issues, ValidationIssue{
					InodeNum: inodeNum,
					Kind:     IssueUnreadableInode,
					Detail:   fmt.Sprintf("inode %d: %s", inodeNum, err),
				})
				continue
			}
			if err := fn(inodeNum, ino); err != nil {
				return issues, err
			}
		}
	}
	return issues, nil
}

// ValidateFS reads every allocated inode and streams its data blocks,
// collecting issues rather than stopping at the first error.
func (fs *FS) ValidateFS() (*ValidationResult, error) {
	result := &ValidationResult{TotalInodes: fs.sb.InodesCount}

	issues, err := fs.WalkInodes(false, func(inodeNum uint32, ino *Inode) error {
		result.ReadableInodes++

		if ino.Flags&InodeFlagExtents != 0 {
			result.addIssue(inodeNum, IssueExtentTree,
				fmt.Sprintf("inode %d uses ext4 extent trees (not supported)", inodeNum))
			return nil
		}
		if ino.IsDir() {
			// A malformed entry chain costs only its own block; record it.
			if _, err := fs.ReadDir(ino, false); err != nil {
				result.addIssue(inodeNum, IssueUnreadableDir,
					fmt.Sprintf("inode %d: %s", inodeNum, err))
			}
		}
		if !ino.IsRegular() && !ino.IsDir() {
			return nil // skip special files (devices, symlinks, sockets, FIFOs)
		}
		rd, err := fs.ReadInodeData(ino)
		if err != nil {
			result.addIssue(inodeNum, IssueUnreadableBlock,
				fmt.Sprintf("inode %d: %s", inodeNum, err))
			return nil
		}
		if _, err := io.Copy(io.Discard, rd); err != nil {
			result.addIssue(inodeNum, IssueUnreadableBlock,
				fmt.Sprintf("inode %d: %s", inodeNum, err))
			return nil
		}
		result.ReadableFiles++
		return nil
	})
	// Unreadable inodes and bitmaps are an advertised issue category; fold the
	// walk's own findings into the result rather than dropping them.
	result.Issues = append(result.Issues, issues...)
	return result, err
}
