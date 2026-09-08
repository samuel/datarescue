package iso

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"slices"
	"testing"
	"testing/fstest"

	"samuelks.com/datarescue/internal/testiso"
)

// failRange is an image whose bytes in [from, to) cannot be read. A read
// reaching them returns what comes before.
type failRange struct {
	data     []byte
	from, to int64
}

func (f failRange) ReadAt(p []byte, off int64) (int, error) {
	end := off + int64(len(p))
	if off < f.to && end > f.from {
		n := 0
		if off < f.from {
			n = copy(p, f.data[off:f.from])
		}
		return n, errUnrecovered
	}
	if off >= int64(len(f.data)) {
		return 0, io.EOF
	}
	n := copy(p, f.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func TestFileReaderAtAcrossExtents(t *testing.T) {
	t.Parallel()
	b, want := multiExtentISO(t, 0, "")
	r := openBuilt(t, b)
	e, err := r.Lookup("BIG.DAT")
	if err != nil {
		t.Fatal(err)
	}
	ra := r.OpenFileAt(e)
	if ra.Size() != int64(len(want)) {
		t.Fatalf("Size() = %d, want %d", ra.Size(), len(want))
	}
	for _, c := range []struct{ off, n int }{
		{0, len(want)},
		{2040, 20},       // across the first and second extents
		{2048 + 990, 20}, // across the second (1000 bytes) and third extents
		{3000, 348},      // to the very end
		{5, 1},
	} {
		got := make([]byte, c.n)
		n, err := ra.ReadAt(got, int64(c.off))
		if n != c.n || (err != nil && !errors.Is(err, io.EOF)) {
			t.Errorf("ReadAt(%d, %d) = %d, %v", c.off, c.n, n, err)
			continue
		}
		if !bytes.Equal(got, want[c.off:c.off+c.n]) {
			t.Errorf("ReadAt(%d, %d) returned the wrong bytes", c.off, c.n)
		}
	}
	p := make([]byte, 10)
	if n, err := ra.ReadAt(p, int64(len(want))-4); n != 4 || !errors.Is(err, io.EOF) {
		t.Errorf("read over the end = %d, %v; want 4, io.EOF", n, err)
	}
	if _, err := ra.ReadAt(p, -1); err == nil {
		t.Error("a negative offset was accepted")
	}
}

func TestFileReaderAtIncomplete(t *testing.T) {
	t.Parallel()
	b, want := multiExtentISO(t, flagMultiExtent, "")
	r := openBuilt(t, b)
	e := walkAll(t, r)["/BIG.DAT"]
	p := make([]byte, 10)
	if n, err := r.OpenFileAt(e).ReadAt(p, int64(len(want))-4); n != 4 || !errors.Is(err, ErrIncompleteFile) {
		t.Errorf("read over the end = %d, %v; want 4, ErrIncompleteFile", n, err)
	}
}

// On a raw image each sector's payload sits between a header and an EDC/ECC
// trailer, so file offsets map to image offsets a sector at a time.
func TestFileReaderAtRaw(t *testing.T) {
	t.Parallel()
	img := testiso.BuildRaw()
	r, err := OpenReader(bytes.NewReader(img))
	if err != nil {
		t.Fatal(err)
	}
	e, err := r.Lookup(testiso.BigName)
	if err != nil {
		t.Fatal(err)
	}
	want := testiso.BigData()
	got := make([]byte, 100)
	if n, err := r.OpenFileAt(e).ReadAt(got, 2000); n != 100 || err != nil {
		t.Fatalf("ReadAt = %d, %v", n, err)
	}
	if !bytes.Equal(got, want[2000:2100]) {
		t.Error("the bytes across a raw sector boundary are wrong")
	}
}

func TestFileReaderAtDamage(t *testing.T) {
	t.Parallel()
	img := testiso.BuildRaw()
	sector := int64(testiso.BigLBA+1) * testiso.RawSectorSize

	// A hole in the payload of the file's second sector, 100 bytes in.
	r, err := OpenReader(failRange{data: img, from: sector + 16 + 100, to: sector + 16 + 200})
	if err != nil {
		t.Fatal(err)
	}
	e, err := r.Lookup(testiso.BigName)
	if err != nil {
		t.Fatal(err)
	}
	p := make([]byte, 4096)
	n, err := r.OpenFileAt(e).ReadAt(p, 0)
	var se *SectorReadError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want a *SectorReadError", err)
	}
	if n != 2048+100 || se.LBA != testiso.BigLBA+1 || se.Offset != 2048 || se.N != 100 {
		t.Errorf("n = %d, error = LBA %d offset %d N %d; want 2148, LBA %d offset 2048 N 100",
			n, se.LBA, se.Offset, se.N, testiso.BigLBA+1)
	}

	// A hole in the same sector's EDC/ECC trailer costs the file nothing.
	r, err = OpenReader(failRange{data: img, from: sector + 16 + 2048, to: sector + testiso.RawSectorSize})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := r.OpenFileAt(e).ReadAt(p, 0); n != len(p) || err != nil {
		t.Errorf("read over a lost trailer = %d, %v; want the whole read", n, err)
	}
}

func TestWalkSkipDirAndSkipAll(t *testing.T) {
	t.Parallel()
	r := openBuilt(t, buildRelocatedISO(t))

	var seen []string
	if err := r.Walk(func(e DirEntry) error {
		seen = append(seen, e.Path)
		if e.Path == "/a/" {
			return fs.SkipDir
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, p := range seen {
		if p == "/a/deep/" {
			t.Errorf("SkipDir on /a/ still walked into it: %v", seen)
		}
	}
	if len(seen) != 2 {
		t.Errorf("seen = %v, want /a/ and /rr_moved/", seen)
	}

	// SkipDir on a file skips the rest of its directory, and the walk carries
	// on in the parent.
	b := newISOBuilder()
	b.buildSetTerminator(b.alloc())
	rootLBA, subLBA := b.alloc(), b.alloc()
	off := dotEntries(b.sector(rootLBA), rootLBA, rootLBA)
	off = putDirRecord(b.sector(rootLBA), off, uint32(subLBA), 2048, true, []byte("SUB"))
	putDirRecord(b.sector(rootLBA), off, 0, 0, false, []byte("AFTER.;1"))
	off = dotEntries(b.sector(subLBA), subLBA, rootLBA)
	for _, n := range []string{"ONE.;1", "TWO.;1", "THREE.;1"} {
		off = putDirRecord(b.sector(subLBA), off, 0, 0, false, []byte(n))
	}
	b.buildPVD(uint32(rootLBA), 2048, "SKIP")
	seen = nil
	if err := openBuilt(t, b).Walk(func(e DirEntry) error {
		seen = append(seen, e.Path)
		if e.Path == "/SUB/ONE" {
			return fs.SkipDir
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"/SUB/", "/SUB/ONE", "/AFTER"}; !slices.Equal(seen, want) {
		t.Errorf("seen = %v, want %v", seen, want)
	}

	var n int
	if err := r.Walk(func(DirEntry) error {
		n++
		return fs.SkipAll
	}); err != nil || n != 1 {
		t.Errorf("SkipAll: err %v after %d entries; want nil after 1", err, n)
	}
}

func TestLookup(t *testing.T) {
	t.Parallel()
	r := openBuilt(t, buildRelocatedISO(t))
	for _, p := range []string{"", "/", "."} {
		if e, err := r.Lookup(p); err != nil || e.Path != "/" || !e.IsDir {
			t.Errorf("Lookup(%q) = %+v, %v; want the root", p, e, err)
		}
	}
	e, err := r.Lookup("/a/deep/f.txt")
	if err != nil || e.Path != "/a/deep/f.txt" || e.Size != 9 {
		t.Fatalf("Lookup(/a/deep/f.txt) = %+v, %v", e, err)
	}
	if data, err := io.ReadAll(r.OpenFile(e)); err != nil || string(data) != "deep data" {
		t.Errorf("read %q, %v", data, err)
	}
	if e, err := r.Lookup("a/deep/"); err != nil || !e.IsDir {
		t.Errorf("Lookup(a/deep/) = %+v, %v; want the relocated directory", e, err)
	}
	for _, p := range []string{"missing", "a/missing", "a/deep/f.txt/x", "A"} {
		if _, err := r.Lookup(p); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("Lookup(%q) error = %v, want fs.ErrNotExist", p, err)
		}
	}
}

// Lookup reads the directories along the path and nothing else.
func TestLookupReadsOnlyThePath(t *testing.T) {
	t.Parallel()
	b := newISOBuilder()
	b.buildSetTerminator(b.alloc())
	rootLBA := b.alloc()
	off := dotEntries(b.sector(rootLBA), rootLBA, rootLBA)
	for _, name := range []string{"A", "B", "C", "D"} {
		sub := b.alloc()
		dotEntries(b.sector(sub), sub, rootLBA)
		off = putDirRecord(b.sector(rootLBA), off, uint32(sub), 2048, true, []byte(name))
		if name == "D" {
			putDirRecord(b.sector(sub), 68, 0, 0, false, []byte("X.;1"))
		}
	}
	b.buildPVD(uint32(rootLBA), 2048, "PATH")

	ra := &countingRA{data: flatten(b)}
	r, err := OpenReader(ra)
	if err != nil {
		t.Fatal(err)
	}
	before := ra.calls
	if _, err := r.Lookup("D/X"); err != nil {
		t.Fatal(err)
	}
	if reads := ra.calls - before; reads != 2 {
		t.Errorf("Lookup made %d reads; the root and D are 2", reads)
	}
}

// A name missing from a damaged directory may be in the part that was lost, so
// Lookup reports the damage rather than saying the path does not exist.
func TestLookupUnderADamagedDirectory(t *testing.T) {
	t.Parallel()
	b := newISOBuilder()
	b.buildSetTerminator(b.alloc())
	rootLBA := b.alloc()
	b.alloc() // the root's second sector, which will not read
	off := dotEntries(b.sector(rootLBA), rootLBA, rootLBA)
	putDirRecord(b.sector(rootLBA), off, 0, 0, false, []byte("HERE.;1"))
	b.buildPVD(uint32(rootLBA), 2*2048, "DAMAGED")

	img := flatten(b)
	r, err := OpenReader(failFrom{data: img, from: int64(rootLBA+1) * 2048})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Lookup("HERE"); err != nil {
		t.Errorf("Lookup(HERE) = %v; the entry that did read should be found", err)
	}
	_, err = r.Lookup("GONE")
	var de *DirError
	if !errors.As(err, &de) || errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Lookup(GONE) error = %v, want a *DirError and not fs.ErrNotExist", err)
	}

	entries, err := r.ReadDir("/")
	if len(entries) != 1 || !errors.As(err, &de) {
		t.Errorf("ReadDir(/) = %d entries, %v; want HERE and a *DirError", len(entries), err)
	}
}

func TestReadDir(t *testing.T) {
	t.Parallel()
	r := openBuilt(t, buildRelocatedISO(t))
	entries, err := r.ReadDir("a")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Path != "/a/deep/" || !entries[0].IsDir {
		t.Errorf("ReadDir(a) = %+v, want the one relocated directory", entries)
	}
	if _, err := r.ReadDir("a/deep/f.txt"); err == nil {
		t.Error("ReadDir of a file succeeded")
	}
}

func TestFS(t *testing.T) {
	t.Parallel()
	r := openBuilt(t, buildRelocatedISO(t))
	if err := fstest.TestFS(r.FS(), "a", "a/deep", "a/deep/f.txt", "rr_moved"); err != nil {
		t.Fatal(err)
	}
	data, err := fs.ReadFile(r.FS(), "a/deep/f.txt")
	if err != nil || string(data) != "deep data" {
		t.Errorf("ReadFile = %q, %v", data, err)
	}
	info, err := fs.Stat(r.FS(), "a/deep/f.txt")
	if err != nil {
		t.Fatal(err)
	}
	if e, ok := info.Sys().(DirEntry); !ok || e.Path != "/a/deep/f.txt" {
		t.Errorf("Sys() = %#v, want the DirEntry", info.Sys())
	}
}

func TestFSMultiExtent(t *testing.T) {
	t.Parallel()
	b, want := multiExtentISO(t, 0, "")
	fsys := openBuilt(t, b).FS()
	if err := fstest.TestFS(fsys, "BIG.DAT"); err != nil {
		t.Fatal(err)
	}
	if data, err := fs.ReadFile(fsys, "BIG.DAT"); err != nil || !bytes.Equal(data, want) {
		t.Errorf("ReadFile = %d bytes, %v; want %d", len(data), err, len(want))
	}

	// An incomplete file must not read as a whole one through io/fs either.
	b, _ = multiExtentISO(t, flagMultiExtent, "")
	if _, err := fs.ReadFile(openBuilt(t, b).FS(), "BIG.DAT"); !errors.Is(err, ErrIncompleteFile) {
		t.Errorf("ReadFile of an incomplete file = %v, want ErrIncompleteFile", err)
	}
}

// Associated files share a name with their main file; io/fs allows one entry
// per name, so they are left out and the main file is what a name opens.
func TestFSLeavesOutAssociatedFiles(t *testing.T) {
	t.Parallel()
	b := newISOBuilder()
	b.buildSetTerminator(b.alloc())
	rootLBA := b.alloc()
	assoc, main := b.alloc(), b.alloc()
	copy(b.sector(assoc), "resource")
	copy(b.sector(main), "data")
	off := dotEntries(b.sector(rootLBA), rootLBA, rootLBA)
	off = putFlagged(b.sector(rootLBA), off, uint32(assoc), 8, flagAssociated, "FILE.;1")
	putFlagged(b.sector(rootLBA), off, uint32(main), 4, 0, "FILE.;1")
	b.buildPVD(uint32(rootLBA), 2048, "ASSOC")

	fsys := openBuilt(t, b).FS()
	if err := fstest.TestFS(fsys, "FILE"); err != nil {
		t.Fatal(err)
	}
	if data, err := fs.ReadFile(fsys, "FILE"); err != nil || string(data) != "data" {
		t.Errorf("ReadFile(FILE) = %q, %v; want the main file", data, err)
	}
}
