package onestep_test

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"

	"samuelks.com/datarescue/internal/test1step"
	"samuelks.com/datarescue/onestep"
)

// TestChunkOffsetsAreRelativeToTheirDisk pins down the layout the format
// actually uses: a chunk's recorded offset is a position in its own disk's
// file, so the offsets restart at the end of every disk's header rather than
// running on through the set.
func TestChunkOffsetsAreRelativeToTheirDisk(t *testing.T) {
	t.Parallel()
	for _, disks := range []int{2, 3, 4} {
		t.Run(diskLabel(disks), func(t *testing.T) {
			t.Parallel()
			a := build(t, test1step.Options{Compress: true, Disks: disks})
			noDiagnostics(t, a)

			// Each disk's chunks tile it from the end of its header.
			ends := map[int]int64{}
			for _, f := range a.Files() {
				for _, c := range f.Chunks {
					if c.Disk < 1 || c.Disk > disks {
						t.Fatalf("%s: chunk %d is on disk %d, out of a set of %d", f.Path, c.Serial, c.Disk, disks)
					}
					if _, seen := ends[c.Disk]; !seen {
						ends[c.Disk] = onestep.HeaderSize
					}
				}
			}
			if len(ends) != disks {
				t.Fatalf("chunks landed on %d disks, want %d", len(ends), disks)
			}
			for _, d := range a.Disks() {
				if d.FirstChunk == 0 {
					t.Errorf("disk %d holds no chunks", d.Header.Disk)
				}
				if d.ExpectedDataLength != d.DataLength {
					t.Errorf("disk %d holds %d bytes of data, the catalog puts %d here",
						d.Header.Disk, d.DataLength, d.ExpectedDataLength)
				}
			}
		})
	}
}

// TestFileSpansDiskBoundary is the case a single-disk backup cannot show: a
// file whose chunks are split across two disks has to be reassembled from
// both, and each disk's chunks are addressed from its own header.
func TestFileSpansDiskBoundary(t *testing.T) {
	t.Parallel()
	for _, disks := range []int{2, 3} {
		t.Run(diskLabel(disks), func(t *testing.T) {
			t.Parallel()
			want := test1step.DefaultFiles()
			a := build(t, test1step.Options{Compress: true, Disks: disks})
			noDiagnostics(t, a)

			var spanning int
			for i := range a.Files() {
				f := &a.Files()[i]
				first := f.Chunks[0].Disk
				for _, c := range f.Chunks {
					if c.Disk != first {
						spanning++
						break
					}
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
					t.Errorf("%s: contents differ", f.Path)
				}
			}
			if spanning == 0 {
				t.Error("no file crosses a disk boundary, so the case is untested")
			}
		})
	}
}

// TestPartiallyRecoveredDisk covers a middle disk that was only partly read:
// the shortfall is measured against what the catalog puts there, the files
// stored in the part that survived still come back, and the ones past it are
// reported rather than filled with whatever lies at their offset.
func TestPartiallyRecoveredDisk(t *testing.T) {
	t.Parallel()
	want := test1step.DefaultFiles()
	const cut = 4096
	// Stored rather than deflated, so the chunk sizes are the data sizes and
	// the cut is a known fraction of a disk rather than of its compression.
	paths := writeDisks(t, test1step.Options{
		Disks:    3,
		Truncate: map[int]int64{2: cut},
	}, nil)
	a := openPaths(t, paths...)

	var truncated bool
	for _, d := range a.Diagnostics() {
		if d.Kind == onestep.DiagDiskTruncated {
			truncated = true
		}
	}
	if !truncated {
		t.Error("the short image was not reported as truncated")
	}
	for _, d := range a.Disks() {
		if d.Header.Disk != 2 {
			continue
		}
		if got := d.ExpectedDataLength - d.DataLength; got != cut {
			t.Errorf("disk 2 is short by %d bytes, want %d", got, cut)
		}
	}

	// A file entirely inside the surviving part of disk 2, or on another disk,
	// must still come back byte for byte.
	var recovered, lost int
	for i := range a.Files() {
		f := &a.Files()[i]
		got, _, err := readAll(t, a, f)
		if err != nil {
			lost++
			continue
		}
		if !bytes.Equal(got, want[i].Data) {
			t.Errorf("%s: read without error but the contents differ", f.Path)
			continue
		}
		recovered++
	}
	if lost == 0 {
		t.Errorf("every file read back from a set missing %d bytes", cut)
	}
	if recovered == 0 {
		t.Error("no file survived, but only the tail of one disk is gone")
	}
}

// TestPlacementWithoutSessionTable checks the fallback: with the session table
// gone there is nothing left mapping a chunk to a disk but the offsets
// themselves, which restart on each one.
func TestPlacementWithoutSessionTable(t *testing.T) {
	t.Parallel()
	want := test1step.DefaultFiles()
	paths, err := test1step.WriteSet(t.TempDir(), test1step.Options{Compress: true, Disks: 3})
	if err != nil {
		t.Fatalf("WriteSet: %s", err)
	}
	blankMember(t, paths, "IOSESS.DBF")

	a := openPaths(t, paths...)
	if len(a.Sessions()) != 0 {
		t.Fatalf("the session table survived with %d rows; the test damaged the wrong bytes", len(a.Sessions()))
	}
	for i := range a.Files() {
		f := &a.Files()[i]
		got, _, err := readAll(t, a, f)
		if err != nil {
			t.Errorf("%s: %s", f.Path, err)
			continue
		}
		if !bytes.Equal(got, want[i].Data) {
			t.Errorf("%s: contents differ when placed from the offsets alone", f.Path)
		}
	}
}

// blankMember zeroes a catalog member in place, which is where the member
// directory says it is rather than wherever a byte pattern happens to match.
func blankMember(t *testing.T, paths []string, name string) {
	t.Helper()
	a, err := onestep.Open(paths...)
	if err != nil {
		t.Fatalf("Open: %s", err)
	}
	var m onestep.Member
	for _, got := range a.Members() {
		if strings.EqualFold(got.Name, name) {
			m = got
		}
	}
	// The catalog is on the last disk of the set.
	last := paths[len(paths)-1]
	a.Close() //nolint:errcheck // read-only
	if m.Size == 0 {
		t.Fatalf("%s is not a member of the catalog", name)
	}

	b, err := os.ReadFile(last)
	if err != nil {
		t.Fatal(err)
	}
	clear(b[m.Offset : m.Offset+m.Size])
	if err := os.WriteFile(last, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestEmptyFile checks the zero-length case, which the format records as a
// chunk at its own compression level holding nothing. It has no bytes on any
// disk, so it comes back even when the disk it was written to is missing.
func TestEmptyFile(t *testing.T) {
	t.Parallel()
	a := build(t, test1step.Options{Compress: true})
	noDiagnostics(t, a)

	f := fileNamed(t, a, "EMPTY.DAT")
	if f.Size != 0 {
		t.Fatalf("EMPTY.DAT has size %d, want 0", f.Size)
	}
	if len(f.Chunks) != 1 || !f.Chunks[0].Empty() {
		t.Fatalf("EMPTY.DAT is stored as %d chunks, want one empty one", len(f.Chunks))
	}
	if f.Chunks[0].HasChecksum() {
		t.Error("an empty chunk records a checksum")
	}
	if !f.Recoverable() {
		t.Error("an empty file reads as unrecoverable")
	}
	got, issues, err := readAll(t, a, f)
	if err != nil {
		t.Fatalf("read: %s", err)
	}
	for _, e := range issues {
		t.Errorf("empty file reported %s", e)
	}
	if len(got) != 0 {
		t.Errorf("read %d bytes from an empty file", len(got))
	}
}

// TestBackupWithoutChecksums covers a writer that recorded none: every chunk
// carries 0, which means absent rather than a value to check the data against.
func TestBackupWithoutChecksums(t *testing.T) {
	t.Parallel()
	want := test1step.DefaultFiles()
	a := build(t, test1step.Options{Compress: true, Disks: 2, OmitChecksums: true})
	noDiagnostics(t, a)

	for i := range a.Files() {
		f := &a.Files()[i]
		for _, c := range f.Chunks {
			if c.HasChecksum() {
				t.Errorf("%s: chunk %d reports a checksum", f.Path, c.Serial)
			}
		}
		if c := a.Check(f); !c.OK() {
			t.Errorf("%s: %v", f.Path, c.Errors)
		}
		got, _, err := readAll(t, a, f)
		if err != nil {
			t.Errorf("%s: %s", f.Path, err)
			continue
		}
		if !bytes.Equal(got, want[i].Data) {
			t.Errorf("%s: contents differ", f.Path)
		}
	}
}

func diskLabel(n int) string {
	return fmt.Sprintf("disks=%d", n)
}
