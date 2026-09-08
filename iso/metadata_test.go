package iso

import (
	"encoding/binary"
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"
)

func TestParseRecordTime(t *testing.T) {
	t.Parallel()
	plus1 := time.FixedZone("", 3600)
	cases := []struct {
		name    string
		in      []byte
		want    time.Time
		wantErr bool
	}{
		{"unset", make([]byte, 7), time.Time{}, false},
		{"utc", []byte{99, 12, 31, 23, 59, 58, 0}, time.Date(1999, 12, 31, 23, 59, 58, 0, time.UTC), false},
		{"offset", []byte{124, 2, 29, 8, 0, 0, 4}, time.Date(2024, 2, 29, 8, 0, 0, 0, plus1), false},
		{"negative offset", []byte{100, 1, 1, 0, 0, 0, 0xEC}, time.Date(2000, 1, 1, 5, 0, 0, 0, time.UTC), false},
		{"month 13", []byte{99, 13, 1, 0, 0, 0, 0}, time.Time{}, true},
		{"february 30", []byte{99, 2, 30, 0, 0, 0, 0}, time.Time{}, true},
		{"february 29 in a common year", []byte{123, 2, 29, 0, 0, 0, 0}, time.Time{}, true},
		{"day 0", []byte{99, 1, 0, 0, 0, 0, 0}, time.Time{}, true},
		{"hour 24", []byte{99, 1, 1, 24, 0, 0, 0}, time.Time{}, true},
		{"offset past GMT+13", []byte{99, 1, 1, 0, 0, 0, 53}, time.Time{}, true},
		{"short", []byte{99, 1}, time.Time{}, true},
	}
	for _, c := range cases {
		got, err := parseRecordTime(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: err = %v, want error %v", c.name, err, c.wantErr)
		}
		if !got.Equal(c.want) || got.IsZero() != c.want.IsZero() {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestParseVolumeTime(t *testing.T) {
	t.Parallel()
	vt := func(digits string, off int8) []byte {
		return append([]byte(digits), byte(off))
	}
	cases := []struct {
		name    string
		in      []byte
		want    time.Time
		wantErr bool
	}{
		{"zero bytes", make([]byte, 17), time.Time{}, false},
		{"zero digits", vt("0000000000000000", 0), time.Time{}, false},
		{"utc", vt("2001090912345678", 0), time.Date(2001, 9, 9, 12, 34, 56, 780*int(time.Millisecond), time.UTC), false},
		{"offset", vt("2001090912000000", -20), time.Date(2001, 9, 9, 17, 0, 0, 0, time.UTC), false},
		{"spaces", vt("                ", 0), time.Time{}, true},
		{"month 00", vt("2001000912000000", 0), time.Time{}, true},
		{"year 0", vt("0000010100000000", 0), time.Time{}, true},
		{"seconds 61", vt("2001010100006100", 0), time.Time{}, true},
	}
	for _, c := range cases {
		got, err := parseVolumeTime(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: err = %v, want error %v", c.name, err, c.wantErr)
		}
		if !got.Equal(c.want) || got.IsZero() != c.want.IsZero() {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// walkAll returns every entry by path.
func walkAll(t *testing.T, r *Reader) map[string]DirEntry {
	t.Helper()
	out := map[string]DirEntry{}
	if err := r.Walk(func(e DirEntry) error {
		out[e.Path] = e
		return nil
	}); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	return out
}

func openBuilt(t *testing.T, b *isoBuilder) *Reader {
	t.Helper()
	path := b.write(t.Name())
	t.Cleanup(func() { _ = os.Remove(path) })
	r, err := openPath(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return r
}

func hasIssue(r *Reader, substr string) bool {
	for _, s := range r.Issues() {
		if strings.Contains(s, substr) {
			return true
		}
	}
	return false
}

func TestRecordDates(t *testing.T) {
	t.Parallel()
	b := newISOBuilder()
	b.buildSetTerminator(b.alloc())
	rootLBA := b.alloc()
	root := b.sector(rootLBA)
	off := putDirRecord(root, 0, uint32(rootLBA), 2048, true, []byte{0x00})
	off = putDirRecord(root, off, uint32(rootLBA), 2048, true, []byte{0x01})

	dated := off
	off = putDirRecord(root, off, 0, 0, false, []byte("DATED.TXT;1"))
	copy(root[dated+18:], []byte{95, 6, 15, 10, 30, 0, 8}) // 1995-06-15 10:30 GMT+2
	undated := off
	off = putDirRecord(root, off, 0, 0, false, []byte("UNDATED.TXT;1"))
	_ = undated
	damaged := off
	putDirRecord(root, off, 0, 0, false, []byte("DAMAGED.TXT;1"))
	copy(root[damaged+18:], []byte{95, 14, 15, 10, 30, 0, 0})
	b.buildPVD(uint32(rootLBA), 2048, "DATES")

	r := openBuilt(t, b)
	got := walkAll(t, r)

	want := time.Date(1995, 6, 15, 8, 30, 0, 0, time.UTC)
	if e := got["/DATED.TXT"]; !e.ModTime.Equal(want) {
		t.Errorf("DATED.TXT ModTime = %v, want %v", e.ModTime, want)
	}
	if e := got["/UNDATED.TXT"]; !e.ModTime.IsZero() {
		t.Errorf("UNDATED.TXT ModTime = %v, want zero", e.ModTime)
	}
	if e := got["/DAMAGED.TXT"]; !e.ModTime.IsZero() {
		t.Errorf("DAMAGED.TXT ModTime = %v, want zero", e.ModTime)
	}
	if !hasIssue(r, "/DAMAGED.TXT: recording date is damaged") {
		t.Errorf("no issue for the damaged date: %q", r.Issues())
	}
}

func TestNameWithoutExtension(t *testing.T) {
	t.Parallel()
	b := newISOBuilder()
	b.buildSetTerminator(b.alloc())
	rootLBA := b.alloc()
	root := b.sector(rootLBA)
	off := putDirRecord(root, 0, uint32(rootLBA), 2048, true, []byte{0x00})
	off = putDirRecord(root, off, uint32(rootLBA), 2048, true, []byte{0x01})
	off = putDirRecord(root, off, 0, 0, false, []byte("README.;1"))
	putDirRecord(root, off, 0, 0, false, []byte("NOVERS."))
	b.buildPVD(uint32(rootLBA), 2048, "NAMES")

	got := walkAll(t, openBuilt(t, b))
	for _, p := range []string{"/README", "/NOVERS"} {
		if _, ok := got[p]; !ok {
			t.Errorf("no entry %s; got %v", p, keys(got))
		}
	}
}

// A Joliet name is left as the disc spells it, trailing dot included.
func TestJolietNameKeepsTrailingDot(t *testing.T) {
	t.Parallel()
	b := newISOBuilder()
	svdLBA := b.alloc()
	b.buildSetTerminator(b.alloc())
	isoRoot := b.alloc()
	putDirRecord(b.sector(isoRoot), 0, uint32(isoRoot), 2048, true, []byte{0x00})
	jRoot := b.alloc()
	jr := b.sector(jRoot)
	off := putDirRecord(jr, 0, uint32(jRoot), 2048, true, []byte{0x00})
	off = putDirRecord(jr, off, uint32(jRoot), 2048, true, []byte{0x01})
	putDirRecord(jr, off, 0, 0, false, encodeUCS2("notes.;1"))
	b.buildJolietSVD(svdLBA, uint32(jRoot), 2048, "J")
	b.buildPVD(uint32(isoRoot), 2048, "J")

	got := walkAll(t, openBuilt(t, b))
	if _, ok := got["/notes."]; !ok {
		t.Errorf("no entry /notes.; got %v", keys(got))
	}
}

func keys(m map[string]DirEntry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestVolumeInfo(t *testing.T) {
	t.Parallel()
	b := newISOBuilder()
	svdLBA := b.alloc()
	b.buildSetTerminator(b.alloc())
	rootLBA := b.alloc()
	putDirRecord(b.sector(rootLBA), 0, uint32(rootLBA), 2048, true, []byte{0x00})
	b.buildPVD(uint32(rootLBA), 2048, "PLAINLABEL")
	b.buildJolietSVD(svdLBA, uint32(rootLBA), 2048, "Joliet Label")

	pvd := b.sector(16)
	pad := func(dst []byte, s string) {
		for i := range dst {
			dst[i] = ' '
		}
		copy(dst, s)
	}
	pad(pvd[318:446], "PLAIN PUBLISHER")
	pad(pvd[446:574], "PLAIN PREPARER")
	pad(pvd[574:702], "PLAIN APP")
	copy(pvd[813:], append([]byte("1998070412000000"), 0))
	copy(pvd[830:], append([]byte("1998070513000000"), 4))
	copy(pvd[847:], append([]byte("0000000000000000"), 0)) // unset, as digits
	copy(pvd[864:], append([]byte("19981341"), make([]byte, 9)...))

	svd := b.sector(svdLBA)
	copy(svd[318:446], padUCS2("Jöliet Publisher", 128))
	// The preparer is blank in Joliet, so the plain one is used.
	copy(svd[446:574], padUCS2("", 128))

	r := openBuilt(t, b)
	v := r.Volume()
	if v.Label != "Joliet Label" || r.VolumeLabel() != "Joliet Label" {
		t.Errorf("label = %q / %q, want Joliet Label", v.Label, r.VolumeLabel())
	}
	if v.Publisher != "Jöliet Publisher" {
		t.Errorf("publisher = %q, want the Joliet one", v.Publisher)
	}
	if v.DataPreparer != "PLAIN PREPARER" {
		t.Errorf("data preparer = %q, want the plain one", v.DataPreparer)
	}
	if v.Application != "PLAIN APP" {
		t.Errorf("application = %q", v.Application)
	}
	if want := time.Date(1998, 7, 4, 12, 0, 0, 0, time.UTC); !v.Created.Equal(want) {
		t.Errorf("created = %v, want %v", v.Created, want)
	}
	if want := time.Date(1998, 7, 5, 12, 0, 0, 0, time.UTC); !v.Modified.Equal(want) {
		t.Errorf("modified = %v, want %v", v.Modified, want)
	}
	if !v.Expires.IsZero() {
		t.Errorf("expires = %v, want zero", v.Expires)
	}
	if !v.Effective.IsZero() {
		t.Errorf("effective = %v, want zero for a damaged date", v.Effective)
	}
	if !hasIssue(r, "volume effective date is damaged") {
		t.Errorf("no issue for the damaged effective date: %q", r.Issues())
	}
	if hasIssue(r, "expiration") {
		t.Errorf("an unset date was reported as damage: %q", r.Issues())
	}
}

// ---- Rock Ridge PX and TF --------------------------------------------------

func both32(v uint32) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint32(b[0:4], v)
	binary.BigEndian.PutUint32(b[4:8], v)
	return b
}

// pxEntry is an RRIP 1.12 PX entry, serial number included.
func pxEntry(mode, uid, gid uint32) []byte {
	e := []byte{'P', 'X', 44, 1}
	for _, v := range []uint32{mode, 1, uid, gid, 0} {
		e = append(e, both32(v)...)
	}
	return e
}

func tfEntry(flags byte, stamps ...[]byte) []byte {
	e := []byte{'T', 'F', 0, 1, flags}
	for _, s := range stamps {
		e = append(e, s...)
	}
	e[2] = byte(len(e))
	return e
}

// rrRoot writes a root directory whose dot entry carries the SP entry that
// turns Rock Ridge on, and returns the offset after the dotdot entry.
func rrRoot(root []byte, rootLBA int) int {
	root[0] = 42
	root[25] = 0x02
	root[32] = 1
	binary.LittleEndian.PutUint32(root[2:6], uint32(rootLBA))
	binary.LittleEndian.PutUint32(root[10:14], 2048)
	copy(root[34:], []byte{'S', 'P', 7, 1, 0xBE, 0xEF, 0})
	return putDirRecord(root, 42, uint32(rootLBA), 2048, true, []byte{0x01})
}

// rrRecord writes a directory record with a System Use Area and a recording
// date.
func rrRecord(dst []byte, offset int, lba uint32, isDir bool, name string, date []byte, sua ...[]byte) int {
	var area []byte
	for _, s := range sua {
		area = append(area, s...)
	}
	next := appendSUARecord(dst, offset, []byte(name), area)
	rec := dst[offset:next]
	binary.LittleEndian.PutUint32(rec[2:6], lba)
	binary.BigEndian.PutUint32(rec[6:10], lba)
	if isDir {
		binary.LittleEndian.PutUint32(rec[10:14], 2048)
		binary.BigEndian.PutUint32(rec[14:18], 2048)
		rec[25] = 0x02
	}
	copy(rec[18:25], date)
	return next
}

func buildPXTFISO(t testing.TB) *isoBuilder {
	t.Helper()
	b := newISOBuilder()
	b.buildSetTerminator(b.alloc())
	rootLBA := b.alloc()
	subLBA := b.alloc()
	root := b.sector(rootLBA)
	off := rrRoot(root, rootLBA)

	recDate := []byte{90, 1, 1, 0, 0, 0, 0}
	modify := []byte{110, 3, 4, 5, 6, 7, 0}
	access := []byte{111, 8, 9, 10, 11, 12, 0}
	create := append([]byte("2005050505050500"), 0)
	change := append([]byte("2006060606060600"), 0)

	off = rrRecord(root, off, 0, false, "PROG.;1", recDate,
		pxEntry(0o104755, 1000, 100),
		tfEntry(tfModify|tfAccess, modify, access))
	off = rrRecord(root, off, 0, false, "LONG.;1", recDate,
		tfEntry(tfLongForm|tfCreation|tfAttributes, create, change))
	off = rrRecord(root, off, 0, false, "TTY.;1", recDate,
		pxEntry(0o020620, 0, 5))
	off = rrRecord(root, off, 0, false, "BADTF.;1", recDate,
		tfEntry(tfModify, []byte{110, 13, 1, 0, 0, 0, 0}))
	off = rrRecord(root, off, 0, false, "SHORTPX.;1", recDate,
		[]byte{'P', 'X', 12, 1, 1, 2, 3, 4, 5, 6, 7, 8})
	rrRecord(root, off, uint32(subLBA), true, "SUB", recDate,
		pxEntry(0o041750, 0, 0))

	sub := b.sector(subLBA)
	so := putDirRecord(sub, 0, uint32(subLBA), 2048, true, []byte{0x00})
	putDirRecord(sub, so, uint32(rootLBA), 2048, true, []byte{0x01})

	b.buildPVD(uint32(rootLBA), 2048, "RR")
	return b
}

func TestRockRidgePXAndTF(t *testing.T) {
	t.Parallel()
	r := openBuilt(t, buildPXTFISO(t))
	got := walkAll(t, r)
	recDate := time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC)

	prog := got["/PROG"]
	if !prog.HasPOSIX || prog.Mode != fs.ModeSetuid|0o755 || prog.UID != 1000 || prog.GID != 100 {
		t.Errorf("PROG: HasPOSIX %v mode %v uid %d gid %d", prog.HasPOSIX, prog.Mode, prog.UID, prog.GID)
	}
	if want := time.Date(2010, 3, 4, 5, 6, 7, 0, time.UTC); !prog.ModTime.Equal(want) {
		t.Errorf("PROG ModTime = %v, want the TF modify time %v", prog.ModTime, want)
	}
	if want := time.Date(2011, 8, 9, 10, 11, 12, 0, time.UTC); !prog.AccessTime.Equal(want) {
		t.Errorf("PROG AccessTime = %v, want %v", prog.AccessTime, want)
	}

	long := got["/LONG"]
	if !long.ModTime.Equal(recDate) {
		t.Errorf("LONG ModTime = %v, want the record date with no TF modify", long.ModTime)
	}
	if want := time.Date(2005, 5, 5, 5, 5, 5, 0, time.UTC); !long.CreateTime.Equal(want) {
		t.Errorf("LONG CreateTime = %v, want %v", long.CreateTime, want)
	}
	if want := time.Date(2006, 6, 6, 6, 6, 6, 0, time.UTC); !long.ChangeTime.Equal(want) {
		t.Errorf("LONG ChangeTime = %v, want %v", long.ChangeTime, want)
	}
	if long.HasPOSIX || long.Mode != 0o444 {
		t.Errorf("LONG without PX: HasPOSIX %v mode %v, want r--r--r--", long.HasPOSIX, long.Mode)
	}

	if tty := got["/TTY"]; tty.Mode != fs.ModeDevice|fs.ModeCharDevice|0o620 {
		t.Errorf("TTY mode = %v, want a character device", tty.Mode)
	}

	if bad := got["/BADTF"]; !bad.ModTime.Equal(recDate) {
		t.Errorf("BADTF ModTime = %v, want the record date when TF is damaged", bad.ModTime)
	}
	if !hasIssue(r, "/BADTF: Rock Ridge TF modify time is damaged") {
		t.Errorf("no issue for the damaged TF: %q", r.Issues())
	}
	if short := got["/SHORTPX"]; short.HasPOSIX {
		t.Error("a truncated PX was used")
	}
	if !hasIssue(r, "/SHORTPX: Rock Ridge PX entry is 8 bytes") {
		t.Errorf("no issue for the truncated PX: %q", r.Issues())
	}

	if sub := got["/SUB/"]; !sub.IsDir || sub.Mode != fs.ModeDir|fs.ModeSticky|0o750 {
		t.Errorf("SUB: IsDir %v mode %v", sub.IsDir, sub.Mode)
	}
}

// PX cannot turn a file record into a directory or the reverse: the record's
// flag decides how the entry is walked, so the mode has to agree with it.
func TestRockRidgePXTypeFollowsTheRecord(t *testing.T) {
	t.Parallel()
	b := newISOBuilder()
	b.buildSetTerminator(b.alloc())
	rootLBA := b.alloc()
	root := b.sector(rootLBA)
	off := rrRoot(root, rootLBA)
	rrRecord(root, off, 0, false, "LIAR.;1", nil, pxEntry(0o040755, 0, 0))
	b.buildPVD(uint32(rootLBA), 2048, "RR")

	e := walkAll(t, openBuilt(t, b))["/LIAR"]
	if e.Mode.IsDir() || e.Mode.Perm() != 0o755 {
		t.Errorf("mode = %v, want a regular file with rwxr-xr-x", e.Mode)
	}
}

// An SL entry on a record flagged as a directory makes it a symlink: it is not
// walked into, and its mode says symlink.
func TestRockRidgeSymlinkOnADirectoryRecord(t *testing.T) {
	t.Parallel()
	b := newISOBuilder()
	b.buildSetTerminator(b.alloc())
	rootLBA := b.alloc()
	root := b.sector(rootLBA)
	off := rrRoot(root, rootLBA)
	target := "/elsewhere"
	sl := append([]byte{'S', 'L', 0, 1, 0, 0x00, byte(len(target))}, target...)
	sl[2] = byte(len(sl))
	rrRecord(root, off, uint32(rootLBA), true, "LINK", nil, sl, pxEntry(0o040755, 0, 0))
	b.buildPVD(uint32(rootLBA), 2048, "RR")

	got := walkAll(t, openBuilt(t, b))
	e, ok := got["/LINK"]
	if !ok {
		t.Fatalf("no /LINK entry; got %v", keys(got))
	}
	if e.IsDir || e.Symlink != target || e.Mode&fs.ModeSymlink == 0 || e.Mode.IsDir() {
		t.Errorf("LINK: IsDir %v symlink %q mode %v, want a symlink", e.IsDir, e.Symlink, e.Mode)
	}
}
