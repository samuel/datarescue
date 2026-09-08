// Package fat reads FAT12, FAT16 and FAT32 filesystems, with the damage a
// failing disk produces in mind rather than assuming a well-formed volume.
//
// The FAT copies are read independently and sector by sector, so a bad sector
// costs only the entries it holds; entries backed by an unreadable sector are
// recorded rather than read as free, since a zero there is indistinguishable
// from an unallocated cluster and would silently truncate a chain. When the
// copies disagree, ChainPolicy decides which one a chain is resolved from.
// Traversal collects failures and continues, and a read that fails partway
// through returns the bytes recovered before it.
//
// Format references:
//
//	https://en.wikipedia.org/wiki/Design_of_the_FAT_file_system
//	https://www.file-recovery.com/recovery-understanding-file-system-fat.htm
//	https://download.microsoft.com/download/1/6/1/161ba512-40e2-4cc9-843a-923143f3456c/fatgen103.doc
//
// Other implementations consulted:
//
//	https://github.com/gokrazy/internal/tree/main/fat
//	https://pkg.go.dev/go.fuchsia.dev/fuchsia/src/lib/thinfs/fs/msdosfs#section-readme
//	https://gitlab.com/FreeDOS/util/dosfsck
package fat
