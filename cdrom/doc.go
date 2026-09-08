// Package cdrom reads CD and DVD drives through the operating system's optical
// device interface: the table of contents, and sectors in either cooked
// (2048-byte) or raw (2352-byte) form.
//
// Linux goes through the linux/cdrom.h ioctls, with SCSI pass-through for the
// full TOC that carries per-track session numbers and INDEX 00 positions.
// macOS goes through the DKIOCCDREAD family. Open reports ErrUnsupportedPlatform
// for other platforms.
//
// Format and interface references:
//
//	https://www.kernel.org/doc/html/latest/userspace-api/ioctl/cdrom.html
//	https://ecma-international.org/wp-content/uploads/ECMA-130_2nd_edition_june_1996.pdf
//	https://www.t10.org/members/w_mmc.htm (MMC command set: READ TOC/PMA/ATIP)
//	https://opensource.apple.com/source/IOCDStorageFamily/ (IOCDMediaBSDClient.h)
package cdrom
