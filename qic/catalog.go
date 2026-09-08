package qic

import (
	"encoding/binary"
	"errors"
	"fmt"
	"time"
)

// Record layout. A record is a fixed part, a long name, a second fixed part,
// a short name, and -- in the data region only -- the path of the directory
// holding it.
const (
	fix1Size = 0x47
	fix2Size = 0x17

	// The offsets within the first fixed part that carry something known.
	offRecLen   = 0x00
	offIndex    = 0x02
	offPathLen  = 0x0A
	offConstA   = 0x0C
	offFlag     = 0x0E
	offConstB   = 0x0F
	offFileLen  = 0x11
	offAttrib   = 0x29
	offCreated  = 0x2D
	offAccessed = 0x35
	offModified = 0x3D
	offNameLen  = 0x45

	// offFix2NameLen is the short name's length, at the end of the second
	// fixed part.
	offFix2NameLen = 0x15

	// constA and constB hold the same values in every record of every archive
	// examined. Together with the length equation they let the catalog, which
	// carries no signature of its own, be resynchronized.
	constA = 0x000A
	constB = 0x0007

	// undefined fills the fields the format leaves unused. The data region
	// also stores it where the catalog keeps a file's size.
	undefined = 0xFFFFFFFF
)

// Record flag bits. These are not the values QIC-113 documents; the ones the
// Windows 9x software writes were worked out by inspection, and the root
// record carries a further bit whose meaning is unknown.
const (
	flagSubdir   = 0x01
	flagEmptyDir = 0x02
	flagDirLast  = 0x08 // last entry in this directory's child list
	flagDirEnd   = 0x30 // last entry in the whole catalog
	flagRoot     = 0x40
)

// Signatures bracketing a record and its data in the data region. Neither
// appears in the catalog.
const (
	datSig  = 0x33CC33CC
	edatSig = 0x66996699

	// trailerSize is one signature and the 16-bit value following it.
	trailerSize = 6
)

var errBadRecord = errors.New("qic: record fields disagree")

// record is one decoded directory entry.
type record struct {
	recLen   int
	pathLen  int
	flag     uint8
	attrib   uint8
	fileLen  int64 // meaningless in the data region, where it is all ones
	created  time.Time
	accessed time.Time
	modified time.Time
	name     string
	short    string
	path     []string // only ever present in the data region

	// size is how many bytes the record occupies, excluding its path. It is
	// recLen+2 whenever the record is sound, and is kept separately so a
	// record can be stepped over even when they disagree.
	size int
}

func (r record) isDir() bool   { return r.flag&flagSubdir != 0 }
func (r record) isEmpty() bool { return r.flag&flagEmptyDir != 0 }
func (r record) dirLast() bool { return r.flag&flagDirLast != 0 }
func (r record) dirEnd() bool  { return r.flag&flagDirEnd == flagDirEnd }

// plausibleRecord is the cheap test for a record at the head of b, needing
// only the fixed part and no I/O past it. The signature sweep and the catalog
// resync use it to screen candidates, the way internal/dbf screens a table
// header.
//
// Four things must hold together, and they do in every record of every archive
// examined: the declared length accounts for the fixed parts and both names
// exactly, so the short name's length follows from it rather than having to be
// read; the two fields at 0x0C and 0x0F hold the constants they always hold;
// and every length counting UTF-16 bytes is even. A run of a file's own bytes
// does not satisfy all of them at once by chance.
func plausibleRecord(b []byte) (size int, ok bool) {
	if len(b) < fix1Size {
		return 0, false
	}
	if binary.LittleEndian.Uint16(b[offConstA:]) != constA ||
		binary.LittleEndian.Uint16(b[offConstB:]) != constB {
		return 0, false
	}
	if binary.LittleEndian.Uint16(b[offPathLen:])%2 != 0 {
		return 0, false
	}
	nameLen := int(binary.LittleEndian.Uint16(b[offNameLen:]))
	if nameLen%2 != 0 {
		return 0, false
	}
	size = int(binary.LittleEndian.Uint16(b[offRecLen:])) + 2
	shortLen := size - fix1Size - fix2Size - nameLen
	if shortLen < 0 || shortLen%2 != 0 {
		return 0, false
	}
	return size, true
}

// recordSize is plausibleRecord with the record required to fit in the bytes
// on hand, as a caller stepping through the catalog needs.
func recordSize(b []byte) (int, bool) {
	size, ok := plausibleRecord(b)
	if !ok || size > len(b) {
		return 0, false
	}
	return size, true
}

// parseRecord decodes the record at the head of b. inData says whether the
// path bytes follow the short name, which they do only in the data region.
func parseRecord(b []byte, inData bool) (record, error) {
	size, ok := recordSize(b)
	if !ok {
		return record{}, errBadRecord
	}
	r := record{
		recLen:   int(binary.LittleEndian.Uint16(b[offRecLen:])),
		pathLen:  int(binary.LittleEndian.Uint16(b[offPathLen:])),
		flag:     b[offFlag],
		attrib:   b[offAttrib],
		created:  stamp(binary.LittleEndian.Uint32(b[offCreated:]), 0),
		accessed: stamp(binary.LittleEndian.Uint32(b[offAccessed:]), 0),
		modified: stamp(binary.LittleEndian.Uint32(b[offModified:]), 0),
		size:     size,
	}
	if n := binary.LittleEndian.Uint32(b[offFileLen:]); n != undefined {
		r.fileLen = int64(n)
	}

	nameLen := int(binary.LittleEndian.Uint16(b[offNameLen:]))
	pos := fix1Size
	r.name = decodeUTF16(b[pos : pos+nameLen])
	pos += nameLen

	// The short name's length is stored as well as implied by the declared
	// record length, and the two must agree. The screening test can only
	// derive it, since it works from the first fixed part alone, so this check
	// rejects a record that claims one length and holds another.
	shortLen := int(binary.LittleEndian.Uint16(b[pos+offFix2NameLen:]))
	if want := size - fix1Size - nameLen - fix2Size; shortLen != want {
		return record{}, fmt.Errorf("%w: a short name of %d bytes where the record's length implies %d",
			errBadRecord, shortLen, want)
	}
	pos += fix2Size
	r.short = decodeUTF16(b[pos : pos+shortLen])
	pos += shortLen

	if inData {
		if pos+r.pathLen > len(b) {
			return r, fmt.Errorf("%w: a %d-byte path does not fit the record", errBadRecord, r.pathLen)
		}
		r.path = decodePath(b[pos : pos+r.pathLen])
	}
	return r, nil
}

// isDataAnchor reports whether b begins with a data region record: the
// signature, both index words unused, and a record whose fields agree. The
// index words keep a chance signature inside a file's own bytes from being
// taken for a record.
func isDataAnchor(b []byte) bool {
	if len(b) < 4+fix1Size {
		return false
	}
	if binary.LittleEndian.Uint32(b) != datSig {
		return false
	}
	rec := b[4:]
	if binary.LittleEndian.Uint32(rec[offIndex:]) != undefined ||
		binary.LittleEndian.Uint32(rec[offIndex+4:]) != undefined {
		return false
	}
	_, ok := plausibleRecord(rec)
	return ok
}

// dataRecordSize is how far past a record's signature its file data begins,
// and how far past that the next record does. The catalog holds no pointer to
// a file's bytes, so every offset in the archive comes from adding these up in
// catalog order, and one wrong record displaces everything after it.
//
// The branch is on the record's own directory flag, not on whether it declares
// a size. The reference implementation branches on the size, which treats a
// zero-length file as a directory: on Windows 95 the two happen to come to the
// same 18 bytes, and on Windows 98 it loses six bytes per empty file.
func dataRecordSize(r record, v Variant) (dataStart, next int64) {
	pos := int64(4 + r.size + r.pathLen)
	if r.isDir() {
		return -1, pos + trailerSize*v.dirTrailers()
	}
	pos += trailerSize
	return pos, pos + r.fileLen + trailerSize*v.fileTrailers()
}
