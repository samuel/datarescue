// Package dbf reads dBase III tables.
//
// The catalog of an Iomega 1-Step backup is a container of these, which is why
// the reader takes an offset and a size rather than a file name: a table here
// is a region inside a larger image, not a file of its own.
//
// Records are read on demand rather than up front. A table coming off damaged
// media is usually mostly intact, so a record that cannot be read is recorded
// and skipped, and only that record is lost.
//
// Format reference:
//
//	https://www.clicketyclick.dk/databases/xbase/format/dbf.html
package dbf

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Version3 is the dBase III table version byte, the only one the 1-Step
// catalog uses. Tables with memo fields declare a different version and carry
// their data in a separate file, which a container member cannot reference.
const Version3 = 0x03

const (
	headerSize      = 32   // fixed header, before the field descriptors
	fieldSize       = 32   // one field descriptor
	fieldTerminator = 0x0D // ends the descriptor list
	eofMarker       = 0x1A // ends the records
	maxFields       = 255  // a record cannot be longer than 65535 bytes anyway
)

// ErrNotTable reports that the bytes at the given offset are not a dBase III
// table, so a caller scanning an image for one can tell that from an I/O
// failure.
var ErrNotTable = errors.New("dbf: not a dBase III table")

// Type is a field's data type. The 1-Step catalog uses only these two, and
// both store their value as fixed-width ASCII.
type Type byte

const (
	TypeNumeric   Type = 'N'
	TypeCharacter Type = 'C'
)

func (t Type) String() string { return string(rune(t)) }

// Field describes one column.
type Field struct {
	Name string
	Type Type
	// Offset is the field's start within a record, counted from the record's
	// first byte. It is at least 1, since byte 0 is the deletion flag.
	Offset int
	// Length is the field's width in bytes.
	Length int
}

// Table is a decoded table header plus the means to read its records.
type Table struct {
	Version byte
	// headerLen is the fixed header plus the field descriptors and their
	// terminator, kept so Size can report the whole table's extent.
	headerLen int
	// Updated is the last-modified date from the header. It is the zero time
	// when the header's date is not a real one.
	Updated    time.Time
	RecordSize int
	Fields     []Field

	r          io.ReaderAt
	dataOffset int64
	count      int
	declared   int

	mu sync.Mutex
	// unreadable samples the records that could not be read, and numUnreadable
	// counts them all. A table over a bad region fails one record per
	// unrecovered sector, so the list is a sample for a report and never an
	// inventory.
	unreadable    []int
	numUnreadable int
}

// maxUnreadable bounds the sample Unreadable returns.
const maxUnreadable = 64

// PlausibleHeader reports whether b could begin a table, testing only the
// fixed header and without any I/O. It is for scanning an image for tables:
// the full parse costs several reads, so a caller sweeping every byte position
// needs a cheap way to discard almost all of them first. A true answer does
// not mean Open will succeed.
func PlausibleHeader(b []byte) bool {
	if len(b) < headerSize || b[0] != Version3 {
		return false
	}
	if b[2] < 1 || b[2] > 12 || b[3] < 1 || b[3] > 31 {
		return false
	}
	headerLen := int(binary.LittleEndian.Uint16(b[8:]))
	if headerLen < headerSize+fieldSize+1 || (headerLen-headerSize-1)%fieldSize != 0 {
		return false
	}
	if (headerLen-headerSize-1)/fieldSize > maxFields {
		return false
	}
	return binary.LittleEndian.Uint16(b[10:]) >= 1
}

// Open decodes the table header at offset. Size bounds the table, so a record
// count corrupted into the billions cannot make the reader address bytes past
// the region the caller gave it.
func Open(r io.ReaderAt, offset, size int64) (*Table, error) {
	if offset < 0 || size < headerSize {
		return nil, ErrNotTable
	}
	hdr := make([]byte, headerSize)
	if _, err := r.ReadAt(hdr, offset); err != nil {
		return nil, fmt.Errorf("dbf: read header at %d: %w", offset, err)
	}
	if hdr[0] != Version3 {
		return nil, fmt.Errorf("%w: version 0x%02x at %d", ErrNotTable, hdr[0], offset)
	}

	headerLen := int(binary.LittleEndian.Uint16(hdr[8:]))
	recordSize := int(binary.LittleEndian.Uint16(hdr[10:]))
	// The descriptor list is a whole number of descriptors plus the terminator.
	if headerLen < headerSize+fieldSize+1 || (headerLen-headerSize-1)%fieldSize != 0 {
		return nil, fmt.Errorf("%w: header length %d at %d", ErrNotTable, headerLen, offset)
	}
	nfields := (headerLen - headerSize - 1) / fieldSize
	if nfields > maxFields || int64(headerLen) > size || recordSize < 1 {
		return nil, fmt.Errorf("%w: %d fields, record size %d at %d", ErrNotTable, nfields, recordSize, offset)
	}

	descs := make([]byte, headerLen-headerSize)
	if _, err := r.ReadAt(descs, offset+headerSize); err != nil {
		return nil, fmt.Errorf("dbf: read field descriptors at %d: %w", offset, err)
	}
	if descs[len(descs)-1] != fieldTerminator {
		return nil, fmt.Errorf("%w: no field terminator at %d", ErrNotTable, offset)
	}

	fields := make([]Field, 0, nfields)
	total := 1 // the deletion flag
	for i := range nfields {
		f, err := parseField(descs[i*fieldSize:])
		if err != nil {
			return nil, fmt.Errorf("%w: field %d at %d: %w", ErrNotTable, i+1, offset, err)
		}
		// A field's position is the lengths before it, so a declared offset
		// that says otherwise would read the wrong bytes for this field and
		// every field after it. Some writers leave the displacement unset, so
		// it is checked only when it is there.
		if f.Offset != 0 && f.Offset != total {
			return nil, fmt.Errorf("%w: field %s at %d starts at %d, %d expected",
				ErrNotTable, f.Name, offset, f.Offset, total)
		}
		f.Offset = total
		total += f.Length
		fields = append(fields, f)
	}
	// A field whose declared offset does not follow the preceding lengths would
	// make every value after it come from the wrong bytes, so disagreement here
	// means the descriptors are not what they claim to be.
	if total != recordSize {
		return nil, fmt.Errorf("%w: fields total %d bytes, record size %d at %d", ErrNotTable, total, recordSize, offset)
	}

	t := &Table{
		Version:    hdr[0],
		headerLen:  headerLen,
		Updated:    parseUpdated(hdr[1:4]),
		RecordSize: recordSize,
		Fields:     fields,
		r:          r,
		dataOffset: offset + int64(headerLen),
		declared:   int(binary.LittleEndian.Uint32(hdr[4:])),
	}
	t.count = t.declared
	if avail := int((size - int64(headerLen)) / int64(recordSize)); t.count > avail {
		t.count = max(avail, 0)
	}
	return t, nil
}

func parseField(b []byte) (Field, error) {
	name := strings.TrimRight(string(b[:11]), "\x00 ")
	if name == "" {
		return Field{}, errors.New("empty name")
	}
	f := Field{
		Name:   name,
		Type:   Type(b[11]),
		Offset: int(binary.LittleEndian.Uint32(b[12:])),
		Length: int(b[16]),
	}
	switch f.Type {
	case TypeNumeric:
	case TypeCharacter:
		// A character field longer than 255 bytes puts the high byte of its
		// length where the decimal count would be. IOJOB.DESCR is 256 bytes
		// this way, and reading only the low byte gives it length 0.
		f.Length |= int(b[17]) << 8
	default:
		return Field{}, fmt.Errorf("unsupported type %q", rune(b[11]))
	}
	if f.Length < 1 || f.Offset < 0 {
		return Field{}, fmt.Errorf("length %d at offset %d", f.Length, f.Offset)
	}
	return f, nil
}

// parseUpdated decodes the header's YY/MM/DD date. The year is an offset from
// 1900, so a 2024 table stores 124.
func parseUpdated(b []byte) time.Time {
	month, day := int(b[1]), int(b[2])
	if month < 1 || month > 12 || day < 1 || day > 31 {
		return time.Time{}
	}
	return time.Date(1900+int(b[0]), time.Month(month), day, 0, 0, 0, 0, time.UTC)
}

// Len returns the number of records the table holds, which is the count from
// the header clamped to the bytes actually available.
func (t *Table) Len() int { return t.count }

// Start returns the byte offset of the table's first byte, so a caller
// scanning a larger image can bound one candidate by the next table it already
// holds: nothing real can sit between the two but the candidate itself.
func (t *Table) Start() int64 { return t.dataOffset - int64(t.headerLen) }

// Size returns how many bytes the whole table occupies, so a caller scanning
// an image can step over it in one move.
func (t *Table) Size() int64 {
	return int64(t.headerLen) + int64(t.count)*int64(t.RecordSize) + 1 // the EOF marker
}

// DeclaredLen returns the record count the header claims, before clamping. A
// caller can compare it against Len to notice a truncated table.
func (t *Table) DeclaredLen() int { return t.declared }

// Unreadable returns the indexes of the records that could not be read, in the
// order they were attempted, capped at a sample. UnreadableCount is how many
// there were.
func (t *Table) Unreadable() []int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.unreadable
}

// UnreadableCount is how many record reads failed, which is not len(Unreadable)
// once the sample is full.
func (t *Table) UnreadableCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.numUnreadable
}

// Record reads record i, numbered from 0. Reading past Len returns io.EOF.
func (t *Table) Record(i int) (Record, error) {
	if i < 0 || i >= t.count {
		return Record{}, io.EOF
	}
	buf := make([]byte, t.RecordSize)
	if _, err := t.r.ReadAt(buf, t.dataOffset+int64(i)*int64(t.RecordSize)); err != nil {
		t.mu.Lock()
		t.numUnreadable++
		if len(t.unreadable) < maxUnreadable {
			t.unreadable = append(t.unreadable, i)
		}
		t.mu.Unlock()
		return Record{}, fmt.Errorf("dbf: read record %d: %w", i, err)
	}
	return Record{fields: t.Fields, data: buf}, nil
}

// Record is one decoded row. Its fields are addressed by name, since the
// on-disk order is up to the writer and identifying a table by its field
// names is more robust than trusting a position.
type Record struct {
	fields []Field
	data   []byte
}

// Deleted reports whether the record is marked deleted. dBase does not remove
// a deleted record, it flags it, so the bytes are still there to recover.
func (r Record) Deleted() bool { return len(r.data) > 0 && r.data[0] == '*' }

// Raw returns a field's bytes as stored, without trimming.
func (r Record) Raw(name string) ([]byte, error) {
	for _, f := range r.fields {
		if !strings.EqualFold(f.Name, name) {
			continue
		}
		end := f.Offset + f.Length
		if f.Offset >= len(r.data) || end > len(r.data) {
			return nil, fmt.Errorf("dbf: field %s runs past the record", name)
		}
		return r.data[f.Offset:end], nil
	}
	return nil, fmt.Errorf("dbf: no field %s", name)
}

// String returns a field's value with the padding removed. Values are padded
// with spaces, and a name written by a program that used NUL instead should
// not come back with the NUL attached.
func (r Record) String(name string) (string, error) {
	b, err := r.Raw(name)
	if err != nil {
		return "", err
	}
	return strings.Trim(string(b), "\x00 "), nil
}

// Int returns a numeric field's value. The padding is not consistent across
// the 1-Step catalog — SERIAL is right-justified while SIZE_LO is left — so
// both sides are trimmed rather than one. A blank field is 0, which is what
// the catalog means by it.
func (r Record) Int(name string) (int64, error) {
	s, err := r.String(name)
	if err != nil {
		return 0, err
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("dbf: field %s: %w", name, err)
	}
	return v, nil
}

// HasFields reports whether the table has a field of each of the given names,
// which is how a caller identifies which table it is looking at.
func (t *Table) HasFields(names ...string) bool {
	for _, n := range names {
		var found bool
		for _, f := range t.Fields {
			if strings.EqualFold(f.Name, n) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
