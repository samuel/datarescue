// Package extfs reads ext2 and ext3 filesystems, read-only and aimed at
// recovery from a damaged image.
//
// The superblock and group descriptor table both fall back to their backup
// copies at the sparse-super groups, and a backup is accepted only when its own
// fields agree with the geometry that located it. Directory data is parsed one
// block at a time, since entries never span a block in ext2 and a corrupt
// rec_len should therefore cost only its own block. Reading an inode's data
// tolerates unreadable indirect pointer blocks by reporting the gap instead of
// abandoning the file.
//
// ext4's extent trees and inline data are rejected rather than misread: both
// reuse the i_block array for something other than block pointers.
//
// Format references:
//
//	https://www.nongnu.org/ext2-doc/ext2.html
//	https://www.kernel.org/doc/html/latest/filesystems/ext4/globals.html
package extfs
