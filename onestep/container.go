package onestep

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"time"
)

// dbfHeaderSize is the fixed part of a dBase table header, the smallest window
// the scan fallback needs to test a position.
const dbfHeaderSize = 32

const (
	containerMagic  = 0xCA7557AC
	containerHeader = 0x10
	memberEntrySize = 0x130
	memberNameSize  = 256
	// maxMembers bounds a corrupt count before it is used to size anything.
	// A catalog holds two members per table and there are seven tables.
	maxMembers = 256
)

// errNotContainer reports that the bytes at the catalog offset are not a
// member directory, which sends the caller to the scan fallback.
var errNotContainer = errors.New("onestep: not a catalog directory")

// Member is one file inside the catalog container. The tables are the .DBF
// members; the .CDX members are FoxPro indexes over the same data and are not
// needed to read it.
type Member struct {
	Name string
	// Offset is absolute within the disk file holding the catalog.
	Offset     int64
	Size       int64
	Modified   time.Time
	Attributes uint32
}

// readContainer decodes the member directory at off. Limit is the end of the
// file, so a corrupt size cannot point a member past it.
func readContainer(r io.ReaderAt, off, limit int64) ([]Member, error) {
	if off < HeaderSize || off+containerHeader > limit {
		return nil, fmt.Errorf("%w: offset %d outside the file", errNotContainer, off)
	}
	hdr := make([]byte, containerHeader)
	if _, err := r.ReadAt(hdr, off); err != nil {
		return nil, fmt.Errorf("onestep: read catalog directory: %w", err)
	}
	if binary.LittleEndian.Uint32(hdr) != containerMagic {
		return nil, fmt.Errorf("%w: magic 0x%08x at %d", errNotContainer, binary.LittleEndian.Uint32(hdr), off)
	}
	count := int(binary.LittleEndian.Uint32(hdr[4:]))
	entriesSize := int64(binary.LittleEndian.Uint32(hdr[8:]))
	dataSize := int64(binary.LittleEndian.Uint32(hdr[12:]))
	if count < 1 || count > maxMembers || entriesSize != int64(count)*memberEntrySize {
		return nil, fmt.Errorf("%w: %d members, %d bytes of entries", errNotContainer, count, entriesSize)
	}
	dataStart := off + containerHeader + entriesSize
	if dataStart > limit || dataSize < 0 || dataStart+dataSize > limit {
		return nil, fmt.Errorf("%w: %d bytes of member data would end past the file", errNotContainer, dataSize)
	}

	entries := make([]byte, entriesSize)
	if _, err := r.ReadAt(entries, off+containerHeader); err != nil {
		return nil, fmt.Errorf("onestep: read catalog member entries: %w", err)
	}

	members := make([]Member, 0, count)
	pos := dataStart
	for i := range count {
		e := entries[i*memberEntrySize:]
		// The entry's own offset field holds the start of the whole data
		// region, the same value in every entry, so member positions come from
		// accumulating the sizes in entry order instead.
		declared := binary.LittleEndian.Uint64(e[0x118:])
		if declared > math.MaxInt64 {
			return nil, fmt.Errorf("%w: member %d declares a size of %d", errNotContainer, i, declared)
		}
		size := int64(declared)
		// Subtract rather than add: pos+size overflows for a size near MaxInt64
		// and lets the member through, which then has dbf.Open walking billions
		// of records.
		if size > limit-pos {
			return nil, fmt.Errorf("%w: member %d of size %d would end past the file", errNotContainer, i, size)
		}
		members = append(members, Member{
			Name:       cstring(e[0x0c : 0x0c+memberNameSize]),
			Offset:     pos,
			Size:       size,
			Modified:   oleTime(math.Float64frombits(binary.LittleEndian.Uint64(e[0x10c:]))),
			Attributes: uint32(binary.LittleEndian.Uint64(e[0x120:]) & 0xffffffff),
		})
		pos += size
	}
	return members, nil
}
