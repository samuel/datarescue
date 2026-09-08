// Package testiso builds a small deterministic ISO 9660 image for tests.
//
// It exists as a normal package, not a test helper, because the qemu-backed
// CD-ROM tests need the same image on both sides: the host attaches it to the
// virtual machine as a disc, and the guest test checks what it reads back
// against these constants. The guest sees only its own test binary, with no
// testdata directory, so the image has to come from shared code rather than a
// committed fixture.
package testiso

import (
	"fmt"
	"os"
	"strings"
	"time"
)

const (
	// SectorSize is the ISO 9660 logical sector size.
	SectorSize = 2048
	// Sectors is the image size in sectors. Keep it small: the qemu-backed
	// tests read the whole disc sector by sector under emulation.
	Sectors = 24
	// RawSectorSize is a sector as it sits on the medium in a raw image, where
	// the 2048 bytes of user data are wrapped in a sync pattern, a header and
	// an ECC area.
	RawSectorSize = 2352
	// VolumeID is the volume identifier in the primary volume descriptor.
	VolumeID = "TESTDISC"
	// Publisher is the publisher identifier in the primary volume descriptor.
	Publisher = "DATARESCUE TESTS"
	// FileName is the only file in the root directory, as the iso reader
	// reports it (the ";1" version suffix stripped).
	FileName = "HELLO.TXT"
	// FileData is that file's contents.
	FileData = "Hello world!"
	// BigName is a second file, several sectors long, so a test can put a hole
	// in the middle of a file rather than only losing the whole of one.
	BigName = "BIG.DAT"
	// BigSectors is how many sectors BigName spans, and BigSize its length: it
	// deliberately does not fill its last sector, so the tail of that sector is
	// not the file's and must never be reported as lost.
	BigSectors = 3
	BigSize    = BigSectors*SectorSize - 300

	// DamagePattern is what Damage writes over a sector it marks unrecovered.
	// It is deliberately neither zero nor a byte BigData produces, so a test
	// can tell "the tool honored the map and zero-filled" from "the tool read
	// the bytes anyway" and from "this is the file's own data".
	DamagePattern = 0xFF

	// pvdLBA, rootLBA, fileLBA and bigLBA are where the image puts each
	// structure.
	pvdLBA  = 16
	rootLBA = 18
	fileLBA = 19
	// BigLBA is where BigName's data starts, which a test damaging one of its
	// sectors has to know.
	BigLBA = 20
)

// Build returns the image: a primary volume descriptor at LBA 16, a terminator,
// and a root directory holding one file. This is the smallest layout iso.Open
// accepts.
func Build() []byte {
	img := make([]byte, Sectors*SectorSize)

	pvd := img[pvdLBA*SectorSize:]
	pvd[0] = 0x01
	copy(pvd[1:6], "CD001")
	pvd[6] = 0x01
	// The volume identifier is a 32-byte space-padded field.
	copy(pvd[40:72], []byte("                                "))
	copy(pvd[40:72], VolumeID)
	putU32both(pvd[80:], Sectors)
	putRoot(pvd[156:], rootLBA, SectorSize)
	pad(pvd[318:446], Publisher)
	copy(pvd[813:], Created.Format("20060102150405")+"00")

	term := img[(pvdLBA+1)*SectorSize:]
	term[0] = 0xFF
	copy(term[1:6], "CD001")
	term[6] = 0x01

	root := img[rootLBA*SectorSize:]
	off := putRecord(root, 0, rootLBA, SectorSize, true, []byte{0x00})
	off = putRecord(root, off, rootLBA, SectorSize, true, []byte{0x01})
	fileRec := off
	off = putRecord(root, off, fileLBA, uint32(len(FileData)), false, []byte(FileName+";1"))
	putRecordTime(root[fileRec+18:], FileTime)
	putRecord(root, off, BigLBA, BigSize, false, []byte(BigName+";1"))
	copy(img[fileLBA*SectorSize:], FileData)
	copy(img[BigLBA*SectorSize:], BigData())

	return img
}

// Created is the volume creation date in the primary volume descriptor, and
// FileTime the recording date on FileName's directory record.
var (
	Created  = time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	FileTime = time.Date(1999, 12, 31, 23, 59, 58, 0, time.UTC)
)

// BigData is BigName's contents. Every byte is between 1 and 251, so neither a
// zero-filled hole nor a sector left holding DamagePattern can be mistaken for
// data the disc held.
func BigData() []byte {
	b := make([]byte, BigSize)
	for i := range b {
		b[i] = byte(1 + i%251)
	}
	return b
}

// BuildRaw returns the same image with each sector framed the way a raw rip
// holds it: a sync pattern, a BCD address, the mode byte, then the 2048 bytes
// of user data. The ECC area is left zero, since nothing that reads an image
// through this package checks it; a test that needs a real EDC should use
// internal/testcd instead.
func BuildRaw() []byte {
	cooked := Build()
	raw := make([]byte, Sectors*RawSectorSize)
	for lba := range Sectors {
		sec := raw[lba*RawSectorSize:]
		sec[0] = 0x00
		for i := 1; i < 11; i++ {
			sec[i] = 0xFF
		}
		sec[11] = 0x00
		m, s2, f := (lba+150)/4500, ((lba+150)/75)%60, (lba+150)%75
		sec[12], sec[13], sec[14] = bcd(m), bcd(s2), bcd(f)
		sec[15] = 0x01 // MODE1
		copy(sec[16:16+SectorSize], cooked[lba*SectorSize:(lba+1)*SectorSize])
	}
	return raw
}

func bcd(n int) byte {
	return byte((n/10)<<4 | n%10) //nolint:gosec // the values are minutes, seconds and frames
}

// WriteRawFile writes BuildRaw's image to path.
func WriteRawFile(path string) error {
	if err := os.WriteFile(path, BuildRaw(), 0o600); err != nil {
		return fmt.Errorf("write raw test iso: %w", err)
	}
	return nil
}

// Damage overwrites the given sectors of img with DamagePattern and returns the
// text of a ddrescue map marking exactly those bytes unrecovered. Writing the
// pattern makes the map testable: a tool that ignores the map returns
// DamagePattern, and one that honors it returns zeros.
func Damage(img []byte, sectorSize int, lbas ...int) string {
	ranges := make([][2]int, 0, len(lbas))
	for _, lba := range lbas {
		ranges = append(ranges, [2]int{lba * sectorSize, (lba + 1) * sectorSize})
	}
	return DamageRange(img, ranges...)
}

// DamageRange is Damage over arbitrary half-open byte ranges, which must be in
// increasing order and must not overlap.
//
// A rescue map's granularity is whatever read size ddrescue used, which on a
// raw image is not a whole 2352-byte sector, so damage inside a sector is the
// ordinary case. It is also what distinguishes a reader counting a sector's
// payload bytes from one counting its raw bytes.
func DamageRange(img []byte, ranges ...[2]int) string {
	var b strings.Builder
	b.WriteString("# Mapfile. Created by testiso.\n")
	b.WriteString("0x00000000     +               1\n")
	b.WriteString("#      Start        Size  Status\n")
	var pos int
	for _, rg := range ranges {
		start, end := rg[0], rg[1]
		for i := start; i < end && i < len(img); i++ {
			img[i] = DamagePattern
		}
		if start > pos {
			fmt.Fprintf(&b, "0x%08x  0x%08x  +\n", pos, start-pos)
		}
		fmt.Fprintf(&b, "0x%08x  0x%08x  -\n", start, end-start)
		pos = end
	}
	if pos < len(img) {
		fmt.Fprintf(&b, "0x%08x  0x%08x  +\n", pos, len(img)-pos)
	}
	return b.String()
}

// WriteFile writes Build's image to path, for handing to a tool or a virtual
// machine that wants a file rather than bytes.
func WriteFile(path string) error {
	if err := os.WriteFile(path, Build(), 0o600); err != nil {
		return fmt.Errorf("write test iso: %w", err)
	}
	return nil
}

// pad fills a descriptor text field with spaces, then writes s at its start.
func pad(dst []byte, s string) {
	for i := range dst {
		dst[i] = ' '
	}
	copy(dst, s)
}

// putRecordTime writes the 7-byte directory record date for a UTC time.
func putRecordTime(dst []byte, t time.Time) {
	//nolint:gosec // every field of a date between 1900 and 2155 fits in a byte
	dst[0], dst[1], dst[2] = byte(t.Year()-1900), byte(t.Month()), byte(t.Day())
	//nolint:gosec // ditto
	dst[3], dst[4], dst[5] = byte(t.Hour()), byte(t.Minute()), byte(t.Second())
	dst[6] = 0 // GMT
}

// putU32both writes an ISO 9660 "both byte orders" 32-bit field: little-endian
// followed by big-endian.
func putU32both(b []byte, v uint32) {
	//nolint:gosec // splitting a uint32 into its four bytes; the truncation is the point
	b[0], b[1], b[2], b[3] = byte(v), byte(v>>8), byte(v>>16), byte(v>>24)
	//nolint:gosec // ditto, in the other byte order
	b[4], b[5], b[6], b[7] = byte(v>>24), byte(v>>16), byte(v>>8), byte(v)
}

// putRoot writes the 34-byte root directory record embedded in the PVD.
func putRoot(b []byte, lba, size uint32) {
	b[0] = 34
	putU32both(b[2:], lba)
	putU32both(b[10:], size)
	b[25] = 0x02 // directory
	b[32] = 1    // name length
	b[33] = 0x00 // the "." identifier
}

// putRecord writes one directory record at offset and returns the offset of the
// next one. Records are padded to an even length.
func putRecord(dst []byte, offset int, lba, size uint32, isDir bool, name []byte) int {
	recLen := 33 + len(name)
	if recLen%2 == 1 {
		recLen++
	}
	r := dst[offset : offset+recLen]
	r[0] = byte(recLen) //nolint:gosec // the caller passes names far shorter than the 255-byte record limit
	putU32both(r[2:], lba)
	putU32both(r[10:], size)
	if isDir {
		r[25] = 0x02
	}
	r[32] = byte(len(name)) //nolint:gosec // ditto
	copy(r[33:], name)
	return offset + recLen
}
