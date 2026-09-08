package iso

import (
	"encoding/binary"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"unicode/utf16"
)

// openPath reads an image from a path. The package itself never takes one —
// every caller reads through a reader it owns, an image coming in through its
// rescue map — so the file is left open for the process, which is all a test
// needs.
func openPath(path string) (*Reader, error) {
	return openOwned(path, func(f *os.File) (*Reader, error) { return OpenReader(f) })
}

func openPathWithSectorSize(path string, sectorSize int) (*Reader, error) {
	return openOwned(path, func(f *os.File) (*Reader, error) { return OpenReaderWithSectorSize(f, sectorSize) })
}

func openOwned(path string, open func(*os.File) (*Reader, error)) (*Reader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	r, err := open(f)
	if err != nil {
		f.Close() //nolint:errcheck // best-effort cleanup on the error path
		return nil, err
	}
	return r, nil
}

// isoBuilder constructs a minimal cooked (2048-byte sector) ISO image.
// Sector layout:
//
//	0–15  system area (blank)
//	16    PVD  ← written by buildPVD; pre-allocated so alloc() skips it
//	17+   caller-allocated sectors (VD terminator, dirs, files…)
type isoBuilder struct {
	sectors [][]byte
}

func newISOBuilder() *isoBuilder {
	b := &isoBuilder{}
	// Sectors 0–16: system area + PVD placeholder.
	for range 17 {
		b.sectors = append(b.sectors, make([]byte, 2048))
	}
	return b
}

// alloc appends a blank sector and returns its LBA.
func (b *isoBuilder) alloc() int {
	lba := len(b.sectors)
	b.sectors = append(b.sectors, make([]byte, 2048))
	return lba
}

func (b *isoBuilder) sector(lba int) []byte { return b.sectors[lba] }

// buildPVD writes the Primary Volume Descriptor at the pre-allocated sector 16.
func (b *isoBuilder) buildPVD(rootLBA, rootSize uint32, label string) {
	pvd := b.sectors[16]
	pvd[0] = 0x01 // PVD type
	copy(pvd[1:6], "CD001")
	pvd[6] = 0x01
	padded := make([]byte, 32)
	copy(padded, strings.ToUpper(label))
	copy(pvd[40:72], padded)
	totalSecs := uint32(len(b.sectors))
	binary.LittleEndian.PutUint32(pvd[80:84], totalSecs)
	binary.BigEndian.PutUint32(pvd[84:88], totalSecs)
	binary.LittleEndian.PutUint16(pvd[128:130], 2048)
	binary.BigEndian.PutUint16(pvd[130:132], 2048)
	putRootDirRecord(pvd, 156, rootLBA, rootSize)
}

// buildJolietSVD writes a Joliet Supplementary Volume Descriptor at lba.
func (b *isoBuilder) buildJolietSVD(lba int, rootLBA, rootSize uint32, label string) {
	svd := b.sectors[lba]
	svd[0] = 0x02 // Supplementary VD
	copy(svd[1:6], "CD001")
	svd[6] = 0x01
	copy(svd[40:72], padUCS2(label, 32))
	copy(svd[88:91], "%/@") // Joliet level 1
	binary.LittleEndian.PutUint16(svd[128:130], 2048)
	binary.BigEndian.PutUint16(svd[130:132], 2048)
	putRootDirRecord(svd, 156, rootLBA, rootSize)
}

// buildSetTerminator writes a VD Set Terminator at lba.
func (b *isoBuilder) buildSetTerminator(lba int) {
	vd := b.sectors[lba]
	vd[0] = 0xFF
	copy(vd[1:6], "CD001")
	vd[6] = 0x01
}

// write flushes all sectors to a temp file and returns its path.
func (b *isoBuilder) write(_ string) string {
	f, err := os.CreateTemp("", "iso_test_*.iso")
	if err != nil {
		panic(err)
	}
	defer f.Close() //nolint:errcheck
	for _, s := range b.sectors {
		if _, err := f.Write(s); err != nil {
			panic(err)
		}
	}
	return f.Name()
}

// putDirRecord writes an ISO 9660 directory record into dst at *offset.
// Returns the offset after the record.
func putDirRecord(dst []byte, offset int, lba, size uint32, isDir bool, name []byte) int {
	nameLen := len(name)
	recLen := 33 + nameLen
	if recLen%2 != 0 {
		recLen++
	}
	if offset+recLen > len(dst) {
		return offset
	}
	rec := dst[offset:]
	rec[0] = byte(recLen)
	binary.LittleEndian.PutUint32(rec[2:6], lba)
	binary.BigEndian.PutUint32(rec[6:10], lba)
	binary.LittleEndian.PutUint32(rec[10:14], size)
	binary.BigEndian.PutUint32(rec[14:18], size)
	if isDir {
		rec[25] = 0x02
	}
	rec[32] = byte(nameLen)
	copy(rec[33:], name)
	return offset + recLen
}

// putRootDirRecord writes the 34-byte root directory record used in VDs.
func putRootDirRecord(vd []byte, offset int, lba, size uint32) {
	rec := vd[offset : offset+34]
	rec[0] = 34
	binary.LittleEndian.PutUint32(rec[2:6], lba)
	binary.BigEndian.PutUint32(rec[6:10], lba)
	binary.LittleEndian.PutUint32(rec[10:14], size)
	binary.BigEndian.PutUint32(rec[14:18], size)
	rec[25] = 0x02
	rec[32] = 1
	rec[33] = 0x00 // dot
}

// ---- UCS-2 helpers ---------------------------------------------------------

func encodeUCS2(s string) []byte {
	u16 := utf16.Encode([]rune(s))
	out := make([]byte, len(u16)*2)
	for i, v := range u16 {
		binary.BigEndian.PutUint16(out[i*2:], v)
	}
	return out
}

func padUCS2(s string, n int) []byte {
	b := encodeUCS2(s)
	padded := make([]byte, n)
	copy(padded, b)
	for i := len(b); i+1 < n; i += 2 {
		padded[i] = 0x00
		padded[i+1] = 0x20
	}
	return padded
}

// ---- Plain ISO 9660 --------------------------------------------------------

func buildPlainISO(t *testing.T) (path string, cleanup func()) {
	t.Helper()
	b := newISOBuilder()

	// VD Set Terminator at 17.
	termLBA := b.alloc() // 17
	b.buildSetTerminator(termLBA)

	// Root directory at 18.
	rootLBA := b.alloc() // 18
	root := b.sector(rootLBA)
	off := putDirRecord(root, 0, uint32(rootLBA), 2048, true, []byte{0x00})
	off = putDirRecord(root, off, uint32(rootLBA), 2048, true, []byte{0x01})

	// File: README.TXT at 19.
	fileLBA := b.alloc() // 19
	fileContent := []byte("hello world\n")
	copy(b.sector(fileLBA), fileContent)
	putDirRecord(root, off, uint32(fileLBA), uint32(len(fileContent)), false, []byte("README.TXT;1"))

	b.buildPVD(uint32(rootLBA), 2048, "TESTDISC")

	path = b.write(t.Name())
	return path, func() { _ = os.Remove(path) }
}

func TestOpenPlainISO(t *testing.T) {
	path, cleanup := buildPlainISO(t)
	defer cleanup()

	r, err := openPath(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if r.VolumeLabel() != "TESTDISC" {
		t.Errorf("VolumeLabel = %q; want %q", r.VolumeLabel(), "TESTDISC")
	}
	if r.HasJoliet() {
		t.Error("HasJoliet() = true; want false")
	}
}

func TestWalkPlainISO(t *testing.T) {
	path, cleanup := buildPlainISO(t)
	defer cleanup()

	r, err := openPath(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	var files []string
	if err := r.Walk(func(e DirEntry) error {
		files = append(files, e.Path)
		return nil
	}); err != nil {
		t.Fatalf("Walk: %v", err)
	}

	if len(files) != 1 || files[0] != "/README.TXT" {
		t.Errorf("Walk files = %v; want [/README.TXT]", files)
	}
	if r.HasRockRidge() {
		t.Error("HasRockRidge() = true; want false")
	}
}

func TestOpenFile(t *testing.T) {
	path, cleanup := buildPlainISO(t)
	defer cleanup()

	r, err := openPath(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	var entry DirEntry
	_ = r.Walk(func(e DirEntry) error {
		if e.Path == "/README.TXT" {
			entry = e
		}
		return nil
	})

	data, err := io.ReadAll(r.OpenFile(entry))
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(data) != "hello world\n" {
		t.Errorf("file data = %q; want %q", data, "hello world\n")
	}
}

func TestOpenWithSectorSize(t *testing.T) {
	path, cleanup := buildPlainISO(t)
	defer cleanup()

	r, err := openPathWithSectorSize(path, 2048)
	if err != nil {
		t.Fatalf("openPathWithSectorSize: %v", err)
	}

	if r.VolumeLabel() != "TESTDISC" {
		t.Errorf("VolumeLabel = %q; want TESTDISC", r.VolumeLabel())
	}
}

func TestOpenNotISO(t *testing.T) {
	f, err := os.CreateTemp("", "notiso_*.bin")
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	defer func() { _ = os.Remove(path) }()
	_, err = f.Write(make([]byte, 2048*20))
	if err != nil {
		t.Fatal(err)
	}
	f.Close() //nolint:errcheck

	if _, err := openPath(path); !errors.Is(err, ErrNotISO9660) {
		t.Errorf("opening a non-ISO = %v; want ErrNotISO9660", err)
	}
}

// ---- Joliet ----------------------------------------------------------------

func buildJolietISO(t *testing.T) (path string, cleanup func()) {
	t.Helper()
	b := newISOBuilder()

	// Sector 17: Joliet SVD (populated after we know root LBAs).
	svdLBA := b.alloc() // 17
	// Sector 18: VD Set Terminator.
	termLBA := b.alloc() // 18
	b.buildSetTerminator(termLBA)

	// Plain ISO root at 19.
	rootLBA := b.alloc() // 19
	isoRoot := b.sector(rootLBA)
	isoOff := putDirRecord(isoRoot, 0, uint32(rootLBA), 2048, true, []byte{0x00})
	isoOff = putDirRecord(isoRoot, isoOff, uint32(rootLBA), 2048, true, []byte{0x01})

	// Shared file at 20.
	fileLBA := b.alloc() // 20
	copy(b.sector(fileLBA), []byte("content"))
	putDirRecord(isoRoot, isoOff, uint32(fileLBA), 7, false, []byte("LONGFIL~.TXT;1"))

	// Joliet root at 21.
	jolietRootLBA := b.alloc() // 21
	jolietRoot := b.sector(jolietRootLBA)
	joff := putDirRecord(jolietRoot, 0, uint32(jolietRootLBA), 2048, true, []byte{0x00})
	joff = putDirRecord(jolietRoot, joff, uint32(jolietRootLBA), 2048, true, []byte{0x01})
	// Joliet name: "LongFileName.txt" in UCS-2 + ";1" suffix in UCS-2.
	jolietName := append(encodeUCS2("LongFileName.txt"), 0x00, 0x3B, 0x00, 0x31)
	putDirRecord(jolietRoot, joff, uint32(fileLBA), 7, false, jolietName)

	b.buildPVD(uint32(rootLBA), 2048, "TESTDISC")
	b.buildJolietSVD(svdLBA, uint32(jolietRootLBA), 2048, "JOLIETDISC")

	path = b.write(t.Name())
	return path, func() { _ = os.Remove(path) }
}

func TestJolietDetection(t *testing.T) {
	path, cleanup := buildJolietISO(t)
	defer cleanup()

	r, err := openPath(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if !r.HasJoliet() {
		t.Error("HasJoliet() = false; want true")
	}
	if r.VolumeLabel() != "JOLIETDISC" {
		t.Errorf("VolumeLabel = %q; want JOLIETDISC", r.VolumeLabel())
	}
}

func TestJolietWalk(t *testing.T) {
	path, cleanup := buildJolietISO(t)
	defer cleanup()

	r, err := openPath(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	var files []string
	if err := r.Walk(func(e DirEntry) error {
		files = append(files, e.Path)
		return nil
	}); err != nil {
		t.Fatalf("Walk: %v", err)
	}

	if len(files) != 1 || files[0] != "/LongFileName.txt" {
		t.Errorf("Walk files = %v; want [/LongFileName.txt]", files)
	}
}

// ---- Rock Ridge ------------------------------------------------------------

func buildRockRidgeISO(t *testing.T) (path string, cleanup func()) {
	t.Helper()
	b := newISOBuilder()

	// VD Set Terminator at 17.
	termLBA := b.alloc() // 17
	b.buildSetTerminator(termLBA)

	// Root directory at 18.
	rootLBA := b.alloc() // 18
	root := b.sector(rootLBA)

	// Dot entry with SP (System Use Sharing Protocol) entry.
	// SP: sig(2) + len(1)=7 + ver(1)=1 + 0xBE + 0xEF + skipCount(1)=0.
	spEntry := []byte{'S', 'P', 7, 1, 0xBE, 0xEF, 0}
	// nameLen=1 (\x00), suaStart=34 (even), dotRecLen = 34+7 = 41 → pad to 42.
	dotRecLen := 42
	root[0] = byte(dotRecLen)
	root[25] = 0x02 // directory flag
	root[32] = 1    // nameLen
	root[33] = 0x00 // dot name
	binary.LittleEndian.PutUint32(root[2:6], uint32(rootLBA))
	binary.LittleEndian.PutUint32(root[10:14], 2048)
	copy(root[34:], spEntry)
	off := dotRecLen

	// Dotdot entry (plain).
	off = putDirRecord(root, off, uint32(rootLBA), 2048, true, []byte{0x01})

	// File at 19 with Rock Ridge NM entry giving a mixed-case long name.
	fileLBA := b.alloc() // 19
	copy(b.sector(fileLBA), []byte("rr content"))
	off = appendRRFileRecord(root, off, uint32(fileLBA), 10, []byte("MIXEDCA~.TXT;1"), "mixedcase.txt")

	// Symlink record with SL entry (no real file LBA).
	appendRRSymlinkRecord(root, off, []byte("SYMLINK.;1"), "/target")

	b.buildPVD(uint32(rootLBA), 2048, "RRDISC")

	path = b.write(t.Name())
	return path, func() { _ = os.Remove(path) }
}

// appendRRFileRecord writes a directory record with an NM System Use entry.
func appendRRFileRecord(dst []byte, offset int, lba, size uint32, isoName []byte, rrName string) int {
	// NM entry: sig(2) + len(1) + ver(1) + flags(1) + name.
	nmEntry := append([]byte{'N', 'M', 0, 1, 0}, []byte(rrName)...)
	nmEntry[2] = byte(len(nmEntry))

	nameLen := len(isoName)
	suaStart := 33 + nameLen
	if suaStart%2 != 0 {
		suaStart++
	}
	recLen := suaStart + len(nmEntry)
	if recLen%2 != 0 {
		recLen++
	}

	rec := dst[offset : offset+recLen]
	rec[0] = byte(recLen)
	binary.LittleEndian.PutUint32(rec[2:6], lba)
	binary.BigEndian.PutUint32(rec[6:10], lba)
	binary.LittleEndian.PutUint32(rec[10:14], size)
	binary.BigEndian.PutUint32(rec[14:18], size)
	rec[32] = byte(nameLen)
	copy(rec[33:], isoName)
	copy(rec[suaStart:], nmEntry)
	return offset + recLen
}

// appendRRSymlinkRecord writes a directory record with an SL System Use entry.
// target must be an absolute path like "/foo/bar".
func appendRRSymlinkRecord(dst []byte, offset int, isoName []byte, target string) int {
	// Build SL component records for the target path.
	// For simplicity we encode the whole target as a single component name.
	// parseSLComponents will include it via the default case.
	comp := append([]byte{0x00, byte(len(target))}, []byte(target)...)
	// SL entry: sig(2) + len(1) + ver(1) + flags(1) + components.
	slEntry := append([]byte{'S', 'L', 0, 1, 0}, comp...)
	slEntry[2] = byte(len(slEntry))

	nameLen := len(isoName)
	suaStart := 33 + nameLen
	if suaStart%2 != 0 {
		suaStart++
	}
	recLen := suaStart + len(slEntry)
	if recLen%2 != 0 {
		recLen++
	}

	rec := dst[offset : offset+recLen]
	rec[0] = byte(recLen)
	rec[32] = byte(nameLen)
	copy(rec[33:], isoName)
	copy(rec[suaStart:], slEntry)
	return offset + recLen
}

func TestRockRidgeDetection(t *testing.T) {
	path, cleanup := buildRockRidgeISO(t)
	defer cleanup()

	r, err := openPath(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if err := r.Walk(func(DirEntry) error { return nil }); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if !r.HasRockRidge() {
		t.Error("HasRockRidge() = false; want true")
	}
}

func TestRockRidgeNMName(t *testing.T) {
	path, cleanup := buildRockRidgeISO(t)
	defer cleanup()

	r, err := openPath(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	var files []string
	if err := r.Walk(func(e DirEntry) error {
		if e.Symlink == "" && !e.IsDir {
			files = append(files, e.Path)
		}
		return nil
	}); err != nil {
		t.Fatalf("Walk: %v", err)
	}

	var found bool
	for _, f := range files {
		if f == "/mixedcase.txt" {
			found = true
		}
	}
	if !found {
		t.Errorf("Walk files = %v; want /mixedcase.txt (Rock Ridge NM name)", files)
	}
}

func TestRockRidgeSymlink(t *testing.T) {
	path, cleanup := buildRockRidgeISO(t)
	defer cleanup()

	r, err := openPath(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	var symlinks []DirEntry
	if err := r.Walk(func(e DirEntry) error {
		if e.Symlink != "" {
			symlinks = append(symlinks, e)
		}
		return nil
	}); err != nil {
		t.Fatalf("Walk: %v", err)
	}

	if len(symlinks) != 1 {
		t.Fatalf("symlink count = %d; want 1", len(symlinks))
	}
	if symlinks[0].Symlink != "/target" {
		t.Errorf("symlink target = %q; want %q", symlinks[0].Symlink, "/target")
	}
}
