// Package apm parses the Apple partition map an Apple-formatted disk carries in
// place of a master boot record.
//
// The map is a contiguous run of 512-byte entries beginning at block 1, each
// one self-describing: it carries its own start and length, so nothing in entry
// N is needed to find or read entry N+1. Unlike the MBR's extended boot record
// chain, where a lost record hides every partition after it, a map block that
// will not read here loses only the one partition it described.
//
// The format documentation says three things that are not true of the disks
// this package was written against, and a parser that believed any of them
// would break:
//
//   - pmPartStatus is 0 on a working Apple_HFS partition. By the flag
//     definitions that means "not valid, not allocated, not readable", on a
//     volume a Mac mounted for twenty years. Nothing may gate on it.
//   - pmDataCnt is 0 on those same partitions, so it is no more usable as a
//     length than the status is as a validity test.
//   - sbBlkCount can be short of the medium — 196606 on a 196608-block disk —
//     so the image's own length is the only sound bound on an offset.
//
// The reserved tail of the map partition is not a reliable terminator either.
// It holds zeros on one disk here and 0xFA, the drive's erase pattern, on
// another. The PM signature is what ends the map; pmMapBlkCnt is a cross-check
// reported on disagreement and never a bound in either direction. Trusted when
// it is too high, the walk invents partitions out of fill bytes; trusted when it
// is too low, a partition added without updating it is lost.
//
// Detection is structural and never textual. One disk in the archive this was
// written for carries the string "Apple_HFS" six times: once in a real map
// entry and five times inside the driver partition's own code.
//
// Format references:
//
//	Inside Macintosh: Devices, chapter 3 (the Block0 and Partition structures)
//	https://en.wikipedia.org/wiki/Apple_Partition_Map
package apm
