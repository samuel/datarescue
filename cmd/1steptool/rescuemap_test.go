package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"samuelks.com/datarescue/ddrescue"
	"samuelks.com/datarescue/internal/subcmd"
	"samuelks.com/datarescue/internal/test1step"
)

// withheldSet writes a three-disk backup and returns the paths of disks 1 and
// 3. Disk 2 is left out, so every chunk stored on it is a hole of its own
// recorded length. The catalog is written at the end of the last disk, so the
// withheld one has to be a middle one for there to be an archive at all.
func withheldSet(t *testing.T) []string {
	t.Helper()
	dir := t.TempDir()
	disks := test1step.Build(test1step.Options{Disks: 3})
	var paths []string
	for i, b := range disks {
		p := filepath.Join(dir, fmt.Sprintf("disk%d.1-Step", i+1))
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		if i != 1 {
			paths = append(paths, p)
		}
	}
	return paths
}

// gapMaps returns the maps written beside the recovered files, by the name of
// the file each belongs to.
func gapMaps(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".map") {
			return err
		}
		out[strings.TrimSuffix(path, ".map")] = path
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// The whole point: a file whose data was on a disk that was never supplied is
// restored at its full length, with zeros where the chunks are missing and a
// map beside it saying which of its byte ranges those are. The chunk sizes come
// from the catalog, so the holes are measured rather than guessed and the
// chunks after them still land where the file says.
func TestExtractWritesGapMap(t *testing.T) {
	paths := withheldSet(t)
	dir := t.TempDir()
	args := append([]string{"-o", dir}, paths...)
	// The files come back whole with their maps beside them and the backup did
	// not, which is what ExitDegraded says.
	if code := runExtract(args); code != subcmd.ExitDegraded {
		t.Fatalf("runExtract = %d; want %d for a backup missing a disk", code, subcmd.ExitDegraded)
	}

	maps := gapMaps(t, dir)
	if len(maps) == 0 {
		t.Fatal("no file was given a map, so the withheld disk cost nothing")
	}

	want := map[string][]byte{}
	for _, f := range test1step.DefaultFiles() {
		want[f.Name] = f.Data
	}
	for data, mapPath := range maps {
		got, err := os.ReadFile(data)
		if err != nil {
			t.Fatal(err)
		}
		orig, ok := want[filepath.Base(data)]
		if !ok {
			t.Errorf("a map was written beside %s, which is not one of the backup's files", data)
			continue
		}
		if len(got) != len(orig) {
			t.Errorf("%s: %d bytes; want the catalog's %d — a missing chunk is a measured hole, not a short file",
				filepath.Base(data), len(got), len(orig))
			continue
		}
		blocks, err := ddrescue.ParseMap(mapPath)
		if err != nil {
			t.Fatalf("%s: %v", mapPath, err)
		}
		bad := ddrescue.Unrecovered(blocks, 0, int64(len(got)))
		if len(bad) == 0 {
			t.Errorf("%s: a map was written recording no damage", mapPath)
		}
		merged := append([]byte(nil), orig...)
		for _, b := range bad {
			clear(merged[b.Offset : b.Offset+b.Size])
		}
		if !bytes.Equal(got, merged) {
			t.Errorf("%s: the restored bytes are not the file's with zeros only where the map says", filepath.Base(data))
		}
	}
}

// A map that cannot be written is a failure and not a warning: unrecorded
// zeros are worse than a short file.
func TestExtractMapWriteFailure(t *testing.T) {
	paths := withheldSet(t)

	// Find a file that needs a map, then block its map on a second run.
	probe := t.TempDir()
	if code := runExtract(append([]string{"-o", probe}, paths...)); code != subcmd.ExitDegraded {
		t.Fatalf("probe run = %d", code)
	}
	var blocked string
	for data := range gapMaps(t, probe) {
		blocked, _ = filepath.Rel(probe, data)
		break
	}
	if blocked == "" {
		t.Fatal("no file needed a map, so the test proves nothing")
	}

	dir := t.TempDir()
	target := filepath.Join(dir, blocked+".map")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("in the way\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := runExtract(append([]string{"-o", dir}, paths...)); code != subcmd.ExitFailure {
		t.Errorf("extract that could not write its map = %d; want %d", code, subcmd.ExitFailure)
	}
	if code := runExtract(append([]string{"-f", "-o", dir}, paths...)); code != subcmd.ExitDegraded {
		t.Errorf("extract with -f = %d; want %d, the damage still being there", code, subcmd.ExitDegraded)
	}
}

// The dbf subcommand is the other half of the loop: fattool extract writes
// IOA.DBF and IOA.DBF.map out of a damaged image, and this reads the table
// through that sidecar rather than decoding the zeros as records.
func TestDBFReadsThroughItsMap(t *testing.T) {
	dir := t.TempDir()
	table := filepath.Join(dir, "IOA.DBF")
	// A table whose records all sit in one region, with that region marked
	// unrecovered: every record over it must fail rather than decode.
	data := buildTinyDBF()
	if err := os.WriteFile(table, data, 0o600); err != nil {
		t.Fatal(err)
	}
	// The header and its one field descriptor were recovered; the records were
	// not. That is the case the map is for — the table opens, and every record
	// over the lost range is reported rather than decoded out of zeros.
	const headerLen = 32 + 32 + 1
	var b strings.Builder
	b.WriteString("# Mapfile. Created by a test.\n0x00000000     +               1\n#      Start        Size  Status\n")
	fmt.Fprintf(&b, "0x%08x  0x%08x  +\n", 0, headerLen)
	fmt.Fprintf(&b, "0x%08x  0x%08x  -\n", headerLen, len(data)-headerLen)
	if err := os.WriteFile(table+".map", []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	if code := runDBF([]string{table}); code != subcmd.ExitDegraded {
		t.Errorf("dbf over an unrecovered region = %d; want %d rather than decoding zeros as records", code, subcmd.ExitDegraded)
	}
	// Read without the map, the same bytes decode without complaint, so the
	// map is what made the difference.
	if code := runDBF([]string{"-no-map", table}); code != subcmd.ExitOK {
		t.Errorf("dbf -no-map = %d; want the table to read when the map is ignored", code)
	}
}

// An offset past the end of the file used to yield a negative size.
func TestDBFRejectsAnOffsetPastTheFile(t *testing.T) {
	dir := t.TempDir()
	table := filepath.Join(dir, "IOA.DBF")
	if err := os.WriteFile(table, buildTinyDBF(), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, off := range []string{"-1", "100000"} {
		if code := runDBF([]string{"-offset", off, table}); code != subcmd.ExitUsage {
			t.Errorf("dbf -offset %s = %d; want %d", off, code, subcmd.ExitUsage)
		}
	}
}

// buildTinyDBF renders a dBase III table of two one-field records.
func buildTinyDBF() []byte {
	const recordSize = 1 + 10 // deletion flag plus one 10-byte character field
	header := make([]byte, 32)
	header[0] = 0x03 // dBase III, no memo
	header[1], header[2], header[3] = 101, 1, 1
	header[4] = 2 // records
	header[8], header[9] = 32+32+1, 0
	header[10], header[11] = recordSize, 0

	field := make([]byte, 32)
	copy(field, "NAME")
	field[11] = 'C'
	field[16] = 10 // length

	b := make([]byte, 0, 32+32+1+2*recordSize+1)
	b = append(b, header...)
	b = append(b, field...)
	b = append(b, 0x0D) // field terminator
	for range 2 {
		rec := make([]byte, recordSize)
		rec[0] = ' '
		copy(rec[1:], "SAMPLE")
		b = append(b, rec...)
	}
	return append(b, 0x1A) // EOF marker
}
