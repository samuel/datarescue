package ddrescue

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestUnrecovered(t *testing.T) {
	t.Parallel()
	// good, bad, good, then nothing at all past 0x300.
	blocks := []Block{
		{Offset: 0x000, Size: 0x100, Status: BlockStatusFinished},
		{Offset: 0x100, Size: 0x100, Status: BlockStatusFailedBlockBadSectors},
		{Offset: 0x200, Size: 0x100, Status: BlockStatusFinished},
	}
	tests := []struct {
		name string
		off  int64
		size int64
		want []Block
	}{
		{"wholly inside a finished block", 0x10, 0x20, nil},
		{"clipped to the range asked about", 0x180, 0x40,
			[]Block{{Offset: 0x180, Size: 0x40, Status: BlockStatusFailedBlockBadSectors}}},
		{"spanning the failure", 0x080, 0x200,
			[]Block{{Offset: 0x100, Size: 0x100, Status: BlockStatusFailedBlockBadSectors}}},
		{"past the last block is non-tried", 0x280, 0x100,
			[]Block{{Offset: 0x300, Size: 0x80, Status: BlockStatusNonTried}}},
		{"an empty range asks nothing", 0x100, 0, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Unrecovered(blocks, tc.off, tc.size)
			if len(got) != len(tc.want) {
				t.Fatalf("Unrecovered(%#x, %#x) = %v; want %v", tc.off, tc.size, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("block %d = %+v; want %+v", i, got[i], tc.want[i])
				}
			}
			var total int64
			for _, b := range tc.want {
				total += b.Size
			}
			if n := UnrecoveredBytes(blocks, tc.off, tc.size); n != total {
				t.Errorf("UnrecoveredBytes = %d; want %d", n, total)
			}
		})
	}
}

// A hole in the middle of the map is unrecovered too: a range the map does not
// cover is not data.
func TestUnrecoveredMapHole(t *testing.T) {
	t.Parallel()
	blocks := []Block{
		{Offset: 0x000, Size: 0x100, Status: BlockStatusFinished},
		{Offset: 0x200, Size: 0x100, Status: BlockStatusFinished},
	}
	got := Unrecovered(blocks, 0, 0x300)
	want := Block{Offset: 0x100, Size: 0x100, Status: BlockStatusNonTried}
	if len(got) != 1 || got[0] != want {
		t.Errorf("Unrecovered over a map hole = %v; want [%+v]", got, want)
	}
}

// ReadAt must not move the sequential offset, since a partition table read and
// a filesystem parser work off the same image.
func TestReadAtKeepsOffset(t *testing.T) {
	t.Parallel()
	r := openTestReader(t, 0x300, goodBadGood)
	if _, err := r.Seek(0x10, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 0x10)
	if n, err := r.ReadAt(buf, 0x200); err != nil || n != len(buf) {
		t.Fatalf("ReadAt = (%d, %v); want (%d, nil)", n, err, len(buf))
	}
	if buf[0] != 0x00 { // the image byte at 0x200 is byte(0x200)
		t.Errorf("ReadAt read the wrong bytes: %x", buf[:4])
	}
	if r.Offset != 0x10 {
		t.Errorf("Offset after ReadAt = %#x; want 0x10", r.Offset)
	}
	n, err := r.Read(buf)
	if err != nil || n != len(buf) {
		t.Fatalf("Read after ReadAt = (%d, %v)", n, err)
	}
	if buf[0] != 0x10 {
		t.Errorf("Read after ReadAt resumed at the wrong offset: %x", buf[:4])
	}
}

// A ReadAt into an unrecovered range must fail rather than hand back whatever
// the image file holds there.
func TestReadAtUnrecovered(t *testing.T) {
	t.Parallel()
	r := openTestReader(t, 0x300, goodBadGood)
	buf := make([]byte, 0x10)
	if _, err := r.ReadAt(buf, 0x110); !errors.Is(err, ErrNotRecovered) {
		t.Errorf("ReadAt in a failed block = %v; want ErrNotRecovered", err)
	}
	// A read straddling the boundary keeps the finished prefix and reports what
	// stopped it, rather than the bare short-read error.
	buf = make([]byte, 0x20)
	n, err := r.ReadAt(buf, 0xF0)
	if n != 0x10 || !errors.Is(err, ErrNotRecovered) {
		t.Errorf("ReadAt across the boundary = (%d, %v); want (16, ErrNotRecovered)", n, err)
	}
}

func TestMapBuilderRecordRange(t *testing.T) {
	t.Parallel()
	b := NewMapBuilder(512)
	b.RecordRange(0, 1000, false)
	b.RecordRange(1000, 24, false) // adjacent and the same status: one block
	b.RecordRange(1024, 512, true)
	b.RecordRange(1536, 13, false)
	want := []Block{
		{Offset: 0, Size: 1024, Status: BlockStatusFinished},
		{Offset: 1024, Size: 512, Status: BlockStatusFailedBlockBadSectors},
		{Offset: 1536, Size: 13, Status: BlockStatusFinished},
	}
	got := b.Blocks()
	if len(got) != len(want) {
		t.Fatalf("Blocks() = %v; want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("block %d = %+v; want %+v", i, got[i], want[i])
		}
	}
}

func TestFindMap(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(name string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	image := write("input.img")

	if got := FindMap(image); got != "" {
		t.Errorf("FindMap with no map beside the image = %q; want none", got)
	}
	// The map named after the medium rather than the image file.
	stripped := write("input.map")
	if got := FindMap(image); got != stripped {
		t.Errorf("FindMap = %q; want %q", got, stripped)
	}
	// The one that includes the image's file name wins when both exist.
	full := write("input.img.map")
	if got := FindMap(image); got != full {
		t.Errorf("FindMap with both = %q; want %q", got, full)
	}
	// A name with no extension has only one candidate, and must not be returned
	// as its own map.
	if got := FindMap(filepath.Join(dir, "input")); got != stripped {
		t.Errorf("FindMap of an extensionless name = %q; want %q", got, stripped)
	}
	// A directory is not a map.
	if err := os.Mkdir(filepath.Join(dir, "other.img.map"), 0o750); err != nil {
		t.Fatal(err)
	}
	if got := FindMap(filepath.Join(dir, "other.img")); got != "" {
		t.Errorf("FindMap picked up a directory: %q", got)
	}
}
