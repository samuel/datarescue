package iso

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
)

// putFlagged writes a directory record with the given file flags.
func putFlagged(dst []byte, offset int, lba, size uint32, flags byte, name string) int {
	next := putDirRecord(dst, offset, lba, size, flags&flagDirectory != 0, []byte(name))
	dst[offset+25] = flags
	return next
}

// dotEntries writes a directory's dot and dotdot records.
func dotEntries(dir []byte, self, parent int) int {
	off := putDirRecord(dir, 0, uint32(self), 2048, true, []byte{0x00})
	return putDirRecord(dir, off, uint32(parent), 2048, true, []byte{0x01})
}

// filled is n bytes of b, so each extent of a test file is recognizable.
func filled(b byte, n int) []byte { return bytes.Repeat([]byte{b}, n) }

func readEntry(t *testing.T, r *Reader, e DirEntry) ([]byte, error) {
	t.Helper()
	return io.ReadAll(r.OpenFile(e))
}

func TestDeepTreeIsWalked(t *testing.T) {
	t.Parallel()
	const levels = 20 // far past ISO 9660 Level 1's eight
	b := newISOBuilder()
	b.buildSetTerminator(b.alloc())
	rootLBA := b.alloc()
	parent, cur := rootLBA, rootLBA
	for range levels {
		next := b.alloc()
		off := dotEntries(b.sector(cur), cur, parent)
		putDirRecord(b.sector(cur), off, uint32(next), 2048, true, []byte("D"))
		parent, cur = cur, next
	}
	dotEntries(b.sector(cur), cur, parent)
	b.buildPVD(uint32(rootLBA), 2048, "DEEP")

	r := openBuilt(t, b)
	got := walkAll(t, r)
	deepest := "/" + strings.Repeat("D/", levels)
	if _, ok := got[deepest]; !ok {
		t.Errorf("no %s in the walk", deepest)
	}
	if len(r.Issues()) != 0 {
		t.Errorf("issues = %q, want none", r.Issues())
	}
}

// multiExtentISO puts a three-extent file in the root. The first extent is a
// whole sector, the second deliberately is not, and the records are split
// across two directory sectors.
func multiExtentISO(t testing.TB, lastFlags byte, after string) (*isoBuilder, []byte) {
	t.Helper()
	b := newISOBuilder()
	b.buildSetTerminator(b.alloc())
	rootLBA := b.alloc()
	root2 := b.alloc()
	ext1, ext2, ext3 := b.alloc(), b.alloc(), b.alloc()
	copy(b.sector(ext1), filled('a', 2048))
	copy(b.sector(ext2), filled('b', 2048)) // only 1000 of these are the file's
	copy(b.sector(ext3), filled('c', 300))
	want := append(append(filled('a', 2048), filled('b', 1000)...), filled('c', 300)...)

	off := dotEntries(b.sector(rootLBA), rootLBA, rootLBA)
	putFlagged(b.sector(rootLBA), off, uint32(ext1), 2048, flagMultiExtent, "BIG.DAT;1")
	off = putFlagged(b.sector(root2), 0, uint32(ext2), 1000, flagMultiExtent, "BIG.DAT;1")
	off = putFlagged(b.sector(root2), off, uint32(ext3), 300, lastFlags, "BIG.DAT;1")
	if after != "" {
		putFlagged(b.sector(root2), off, uint32(ext3), 300, 0, after)
	}
	b.buildPVD(uint32(rootLBA), 2*2048, "MULTI")
	return b, want
}

func TestMultiExtentFile(t *testing.T) {
	t.Parallel()
	b, want := multiExtentISO(t, 0, "")
	r := openBuilt(t, b)
	got := walkAll(t, r)
	if len(got) != 1 {
		t.Fatalf("entries = %v, want the one merged file", keys(got))
	}
	e := got["/BIG.DAT"]
	if e.Size != int64(len(want)) || len(e.Extents) != 3 || e.Incomplete {
		t.Fatalf("entry = size %d, %d extents, incomplete %v; want %d bytes in 3 extents",
			e.Size, len(e.Extents), e.Incomplete, len(want))
	}
	data, err := readEntry(t, r, e)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, want) {
		t.Errorf("read %d bytes that do not match the three extents in order", len(data))
	}
	if len(r.Issues()) != 0 {
		t.Errorf("issues = %q, want none", r.Issues())
	}
	if off, length := r.ExtentBytes(e.Extents[1]); off != e.Extents[1].LBA*2048 || length != 2048 {
		t.Errorf("ExtentBytes(second extent) = %d, %d", off, length)
	}
}

// A chain whose last record still says another extent follows is cut short.
// The extents found are delivered and the read then fails rather than ending
// cleanly at a length that is not the file's.
func TestMultiExtentChainEndsEarly(t *testing.T) {
	t.Parallel()
	b, want := multiExtentISO(t, flagMultiExtent, "")
	r := openBuilt(t, b)
	e := walkAll(t, r)["/BIG.DAT"]
	if !e.Incomplete || len(e.Extents) != 3 {
		t.Fatalf("entry: incomplete %v with %d extents, want incomplete with 3", e.Incomplete, len(e.Extents))
	}
	if !hasIssue(r, "/BIG.DAT: multi-extent file ends without a final extent") {
		t.Errorf("no issue for the open chain: %q", r.Issues())
	}
	data, err := readEntry(t, r, e)
	if !errors.Is(err, ErrIncompleteFile) {
		t.Errorf("read error = %v, want ErrIncompleteFile", err)
	}
	if !bytes.Equal(data, want) {
		t.Errorf("read %d bytes, want the %d of the extents found", len(data), len(want))
	}
}

func TestMultiExtentNameMismatch(t *testing.T) {
	t.Parallel()
	b, _ := multiExtentISO(t, flagMultiExtent, "OTHER.DAT;1")
	r := openBuilt(t, b)
	got := walkAll(t, r)
	if e := got["/BIG.DAT"]; !e.Incomplete {
		t.Error("BIG.DAT is not marked incomplete")
	}
	if e, ok := got["/OTHER.DAT"]; !ok || e.Size != 300 || e.Incomplete {
		t.Errorf("OTHER.DAT = %+v, want an ordinary 300-byte file", e)
	}
	if !hasIssue(r, `is followed by "OTHER.DAT" instead of its next extent`) {
		t.Errorf("no issue for the mismatch: %q", r.Issues())
	}
}

// A directory sector that will not read can hold the rest of a chain, so the
// file is marked incomplete rather than ending at the extents before it.
func TestMultiExtentChainCutByUnreadableSector(t *testing.T) {
	t.Parallel()
	b := newISOBuilder()
	b.buildSetTerminator(b.alloc())
	ext := b.alloc()
	copy(b.sector(ext), filled('a', 2048))
	rootLBA := b.alloc()
	b.alloc() // the root's second sector, which will not read
	off := dotEntries(b.sector(rootLBA), rootLBA, rootLBA)
	putFlagged(b.sector(rootLBA), off, uint32(ext), 2048, flagMultiExtent, "BIG.DAT;1")
	b.buildPVD(uint32(rootLBA), 2*2048, "CUT")

	img := flatten(b)
	r, err := OpenReader(failFrom{data: img, from: int64(rootLBA+1) * 2048})
	if err != nil {
		t.Fatal(err)
	}
	e := walkAll(t, r)["/BIG.DAT"]
	if !e.Incomplete || e.Size != 2048 {
		t.Errorf("entry: incomplete %v, size %d; want incomplete with the first 2048 bytes", e.Incomplete, e.Size)
	}
	if !hasIssue(r, "is cut short by the unreadable directory sector") {
		t.Errorf("no issue for the cut chain: %q", r.Issues())
	}
}

func TestAssociatedFile(t *testing.T) {
	t.Parallel()
	b := newISOBuilder()
	b.buildSetTerminator(b.alloc())
	rootLBA := b.alloc()
	off := dotEntries(b.sector(rootLBA), rootLBA, rootLBA)
	off = putFlagged(b.sector(rootLBA), off, 0, 10, flagAssociated, "FILE.;1")
	putFlagged(b.sector(rootLBA), off, 0, 20, 0, "FILE.;1")
	b.buildPVD(uint32(rootLBA), 2048, "ASSOC")

	var entries []DirEntry
	if err := openBuilt(t, b).Walk(func(e DirEntry) error {
		entries = append(entries, e)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want the associated file and its main file", len(entries))
	}
	if !entries[0].Associated || entries[0].Size != 10 || entries[1].Associated || entries[1].Size != 20 {
		t.Errorf("entries = %+v / %+v", entries[0], entries[1])
	}
}

func clEntry(lba uint32) []byte { return append([]byte{'C', 'L', 12, 1}, both32(lba)...) }

func nmEntry(name string) []byte {
	e := append([]byte{'N', 'M', 0, 1, 0}, name...)
	e[2] = byte(len(e))
	return e
}

// buildRelocatedISO is the layout mkisofs writes for a tree too deep for ISO
// 9660: /a holds a placeholder "deep" whose CL points at the real directory,
// which is stored under /rr_moved and marked RE.
func buildRelocatedISO(t testing.TB) *isoBuilder {
	t.Helper()
	b := newISOBuilder()
	b.buildSetTerminator(b.alloc())
	rootLBA, aLBA, movedLBA, deepLBA, fileLBA := b.alloc(), b.alloc(), b.alloc(), b.alloc(), b.alloc()
	copy(b.sector(fileLBA), "deep data")

	root := b.sector(rootLBA)
	off := rrRoot(root, rootLBA)
	off = rrRecord(root, off, uint32(aLBA), true, "A", nil, nmEntry("a"))
	rrRecord(root, off, uint32(movedLBA), true, "RR_MOVED", nil, nmEntry("rr_moved"))

	a := b.sector(aLBA)
	off = dotEntries(a, aLBA, rootLBA)
	rrRecord(a, off, 0, false, "DEEP", nil, nmEntry("deep"), clEntry(uint32(deepLBA)))

	moved := b.sector(movedLBA)
	off = dotEntries(moved, movedLBA, rootLBA)
	rrRecord(moved, off, uint32(deepLBA), true, "DEEP", nil, nmEntry("deep"), []byte{'R', 'E', 4, 1})

	deep := b.sector(deepLBA)
	off = dotEntries(deep, deepLBA, movedLBA)
	rrRecord(deep, off, uint32(fileLBA), false, "F.TXT;1", nil, nmEntry("f.txt"))
	copy(deep[off+10:], both32(9)) // the file's size

	b.buildPVD(uint32(rootLBA), 2048, "RELOC")
	return b
}

func TestRockRidgeRelocation(t *testing.T) {
	t.Parallel()
	r := openBuilt(t, buildRelocatedISO(t))
	got := walkAll(t, r)

	d, ok := got["/a/deep/"]
	if !ok || !d.IsDir {
		t.Fatalf("no /a/deep/ directory at the placeholder; got %v", keys(got))
	}
	f, ok := got["/a/deep/f.txt"]
	if !ok {
		t.Fatalf("the relocated directory's file is not under /a/deep; got %v", keys(got))
	}
	if data, err := readEntry(t, r, f); err != nil || string(data) != "deep data" {
		t.Errorf("read %q, %v", data, err)
	}
	for p := range got {
		if strings.HasPrefix(p, "/rr_moved/deep") {
			t.Errorf("the relocated copy is listed where it is stored: %s", p)
		}
	}
	if _, ok := got["/rr_moved/"]; !ok {
		t.Error("rr_moved itself is missing")
	}
	if len(r.Issues()) != 0 {
		t.Errorf("issues = %q, want none", r.Issues())
	}
}

func TestRockRidgeDetectedAtOpen(t *testing.T) {
	t.Parallel()
	path, cleanup := buildRockRidgeISO(t)
	defer cleanup()
	r, err := openPath(path)
	if err != nil {
		t.Fatal(err)
	}
	if !r.HasRockRidge() {
		t.Error("HasRockRidge() = false before any Walk")
	}
}

// Walks, reads and Issues may run at once; run under -race this checks that
// nothing they share is written.
func TestConcurrentUse(t *testing.T) {
	t.Parallel()
	r := openBuilt(t, buildRelocatedISO(t))
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			_ = r.Walk(func(e DirEntry) error {
				if !e.IsDir && e.Symlink == "" {
					if _, err := io.ReadAll(r.OpenFile(e)); err != nil {
						t.Error(err)
					}
				}
				_ = r.Issues()
				return nil
			})
		})
	}
	wg.Wait()
}
