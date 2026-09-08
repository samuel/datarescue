// Package mbr parses the Master Boot Record partition table at the start of a
// partitioned disk.
//
// Beyond the four primary entries it walks the extended boot record chain to
// find logical partitions, and recognizes the protective MBR a GPT disk carries
// — read as an ordinary entry, that one entry looks like a partition spanning
// the whole drive.
//
// Format references:
//
//	https://en.wikipedia.org/wiki/Master_boot_record
//	https://en.wikipedia.org/wiki/Extended_boot_record
//	https://uefi.org/specs/UEFI/2.10/05_GUID_Partition_Table_Format.html
package mbr
