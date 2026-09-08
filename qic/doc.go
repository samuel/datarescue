// Package qic reads Windows 9x MS Backup archives.
//
// Microsoft Backup shipped with Windows 95 through ME and wrote one *.qic file
// per backup. Past a head of 128-byte volume table records the file is a hard
// grid of 29696-byte segments, a data region followed by a catalog, and in a
// compressed archive every segment carries a header giving the cumulative
// count of uncompressed bytes preceding it.
//
// That count is what this package is built around. It makes each segment an
// independent anchor into the decompressed stream, so any file can be reached
// without expanding what precedes it, and damage to one segment costs the
// bytes it held rather than shifting every file after it. The reference
// implementation decompresses from the start and exits the process on a read
// error, which loses everything past the first bad sector.
//
// The catalog records a size and a tree but no pointer to a file's data, so
// data offsets have to be accumulated across every record in order. The data
// region repeats each record inline ahead of the bytes it describes, so an
// accumulated offset can be checked before it is trusted, and a backup whose
// catalog is unreadable can still be recovered by scanning.
//
// Damage is reported rather than logged. A segment that will not decompress
// costs the files overlapping it and no more, a file whose data cannot be read
// yields the bytes recovered before the failure, and both are described by
// Diagnostics.
//
// Format references:
//
//	references/rd113/113-format.htm
//	references/msqic/MSBACKUP.HTM
//
// Other implementations consulted:
//
//	references/msqic/MSQIC.C
//	references/msqic/QICDCOMP.C
//	references/msqic/MSQICRCV.C
package qic
