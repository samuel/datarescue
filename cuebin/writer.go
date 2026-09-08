package cuebin

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"samuelks.com/datarescue/cdrom"
	"samuelks.com/datarescue/ddrescue"
	"samuelks.com/datarescue/internal/xio"
)

type Writer struct {
	bin     *os.File
	toc     cdrom.TOC
	mapping *ddrescue.MapBuilder
	binName string // base filename for the CUE FILE line
	cuePath string
	force   bool
	// binStartLBA is the disc LBA the BIN begins at (the first track's). Every
	// position the sidecars record is relative to it: both the cue's INDEX
	// values and the map's offsets are into this file, not onto the disc.
	binStartLBA    int64
	detectedIndex0 map[uint8]int64 // track number → first detected pregap LBA
	// detectedMode is each data track's sector mode, read from byte 15 of its
	// first sector rather than assumed.
	detectedMode map[uint8]byte
}

// NewWriter creates <stem>.bin; the matching cue sheet is written by Close. It
// refuses to truncate an existing BIN unless force is set, since re-ripping
// over a good previous rip of one-shot media destroys the only copy.
func NewWriter(stem string, toc cdrom.TOC, force bool) (*Writer, error) {
	binPath := stem + ".bin"
	f, err := xio.Create(binPath, force)
	if err != nil {
		return nil, fmt.Errorf("cuebin: %w", err)
	}
	var binStart int64
	if len(toc.Tracks) > 0 {
		binStart = toc.Tracks[0].StartLBA
	}
	return &Writer{
		bin:            f,
		force:          force,
		toc:            toc,
		mapping:        ddrescue.NewMapBuilder(cdrom.RawSectorSize),
		binName:        filepath.Base(binPath),
		cuePath:        stem + ".cue",
		binStartLBA:    binStart,
		detectedIndex0: make(map[uint8]int64),
		detectedMode:   make(map[uint8]byte),
	}, nil
}

// record notes a sector's outcome in the map at its position in the BIN. The
// LBA is a disc LBA and the BIN begins at the first track's, so the two differ
// by binStartLBA when the first track does not start at 0. A map left in disc
// offsets would attribute every byte of damage to the wrong sector.
func (w *Writer) record(lba int64, failed bool) {
	w.mapping.Record(lba-w.binStartLBA, failed)
}

// WriteRawSector appends a 2352-byte raw sector to the BIN file.
// lba is used only for map tracking; sectors must be written in order.
func (w *Writer) WriteRawSector(lba int64, data []byte) error {
	if len(data) != cdrom.RawSectorSize {
		return fmt.Errorf("cuebin: raw sector must be %d bytes, got %d", cdrom.RawSectorSize, len(data))
	}
	if _, err := w.bin.Write(data); err != nil {
		return fmt.Errorf("cuebin: write sector %d: %w", lba, err)
	}
	w.record(lba, false)
	w.detectPregap(lba, data)
	w.detectMode(lba, data)
	return nil
}

// WriteZeroSector appends 2352 zero bytes for an unreadable sector and marks
// it as failed in the map.
func (w *Writer) WriteZeroSector(lba int64) error {
	zeros := make([]byte, cdrom.RawSectorSize)
	if _, err := w.bin.Write(zeros); err != nil {
		return fmt.Errorf("cuebin: write zero sector %d: %w", lba, err)
	}
	w.record(lba, true)
	return nil
}

// WritePregapSector appends 2352 zero bytes for an unreadable pregap sector.
// Unlike WriteZeroSector it marks the sector finished, since an unreadable
// pregap is expected rather than lost data.
func (w *Writer) WritePregapSector(lba int64) error {
	zeros := make([]byte, cdrom.RawSectorSize)
	if _, err := w.bin.Write(zeros); err != nil {
		return fmt.Errorf("cuebin: write pregap sector %d: %w", lba, err)
	}
	w.record(lba, false)
	w.detectPregap(lba, nil) // nil = known non-data sector
	return nil
}

// detectPregap records where each audio track's pregap starts for tracks whose
// Index0LBA the TOC did not report. Only data sectors carry the 12-byte sync
// pattern, so its absence marks the transition.
func (w *Writer) detectPregap(lba int64, data []byte) {
	if len(data) >= 12 && [12]byte(data[:12]) == syncPattern {
		return // sync header present → MODE1 data, not pregap
	}
	next, ok := w.toc.NextTrackAfter(lba)
	if !ok || next.Number <= 1 || next.Type != cdrom.TrackTypeAudio {
		return
	}
	if next.Index0LBA == 0 && w.detectedIndex0[next.Number] == 0 {
		w.detectedIndex0[next.Number] = lba
	}
}

// detectMode records a data track's sector mode from byte 15 of its first
// sector, the mode byte in the sector header. Labeling every data track
// MODE1/2352 would give a MODE2/XA disc a cue that makes verify fail the EDC
// of every sector and export drop 288 bytes of each.
func (w *Writer) detectMode(lba int64, data []byte) {
	if len(data) < 16 || [12]byte(data[:12]) != syncPattern {
		return
	}
	tr, ok := w.toc.TrackAt(lba)
	if !ok || tr.Type != cdrom.TrackTypeData {
		return
	}
	if _, seen := w.detectedMode[tr.Number]; !seen {
		w.detectedMode[tr.Number] = data[15]
	}
}

func (w *Writer) WriteMap(path string) error {
	// NewWriter already decided whether the BIN may be overwritten, so its
	// sidecars are overwritten unconditionally: a stale map next to a fresh
	// image is worse than none.
	f, err := xio.Create(path, true)
	if err != nil {
		return fmt.Errorf("cuebin: create map file %s: %w", path, err)
	}
	_, err = w.mapping.WriteTo(f)
	return xio.CloseAfter(f, err)
}

// Close flushes the CUE sheet and closes the BIN file.
func (w *Writer) Close() error {
	cueErr := w.writeCUE()
	binErr := w.bin.Close()
	if cueErr != nil {
		return cueErr
	}
	return binErr
}

// binSectorToMSF converts a BIN-relative sector index to a CUE MSF string.
// CUE INDEX positions are raw frame counts from BIN sector 0, so unlike
// cdrom.LBAToMSF no 150-sector pregap is added.
func binSectorToMSF(n int64) (m, s, f byte) {
	// A position outside what MSF can express is clamped rather than wrapped: a
	// wrapped INDEX would point somewhere else in the image.
	n = min(max(n, 0), maxCueSector)
	f = byte(n % 75)
	n /= 75
	s = byte(n % 60)
	m = byte(n / 60)
	return m, s, f
}

// maxCueSector is the highest position a CUE MSF field can express: 99 minutes,
// 59 seconds, 74 frames.
const maxCueSector = (99*60+59)*75 + 74

func (w *Writer) writeCUE() error {
	var sb strings.Builder
	fmt.Fprintf(&sb, "FILE %q BINARY\n", w.binName)

	// The BIN starts at the first track's IOKit LBA; subtract it so INDEX
	// positions are sector offsets within the BIN, not absolute disc LBAs.
	binStartLBA := w.binStartLBA

	for _, tr := range w.toc.Tracks {
		trackMode := w.trackModeString(tr)
		fmt.Fprintf(&sb, "  TRACK %02d %s\n", tr.Number, trackMode)

		// Prefer the TOC's Index0LBA, falling back to the pregap detected while
		// writing for drives that report no in-track descriptors. Track 1's
		// pregap is never ripped into the BIN, so skip it.
		index0LBA := tr.Index0LBA
		if index0LBA == 0 {
			index0LBA = w.detectedIndex0[tr.Number]
		}
		if tr.Number > 1 && index0LBA != 0 {
			m0, s0, f0 := binSectorToMSF(index0LBA - binStartLBA)
			fmt.Fprintf(&sb, "    INDEX 00 %02d:%02d:%02d\n", m0, s0, f0)
		}

		m, s, frm := binSectorToMSF(tr.StartLBA - binStartLBA)
		fmt.Fprintf(&sb, "    INDEX 01 %02d:%02d:%02d\n", m, s, frm)
	}

	f, err := xio.Create(w.cuePath, true)
	if err != nil {
		return fmt.Errorf("cuebin: create cue %s: %w", w.cuePath, err)
	}
	_, err = f.WriteString(sb.String())
	return xio.CloseAfter(f, err)
}

// trackModeString returns the CUE track mode string for a track, taking a data
// track's mode from the sector header the rip read rather than assuming MODE1.
// If the mode byte was never read, it falls back to MODE1, which most data
// discs are.
func (w *Writer) trackModeString(tr cdrom.Track) string {
	if tr.Type == cdrom.TrackTypeAudio {
		return "AUDIO"
	}
	if w.detectedMode[tr.Number] == 2 {
		return "MODE2/2352"
	}
	return "MODE1/2352"
}

// Stem returns the output stem (path without extension).
func Stem(output string) string {
	ext := strings.ToLower(filepath.Ext(output))
	if ext == ".bin" || ext == ".cue" {
		return strings.TrimSuffix(output, filepath.Ext(output))
	}
	return output
}
