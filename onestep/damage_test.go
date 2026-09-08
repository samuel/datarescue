package onestep_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"samuelks.com/datarescue/ddrescue"
	"samuelks.com/datarescue/internal/test1step"
	"samuelks.com/datarescue/onestep"
)

// writeDisks builds a set, hands each disk's bytes to damage for mutation, and
// returns the paths written.
func writeDisks(t *testing.T, opts test1step.Options, damage func(disk int, b []byte) []byte) []string {
	t.Helper()
	dir := t.TempDir()
	disks := test1step.Build(opts)
	paths := make([]string, 0, len(disks))
	for i, b := range disks {
		if damage != nil {
			b = damage(i+1, b)
		}
		path := filepath.Join(dir, fmt.Sprintf("disk%d.1-Step", i+1))
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	return paths
}

func openPaths(t *testing.T, paths ...string) *onestep.Archive {
	t.Helper()
	a, err := onestep.Open(paths...)
	if err != nil {
		t.Fatalf("Open: %s", err)
	}
	t.Cleanup(func() { a.Close() }) //nolint:errcheck // read-only
	return a
}

func fileNamed(t *testing.T, a *onestep.Archive, name string) *onestep.File {
	t.Helper()
	for i := range a.Files() {
		if a.Files()[i].Name == name {
			return &a.Files()[i]
		}
	}
	t.Fatalf("%s is not in the backup", name)
	return nil
}

func kinds(diags []onestep.Diagnostic) map[onestep.DiagnosticKind]int {
	out := map[onestep.DiagnosticKind]int{}
	for _, d := range diags {
		out[d.Kind]++
	}
	return out
}

// attribChunk is the serial of ATTRIB.EXE's only chunk: the files before it in
// DefaultFiles contribute one chunk each.
const attribChunk = 3

// fileData returns the contents of the named file in a DefaultFiles set.
func fileData(files []test1step.File, name string) []byte {
	for _, f := range files {
		if f.Name == name {
			return f.Data
		}
	}
	return nil
}

// TestBadChecksumIsReported checks that a chunk whose data does not match its
// recorded checksum is still delivered, and that the caller is told.
func TestBadChecksumIsReported(t *testing.T) {
	t.Parallel()
	want := test1step.DefaultFiles()
	paths := writeDisks(t, test1step.Options{Compress: true, BadChecksum: attribChunk}, nil)
	a := openPaths(t, paths...)
	noDiagnostics(t, a)

	f := fileNamed(t, a, "ATTRIB.EXE")
	got, issues, err := readAll(t, a, f)
	if err != nil {
		t.Fatalf("read: %s", err)
	}
	if len(issues) == 0 {
		t.Error("a wrong checksum was not reported")
	}
	for _, e := range issues {
		if !strings.Contains(e.Error(), "checksum") {
			t.Errorf("unexpected problem: %s", e)
		}
	}
	// The bytes are handed back regardless: they may still be the file.
	if !bytes.Equal(got, fileData(want, "ATTRIB.EXE")) {
		t.Error("the data was withheld or altered because its checksum was wrong")
	}

	if c := a.Check(f); c.OK() {
		t.Error("Check reported a file with a wrong checksum as intact")
	}
}

// TestTruncatedDiskInSet checks a set whose first image is cut short: the
// mismatch is reported, the data that is gone is reported as unreachable, and
// the files stored on the intact disk still come back.
func TestTruncatedDiskInSet(t *testing.T) {
	t.Parallel()
	want := test1step.DefaultFiles()
	var cut int
	paths := writeDisks(t, test1step.Options{Disks: 2}, func(disk int, b []byte) []byte {
		if disk != 1 {
			return b
		}
		// Drop half of the first image's data, which loses the files stored
		// there without touching the catalog on the second.
		cut = (len(b) - onestep.HeaderSize) / 2
		return b[:len(b)-cut]
	})
	a := openPaths(t, paths...)

	got := kinds(a.Diagnostics())
	if got[onestep.DiagDiskTruncated] == 0 {
		t.Error("a truncated image was not reported")
	}
	if got[onestep.DiagChunkUnplaced] == 0 {
		t.Error("the data the truncation lost was not reported")
	}

	// Something has to be lost and something has to survive, or the test is
	// not exercising the boundary it means to.
	var recovered, lost int
	for i := range a.Files() {
		f := &a.Files()[i]
		data, _, err := readAll(t, a, f)
		if err != nil {
			lost++
			continue
		}
		if !bytes.Equal(data, want[i].Data) {
			t.Errorf("%s: read without error but the contents differ", f.Path)
			continue
		}
		recovered++
	}
	if lost == 0 {
		t.Errorf("every file read back from a set missing %d bytes", cut)
	}
	if recovered == 0 {
		t.Error("no file survived, but the second disk is intact")
	}
}

// TestCorruptDeflateStream checks that a chunk whose compressed bytes are
// damaged reports the failure and does not take the rest of the backup with it.
func TestCorruptDeflateStream(t *testing.T) {
	t.Parallel()
	want := test1step.DefaultFiles()
	// The first chunk starts right after the header; scribble over it.
	paths := writeDisks(t, test1step.Options{Compress: true}, func(_ int, b []byte) []byte {
		copy(b[onestep.HeaderSize:], bytes.Repeat([]byte{0xFF}, 8))
		return b
	})
	a := openPaths(t, paths...)

	first := a.Files()[0]
	if _, _, err := readAll(t, a, &first); err == nil {
		t.Error("reading a corrupt deflate stream succeeded")
	} else {
		if _, ok := errors.AsType[*onestep.ChunkReadError](err); !ok {
			t.Errorf("got %v, want a *ChunkReadError", err)
		}
	}

	// Every later file is stored elsewhere in the stream and must be intact.
	for i := 1; i < len(a.Files()); i++ {
		f := &a.Files()[i]
		got, issues, err := readAll(t, a, f)
		if err != nil {
			t.Errorf("%s: %s", f.Path, err)
			continue
		}
		for _, e := range issues {
			t.Errorf("%s: %s", f.Path, e)
		}
		if !bytes.Equal(got, want[i].Data) {
			t.Errorf("%s: damage to another chunk changed its contents", f.Path)
		}
	}
}

// TestLatchedError checks that a reader stopped by damage stays stopped, so a
// caller cannot splice later data in at the wrong offset.
func TestLatchedError(t *testing.T) {
	t.Parallel()
	paths := writeDisks(t, test1step.Options{Compress: true}, func(_ int, b []byte) []byte {
		copy(b[onestep.HeaderSize:], bytes.Repeat([]byte{0xFF}, 8))
		return b
	})
	a := openPaths(t, paths...)
	f := a.Files()[0]
	r := a.OpenFile(&f)
	if _, err := io.Copy(io.Discard, r); err == nil {
		t.Fatal("reading a corrupt chunk succeeded")
	}
	buf := make([]byte, 64)
	n, err := r.Read(buf)
	if n != 0 || err == nil {
		t.Errorf("a retry returned %d bytes and %v; the error should be latched", n, err)
	}
}

// TestUnreadableSectors checks the ddrescue path: a bad sector costs the file
// that lives in it, and the files around it still come back.
func TestUnreadableSectors(t *testing.T) {
	t.Parallel()
	want := test1step.DefaultFiles()
	disks := test1step.Build(test1step.Options{Compress: true})
	dir := t.TempDir()
	image := filepath.Join(dir, "disk1.1-Step")
	if err := os.WriteFile(image, disks[0], 0o600); err != nil {
		t.Fatal(err)
	}

	// Open the image undamaged first to learn where the last file's data is,
	// so the map can mark exactly that as unreadable.
	probe := openPaths(t, image)
	target := probe.Files()[len(probe.Files())-1]
	if len(target.Chunks) != 1 {
		t.Fatalf("%s has %d chunks; the test wants one", target.Name, len(target.Chunks))
	}
	badStart := target.Chunks[0].Offset
	badSize := target.Chunks[0].CompSize

	// A chunk's offset is a position in its own disk's file, so the failed
	// range goes straight into the map.
	total := int64(len(disks[0]))
	body := fmt.Sprintf("0x%08x  0x%08x  +\n0x%08x  0x%08x  -\n0x%08x  0x%08x  +\n",
		0, badStart, badStart, badSize, badStart+badSize, total-badStart-badSize)
	mapFile := image + ".map"
	if err := os.WriteFile(mapFile, []byte("0x00000000     ?               1\n"+body), 0o600); err != nil {
		t.Fatal(err)
	}

	a := openPaths(t, image)
	if got := a.Disks()[0].MapPath; got != mapFile {
		t.Errorf("MapPath = %q, want the sidecar to be found automatically", got)
	}
	for i := range a.Files() {
		f := &a.Files()[i]
		got, issues, err := readAll(t, a, f)
		if f.Name == target.Name {
			if err == nil {
				t.Errorf("%s: reading an unrecovered sector succeeded", f.Path)
			} else if !errors.Is(err, ddrescue.ErrNotRecovered) {
				t.Errorf("%s: got %v, want ErrNotRecovered", f.Path, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %s", f.Path, err)
			continue
		}
		for _, e := range issues {
			t.Errorf("%s: %s", f.Path, e)
		}
		if !bytes.Equal(got, want[i].Data) {
			t.Errorf("%s: contents differ", f.Path)
		}
	}
}

// TestCatalogDirectoryScanFallback checks that the tables are still found when
// the member directory at the head of the catalog is unreadable.
func TestCatalogDirectoryScanFallback(t *testing.T) {
	t.Parallel()
	want := test1step.DefaultFiles()
	paths := writeDisks(t, test1step.Options{Compress: true}, func(_ int, b []byte) []byte {
		// The catalog offset is in the header; wipe the directory magic there.
		off := int64(b[0x1c]) | int64(b[0x1d])<<8 | int64(b[0x1e])<<16 | int64(b[0x1f])<<24
		copy(b[off:], bytes.Repeat([]byte{0}, 16))
		return b
	})
	a := openPaths(t, paths...)

	got := kinds(a.Diagnostics())
	if got[onestep.DiagDirectoryUnreadable] == 0 {
		t.Error("the unreadable directory was not reported")
	}
	if got[onestep.DiagDirectoryScan] == 0 {
		t.Error("the fallback scan was not reported")
	}
	if len(a.Files()) != len(want) {
		t.Fatalf("the scan found %d files, want %d", len(a.Files()), len(want))
	}
	for i := range a.Files() {
		f := &a.Files()[i]
		if f.Path != want[i].Path() {
			t.Errorf("file %d path = %q, want %q", i, f.Path, want[i].Path())
		}
		data, issues, err := readAll(t, a, f)
		if err != nil {
			t.Errorf("%s: %s", f.Path, err)
			continue
		}
		for _, e := range issues {
			t.Errorf("%s: %s", f.Path, e)
		}
		if !bytes.Equal(data, want[i].Data) {
			t.Errorf("%s: contents differ", f.Path)
		}
	}
	// The member list comes from the directory, so it is gone.
	if len(a.Members()) != 0 {
		t.Errorf("got %d members with no directory to name them", len(a.Members()))
	}
}

// bigFirstChunk is the serial of BIG.BIN's first chunk in a DefaultFiles set:
// the three files before it contribute one chunk each.
const bigFirstChunk = 4

// TestChunkStreamEndsShort checks a chunk whose recorded compressed size is
// smaller than the original size it declares — one corrupt catalog digit. The
// stream then ends cleanly part way through the chunk, which is damage and not
// a finished chunk: calling it finished would land every chunk after it at the
// wrong file offset with nothing to mark the gap.
func TestChunkStreamEndsShort(t *testing.T) {
	t.Parallel()
	const short = 100
	paths := writeDisks(t, test1step.Options{
		ShortCompSize: map[int]int64{bigFirstChunk: short},
	}, nil)
	a := openPaths(t, paths...)

	f := fileNamed(t, a, "BIG.BIN")
	got, _, err := readAll(t, a, f)
	if err == nil {
		t.Fatal("a chunk that ended short of its recorded size was accepted")
	}
	if _, ok := errors.AsType[*onestep.ChunkReadError](err); !ok {
		t.Fatalf("got %v, want a *ChunkReadError", err)
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("got %v, want it to wrap io.ErrUnexpectedEOF", err)
	}
	if int64(len(got)) != test1step.ChunkSize-short {
		t.Errorf("recovered %d bytes, want the %d the chunk delivered", len(got), test1step.ChunkSize-short)
	}
}

// The same chunk read through an armed gap log: the hole is the bytes the
// stream did not deliver, so the file comes back at its declared length and
// every chunk after the damage still lands where the file puts it.
func TestFillChunkStreamEndsShort(t *testing.T) {
	t.Parallel()
	const short = 100
	want := fileData(test1step.DefaultFiles(), "BIG.BIN")
	paths := writeDisks(t, test1step.Options{
		ShortCompSize: map[int]int64{bigFirstChunk: short},
	}, nil)
	a := openPaths(t, paths...)

	f := fileNamed(t, a, "BIG.BIN")
	got, filled := fill(t, a.OpenFile(f))
	if filled.Err != nil {
		t.Fatalf("filling: %s", filled.Err)
	}
	if len(filled.Gaps) != 1 {
		t.Fatalf("recorded %d gaps, want 1: %v", len(filled.Gaps), filled.Gaps)
	}
	if g := filled.Gaps[0]; g.FileOffset != test1step.ChunkSize-short || g.Length != short {
		t.Errorf("gap at %d of %d bytes; want %d of %d", g.FileOffset, g.Length, test1step.ChunkSize-short, short)
	}
	if int64(len(got)) != f.Size {
		t.Fatalf("recovered %d bytes, want the declared %d", len(got), f.Size)
	}
	if !bytes.Equal(got, zeroed(want, filled.Gaps)) {
		t.Error("the chunks after the hole did not land where the file puts them")
	}
}
