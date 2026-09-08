// Package appledouble encodes the AppleDouble sidecar that carries a classic
// Mac file's resource fork and Finder information beside its data fork.
//
// A Mac file has two forks, and on an HFS volume much of the content is in the
// resource fork rather than the data fork. Applications, fonts, system
// suitcases and older documents keep everything there and leave the data fork
// empty. Writing only the data fork loses most of such a disk.
//
// macOS itself writes AppleDouble version 2 onto a volume that cannot hold two
// forks, and its own tools read it back, so a "._Name" companion written here
// is rejoined with its data fork when the pair is copied to a Mac volume.
//
// Format reference:
//
//	AppleSingle/AppleDouble Formats for Foreign Files Developer's Note (RFC 1740)
package appledouble

import (
	"bytes"
	"encoding/binary"
	"io"
	"path"
	"time"
)

// magic and version identify an AppleDouble file. AppleSingle, which carries
// the data fork too, is 0x00051600 and is not what this writes.
const (
	magic   uint32 = 0x00051607
	version uint32 = 0x00020000
)

// Entry IDs, from the format note.
const (
	idResourceFork uint32 = 2
	idRealName     uint32 = 3
	idFileDates    uint32 = 8
	idFinderInfo   uint32 = 9
)

// unixAt2000 is 2000-01-01 00:00:00 UTC in Unix seconds. AppleDouble counts its
// dates from there, signed, where HFS counts from 1904.
const unixAt2000 = 946684800

// Header is the fixed part of a sidecar: everything written in front of the
// resource fork's own bytes.
type Header struct {
	// FinderInfo is the 32 bytes the catalog stores (FInfo then FXInfo),
	// copied verbatim so the file keeps its type and creator.
	FinderInfo [32]byte
	// The zero value of a time means it was not recorded.
	Created, Modified, Backup, Accessed time.Time
	// RealName is the name as the volume spells it, written only when it
	// differs from the name the file is stored under. Otherwise the name
	// sanitized for this filesystem would be the only record.
	RealName string
	// RsrcSize is the resource fork's recorded length.
	RsrcSize int64
}

// entry is one descriptor in the header's table.
type entry struct {
	id   uint32
	data []byte
	// size is the length to declare, which for the resource fork is its
	// recorded length rather than any bytes held here.
	size int64
}

// entries is the descriptor list, in the order it is written. The resource fork
// comes last so everything before it is a fixed-length prefix.
func (h Header) entries() []entry {
	var out []entry
	if h.RealName != "" {
		out = append(out, entry{id: idRealName, data: []byte(h.RealName), size: int64(len(h.RealName))})
	}
	dates := make([]byte, 16)
	for i, t := range []time.Time{h.Created, h.Modified, h.Backup, h.Accessed} {
		binary.BigEndian.PutUint32(dates[i*4:], macDate(t))
	}
	out = append(out, entry{id: idFileDates, data: dates, size: 16})
	fi := h.FinderInfo
	out = append(out, entry{id: idFinderInfo, data: fi[:], size: 32})
	out = append(out, entry{id: idResourceFork, size: h.RsrcSize})
	return out
}

// macDate renders a time as AppleDouble's signed seconds from 2000. A zero time
// is written as zero, which the format defines as "unknown".
func macDate(t time.Time) uint32 {
	if t.IsZero() {
		return 0
	}
	//nolint:gosec // the conversion is the format's own signed-seconds encoding
	return uint32(int32(t.Unix() - unixAt2000))
}

// Bytes encodes the header.
func (h Header) Bytes() []byte {
	es := h.entries()
	var b bytes.Buffer
	binary.Write(&b, binary.BigEndian, magic)           //nolint:errcheck // bytes.Buffer
	binary.Write(&b, binary.BigEndian, version)         //nolint:errcheck // bytes.Buffer
	b.Write(make([]byte, 16))                           // filler
	binary.Write(&b, binary.BigEndian, uint16(len(es))) //nolint:errcheck,gosec // a fixed small count

	// Each descriptor is id, offset and length; the offsets follow the table.
	off := int64(b.Len() + 12*len(es))
	for _, e := range es {
		binary.Write(&b, binary.BigEndian, e.id)           //nolint:errcheck // bytes.Buffer
		binary.Write(&b, binary.BigEndian, uint32(off))    //nolint:errcheck,gosec // bounded by the header and the fork
		binary.Write(&b, binary.BigEndian, uint32(e.size)) //nolint:errcheck,gosec // the volume's own recorded length
		off += e.size
	}
	for _, e := range es {
		b.Write(e.data)
	}
	return b.Bytes()
}

// Size is how many bytes precede the resource fork in the written file, which
// is the offset every hole in that fork has to be shifted by.
func (h Header) Size() int64 { return int64(len(h.Bytes())) }

// Reader returns the header followed by the resource fork's bytes.
func (h Header) Reader(rsrc io.Reader) io.Reader {
	return io.MultiReader(bytes.NewReader(h.Bytes()), rsrc)
}

// Prefix marks a companion. macOS matches "._Name" to "Name" by exact
// filename, so a caller building the two names itself must use this prefix
// and count its bytes against the same path-component length limit.
const Prefix = "._"

// SidecarName is the AppleDouble name for a path: the file's own name with the
// prefix, in the same directory.
func SidecarName(p string) string {
	dir, name := path.Split(p)
	return dir + Prefix + name
}
