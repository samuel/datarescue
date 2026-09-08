package iso

import (
	"encoding/binary"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

// ceEntry is a Rock Ridge CE (continuation area) System Use entry pointing at
// the given LBA, offset and length.
func ceEntry(lba, off, length uint32) []byte {
	e := make([]byte, 16)
	e[0], e[1], e[2], e[3] = 'C', 'E', 16, 1
	binary.LittleEndian.PutUint32(e[4:], lba)
	binary.LittleEndian.PutUint32(e[8:], off)
	binary.LittleEndian.PutUint32(e[12:], length)
	return e
}

// appendSUARecord writes a directory record whose System Use Area is sua.
func appendSUARecord(dst []byte, offset int, isoName []byte, sua []byte) int {
	nameLen := len(isoName)
	suaStart := 33 + nameLen
	if suaStart%2 != 0 {
		suaStart++
	}
	recLen := suaStart + len(sua)
	if recLen%2 != 0 {
		recLen++
	}
	rec := dst[offset : offset+recLen]
	rec[0] = byte(recLen)
	rec[32] = byte(nameLen)
	copy(rec[33:], isoName)
	copy(rec[suaStart:], sua)
	return offset + recLen
}

// A CE entry gives an LBA and an offset read off the disc, so it can point back
// at the area that contains it. Following that without a guard recurses until
// the stack runs out, which is a crash rather than a wrong answer.
func TestRockRidgeSelfReferentialContinuation(t *testing.T) {
	b := newISOBuilder()
	b.buildSetTerminator(b.alloc()) // 17

	rootLBA := b.alloc() // 18
	root := b.sector(rootLBA)

	// Dot entry with the SP entry that turns Rock Ridge parsing on.
	dotRecLen := 42
	root[0] = byte(dotRecLen)
	root[25] = 0x02
	root[32] = 1
	root[33] = 0x00
	binary.LittleEndian.PutUint32(root[2:6], uint32(rootLBA))
	binary.LittleEndian.PutUint32(root[10:14], 2048)
	copy(root[34:], []byte{'S', 'P', 7, 1, 0xBE, 0xEF, 0})
	off := dotRecLen
	off = putDirRecord(root, off, uint32(rootLBA), 2048, true, []byte{0x01})

	// A continuation area holding nothing but a CE pointing back at itself.
	ceLBA := b.alloc() // 19
	copy(b.sector(ceLBA), ceEntry(uint32(ceLBA), 0, 16))

	fileLBA := b.alloc() // 20
	appendSUARecord(root, off, []byte("LOOP.TXT;1"), ceEntry(uint32(ceLBA), 0, 16))
	copy(b.sector(fileLBA), []byte("x"))

	b.buildPVD(uint32(rootLBA), 2048, "CELOOP")
	path := b.write(t.Name())
	defer os.Remove(path) //nolint:errcheck // test cleanup

	r, err := openPath(path)
	if err != nil {
		t.Fatal(err)
	}

	var entries []DirEntry
	if err := r.Walk(func(e DirEntry) error {
		entries = append(entries, e)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Error("the walk found nothing; the record with the looping continuation cost the directory")
	}
	var named bool
	for _, is := range r.Issues() {
		if strings.Contains(is, "links back on itself") {
			named = true
		}
	}
	if !named {
		t.Errorf("issues = %v; want one naming the continuation area that links back on itself", r.Issues())
	}
}

// A chain of continuations that never repeats a position is stopped by the
// depth cap instead: two areas pointing at each other repeat no single
// position, and the sequence still never ends.
func TestRockRidgeContinuationChainDepth(t *testing.T) {
	b := newISOBuilder()
	b.buildSetTerminator(b.alloc()) // 17

	rootLBA := b.alloc() // 18
	root := b.sector(rootLBA)
	dotRecLen := 42
	root[0] = byte(dotRecLen)
	root[25] = 0x02
	root[32] = 1
	root[33] = 0x00
	binary.LittleEndian.PutUint32(root[2:6], uint32(rootLBA))
	binary.LittleEndian.PutUint32(root[10:14], 2048)
	copy(root[34:], []byte{'S', 'P', 7, 1, 0xBE, 0xEF, 0})
	off := dotRecLen
	off = putDirRecord(root, off, uint32(rootLBA), 2048, true, []byte{0x01})

	// Two continuation areas, each pointing at a fresh offset in the other, so
	// no (lba, offset) pair is ever seen twice.
	a, c := b.alloc(), b.alloc() // 19, 20
	for i := range maxCEDepth + 4 {
		copy(b.sector(a)[i*16:], ceEntry(uint32(c), uint32(i*16), 16))
		copy(b.sector(c)[i*16:], ceEntry(uint32(a), uint32((i+1)*16), 16))
	}
	appendSUARecord(root, off, []byte("DEEP.TXT;1"), ceEntry(uint32(a), 0, 16))

	b.buildPVD(uint32(rootLBA), 2048, "CEDEEP")
	path := b.write(t.Name())
	defer os.Remove(path) //nolint:errcheck // test cleanup

	r, err := openPath(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := r.Walk(func(DirEntry) error { return nil }); err != nil {
		t.Fatal(err)
	}
	var named bool
	for _, is := range r.Issues() {
		if strings.Contains(is, "deeper than") {
			named = true
		}
	}
	if !named {
		t.Errorf("issues = %v; want one naming the continuation chain as too deep", r.Issues())
	}
}

// failFrom is an image that reads normally below a byte offset and refuses
// everything at or above it, which is what a rescue whose tail was never
// recovered looks like.
type failFrom struct {
	data []byte
	from int64
}

var errUnrecovered = errors.New("unrecovered")

func (f failFrom) ReadAt(p []byte, off int64) (int, error) {
	if off >= f.from || off >= int64(len(f.data)) {
		return 0, errUnrecovered
	}
	end := min(off+int64(len(p)), min(f.from, int64(len(f.data))))
	n := copy(p, f.data[off:end])
	if n < len(p) {
		return n, errUnrecovered
	}
	return n, nil
}

// A directory's declared size comes off the disc. A corrupt one drives a read
// per sector over an image that cannot hold that many, and appends an issue for
// each: the volume's own sector count bounds the loop, and the per-directory
// cap bounds what one bad directory can say.
func TestDirectorySizeBoundedByTheVolume(t *testing.T) {
	b := newISOBuilder()
	b.buildSetTerminator(b.alloc()) // 17
	rootLBA := b.alloc()            // 18
	root := b.sector(rootLBA)
	off := putDirRecord(root, 0, uint32(rootLBA), 2048, true, []byte{0x00})
	off = putDirRecord(root, off, uint32(rootLBA), 2048, true, []byte{0x01})
	fileLBA := b.alloc() // 19
	putDirRecord(root, off, uint32(fileLBA), 4, false, []byte("A.TXT;1"))
	copy(b.sector(fileLBA), []byte("data"))
	// Very nearly 4 GiB of declared directory, on a volume of twenty-odd
	// sectors: two million sector reads, and an issue appended for each.
	b.buildPVD(uint32(rootLBA), 0xFFFFF800, "BIGDIR")
	img := make([]byte, 0, len(b.sectors)*2048)
	for _, s := range b.sectors {
		img = append(img, s...)
	}

	// Everything past the root directory's own sector is unreadable, so every
	// sector the declared size reaches for fails.
	r, err := OpenReader(failFrom{data: img, from: int64(rootLBA+1) * 2048})
	if err != nil {
		t.Fatal(err)
	}
	var entries []DirEntry
	if err := r.Walk(func(e DirEntry) error {
		entries = append(entries, e)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Path != "/A.TXT" {
		t.Errorf("entries = %v; want the one in the sector that did read", entries)
	}
	issues := r.Issues()
	if len(issues) > maxDirIssues+2 {
		t.Errorf("the walk reported %d issues for one directory; the cap is %d:\n%s",
			len(issues), maxDirIssues, strings.Join(issues, "\n"))
	}
	var bounded bool
	for _, is := range issues {
		if strings.Contains(is, "more than") && strings.Contains(is, "of volume after it") {
			bounded = true
		}
	}
	if !bounded {
		t.Errorf("issues = %v; want one naming the declared size as larger than the volume", issues)
	}
}

// One directory whose every sector is unreadable says the same thing once per
// sector. The cap keeps it from filling the report and crowding out every other
// directory's findings, on a list whose length is otherwise set by a
// disc-supplied size.
func TestDirectoryIssueCap(t *testing.T) {
	const dirSectors = 40
	b := newISOBuilder()
	b.buildSetTerminator(b.alloc()) // 17
	rootLBA := b.alloc()            // 18
	root := b.sector(rootLBA)
	off := putDirRecord(root, 0, uint32(rootLBA), dirSectors*2048, true, []byte{0x00})
	putDirRecord(root, off, uint32(rootLBA), dirSectors*2048, true, []byte{0x01})
	// The rest of the declared directory, all inside the volume so the size is
	// not what bounds the loop.
	for range dirSectors + 2 {
		b.alloc()
	}
	b.buildPVD(uint32(rootLBA), dirSectors*2048, "MANYBAD")
	img := make([]byte, 0, len(b.sectors)*2048)
	for _, s := range b.sectors {
		img = append(img, s...)
	}

	r, err := OpenReader(failFrom{data: img, from: int64(rootLBA+1) * 2048})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Walk(func(DirEntry) error { return nil }); err != nil {
		t.Fatal(err)
	}
	issues := r.Issues()
	if len(issues) > maxDirIssues {
		t.Errorf("one directory reported %d issues; the cap is %d:\n%s",
			len(issues), maxDirIssues, strings.Join(issues, "\n"))
	}
	if !strings.Contains(issues[len(issues)-1], "not listed") {
		t.Errorf("the last issue does not say the rest were dropped:\n%s", strings.Join(issues, "\n"))
	}
}

// A directory chain past the depth cap is cut short instead of recursing until
// the Go stack runs out.
func TestDirectoryDepthCap(t *testing.T) {
	b := newISOBuilder()
	b.buildSetTerminator(b.alloc()) // 17
	rootLBA := b.alloc()            // 18
	lastLBA := rootLBA
	// One subdirectory per level, deeper than any real disc tree.
	for range maxDirDepth + 4 {
		dir := b.sector(lastLBA)
		off := putDirRecord(dir, 0, uint32(lastLBA), 2048, true, []byte{0x00})
		off = putDirRecord(dir, off, uint32(lastLBA), 2048, true, []byte{0x01})
		next := b.alloc()
		putDirRecord(dir, off, uint32(next), 2048, true, []byte("SUB"))
		lastLBA = next
	}
	b.buildPVD(uint32(rootLBA), 2048, "DEEPTREE")
	path := b.write(t.Name())
	defer os.Remove(path) //nolint:errcheck // test cleanup

	r, err := openPath(path)
	if err != nil {
		t.Fatal(err)
	}
	var dirs int
	if err := r.Walk(func(e DirEntry) error {
		if e.IsDir {
			dirs++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if dirs != maxDirDepth {
		t.Errorf("the walk descended %d levels, want the %d the cap allows", dirs, maxDirDepth)
	}
	var named bool
	for _, is := range r.Issues() {
		if strings.Contains(is, "deeper than") {
			named = true
		}
	}
	if !named {
		t.Errorf("issues = %v; want one naming the depth cap", r.Issues())
	}
}

// countingRA is an image whose reader counts every ReadAt call, so a test can
// pin how much work a disc-supplied size made the walk do.
type countingRA struct {
	data  []byte
	calls int64
}

func (c *countingRA) ReadAt(p []byte, off int64) (int, error) {
	c.calls++
	if off >= int64(len(c.data)) {
		return 0, io.EOF
	}
	n := copy(p, c.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// flatten joins the builder's sectors into one image in memory, the way the
// other damage tests build theirs.
func flatten(b *isoBuilder) []byte {
	img := make([]byte, 0, len(b.sectors)*2048)
	for _, s := range b.sectors {
		img = append(img, s...)
	}
	return img
}

// A directory beyond the end of the volume must not silently yield nothing. The
// room to the volume's end goes negative, and the clamp turns it into zero
// sectors plus a report instead of a negative count the loop ignores.
func TestDirectoryPastEndOfVolume(t *testing.T) {
	b := newISOBuilder()
	b.buildSetTerminator(b.alloc()) // 17
	rootLBA := b.alloc()            // 18
	root := b.sector(rootLBA)
	off := putDirRecord(root, 0, uint32(rootLBA), 2048, true, []byte{0x00})
	off = putDirRecord(root, off, uint32(rootLBA), 2048, true, []byte{0x01})
	fileLBA := b.alloc() // 19
	putDirRecord(root, off, uint32(fileLBA), 4, false, []byte("A.TXT;1"))
	copy(b.sector(fileLBA), []byte("data"))
	// The root directory record points past the end of a three-sector volume.
	b.buildPVD(uint32(rootLBA), 2048, "ROOMLESS")
	binary.LittleEndian.PutUint32(b.sectors[16][80:84], 3)

	r, err := OpenReader(&countingRA{data: flatten(b)})
	if err != nil {
		t.Fatal(err)
	}
	var entries int
	if err := r.Walk(func(DirEntry) error {
		entries++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if entries != 0 {
		t.Errorf("the walk found %d entries in a directory past the volume end", entries)
	}
	var named bool
	for _, is := range r.Issues() {
		if strings.Contains(is, "more than") && strings.Contains(is, "of volume after it") {
			named = true
		}
	}
	if !named {
		t.Errorf("issues = %v; want one naming the directory as past the volume", r.Issues())
	}
}

// A PVD whose volume size is zero cannot bound a corrupt declared size, so a
// constant cap stands in: the two million sector reads a 4 GiB declaration
// would drive are cut to the cap, with an issue saying so.
func TestDirectorySizeClampedWithoutVolumeCount(t *testing.T) {
	b := newISOBuilder()
	b.buildSetTerminator(b.alloc()) // 17
	rootLBA := b.alloc()            // 18
	root := b.sector(rootLBA)
	off := putDirRecord(root, 0, uint32(rootLBA), 2048, true, []byte{0x00})
	putDirRecord(root, off, uint32(rootLBA), 2048, true, []byte{0x01})
	b.buildPVD(uint32(rootLBA), 0xFFFFF800, "OGRE")
	binary.LittleEndian.PutUint32(b.sectors[16][80:84], 0)

	img := countingRA{data: flatten(b)}
	r, err := OpenReader(&img)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Walk(func(DirEntry) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if img.calls > maxDirSectors+8 {
		t.Errorf("the walk made %d sector reads; want no more than the %d-sector cap", img.calls, maxDirSectors)
	}
	var named bool
	for _, is := range r.Issues() {
		if strings.Contains(is, "more than") && strings.Contains(is, "a directory can span") {
			named = true
		}
	}
	if !named {
		t.Errorf("issues = %v; want one naming the per-directory sector cap", r.Issues())
	}
}

// A damaged area can hold many CE entries, each pointing at a different
// offset. Following all of them fans out past anything an image could hold, so
// only the first continuation of each area is followed.
func TestRockRidgeContinuationFanOut(t *testing.T) {
	b := newISOBuilder()
	b.buildSetTerminator(b.alloc()) // 17
	rootLBA := b.alloc()            // 18
	ceLBA := b.alloc()              // 19
	root := b.sector(rootLBA)
	dotRecLen := 42
	root[0] = byte(dotRecLen)
	root[25] = 0x02
	root[32] = 1
	binary.LittleEndian.PutUint32(root[2:6], uint32(rootLBA))
	binary.LittleEndian.PutUint32(root[10:14], 2048)
	copy(root[34:], []byte{'S', 'P', 7, 1, 0xBE, 0xEF, 0})
	off := putDirRecord(root, dotRecLen, uint32(rootLBA), 2048, true, []byte{0x01})
	appendSUARecord(root, off, []byte("F.;1"), ceEntry(uint32(ceLBA), 0, 2048))

	// The continuation sector is nothing but CE entries, each pointing at the
	// next 16-byte slot of the same sector.
	area := b.sector(ceLBA)
	for slot := range 2048 / 16 {
		copy(area[slot*16:], ceEntry(uint32(ceLBA), uint32((slot+1)*16%2048), 2048-uint32((slot+1)*16%2048)))
	}
	b.buildPVD(uint32(rootLBA), 2048, "FANOUT")

	ra := &countingRA{data: flatten(b)}
	r, err := OpenReader(ra)
	if err != nil {
		t.Fatal(err)
	}
	before := ra.calls
	if err := r.Walk(func(DirEntry) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if reads := ra.calls - before; reads > 2+maxCEDepth {
		t.Errorf("the walk made %d reads; one per continuation level is at most %d", reads, 2+maxCEDepth)
	}
}

// The volume size bounding a directory comes off the disc too. When it is
// damaged as well, nothing on the disc bounds the loop, so the walk stops at
// the end of the image instead of making a failing read for every sector the
// declared size reaches.
func TestDirectoryStopsAtTheEndOfTheImage(t *testing.T) {
	b := newISOBuilder()
	b.buildSetTerminator(b.alloc()) // 17
	rootLBA := b.alloc()            // 18
	root := b.sector(rootLBA)
	off := putDirRecord(root, 0, uint32(rootLBA), 2048, true, []byte{0x00})
	off = putDirRecord(root, off, uint32(rootLBA), 2048, true, []byte{0x01})
	putDirRecord(root, off, 0, 0, false, []byte("A.TXT;1"))
	b.buildPVD(uint32(rootLBA), 0xFFFFF800, "PASTEND")
	pvd := b.sector(16)
	binary.LittleEndian.PutUint32(pvd[80:84], 0xFFFFFFF0)
	binary.BigEndian.PutUint32(pvd[84:88], 0xFFFFFFF0)

	ra := &countingRA{data: flatten(b)}
	r, err := OpenReader(ra)
	if err != nil {
		t.Fatal(err)
	}
	before := ra.calls
	if err := r.Walk(func(DirEntry) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if reads := ra.calls - before; reads > 4 {
		t.Errorf("the walk made %d reads over a one-sector directory at the end of the image", reads)
	}
	var stopped bool
	for _, is := range r.Issues() {
		if strings.Contains(is, "runs past the end of the image") {
			stopped = true
		}
	}
	if !stopped {
		t.Errorf("issues = %q, want one saying the directory runs past the end", r.Issues())
	}
}
