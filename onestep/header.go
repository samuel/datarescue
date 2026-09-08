package onestep

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"time"

	"samuelks.com/datarescue/internal/compressed"
)

// HeaderSize is the fixed header at the start of every *.1-Step file. File
// data begins immediately after it.
const HeaderSize = 0x200

const signature = 0xABCDABCD

// ErrNot1Step reports that a file does not carry the 1-Step signature, so a
// caller probing a file can tell that from an I/O failure.
var ErrNot1Step = errors.New("onestep: not a 1-Step backup file")

// Header is the per-disk file header.
type Header struct {
	// Job is the backup job number, assigned by the machine the backup ran on.
	Job int
	// Disk is this file's position in the backup set, counted from 1.
	Disk int
	// Created identifies the set: every disk of one backup carries the same
	// value, and it matches the timestamp in the name the program gave the file.
	Created time.Time
	// CatalogOffset is where the catalog starts, or 0 when this disk carries
	// none. Only the last disk of a set has one.
	CatalogOffset int64
	// Description is the text the operator typed when starting the backup.
	Description string
}

// ReadHeader reads just the header of one disk image. Every disk of a set
// carries the job number, the set timestamp and its own position, so a caller
// holding a directory of images can sort them into sets with this. Open cannot
// do that, since it needs the catalog and only the last disk has one.
func ReadHeader(path string) (Header, error) {
	f, err := os.Open(path)
	if err != nil {
		return Header{}, fmt.Errorf("onestep: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only

	b := make([]byte, HeaderSize)
	if _, err := f.ReadAt(b, 0); err != nil {
		return Header{}, fmt.Errorf("onestep: %s: read header: %w", path, err)
	}
	h, err := parseHeader(b)
	if err != nil {
		return Header{}, fmt.Errorf("%s: %w", path, err)
	}
	return h, nil
}

func parseHeader(b []byte) (Header, error) {
	if len(b) < HeaderSize {
		return Header{}, fmt.Errorf("%w: short header", ErrNot1Step)
	}
	if binary.LittleEndian.Uint32(b) != signature {
		return Header{}, fmt.Errorf("%w: signature 0x%08x%s", ErrNot1Step, binary.LittleEndian.Uint32(b), compressed.Hint(b))
	}
	h := Header{
		Created:       oleTime(math.Float64frombits(binary.LittleEndian.Uint64(b[0x0c:]))),
		Job:           int(binary.LittleEndian.Uint16(b[0x18:])),
		Disk:          int(binary.LittleEndian.Uint16(b[0x1a:])),
		CatalogOffset: int64(binary.LittleEndian.Uint32(b[0x1c:])),
		Description:   cstring(b[0x34:HeaderSize]),
	}
	return h, nil
}

// oleTime decodes an OLE automation date: a float64 count of days since
// 1899-12-30. The reference documentation never worked this field out, but it
// reproduces the timestamp in the file's own name to the second.
func oleTime(days float64) time.Time {
	// Reject a value that is not a date at all rather than returning some
	// point tens of thousands of years away from a corrupt header.
	if math.IsNaN(days) || days <= 0 || days > 1e6 {
		return time.Time{}
	}
	const secondsPerDay = 24 * 60 * 60
	epoch := time.Date(1899, time.December, 30, 0, 0, 0, 0, time.UTC)
	return epoch.Add(time.Duration(math.Round(days*secondsPerDay)) * time.Second)
}

// cstring returns the NUL-terminated string at the start of b, or all of b
// when it has no terminator.
func cstring(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}
