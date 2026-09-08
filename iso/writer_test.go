package iso

import (
	"os"
	"strings"
	"testing"
)

func TestWriterSectors(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/test.iso"

	w, err := NewWriter(path, false)
	if err != nil {
		t.Fatal(err)
	}

	sector := make([]byte, 2048)
	for i := range sector {
		sector[i] = 0xAB
	}
	if err := w.WriteSector(0, sector); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteZeroSector(1); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 2*2048 {
		t.Fatalf("file size = %d; want %d", len(data), 2*2048)
	}
	// First sector should be 0xAB
	if data[0] != 0xAB || data[2047] != 0xAB {
		t.Error("first sector data wrong")
	}
	// Second sector should be zeros
	for i, b := range data[2048:] {
		if b != 0 {
			t.Errorf("zero sector byte %d = %d; want 0", i, b)
			break
		}
	}
}

func TestWriteMap(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/test.iso"

	w, err := NewWriter(path, false)
	if err != nil {
		t.Fatal(err)
	}
	sector := make([]byte, 2048)
	if err := w.WriteSector(0, sector); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteZeroSector(1); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteSector(2, sector); err != nil {
		t.Fatal(err)
	}

	mapPath := dir + "/test.map"
	if err := w.WriteMap(mapPath); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	mapData, err := os.ReadFile(mapPath)
	if err != nil {
		t.Fatal(err)
	}
	content := string(mapData)
	if !strings.Contains(content, "+") {
		t.Error("map missing '+' entry")
	}
	if !strings.Contains(content, "-") {
		t.Error("map missing '-' entry for bad sector")
	}
}
