package hfs

import (
	"strings"
	"time"
)

// OSType is a four-character Mac OS type or creator code.
type OSType [4]byte

func (t OSType) String() string {
	return strings.TrimRight(decodeMacRoman(t[:]), " \x00")
}

// IsZero reports whether the code is unset.
func (t OSType) IsZero() bool { return t == OSType{} }

// MacTime is an HFS timestamp: seconds since 1904-01-01 00:00:00 as the clock
// on the Mac that wrote it read, with no zone recorded anywhere on the volume.
type MacTime uint32

// IsZero reports whether no time was recorded.
func (t MacTime) IsZero() bool { return t == 0 }

// macEpoch is the zero point HFS counts from.
var macEpoch = time.Date(1904, 1, 1, 0, 0, 0, 0, time.UTC)

// Time interprets the timestamp as wall-clock time in loc, or UTC when loc is
// nil.
//
// The stored number is local wall-clock seconds, so the conversion builds the
// date from the epoch in UTC and then re-attaches loc to those fields.
// Anchoring the epoch in loc and adding looks equivalent and is not: it applies
// loc's 1904 offset, which for most zones is a local mean time reading minutes
// off the hour, and it takes no account of whether the file's own date fell in
// daylight time.
func (t MacTime) Time(loc *time.Location) time.Time {
	if t == 0 {
		return time.Time{}
	}
	if loc == nil {
		loc = time.UTC
	}
	u := macEpoch.Add(time.Duration(t) * time.Second)
	return time.Date(u.Year(), u.Month(), u.Day(), u.Hour(), u.Minute(), u.Second(), 0, loc)
}

// Entry is one catalog node — a file or a folder — with its path reconstructed
// from the parent chain.
type Entry struct {
	// Path is the entry's components from the volume root, outermost first.
	//
	// It is components and not a joined string because "/" is an ordinary
	// character in an HFS filename — the classic Mac separator is ":" — so a
	// file called "10/20/99 Budget" joined with slashes is indistinguishable
	// from three nested folders, and an extract would build a tree out of one
	// file.
	Path []string
	Name string
	// CNID is the catalog node ID, and ParentID the folder holding it.
	CNID, ParentID uint32
	IsDir          bool

	Created, Modified, Backup MacTime

	// Files only. Type and Creator are the four-character codes classic Mac OS
	// uses in place of an extension to say what a file is and what opens it.
	Type, Creator OSType
	FinderFlags   uint16
	// finderInfo and finderExt are the record's FInfo and FXInfo exactly as the
	// catalog stores them. The parsed fields above are conveniences read out of
	// the first; these are what an AppleDouble sidecar carries verbatim, and
	// reassembling them from the parsed fields would be a chance to get the
	// layout wrong for no gain.
	finderInfo  [16]byte
	finderExt   [16]byte
	DataSize    int64
	DataExtents ExtentsRecord
	RsrcSize    int64
	RsrcExtents ExtentsRecord

	// Directories only.
	Valence uint16
}

// HasResourceFork reports whether the entry carries resource-fork data.
func (e Entry) HasResourceFork() bool { return e.RsrcSize > 0 }

// DataFork returns a reference to the entry's data fork.
func (e Entry) DataFork() ForkRef {
	return ForkRef{CNID: e.CNID, Type: ForkTypeData, Extents: e.DataExtents, Size: e.DataSize}
}

// ResourceFork returns a reference to the entry's resource fork.
func (e Entry) ResourceFork() ForkRef {
	return ForkRef{CNID: e.CNID, Type: ForkTypeResource, Extents: e.RsrcExtents, Size: e.RsrcSize}
}

// FinderInfo is the 32 bytes an AppleDouble sidecar carries verbatim: the
// 16-byte FInfo followed by the 16-byte FXInfo, exactly as the catalog stores
// them.
func (e Entry) FinderInfo() [32]byte {
	var out [32]byte
	copy(out[0:16], e.finderInfo[:])
	copy(out[16:32], e.finderExt[:])
	return out
}

// setFinder records the record's Finder information and reads the fields worth
// having parsed out of it.
func (e *Entry) setFinder(fInfo, fxInfo [16]byte) {
	e.finderInfo, e.finderExt = fInfo, fxInfo
	copy(e.Type[:], fInfo[0:4])
	copy(e.Creator[:], fInfo[4:8])
	e.FinderFlags = uint16(fInfo[8])<<8 | uint16(fInfo[9])
}
