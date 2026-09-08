package qic

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"samuelks.com/datarescue/ddrescue"
)

// Source is one archive to read. Reader lets a caller supply the bytes
// directly, as the fuzzer does. Path is then only a label and need not be a
// file on disk, and Map cannot be used with it.
type Source struct {
	Path   string      // the archive, and the label used in reports
	Map    string      // a ddrescue map; the sidecar beside Path is used when it exists
	NoMap  bool        // read the archive as it is, even with a sidecar beside it
	Reader io.ReaderAt // bytes supplied by the caller; Size is then required
	Size   int64
}

// Mode decides where the file list comes from.
type Mode uint8

const (
	// ModeAuto reads the catalog and falls back to scanning the data region
	// when it will not parse.
	ModeAuto Mode = iota
	// ModeCatalog reads the catalog and fails if it cannot, so an operator can
	// find out whether the catalog is intact.
	ModeCatalog
	// ModeScan ignores the catalog and scans the data region, which is how a
	// catalog that reads but is not trusted gets cross-checked.
	ModeScan
)

// Options controls what Open reads.
type Options struct {
	// Volume selects among several volume table records, 1-based. Windows 98
	// and ME write one per source drive; 0 takes the first.
	Volume int
	Mode   Mode
}

// Archive is an opened archive.
type Archive struct {
	diags diagnostics

	src    Source
	r      io.ReaderAt
	closer io.Closer
	size   int64
	opts   Options

	volumes     []Volume
	volume      int
	mdid        *MediumID
	lay         layout
	index       *segIndex
	data        *dataStream
	files       []File
	driveLetter string
	catEnd      int64 // where the accumulated offsets run out
	scanned     bool
	// sweeps counts the searches of the data region for a record that is not
	// where the catalog puts it, which maxAnchorSweeps bounds.
	sweeps int
}

// maxAnchorSweeps bounds how many times the data region is searched for a
// record the catalog misplaced. A search runs to the end of the region, and one
// rebase moves every file after the record that drifted, so a sound archive
// needs at most one.
const maxAnchorSweeps = 8

// Open reads the archive at path, taking a ddrescue map beside it when one is
// there.
func Open(path string) (*Archive, error) {
	return OpenSource(Source{Path: path}, Options{})
}

// OpenSource reads one archive.
func OpenSource(src Source, opts Options) (*Archive, error) {
	a := &Archive{src: src, opts: opts}
	if err := a.open(); err != nil {
		return nil, a.failed(err)
	}
	return a, nil
}

func (a *Archive) open() error {
	if err := a.attach(); err != nil {
		return err
	}

	vols, mdid, err := readHeaderRegion(a.r, a.size, a.diags.add)
	if err != nil {
		return err
	}
	a.volumes, a.mdid = vols, mdid

	variant := VariantWin95
	if mdid != nil {
		variant = VariantWin98
	}
	for i := range a.volumes {
		a.volumes[i].Variant = variant
	}

	a.volume = 0
	if a.opts.Volume > 0 {
		if a.opts.Volume > len(a.volumes) {
			return fmt.Errorf("qic: volume %d was asked for but the archive holds %d", a.opts.Volume, len(a.volumes))
		}
		a.volume = a.opts.Volume - 1
	}
	v := a.volumes[a.volume]

	if v.Compressed && v.Algorithm != 1 {
		a.diags.add(DiagUnknownCompression,
			"the volume is compressed with algorithm %d where only QIC-122 is known, so its segments are read as stored",
			v.Algorithm)
	}
	if v.Flags&volSpansMedia != 0 {
		a.diags.add(DiagSpansCartridges,
			"the volume spans several cartridges; only the part in this file can be recovered, since the catalog does not say which cartridge holds what")
	}
	if a.lay, err = resolveLayout(v, len(a.volumes), variant, a.size, a.diags.add); err != nil {
		return err
	}
	a.index = buildSegIndex(a.r, a.lay, v, a.diags.add)
	a.data = newDataStream(a.r, a.index, a.diags.add)

	a.driveLetter = v.Drive
	a.loadFiles(v)
	if len(a.files) == 0 {
		return errors.New("qic: no file could be recovered from the catalog or the data region")
	}
	a.markIntact()
	a.verifyFirstAnchor()
	return nil
}

// loadFiles fills the file list, from the catalog where it can be read and by
// scanning the data region where it cannot. The fallback matters because the
// data region repeats every record inline together with its path, so a backup
// whose catalog is gone is still recoverable in full.
func (a *Archive) loadFiles(v Volume) {
	if a.opts.Mode != ModeScan {
		cr, err := loadCatalogRegion(a.r, a.index, v.DirSize, a.diags.add)
		if err == nil {
			a.files, a.driveLetter, a.catEnd = decodeCatalog(cr, a.lay.variant, v.Drive, a.diags.add)
		} else {
			a.diags.add(DiagCatalogUnreadable, "the catalog could not be read: %v", err)
		}
		if len(a.files) > 0 {
			a.numberFiles()
			a.checkTotal(v)
			return
		}
		if a.opts.Mode == ModeCatalog {
			return
		}
		a.diags.add(DiagDataScan, "the catalog yielded no file, so the data region was scanned for records instead")
	}
	files, err := a.scanData()
	if err != nil {
		a.diags.add(DiagDataScan, "the data region could not be scanned: %v", err)
		return
	}
	a.files, a.scanned = files, true
	a.numberFiles()
}

// numberFiles records each entry's position, so a File a caller copied can be
// resolved back to the archive's own.
func (a *Archive) numberFiles() {
	for i := range a.files {
		a.files[i].idx = i
	}
}

// own is the archive's own entry for f. Files hands out a slice a caller can
// copy from, and a drift found at one record moves every file after it, so a
// read goes through the archive's entry rather than through a copy still
// holding the position it had before.
func (a *Archive) own(f *File) *File {
	if f.idx >= 0 && f.idx < len(a.files) && a.files[f.idx].Path == f.Path {
		return &a.files[f.idx]
	}
	return f
}

// checkTotal compares where the accumulated offsets end against the size the
// volume table records. They agree to the byte on a sound archive, so a
// difference means an offset somewhere is wrong and every file is worth
// checking against the record the data region repeats at it.
func (a *Archive) checkTotal(v Volume) {
	if len(a.files) == 0 || v.DataSize == 0 || a.catEnd == v.DataSize {
		return
	}
	a.diags.add(DiagVTBLInconsistent,
		"the catalog accounts for %d bytes of data where the volume table records %d, so every file's position is checked against the data region",
		a.catEnd, v.DataSize)
	for i := range a.files {
		a.verifyAnchor(&a.files[i])
	}
}

// markIntact records whether every segment a file spans yields what its offset
// chain claims, so a listing can flag a file before anything reads it.
func (a *Archive) markIntact() {
	for i := range a.files {
		f := &a.files[i]
		f.intact = f.Dir || f.dataOff >= 0 && a.index.intact(f.dataOff, f.Size)
	}
}

// verifyFirstAnchor checks the first file's position at Open. Getting the
// variant wrong, or the root record, shifts every offset in the archive, and
// checking here costs one segment and catches it before an extraction runs.
func (a *Archive) verifyFirstAnchor() {
	for i := range a.files {
		if a.files[i].dataOff < 0 {
			continue
		}
		a.verifyAnchor(&a.files[i])
		return
	}
}

// Drive is the source drive the backup was taken from, where the archive says
// so. Windows 98 and ME record it in the volume table; Windows 95 only carries
// it inside the root catalog record's name.
func (a *Archive) Drive() string { return a.driveLetter }

// attach opens the bytes, going through ddrescue when a map records which
// parts of the image were recovered.
func (a *Archive) attach() error {
	switch {
	case a.src.Reader != nil:
		if a.src.Map != "" {
			return errors.New("qic: a supplied reader cannot also take a ddrescue map")
		}
		a.r, a.size = a.src.Reader, a.src.Size
	default:
		if a.src.Path == "" {
			return errors.New("qic: no archive was named")
		}
		src, err := ddrescue.OpenSource(a.src.Path, ddrescue.Options{Map: a.src.Map, NoMap: a.src.NoMap})
		if err != nil {
			return fmt.Errorf("qic: %w", err)
		}
		a.src.Map, a.size = src.MapPath, src.Size
		a.r, a.closer = src.Checked(), src
	}
	if a.size < VTBLSize {
		return fmt.Errorf("%w: %d bytes is shorter than a volume table record", ErrNotQIC, a.size)
	}
	return nil
}

// failed folds the diagnostics into the error, since a caller that never
// receives an Archive cannot ask it for them.
func (a *Archive) failed(err error) error {
	a.Close() //nolint:errcheck // best-effort cleanup on the error path
	diags := a.diags.drain()
	if len(diags) == 0 {
		return err
	}
	msgs := make([]string, 0, len(diags))
	for _, d := range diags {
		msgs = append(msgs, d.Message)
	}
	return fmt.Errorf("%w\n  %s", err, strings.Join(msgs, "\n  "))
}

// Volumes lists the volume table records the archive carries.
func (a *Archive) Volumes() []Volume { return a.volumes }

// Volume is the record the archive was opened against.
func (a *Archive) Volume() Volume { return a.volumes[a.volume] }

// MediumID is the MDID record, or nil for a Windows 95 archive.
func (a *Archive) MediumID() *MediumID { return a.mdid }

// Segments is the segment index, which shows where a bad region of the medium
// landed in the uncompressed stream.
func (a *Archive) Segments() []Segment { return a.index.segs }

// DataSegments is how many segments precede the catalog.
func (a *Archive) DataSegments() int { return a.index.dataSegs }

// DataSize is the length of the uncompressed data region as the segment
// offsets account for it.
func (a *Archive) DataSize() int64 { return a.index.dataLength }

// CatalogSize is how many catalog bytes were recovered.
func (a *Archive) CatalogSize() int64 { return a.index.catalogLen }

// Files lists the entries in catalog order, directories included.
func (a *Archive) Files() []File { return a.files }

// FromScan reports whether the file list came from scanning the data region
// rather than from the catalog, in which case sizes are derived rather than
// recorded.
func (a *Archive) FromScan() bool { return a.scanned }

// Layout is where the two regions begin in the file.
func (a *Archive) Layout() (dataStart, catalogStart int64) {
	return a.lay.dataStart, a.lay.catalogStart
}

// DataReaderAt reads the uncompressed data region, expanding only what a read
// touches.
func (a *Archive) DataReaderAt() io.ReaderAt { return a.data }

// Find looks a file up by the path Files reports, matching without regard to
// case or separator.
func (a *Archive) Find(path string) (*File, bool) {
	want := normalizePath(path)
	for i := range a.files {
		if normalizePath(a.files[i].Path) == want {
			return &a.files[i], true
		}
	}
	return nil, false
}

// Diagnostics returns the problems noticed so far and clears the list.
func (a *Archive) Diagnostics() []Diagnostic { return a.diags.drain() }

// Source describes where the archive was read from.
func (a *Archive) Source() Source { return a.src }

// Close releases the archive. A caller-supplied reader is left alone.
func (a *Archive) Close() error {
	if a.closer == nil {
		return nil
	}
	err := a.closer.Close()
	a.closer = nil
	return err
}

// Recoverable reports whether the file has a known position and every segment
// it spans yields what the offsets claim.
func (f *File) Recoverable() bool {
	return !f.Dir && f.dataOff >= 0 &&
		f.checked != anchorLost && f.checked != anchorUnresolved && f.intact
}

// RecordOffset is where the record describing the file sits in the
// uncompressed data region, which is what an accumulated offset is checked
// against.
func (f *File) RecordOffset() int64 { return f.recOff }

// DataOffset is where the file's bytes start in the uncompressed data region,
// or -1 for a directory or a file whose position was lost.
func (f *File) DataOffset() int64 {
	if f.checked == anchorLost || f.checked == anchorUnresolved {
		return -1
	}
	return f.dataOff
}
