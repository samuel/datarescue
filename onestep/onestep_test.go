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

	"samuelks.com/datarescue/internal/test1step"
	"samuelks.com/datarescue/onestep"
)

// build writes a synthetic set into a temporary directory and opens it.
func build(t *testing.T, opts test1step.Options) *onestep.Archive {
	t.Helper()
	paths, err := test1step.WriteSet(t.TempDir(), opts)
	if err != nil {
		t.Fatalf("WriteSet: %s", err)
	}
	a, err := onestep.Open(paths...)
	if err != nil {
		t.Fatalf("Open: %s", err)
	}
	t.Cleanup(func() { a.Close() }) //nolint:errcheck // read-only
	return a
}

func noDiagnostics(t *testing.T, a *onestep.Archive) {
	t.Helper()
	for _, d := range a.Diagnostics() {
		t.Errorf("unexpected diagnostic [%s] %s", d.Kind, d.Message)
	}
}

// readAll returns a file's contents, the problems the reader found in the data
// it delivered, and the error that stopped it.
func readAll(t *testing.T, a *onestep.Archive, f *onestep.File) ([]byte, []error, error) {
	t.Helper()
	r := a.OpenFile(f)
	var buf bytes.Buffer
	_, err := io.Copy(&buf, r)
	return buf.Bytes(), r.Errors(), err
}

func TestRoundTrip(t *testing.T) {
	t.Parallel()
	for _, compress := range []bool{false, true} {
		t.Run(fmt.Sprintf("compress=%v", compress), func(t *testing.T) {
			t.Parallel()
			want := test1step.DefaultFiles()
			a := build(t, test1step.Options{Compress: compress})
			noDiagnostics(t, a)

			files := a.Files()
			if len(files) != len(want) {
				t.Fatalf("got %d files, want %d", len(files), len(want))
			}
			if got := a.Job().Compressed; got != compress {
				t.Errorf("Job().Compressed = %v, want %v", got, compress)
			}
			for i := range files {
				f := &files[i]
				if f.Path != want[i].Path() {
					t.Errorf("file %d path = %q, want %q", i, f.Path, want[i].Path())
				}
				if f.Size != int64(len(want[i].Data)) {
					t.Errorf("%s size = %d, want %d", f.Path, f.Size, len(want[i].Data))
				}
				got, issues, err := readAll(t, a, f)
				if err != nil {
					t.Errorf("%s: %s", f.Path, err)
					continue
				}
				for _, e := range issues {
					t.Errorf("%s: %s", f.Path, e)
				}
				if !bytes.Equal(got, want[i].Data) {
					t.Errorf("%s: contents differ (%d bytes read, %d expected)", f.Path, len(got), len(want[i].Data))
				}
			}
		})
	}
}

// TestMixedStorage checks that a compressed backup still stores a chunk it
// could not shrink, and that both kinds come back from the same file set.
func TestMixedStorage(t *testing.T) {
	t.Parallel()
	a := build(t, test1step.Options{Compress: true})
	noDiagnostics(t, a)

	var stored, deflated int
	for _, f := range a.Files() {
		for _, c := range f.Chunks {
			if c.Stored() {
				stored++
			} else {
				deflated++
			}
		}
	}
	if stored == 0 {
		t.Error("no chunk was stored; the incompressible file should produce one")
	}
	if deflated == 0 {
		t.Error("no chunk was deflated in a compressed backup")
	}
}

// TestChunking checks that a file longer than one buffer is split the way the
// writer split it, and that the pieces are chained by the last-chunk flag.
func TestChunking(t *testing.T) {
	t.Parallel()
	a := build(t, test1step.Options{Compress: true})
	var big *onestep.File
	for i := range a.Files() {
		if a.Files()[i].Name == "BIG.BIN" {
			big = &a.Files()[i]
		}
	}
	if big == nil {
		t.Fatal("BIG.BIN is not in the backup")
	}
	want := (big.Size + onestep.ChunkSize - 1) / onestep.ChunkSize
	if int64(len(big.Chunks)) != want {
		t.Errorf("BIG.BIN has %d chunks, want %d", len(big.Chunks), want)
	}
	for i, c := range big.Chunks {
		if c.Sequence != i+1 {
			t.Errorf("chunk %d has sequence %d", i, c.Sequence)
		}
		if last := i == len(big.Chunks)-1; c.Last != last {
			t.Errorf("chunk %d Last = %v, want %v", i, c.Last, last)
		}
	}
}

func TestPaths(t *testing.T) {
	t.Parallel()
	a := build(t, test1step.Options{})
	want := map[string]bool{
		`C:\ROOT.TXT`:           true,
		`C:\DOS\EMPTY.DAT`:      true,
		`C:\DOS\ATTRIB.EXE`:     true,
		`C:\DOS\SUB\NESTED.TXT`: true,
		`C:\DOS\BIG.BIN`:        true,
		`C:\DOS\RANDOM.BIN`:     true,
	}
	for _, f := range a.Files() {
		if !want[f.Path] {
			t.Errorf("unexpected path %q", f.Path)
		}
		delete(want, f.Path)
	}
	for p := range want {
		t.Errorf("missing path %q", p)
	}
}

func TestFindIsCaseAndSeparatorInsensitive(t *testing.T) {
	t.Parallel()
	a := build(t, test1step.Options{})
	for _, p := range []string{
		`C:\DOS\ATTRIB.EXE`,
		`c:\dos\attrib.exe`,
		"c:/dos/attrib.exe",
		`\C:\DOS\ATTRIB.EXE`,
	} {
		if _, ok := a.Find(p); !ok {
			t.Errorf("Find(%q) did not match", p)
		}
	}
	if _, ok := a.Find(`C:\DOS\NOPE.EXE`); ok {
		t.Error("Find matched a path that is not in the backup")
	}
}

func TestMultiDiskSet(t *testing.T) {
	t.Parallel()
	for _, disks := range []int{2, 3} {
		t.Run(fmt.Sprintf("disks=%d", disks), func(t *testing.T) {
			t.Parallel()
			want := test1step.DefaultFiles()
			a := build(t, test1step.Options{Compress: true, Disks: disks})
			noDiagnostics(t, a)

			if got := len(a.Disks()); got != disks {
				t.Fatalf("got %d disks, want %d", got, disks)
			}
			if got := a.Job().Disks; got != disks {
				t.Errorf("Job().Disks = %d, want %d", got, disks)
			}
			// The split lands inside a chunk, so at least one file's data has
			// to be read across a disk boundary for this to pass.
			for i := range a.Files() {
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
					t.Errorf("%s: contents differ across the disk boundary", f.Path)
				}
			}
		})
	}
}

// TestDisksOutOfOrder checks that the set is assembled from the headers rather
// than from the order the paths were given in.
func TestDisksOutOfOrder(t *testing.T) {
	t.Parallel()
	paths, err := test1step.WriteSet(t.TempDir(), test1step.Options{Compress: true, Disks: 3})
	if err != nil {
		t.Fatalf("WriteSet: %s", err)
	}
	paths[0], paths[2] = paths[2], paths[0]
	a, err := onestep.Open(paths...)
	if err != nil {
		t.Fatalf("Open: %s", err)
	}
	defer a.Close() //nolint:errcheck // read-only
	noDiagnostics(t, a)
	for i, d := range a.Disks() {
		if d.Header.Disk != i+1 {
			t.Errorf("disk at index %d is numbered %d", i, d.Header.Disk)
		}
	}
}

// TestMissingDisk checks that a set opened without one of its disks still
// lists everything and reports which files cannot be read.
func TestMissingDisk(t *testing.T) {
	t.Parallel()
	paths, err := test1step.WriteSet(t.TempDir(), test1step.Options{Compress: true, Disks: 3})
	if err != nil {
		t.Fatalf("WriteSet: %s", err)
	}
	// Drop the middle disk; the last one carries the catalog.
	a, err := onestep.Open(paths[0], paths[2])
	if err != nil {
		t.Fatalf("Open: %s", err)
	}
	defer a.Close() //nolint:errcheck // read-only

	var missing, unplaced bool
	for _, d := range a.Diagnostics() {
		switch d.Kind {
		case onestep.DiagDiskMissing:
			missing = true
		case onestep.DiagChunkUnplaced:
			unplaced = true
		}
	}
	if !missing {
		t.Error("no diagnostic reported the missing disk")
	}
	if !unplaced {
		t.Error("no diagnostic reported the data that is now unreachable")
	}
	if len(a.Files()) != len(test1step.DefaultFiles()) {
		t.Errorf("got %d files, want the whole catalog", len(a.Files()))
	}

	var recoverable, lost int
	for i := range a.Files() {
		if a.Files()[i].Recoverable() {
			recoverable++
		} else {
			lost++
		}
	}
	if lost == 0 {
		t.Error("every file reads as recoverable with a disk missing")
	}
	if recoverable == 0 {
		t.Error("no file is recoverable, but two of three disks are present")
	}
}

func TestNoCatalog(t *testing.T) {
	t.Parallel()
	paths, err := test1step.WriteSet(t.TempDir(), test1step.Options{Disks: 2})
	if err != nil {
		t.Fatalf("WriteSet: %s", err)
	}
	// The first disk of a set carries no catalog.
	if _, err := onestep.Open(paths[0]); err == nil {
		t.Fatal("Open succeeded without the disk holding the catalog")
	}
}

func TestNot1Step(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "bogus.1-Step")
	if err := os.WriteFile(path, bytes.Repeat([]byte{0xEE}, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := onestep.Open(path)
	if !errors.Is(err, onestep.ErrNot1Step) {
		t.Errorf("Open of a non-backup returned %v, want ErrNot1Step", err)
	}
}

func TestCompressed1Step(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "backup.1-Step.gz")
	data := append([]byte{0x1f, 0x8b, 0x08, 0x00}, make([]byte, 4092)...)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := onestep.Open(path)
	if !errors.Is(err, onestep.ErrNot1Step) {
		t.Fatalf("Open of a compressed backup returned %v, want ErrNot1Step", err)
	}
	if !strings.Contains(err.Error(), "gzip-compressed") {
		t.Errorf("error %q does not say the file is gzip-compressed", err)
	}
}
