package appledouble

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"
	"time"
)

func decode(t *testing.T, b []byte) map[uint32][2]uint32 {
	t.Helper()
	if got := binary.BigEndian.Uint32(b[0:4]); got != magic {
		t.Fatalf("magic = %#08x; want %#08x", got, magic)
	}
	if got := binary.BigEndian.Uint32(b[4:8]); got != version {
		t.Fatalf("version = %#08x; want %#08x — version 2 is what macOS reads", got, version)
	}
	n := int(binary.BigEndian.Uint16(b[24:26]))
	out := make(map[uint32][2]uint32, n)
	for i := range n {
		off := 26 + i*12
		id := binary.BigEndian.Uint32(b[off : off+4])
		out[id] = [2]uint32{
			binary.BigEndian.Uint32(b[off+4 : off+8]),
			binary.BigEndian.Uint32(b[off+8 : off+12]),
		}
	}
	return out
}

func TestHeaderCarriesTheFinderInfoAndDates(t *testing.T) {
	var fi [32]byte
	copy(fi[0:4], "STAK")
	copy(fi[4:8], "WILD")
	h := Header{
		FinderInfo: fi,
		Created:    time.Date(1988, 10, 29, 20, 53, 40, 0, time.UTC),
		Modified:   time.Date(1989, 8, 1, 20, 33, 39, 0, time.UTC),
		RsrcSize:   182483,
	}
	b := h.Bytes()
	ents := decode(t, b)

	fin, ok := ents[idFinderInfo]
	if !ok {
		t.Fatal("no Finder info entry")
	}
	if fin[1] != 32 {
		t.Errorf("Finder info is %d bytes; want 32", fin[1])
	}
	if got := b[fin[0] : fin[0]+4]; string(got) != "STAK" {
		t.Errorf("Finder info starts %q; want the type code", got)
	}
	if got := b[fin[0]+4 : fin[0]+8]; string(got) != "WILD" {
		t.Errorf("Finder info continues %q; want the creator code", got)
	}

	dates, ok := ents[idFileDates]
	if !ok || dates[1] != 16 {
		t.Fatalf("file dates entry = %v; want 16 bytes", dates)
	}
	// Dates are signed seconds from 2000, so a 1988 file's is negative.
	created := int32(binary.BigEndian.Uint32(b[dates[0] : dates[0]+4])) //nolint:gosec // the format's own encoding
	if created >= 0 {
		t.Errorf("a 1988 date encoded as %d; AppleDouble counts from 2000, so it must be negative", created)
	}
	if want := int32(h.Created.Unix() - unixAt2000); created != want { //nolint:gosec // the format's own encoding
		t.Errorf("created = %d; want %d", created, want)
	}
	// A time that was never recorded is written as zero, which the format
	// defines as "unknown", rather than as the year 2000.
	backup := binary.BigEndian.Uint32(b[dates[0]+8 : dates[0]+12])
	if backup != 0 {
		t.Errorf("an unrecorded backup date encoded as %d; want 0", backup)
	}
}

// The resource fork is declared at its recorded length and comes last, so
// everything before it is a fixed-length prefix.
func TestTheResourceForkComesLastAtItsRecordedLength(t *testing.T) {
	h := Header{RsrcSize: 1234}
	b := h.Bytes()
	ents := decode(t, b)
	rf, ok := ents[idResourceFork]
	if !ok {
		t.Fatal("no resource fork entry")
	}
	if rf[1] != 1234 {
		t.Errorf("resource fork declared %d bytes; want 1234", rf[1])
	}
	if int64(rf[0]) != h.Size() {
		t.Errorf("resource fork starts at %d; want %d, the end of the header", rf[0], h.Size())
	}
	for id, e := range ents {
		if id != idResourceFork && int64(e[0]) >= h.Size() {
			t.Errorf("entry %d starts at %d, past the header; the fork must come last", id, e[0])
		}
	}
}

// Size is the offset every hole in the fork is shifted by, so it must match
// where the fork's first byte is actually written.
func TestSizeIsWhereTheForkBegins(t *testing.T) {
	h := Header{RsrcSize: 8, RealName: "Some Name"}
	body := []byte("12345678")
	got, err := io.ReadAll(h.Reader(bytes.NewReader(body)))
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(got)) != h.Size()+int64(len(body)) {
		t.Fatalf("wrote %d bytes; want the header plus the fork", len(got))
	}
	if !bytes.Equal(got[h.Size():], body) {
		t.Errorf("the fork does not begin at Size(): %q", got[h.Size():])
	}
}

// The volume's spelling of the name is kept when it differs from the name the
// file is written under; otherwise only the sanitized name would remain.
func TestRealNameIsWrittenOnlyWhenItIsNeeded(t *testing.T) {
	with := decode(t, Header{RealName: "10/20/99 Budget"}.Bytes())
	if _, ok := with[idRealName]; !ok {
		t.Error("the real name was not written")
	}
	without := decode(t, Header{}.Bytes())
	if _, ok := without[idRealName]; ok {
		t.Error("an empty real name was written as an entry")
	}
}

func TestSidecarName(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"README", "._README"},
		{"dir/file.txt", "dir/._file.txt"},
		{"a/b/c", "a/b/._c"},
	} {
		if got := SidecarName(tc.in); got != tc.want {
			t.Errorf("SidecarName(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
}
