// Package hfs reads Apple HFS volumes — the "Mac OS Standard" filesystem, not
// HFS+.
//
// Files are found through the catalog B-tree. Its leaf nodes form a linked
// list, so one unreadable node would truncate everything after it: a malformed
// record is skipped rather than abandoning its node, and when the chain breaks
// the remaining nodes are swept directly for leaves. Thread records are used as
// a second source for the parent map, which places a folder whose own record is
// damaged. Forks that outgrow the three extents in their catalog record are
// followed into the extents overflow file.
//
// Filenames are Mac OS Roman, not UTF-8, and are decoded accordingly.
//
// Format references:
//
//	https://developer.apple.com/library/archive/technotes/tn/tn1150.html
//	https://github.com/libyal/libfshfs/blob/main/documentation/Hierarchical%20File%20System%20(HFS).asciidoc
//	http://fileformats.archiveteam.org/wiki/HFS
package hfs
