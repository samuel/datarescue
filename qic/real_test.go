package qic_test

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"samuelks.com/datarescue/qic"
)

// realDirEnv points the test at a directory of real archives. They cannot be
// committed, so this is how an operator who has some gets them checked.
const realDirEnv = "DATARESCUE_QIC_DIR"

// magics are the signatures the extensions in these archives imply. They check
// whether a file's recovered bytes start in the right place without trusting
// any implementation of the format, this one included. An extension carries
// alternatives where more than one format used it: .ICO was also the Windows
// 3.x icon resource, which begins "CI" and not the icon directory later
// versions write.
var magics = map[string][][]byte{
	".BMP": {[]byte("BM")},
	".EXE": {[]byte("MZ")},
	".DLL": {[]byte("MZ")},
	".VBX": {[]byte("MZ")},
	".WAV": {[]byte("RIFF")},
	".MID": {[]byte("MThd")},
	".ICO": {{0x00, 0x00, 0x01, 0x00}, []byte("CI\x1a")},
	".ZIP": {{'P', 'K', 0x03, 0x04}},
	".GIF": {[]byte("GIF8")},
	".PDF": {[]byte("%PDF")},
}

// TestRealArchives checks the whole-archive invariants that pinned the format
// down, which are properties of the format rather than of this reader's model
// of it. Damage is reported rather than failed on, since a damaged archive is
// what the test is usually pointed at; what does fail is a segment that
// decodes cleanly to the wrong length, a file that reads with no error and
// comes back the wrong size, and a recovered file that does not begin with the
// signature its name implies.
func TestRealArchives(t *testing.T) {
	dir := os.Getenv(realDirEnv)
	if dir == "" {
		t.Skipf("%s is not set", realDirEnv)
	}
	paths, err := filepath.Glob(filepath.Join(dir, "*.qic"))
	if err != nil {
		t.Fatal(err)
	}
	if more, _ := filepath.Glob(filepath.Join(dir, "*.QIC")); len(more) > 0 {
		paths = append(paths, more...)
	}
	if len(paths) == 0 {
		t.Skipf("%s holds no .qic file", dir)
	}

	var checked int
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			checked += checkArchive(t, path)
		})
	}
	if checked == 0 {
		t.Error("no file could be checked in any archive")
	}
}

func checkArchive(t *testing.T, path string) int {
	a, err := qic.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer a.Close() //nolint:errcheck // read-only

	v := a.Volume()
	t.Logf("%s, %s, %s, %d segments, %s of data, %s of directory",
		v.Label, v.Variant, compression(v), len(a.Segments()),
		bytesOf(v.DataSize), bytesOf(v.DirSize))

	// The grid: everything past the header region is whole segments.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	dataStart, _ := a.Layout()
	if grid := info.Size() - dataStart; grid%qic.SegmentSize != 0 {
		t.Errorf("the file holds %d bytes past its header region, not a whole number of %d-byte segments",
			grid, qic.SegmentSize)
	}

	// The segment offsets: each one continues the one before it without a gap,
	// and they total what the volume table records.
	segs := a.Segments()
	var pos int64
	for i, s := range segs[:a.DataSegments()] {
		if s.State != qic.SegmentOK {
			t.Logf("  segment %d is damaged: %s", i, s.State)
			pos = s.End()
			continue
		}
		if s.Start != pos {
			t.Errorf("segment %d starts at %d where the segments before it end at %d", i, s.Start, pos)
		}
		pos = s.End()
	}
	if pos != v.DataSize {
		t.Errorf("the segment offsets account for %d bytes where the volume table records %d", pos, v.DataSize)
	}
	if a.CatalogSize() < v.DirSize {
		t.Errorf("the catalog segments hold %d bytes where the volume table records %d", a.CatalogSize(), v.DirSize)
	}

	// Every file's position, and every file's bytes.
	var read, damaged, magicOK, magicBad int
	for i := range a.Files() {
		f := &a.Files()[i]
		if f.Dir {
			continue
		}
		data, err := io.ReadAll(a.OpenFile(f))
		switch {
		case err != nil:
			damaged++
			t.Logf("  %s stopped after %d of %d bytes: %v", f.Path, len(data), f.Size, err)
			continue
		case int64(len(data)) != f.Size:
			// Reading through without an error and still coming back short is
			// not damage, it is a reader that lost track of the file.
			t.Errorf("%s read without error but gave %d of %d bytes", f.Path, len(data), f.Size)
			continue
		}
		read++
		if want, ok := magics[strings.ToUpper(filepath.Ext(f.Name))]; ok {
			switch matched, tested := hasMagic(data, want); {
			case !tested:
			case matched:
				magicOK++
			default:
				magicBad++
				t.Errorf("%s begins %x, which is none of the signatures %s implies, so its position is wrong",
					f.Path, data[:min(4, len(data))], strings.ToUpper(filepath.Ext(f.Name)))
			}
		}
	}
	t.Logf("  %d files read whole, %d stopped on damage, %d signatures matched", read, damaged, magicOK)
	if magicBad > 0 {
		t.Errorf("%d files do not begin with the signature their name implies", magicBad)
	}

	for _, d := range a.Diagnostics() {
		t.Logf("  %s: %s", d.Kind, d.Message)
	}
	checkScanAgrees(t, path, a)
	return read
}

// checkScanAgrees cross-checks the two independent ways of locating a file.
// The catalog holds a size and a tree but no pointer to any data, so its
// offsets are a running total over every record; the data region holds no size
// but repeats each record inline with its path, so its offsets are read
// directly and its sizes are the distance to the next record less the trailer
// bytes. Nothing is shared between the two, so each checks the other: on both
// real archives they agree on every file, its size and its position.
func checkScanAgrees(t *testing.T, path string, fromCatalog *qic.Archive) {
	if fromCatalog.FromScan() {
		return // the catalog was unreadable, so there is nothing to compare
	}
	scan, err := qic.OpenSource(qic.Source{Path: path}, qic.Options{Mode: qic.ModeScan})
	if err != nil {
		t.Errorf("scanning the data region: %v", err)
		return
	}
	defer scan.Close() //nolint:errcheck // read-only

	var missing, wrongSize, wrongOffset int
	for i := range fromCatalog.Files() {
		want := fromCatalog.Files()[i]
		got, ok := scan.Find(want.Path)
		if !ok {
			missing++
			continue
		}
		if got.Size != want.Size {
			wrongSize++
		}
		if got.DataOffset() != want.DataOffset() {
			wrongOffset++
		}
	}
	if missing+wrongSize+wrongOffset > 0 {
		t.Errorf("the data region and the catalog disagree: %d files missing from the scan, %d sizes and %d positions differ",
			missing, wrongSize, wrongOffset)
	}
	t.Logf("  the data region's own records agree with the catalog on all %d entries", len(fromCatalog.Files()))
	_ = scan.Diagnostics()
}

// hasMagic reports whether the data starts with any of the signatures, and
// whether it was long enough to tell.
func hasMagic(data []byte, want [][]byte) (matched, tested bool) {
	for _, m := range want {
		if len(data) < len(m) {
			continue
		}
		tested = true
		if bytes.HasPrefix(data, m) {
			return true, true
		}
	}
	return false, tested
}

func compression(v qic.Volume) string {
	if !v.Compressed {
		return "stored"
	}
	return "QIC-122"
}

func bytesOf(n int64) string {
	switch {
	case n >= 1<<20:
		return itoa(n/(1<<20)) + " MiB"
	case n >= 1<<10:
		return itoa(n/(1<<10)) + " KiB"
	}
	return itoa(n) + " bytes"
}

func itoa(n int64) string {
	var b []byte
	if n == 0 {
		return "0"
	}
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
