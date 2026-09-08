package dbf

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"
)

// table renders a dBase III table for the tests to read back.
type table struct {
	version    byte
	year       byte
	month      byte
	day        byte
	count      uint32 // 0 means "as many rows as given"
	recordSize uint16 // 0 means "derived from the fields"
	fields     []field
	rows       [][]byte
	terminator byte // 0 means the real 0x0D
	omitEOF    bool
}

type field struct {
	name     string
	typ      byte
	length   byte
	decimals byte
}

func (tb table) bytes() []byte {
	headerLen := headerSize + len(tb.fields)*fieldSize + 1
	recordSize := tb.recordSize
	if recordSize == 0 {
		recordSize = 1
		for _, f := range tb.fields {
			recordSize += uint16(f.length) | uint16(f.decimals)<<8
		}
	}
	count := tb.count
	if count == 0 {
		count = uint32(len(tb.rows))
	}
	version := tb.version
	if version == 0 {
		version = Version3
	}

	header := make([]byte, headerLen)
	header[0] = version
	header[1], header[2], header[3] = tb.year, tb.month, tb.day
	binary.LittleEndian.PutUint32(header[4:], count)
	binary.LittleEndian.PutUint16(header[8:], uint16(headerLen))
	binary.LittleEndian.PutUint16(header[10:], recordSize)
	offset := 1
	for i, f := range tb.fields {
		d := header[headerSize+i*fieldSize:]
		copy(d[:11], f.name)
		d[11] = f.typ
		binary.LittleEndian.PutUint32(d[12:], uint32(offset))
		d[16] = f.length
		d[17] = f.decimals
		offset += int(f.length) | int(f.decimals)<<8
	}
	term := tb.terminator
	if term == 0 {
		term = fieldTerminator
	}
	header[headerLen-1] = term

	out := make([]byte, 0, headerLen+len(tb.rows)*int(recordSize)+1)
	out = append(out, header...)
	for _, r := range tb.rows {
		rec := make([]byte, recordSize)
		copy(rec, r)
		out = append(out, rec...)
	}
	if !tb.omitEOF {
		out = append(out, eofMarker)
	}
	return out
}

func (tb table) open(t *testing.T) *Table {
	t.Helper()
	b := tb.bytes()
	tab, err := Open(bytes.NewReader(b), 0, int64(len(b)))
	if err != nil {
		t.Fatalf("Open: %s", err)
	}
	return tab
}

// sample is the shape of a 1-Step catalog table: a serial, a wide name and a
// timestamp, with a leading deletion flag.
func sample() table {
	return table{
		year: 101, month: 2, day: 3,
		fields: []field{
			{name: "SERIAL", typ: 'N', length: 12},
			{name: "SIZE_LO", typ: 'N', length: 12},
			{name: "NAME", typ: 'C', length: 240},
		},
		rows: [][]byte{
			[]byte(" " + "           0" + "           2" + "                    "),
			[]byte(" " + "           1" + "4096        " + "ATTRIB.EXE"),
		},
	}
}

func TestOpen(t *testing.T) {
	t.Parallel()
	tab := sample().open(t)
	if tab.Version != Version3 {
		t.Errorf("Version = 0x%02x", tab.Version)
	}
	if got := tab.Updated.Format("2006-01-02"); got != "2001-02-03" {
		t.Errorf("Updated = %s, want 2001-02-03", got)
	}
	if tab.RecordSize != 1+12+12+240 {
		t.Errorf("RecordSize = %d", tab.RecordSize)
	}
	if tab.Len() != 2 {
		t.Errorf("Len = %d, want 2", tab.Len())
	}
	if !tab.HasFields("serial", "NAME") {
		t.Error("HasFields did not match, and it should ignore case")
	}
	if tab.HasFields("SERIAL", "NOPE") {
		t.Error("HasFields matched a field that is not there")
	}
}

func TestRecordValues(t *testing.T) {
	t.Parallel()
	tab := sample().open(t)
	r, err := tab.Record(1)
	if err != nil {
		t.Fatalf("Record: %s", err)
	}
	if r.Deleted() {
		t.Error("the record reads as deleted")
	}
	if got, err := r.String("NAME"); err != nil || got != "ATTRIB.EXE" {
		t.Errorf("String(NAME) = %q, %v", got, err)
	}
	// SERIAL is right-justified and SIZE_LO left-justified, which is how the
	// catalog really stores them.
	if got, err := r.Int("SERIAL"); err != nil || got != 1 {
		t.Errorf("Int(SERIAL) = %d, %v", got, err)
	}
	if got, err := r.Int("SIZE_LO"); err != nil || got != 4096 {
		t.Errorf("Int(SIZE_LO) = %d, %v", got, err)
	}
	if _, err := r.Int("MISSING"); err == nil {
		t.Error("Int of an absent field succeeded")
	}
}

// TestBlankNumericIsZero covers the control record, whose character fields are
// blank and whose unused numeric fields the catalog leaves empty.
func TestBlankNumericIsZero(t *testing.T) {
	t.Parallel()
	tb := sample()
	tb.rows = [][]byte{[]byte(" " + strings.Repeat(" ", 12+12+240))}
	tab := tb.open(t)
	r, err := tab.Record(0)
	if err != nil {
		t.Fatalf("Record: %s", err)
	}
	if got, err := r.Int("SERIAL"); err != nil || got != 0 {
		t.Errorf("Int of a blank field = %d, %v; want 0", got, err)
	}
	if got, err := r.String("NAME"); err != nil || got != "" {
		t.Errorf("String of a blank field = %q, %v", got, err)
	}
}

// TestWideCharacterField covers the length that does not fit in one byte: the
// catalog's DESCR field is 256 bytes, with the high byte where the decimal
// count would be. Reading only the low byte gives it length 0, and then the
// record length no longer matches.
func TestWideCharacterField(t *testing.T) {
	t.Parallel()
	tb := table{
		fields: []field{
			{name: "SERIAL", typ: 'N', length: 12},
			{name: "DESCR", typ: 'C', length: 0, decimals: 1},
		},
		rows: [][]byte{[]byte(" " + "           1" + "a description")},
	}
	tab := tb.open(t)
	if got := tab.Fields[1].Length; got != 256 {
		t.Fatalf("DESCR length = %d, want 256", got)
	}
	if tab.RecordSize != 1+12+256 {
		t.Errorf("RecordSize = %d, want %d", tab.RecordSize, 1+12+256)
	}
	r, err := tab.Record(0)
	if err != nil {
		t.Fatalf("Record: %s", err)
	}
	if got, err := r.String("DESCR"); err != nil || got != "a description" {
		t.Errorf("String(DESCR) = %q, %v", got, err)
	}
}

func TestDeletedRecord(t *testing.T) {
	t.Parallel()
	tb := sample()
	tb.rows = [][]byte{[]byte("*" + "           1" + "           0" + "GONE.TXT")}
	tab := tb.open(t)
	r, err := tab.Record(0)
	if err != nil {
		t.Fatalf("Record: %s", err)
	}
	if !r.Deleted() {
		t.Error("a record flagged deleted does not read as deleted")
	}
	// dBase leaves the bytes in place, so the name is still recoverable.
	if got, _ := r.String("NAME"); got != "GONE.TXT" {
		t.Errorf("String(NAME) = %q", got)
	}
}

// TestTruncatedTable checks that a record count larger than the bytes present
// is clamped, since it is used to address them.
func TestTruncatedTable(t *testing.T) {
	t.Parallel()
	tb := sample()
	tb.count = 1000
	b := tb.bytes()
	tab, err := Open(bytes.NewReader(b), 0, int64(len(b)))
	if err != nil {
		t.Fatalf("Open: %s", err)
	}
	if tab.DeclaredLen() != 1000 {
		t.Errorf("DeclaredLen = %d, want the header's 1000", tab.DeclaredLen())
	}
	if tab.Len() != 2 {
		t.Errorf("Len = %d, want the 2 rows that fit", tab.Len())
	}
	if _, err := tab.Record(2); !errors.Is(err, io.EOF) {
		t.Errorf("Record past the end returned %v, want io.EOF", err)
	}
}

// TestUnreadableRecord checks that a read failure inside a table costs its own
// row and is recorded, rather than ending the table.
func TestUnreadableRecord(t *testing.T) {
	t.Parallel()
	tb := sample()
	tb.rows = append(tb.rows, tb.rows[1], tb.rows[1])
	b := tb.bytes()
	recordStart := int64(headerSize + len(tb.fields)*fieldSize + 1)
	bad := recordStart + int64(tb.open(t).RecordSize) // the second row

	r := &faultReader{b: b, from: bad, to: bad + 8}
	tab, err := Open(r, 0, int64(len(b)))
	if err != nil {
		t.Fatalf("Open: %s", err)
	}
	var ok, failed int
	for i := range tab.Len() {
		if _, err := tab.Record(i); err != nil {
			failed++
		} else {
			ok++
		}
	}
	if failed != 1 {
		t.Errorf("%d rows failed, want 1", failed)
	}
	if ok != tab.Len()-1 {
		t.Errorf("%d rows read, want %d", ok, tab.Len()-1)
	}
	if got := tab.Unreadable(); len(got) != 1 || got[0] != 1 {
		t.Errorf("Unreadable = %v, want [1]", got)
	}
}

func TestRejectsNonTables(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(table) table
		raw    []byte
	}{
		{name: "empty", raw: nil},
		{name: "short", raw: []byte{Version3, 1, 2, 3}},
		{name: "wrong version", mutate: func(tb table) table { tb.version = 0x30; return tb }},
		{name: "no field terminator", mutate: func(tb table) table { tb.terminator = 0xFF; return tb }},
		{name: "record size disagrees with the fields", mutate: func(tb table) table { tb.recordSize = 99; return tb }},
		{name: "unsupported field type", mutate: func(tb table) table {
			tb.fields[1].typ = 'M'
			return tb
		}},
		{name: "zero-length field", mutate: func(tb table) table {
			tb.fields[0].length = 0
			return tb
		}},
		{name: "no fields", mutate: func(tb table) table { tb.fields = nil; return tb }},
	}
	for _, tc := range tests {
		b := tc.raw
		if tc.mutate != nil {
			b = tc.mutate(sample()).bytes()
		}
		if _, err := Open(bytes.NewReader(b), 0, int64(len(b))); !errors.Is(err, ErrNotTable) {
			t.Errorf("%s: Open returned %v, want ErrNotTable", tc.name, err)
		}
	}
}

// faultReader fails reads that begin inside a byte range, standing in for a
// sector the medium would not give up.
type faultReader struct {
	b        []byte
	from, to int64
}

func (f *faultReader) ReadAt(p []byte, off int64) (int, error) {
	if off < f.to && off+int64(len(p)) > f.from {
		return 0, errors.New("injected read error")
	}
	return bytes.NewReader(f.b).ReadAt(p, off)
}

// A field's position in a record is the lengths of the fields before it, so a
// declared displacement that disagrees with them reads every value from there
// on out of the wrong bytes. Validating only the record total lets a field
// point past its own record, which is what the fuzzer found.
func TestFieldDisplacementMustFollowTheLengths(t *testing.T) {
	t.Parallel()
	tb := sample()
	b := tb.bytes()

	// The second field's displacement is 13: the deletion flag plus SERIAL.
	// Anything else contradicts the lengths ahead of it, 0 excepted — that is
	// how a writer spells "unset".
	const second = headerSize + fieldSize
	for _, bad := range []uint32{12, 14, 1 << 20} {
		binary.LittleEndian.PutUint32(b[second+12:], bad)
		if _, err := Open(bytes.NewReader(b), 0, int64(len(b))); !errors.Is(err, ErrNotTable) {
			t.Errorf("a displacement of %d was accepted where the lengths before it total 13: %v", bad, err)
		}
	}

	// Some writers leave the displacement unset throughout, which is not a
	// disagreement with anything and has to keep working.
	for i := range tb.fields {
		binary.LittleEndian.PutUint32(b[headerSize+i*fieldSize+12:], 0)
	}
	if _, err := Open(bytes.NewReader(b), 0, int64(len(b))); err != nil {
		t.Errorf("a table whose displacements are all unset was refused: %s", err)
	}
}
