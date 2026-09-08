package ddrescue

import (
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// writeImage creates an image whose byte at offset i is byte(i) and a map file
// with the given block lines, then opens the pair.
func openTestReader(t *testing.T, size int, mapBody string) *Reader {
	t.Helper()
	dir := t.TempDir()
	img := filepath.Join(dir, "disk.img")
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i)
	}
	if err := os.WriteFile(img, data, 0o600); err != nil {
		t.Fatal(err)
	}
	mapFile := filepath.Join(dir, "disk.map")
	body := "# Mapfile created by GNU ddrescue\n" +
		"#      pos       size  status\n" +
		"0x00000000     ?               1\n" + mapBody
	if err := os.WriteFile(mapFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := Open(img, mapFile)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

// three 0x100-byte blocks: good, bad, good.
const goodBadGood = "" +
	"0x00000000  0x00000100  +\n" +
	"0x00000100  0x00000100  -\n" +
	"0x00000200  0x00000100  +\n"

func TestReadOverlap(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		mapBody string
		offset  int64
		length  int
		wantN   int
		wantErr error
	}{
		{"entirely inside a good block", goodBadGood, 0x00, 0x40, 0x40, nil},
		{"entirely inside a bad block", goodBadGood, 0x110, 0x40, 0, ErrNotRecovered},
		{"good extending into bad", goodBadGood, 0xC0, 0x80, 0x40, nil},
		{"bad extending into good", goodBadGood, 0x180, 0x100, 0, ErrNotRecovered},
		{"spanning three blocks, middle bad", goodBadGood, 0x00, 0x300, 0x100, nil},
		{"spanning three good blocks", "" +
			"0x00000000  0x00000100  +\n" +
			"0x00000100  0x00000100  +\n" +
			"0x00000200  0x00000100  +\n", 0x80, 0x200, 0x200, nil},
		{"exactly up to the bad block", goodBadGood, 0x00, 0x100, 0x100, nil},
		{"starting at the bad block", goodBadGood, 0x100, 0x10, 0, ErrNotRecovered},
		{"past the end of the map", goodBadGood, 0x300, 0x10, 0, io.EOF},
		{"gap in the map", "" +
			"0x00000000  0x00000100  +\n" +
			"0x00000200  0x00000100  +\n", 0x80, 0x200, 0x80, nil},
		{"starting in a gap", "" +
			"0x00000000  0x00000100  +\n" +
			"0x00000200  0x00000100  +\n", 0x180, 0x40, 0, ErrNotRecovered},
		{"non-tried block", "0x00000000  0x00000100  ?\n", 0x00, 0x40, 0, ErrNotRecovered},
		{"non-trimmed block", "0x00000000  0x00000100  *\n", 0x00, 0x40, 0, ErrNotRecovered},
		{"non-scraped block", "0x00000000  0x00000100  /\n", 0x00, 0x40, 0, ErrNotRecovered},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := openTestReader(t, 0x300, tc.mapBody)
			if _, err := r.Seek(tc.offset, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			buf := make([]byte, tc.length)
			n, err := r.Read(buf)
			if n != tc.wantN {
				t.Errorf("n = %d; want %d", n, tc.wantN)
			}
			if tc.wantErr == nil {
				if err != nil {
					t.Errorf("err = %v; want nil", err)
				}
			} else if !errors.Is(err, tc.wantErr) {
				t.Errorf("err = %v; want %v", err, tc.wantErr)
			}
			// Whatever was returned must be the real image bytes.
			for i := range n {
				//nolint:gosec // deliberate truncation: the image byte at offset n is byte(n)
				if want := byte(tc.offset + int64(i)); buf[i] != want {
					t.Fatalf("byte %d = 0x%02X; want 0x%02X", i, buf[i], want)
					break
				}
			}
		})
	}
}

// A read stopped at a bad block must fail when resumed at that block, never
// skip over it.
func TestReadResumesIntoFailure(t *testing.T) {
	t.Parallel()
	r := openTestReader(t, 0x300, goodBadGood)
	buf := make([]byte, 0x300)
	n, err := r.Read(buf)
	if n != 0x100 || err != nil {
		t.Fatalf("first read = (%d, %v); want (256, nil)", n, err)
	}
	n, err = r.Read(buf)
	if n != 0 || !errors.Is(err, ErrNotRecovered) {
		t.Fatalf("second read = (%d, %v); want (0, ErrNotRecovered)", n, err)
	}
}

func TestParseBlocks(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		raw     string
		want    []Block
		wantErr bool
	}{
		{
			name: "comments, status line and blocks",
			raw: "# Mapfile created by GNU ddrescue\n" +
				"# Command line: ddrescue /dev/sr0 out.iso out.map\n" +
				"#      pos        size  status\n" +
				"0x00000000     ?               1\n" +
				"0x00000000  0x00001000  +\n" +
				"0x00001000  0x00000200  -\n",
			want: []Block{
				{Offset: 0, Size: 0x1000, Status: BlockStatusFinished},
				{Offset: 0x1000, Size: 0x200, Status: BlockStatusFailedBlockBadSectors},
			},
		},
		{
			name: "indented lines",
			raw: "0x00000000     ?               1\n" +
				"   0x00000000  0x00001000  +\n" +
				"\t0x00001000  0x00000200  ?\n",
			want: []Block{
				{Offset: 0, Size: 0x1000, Status: BlockStatusFinished},
				{Offset: 0x1000, Size: 0x200, Status: BlockStatusNonTried},
			},
		},
		{
			name: "CRLF line endings",
			raw:  "0x00000000     ?               1\r\n0x00000000  0x00001000  +\r\n",
			want: []Block{{Offset: 0, Size: 0x1000, Status: BlockStatusFinished}},
		},
		{
			name: "out of order blocks are sorted",
			raw: "0x00000000     ?               1\n" +
				"0x00002000  0x00001000  +\n" +
				"0x00000000  0x00001000  -\n",
			want: []Block{
				{Offset: 0, Size: 0x1000, Status: BlockStatusFailedBlockBadSectors},
				{Offset: 0x2000, Size: 0x1000, Status: BlockStatusFinished},
			},
		},
		{
			name:    "unknown status character",
			raw:     "0x00000000     ?               1\n0x00000000  0x00001000  x\n",
			wantErr: true,
		},
		{
			name:    "multi-character status field",
			raw:     "0x00000000     ?               1\n0x00000000  0x00001000  ++\n",
			wantErr: true,
		},
		{
			name:    "unparseable offset",
			raw:     "0x00000000     ?               1\n0xZZ  0x00001000  +\n",
			wantErr: true,
		},
		{
			// The status line is recognized by its shape, so a map that
			// carries none keeps its first block instead of losing it.
			name: "no status line",
			raw: "#      pos        size  status\n" +
				"0x00000000  0x00001000  +\n" +
				"0x00001000  0x00000200  -\n",
			want: []Block{
				{Offset: 0, Size: 0x1000, Status: BlockStatusFinished},
				{Offset: 0x1000, Size: 0x200, Status: BlockStatusFailedBlockBadSectors},
			},
		},
		{
			// GNU ddrescue 1.2x, whose status line carries no pass number when
			// the run has not started one.
			name: "status line with no pass",
			raw: "# Mapfile. Created by GNU ddrescue version 1.28\n" +
				"# Command line: ddrescue -d /dev/sr0 out.iso out.map\n" +
				"# Start time:   2026-01-02 03:04:05\n" +
				"# Current time: 2026-01-02 03:05:06\n" +
				"# Finished\n" +
				"# current_pos  current_status  current_pass\n" +
				"0x00048000     +\n" +
				"#      pos        size  status\n" +
				"0x00000000  0x00048000  +\n",
			want: []Block{{Offset: 0, Size: 0x48000, Status: BlockStatusFinished}},
		},
		{
			// Nothing here resolves an overlap, and walking coverage forward
			// over one lets a finished block admit a failed block's bytes as
			// data, which this package guarantees never happens.
			name: "overlapping blocks",
			raw: "0x00000000     ?               1\n" +
				"0x00000000  0x00001000  +\n" +
				"0x00000800  0x00001000  -\n",
			wantErr: true,
		},
		{
			name:    "a line cut short mid-write",
			raw:     "0x00000000     ?               1\n0x00000000  0x00001000  +\n0x00001000  0x000\n",
			wantErr: true,
		},
		{
			// Offset+Size wraps int64 here. Should the parser accept it, Reader.end
			// goes negative and every read reports EOF.
			name: "offset and size sum past the addressable range",
			raw: "0x00000000     ?               1\n" +
				"0x7ffffffffffffffe  0x0000000000000005  +\n",
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseBlocks(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseBlocks() = %v, nil; want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseBlocks() = %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %d blocks, want %d: %v", len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("block %d = %+v; want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// A range whose end wraps int64 must not read as "nothing lost". Without a
// guard the end goes negative, the walk over the blocks stops immediately, and
// the whole range is reported as recovered.
func TestUnrecoveredRangeOverflow(t *testing.T) {
	t.Parallel()
	blocks := []Block{{Offset: 0, Size: 1 << 20, Status: BlockStatusFinished}}
	got := Unrecovered(blocks, math.MaxInt64-64, 128)
	want := []Block{{Offset: math.MaxInt64 - 64, Size: 64, Status: BlockStatusNonTried}}
	if len(got) != 1 || got[0] != want[0] {
		t.Errorf("Unrecovered = %+v; want %+v", got, want)
	}
	if n := UnrecoveredBytes(blocks, math.MaxInt64-64, 128); n != 64 {
		t.Errorf("UnrecoveredBytes = %d; want 64", n)
	}
}

func TestBlockStatusValid(t *testing.T) {
	t.Parallel()
	for _, s := range []BlockStatus{'?', '*', '/', '-', '+'} {
		if !s.Valid() {
			t.Errorf("BlockStatus(%q).Valid() = false; want true", string(s))
		}
	}
	for _, s := range []BlockStatus{0, 'x', ' ', '#'} {
		if s.Valid() {
			t.Errorf("BlockStatus(%q).Valid() = true; want false", string(s))
		}
	}
}

func TestEmptyMap(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	img := filepath.Join(dir, "disk.img")
	mapFile := filepath.Join(dir, "disk.map")
	if err := os.WriteFile(img, make([]byte, 16), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mapFile, []byte("# no blocks\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := Open(img, mapFile)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close() //nolint:errcheck // test cleanup
	if _, err := r.Read(make([]byte, 4)); err == nil || !strings.Contains(err.Error(), "no blocks") {
		t.Errorf("Read on an empty map = %v; want a no-blocks error", err)
	}
}

func TestMapBuilder(t *testing.T) {
	t.Parallel()
	b := NewMapBuilder(2048)
	for lba := range int64(4) {
		b.Record(lba, false)
	}
	for lba := int64(4); lba < 6; lba++ {
		b.Record(lba, true)
	}
	b.Record(6, false)

	want := []Block{
		{Offset: 0, Size: 4 * 2048, Status: BlockStatusFinished},
		{Offset: 4 * 2048, Size: 2 * 2048, Status: BlockStatusFailedBlockBadSectors},
		{Offset: 6 * 2048, Size: 2048, Status: BlockStatusFinished},
	}
	got := b.Blocks()
	if len(got) != len(want) {
		t.Fatalf("got %d blocks, want %d: %+v", len(got), len(want), got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("block %d = %+v; want %+v", i, got[i], want[i])
		}
	}

	// Blocks() must not write the open run into the backing array, or the next
	// Record would overwrite the block a previous caller is holding.
	held := b.Blocks()
	b.Record(7, true)
	b.Record(8, false)
	if held[len(held)-1] != want[len(want)-1] {
		t.Errorf("earlier Blocks() result was clobbered: %+v; want %+v", held[len(held)-1], want[len(want)-1])
	}

	// The output must round-trip through the parser.
	var sb strings.Builder
	if _, err := b.WriteTo(&sb); err != nil {
		t.Fatal(err)
	}
	parsed, err := parseBlocks(sb.String())
	if err != nil {
		t.Fatalf("parseBlocks on generated map: %v", err)
	}
	for i, blk := range b.Blocks() {
		if i >= len(parsed) {
			t.Fatalf("generated map lost block %d", i)
		}
		if parsed[i] != blk {
			t.Errorf("round-trip block %d = %+v; want %+v", i, parsed[i], blk)
		}
	}
}

// The status line is "current_pos current_status [current_pass]": one status
// character in the second field, where a block line carries a size. GNU
// ddrescue rejects an entire mapfile whose status line has another shape, so a
// sidecar with a number there is unreadable by other tools.
func TestMapHeaderStatusLine(t *testing.T) {
	t.Parallel()
	var sb strings.Builder
	b := NewMapBuilder(512)
	b.Record(0, false)
	if _, err := b.WriteTo(&sb); err != nil {
		t.Fatal(err)
	}
	var line string
	for l := range strings.SplitSeq(sb.String(), "\n") {
		if l = strings.TrimSpace(l); l != "" && l[0] != '#' {
			line = l
			break
		}
	}
	p := reWS.Split(line, -1)
	if len(p) != 3 {
		t.Fatalf("status line %q has %d fields; want position, status and pass", line, len(p))
	}
	if _, err := strconv.ParseUint(p[0], 0, 64); err != nil {
		t.Errorf("status line position %q does not parse: %v", p[0], err)
	}
	// The current-status characters from the ddrescue manual, which are not the
	// block statuses: 'F' and 'G' are only ever a run's state.
	if len(p[1]) != 1 || !strings.ContainsAny(p[1], "?*/-+FG") {
		t.Errorf("status line status %q is not one ddrescue writes", p[1])
	}
	if _, err := strconv.ParseUint(p[2], 10, 64); err != nil {
		t.Errorf("status line pass %q does not parse: %v", p[2], err)
	}
	if !isStatusLine(p) {
		t.Error("our own parser does not recognize the status line we write")
	}
}
