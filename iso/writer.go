package iso

import (
	"fmt"
	"os"

	"samuelks.com/datarescue/ddrescue"
	"samuelks.com/datarescue/internal/xio"
)

// sectorSize is the ISO payload sector size.
const sectorSize = 2048

type Writer struct {
	f       *os.File
	mapping *ddrescue.MapBuilder
	cooked  *ddrescue.MapBuilder // sectors recovered via block-level fallback, not low-level verified
}

// NewWriter creates the output image. It refuses to truncate an existing file
// unless force is set: re-running a rip over a good previous rip of one-shot
// media destroys the only copy.
func NewWriter(path string, force bool) (*Writer, error) {
	f, err := xio.Create(path, force)
	if err != nil {
		return nil, fmt.Errorf("iso: %w", err)
	}
	return &Writer{f: f, mapping: ddrescue.NewMapBuilder(sectorSize), cooked: ddrescue.NewMapBuilder(sectorSize)}, nil
}

func (w *Writer) WriteSector(lba int64, data []byte) error {
	if len(data) != sectorSize {
		return fmt.Errorf("iso: sector data must be %d bytes, got %d", sectorSize, len(data))
	}
	if _, err := w.f.WriteAt(data, lba*sectorSize); err != nil {
		return fmt.Errorf("iso: write sector %d: %w", lba, err)
	}
	w.mapping.Record(lba, false)
	return nil
}

// WriteCookedSector writes a sector recovered through a block-level fallback
// rather than the low-level read path, recording it in both the main map and
// the cooked-provenance map so the two can be cross-referenced later.
func (w *Writer) WriteCookedSector(lba int64, data []byte) error {
	if len(data) != sectorSize {
		return fmt.Errorf("iso: sector data must be %d bytes, got %d", sectorSize, len(data))
	}
	if _, err := w.f.WriteAt(data, lba*sectorSize); err != nil {
		return fmt.Errorf("iso: write sector %d: %w", lba, err)
	}
	w.mapping.Record(lba, false)
	w.cooked.Record(lba, false)
	return nil
}

func (w *Writer) WriteZeroSector(lba int64) error {
	zeros := make([]byte, sectorSize)
	if _, err := w.f.WriteAt(zeros, lba*sectorSize); err != nil {
		return fmt.Errorf("iso: write zero sector %d: %w", lba, err)
	}
	w.mapping.Record(lba, true)
	return nil
}

func (w *Writer) WriteMap(path string) error {
	// NewWriter already decided whether the image may be overwritten, so its
	// sidecar is overwritten unconditionally: a stale map next to a fresh
	// image is worse than none.
	f, err := xio.Create(path, true)
	if err != nil {
		return fmt.Errorf("iso: create map file %s: %w", path, err)
	}
	_, err = w.mapping.WriteTo(f)
	return xio.CloseAfter(f, err)
}

// WriteCookedMap writes the cooked-provenance sidecar: a ddrescue-format map
// where "finished" means a sector came from the block-level fallback rather
// than the low-level read path, and an absent range means it did not.
func (w *Writer) WriteCookedMap(path string) error {
	f, err := xio.Create(path, true)
	if err != nil {
		return fmt.Errorf("iso: create cooked map file %s: %w", path, err)
	}
	_, err = w.cooked.WriteTo(f)
	return xio.CloseAfter(f, err)
}

func (w *Writer) Close() error {
	return w.f.Close()
}
