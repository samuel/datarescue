// Package onestep reads Iomega 1-Step Backup archives.
//
// 1-Step Backup shipped with Zip and Jaz drives on Windows 9x and wrote one
// *.1-Step file per media disk. A backup's file data runs from the first disk
// onward and the catalog is written at the end of the last one, so restoring
// from a set means reading the catalog from whichever disk carries it and each
// chunk from the disk that holds it. Chunk offsets are positions within their
// own disk's file and restart on every disk.
//
// The catalog is a container of dBase III tables: a directory at the catalog
// offset lists each member and its size, and seven of the members are the
// tables describing the drives, directories, files, compressed chunks, job,
// selected paths and sessions. Because the directory locates them, this
// package needs none of the fixed byte offsets or forward string scanning the
// reference implementation relies on. Scanning remains only as a fallback for
// when the directory itself cannot be read.
//
// Damage is reported rather than logged. A table with an unreadable record
// costs those rows and no more, a file whose data cannot be read yields the
// bytes recovered before the failure, and both are described by Diagnostics.
//
// Format references:
//
//	http://www.willsworks.net/file-format/iomega-1-step-backup
//	references/1Srest/1-Step-format.htm
//
// Other implementations consulted:
//
//	references/1Srest/1Srest.c
package onestep
