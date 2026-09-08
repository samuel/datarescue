package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"samuelks.com/datarescue/ddrescue"
	"samuelks.com/datarescue/internal/subcmd"
	"samuelks.com/datarescue/internal/testqic"
)

// damagedArchive writes an archive one of whose data segments will not expand.
func damagedArchive(t *testing.T) string {
	t.Helper()
	path, err := testqic.Write(t.TempDir(), testqic.Options{Compress: true, BadSegment: 1})
	if err != nil {
		t.Fatal(err)
	}
	return path
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

// A file over a segment that will not expand is written at its full length,
// with zeros where the segment was and a map beside it listing those byte
// ranges. Each segment records the count of uncompressed bytes before it, so
// the hole's length is the distance to where the next segment resumes:
// measured, not guessed.
func TestExtractWritesGapMap(t *testing.T) {
	archive := damagedArchive(t)
	dir := t.TempDir()
	// The file came back whole with its map beside it and the archive did not,
	// which is what ExitDegraded says.
	if code := runExtract([]string{"-o", dir, archive}); code != subcmd.ExitDegraded {
		t.Fatalf("runExtract = %d; want %d for a damaged archive", code, subcmd.ExitDegraded)
	}

	maps := gapMaps(t, dir)
	if len(maps) == 0 {
		t.Fatal("no file was given a map, so the corrupt segment cost nothing")
	}

	want := map[string]int{}
	for _, f := range testqic.DefaultFiles() {
		if !f.IsDir {
			want[strings.ToUpper(f.Name)] = len(f.Data)
		}
	}
	for data, mapPath := range maps {
		got, err := os.ReadFile(data)
		if err != nil {
			t.Fatal(err)
		}
		size, ok := want[strings.ToUpper(filepath.Base(data))]
		if !ok {
			t.Errorf("a map was written beside %s, which is not one of the archive's files", data)
			continue
		}
		if len(got) != size {
			t.Errorf("%s: %d bytes; want the catalog's %d — a lost segment is a measured hole, not a short file",
				filepath.Base(data), len(got), size)
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
		for _, b := range bad {
			hole := got[b.Offset : b.Offset+b.Size]
			if !bytes.Equal(hole, make([]byte, len(hole))) {
				t.Errorf("%s: the range the map calls unrecovered is not zeros", filepath.Base(data))
			}
		}
	}
}

// A map that cannot be written is a failure and not a warning: unrecorded
// zeros are worse than a short file.
func TestExtractMapWriteFailure(t *testing.T) {
	archive := damagedArchive(t)

	probe := t.TempDir()
	if code := runExtract([]string{"-o", probe, archive}); code != subcmd.ExitDegraded {
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
	if code := runExtract([]string{"-o", dir, archive}); code != subcmd.ExitFailure {
		t.Errorf("extract that could not write its map = %d; want %d", code, subcmd.ExitFailure)
	}
	if code := runExtract([]string{"-f", "-o", dir, archive}); code != subcmd.ExitDegraded {
		t.Errorf("extract with -f = %d; want %d, the damage still being there", code, subcmd.ExitDegraded)
	}
}
