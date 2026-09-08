package iso

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"samuelks.com/datarescue/ddrescue"
)

// A rip records both the sectors it recovered and the ones it zero-filled, and
// the map it writes must say which is which.
func TestWriterMapRecordsBadSectors(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	imgPath := filepath.Join(dir, "out.iso")
	w, err := NewWriter(imgPath, false)
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, sectorSize)
	for lba := range int64(4) {
		for i := range data {
			data[i] = byte(lba)
		}
		if err := w.WriteSector(lba, data); err != nil {
			t.Fatal(err)
		}
	}
	for lba := int64(4); lba < 6; lba++ {
		if err := w.WriteZeroSector(lba); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.WriteSector(6, data); err != nil {
		t.Fatal(err)
	}

	mapPath := imgPath + ".map"
	if err := w.WriteMap(mapPath); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	blocks, err := ddrescue.ParseMap(mapPath)
	if err != nil {
		t.Fatalf("the map we wrote does not parse: %v", err)
	}
	want := []ddrescue.Block{
		{Offset: 0, Size: 4 * sectorSize, Status: ddrescue.BlockStatusFinished},
		{Offset: 4 * sectorSize, Size: 2 * sectorSize, Status: ddrescue.BlockStatusFailedBlockBadSectors},
		{Offset: 6 * sectorSize, Size: sectorSize, Status: ddrescue.BlockStatusFinished},
	}
	if len(blocks) != len(want) {
		t.Fatalf("got %d blocks, want %d: %+v", len(blocks), len(want), blocks)
	}
	for i := range blocks {
		if blocks[i] != want[i] {
			t.Errorf("block %d = %+v; want %+v", i, blocks[i], want[i])
		}
	}
}

// Re-ripping over a good previous rip of one-shot media would destroy the only
// copy, so the writer refuses without force.
func TestWriterRefusesExistingImage(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "out.iso")
	if err := os.WriteFile(path, []byte("a previous rip"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewWriter(path, false); err == nil {
		t.Fatal("NewWriter over an existing image = nil; want a refusal")
	} else if !strings.Contains(err.Error(), "-f") {
		t.Errorf("the refusal does not mention how to override it: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "a previous rip" {
		t.Errorf("the existing image was modified: %q", got)
	}

	w, err := NewWriter(path, true)
	if err != nil {
		t.Fatalf("NewWriter with force = %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestWriterRejectsWrongSectorSize(t *testing.T) {
	t.Parallel()
	w, err := NewWriter(filepath.Join(t.TempDir(), "out.iso"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close() //nolint:errcheck // test cleanup
	if err := w.WriteSector(0, make([]byte, 1024)); err == nil {
		t.Error("WriteSector accepted a 1024-byte sector")
	}
}
