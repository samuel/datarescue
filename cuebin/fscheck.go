package cuebin

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

	"samuelks.com/datarescue/apm"
	"samuelks.com/datarescue/ddrescue"
	"samuelks.com/datarescue/hfs"
	"samuelks.com/datarescue/iso"
)

// FSType identifies a detected filesystem on the disc.
type FSType int

const (
	FSUnknown FSType = iota
	FSISO9660
	FSApplePartitionMap
	FSHFS
)

func (t FSType) String() string {
	switch t {
	case FSISO9660:
		return "ISO 9660"
	case FSApplePartitionMap:
		return "Apple Partition Map"
	case FSHFS:
		return "HFS"
	default:
		return "Unknown"
	}
}

// FileEntry describes one file's location on the disc.
type FileEntry struct {
	Path       string
	StartLBA   int64
	EndLBA     int64
	BadSectors []int64
}

// FullyLost reports whether every sector of the file is bad.
func (e *FileEntry) FullyLost() bool {
	if len(e.BadSectors) == 0 {
		return false
	}
	return int64(len(e.BadSectors)) >= e.EndLBA-e.StartLBA
}

// FSResult holds the outcome of filesystem detection and traversal.
type FSResult struct {
	Type            FSType
	VolumeLabel     string
	DeclaredSectors int64
	Files           []FileEntry
	AffectedFiles   int
	// Issues records damage found while reading the filesystem. Without it a
	// disc whose catalog will not parse reports zero affected files, which reads
	// as a clean result.
	Issues []string
}

func (r *FSResult) addIssue(format string, args ...any) {
	r.Issues = append(r.Issues, fmt.Sprintf(format, args...))
}

// CheckFilesystem detects and traverses the filesystem on the disc, marking
// each file's bad sectors from vr when it is non-nil. It reads src.Checked, so
// a range the rescue never recovered is never decoded as structure.
//
// Damage is collected rather than fatal. There is no error return because no
// failure here should cost the operator the rest of the report: a filesystem
// that cannot be reached is reported as an issue on an FSUnknown result.
func CheckFilesystem(src *Source, tracks []CueTrack, vr *VerifyResult) *FSResult {
	sectorSize, err := BinSectorSize(tracks)
	if err != nil {
		// An unusable sector stride is reported like any other finding rather
		// than failing the whole report.
		res := &FSResult{Type: FSUnknown}
		res.addIssue("%s", err)
		return res
	}
	ra := src.Checked()

	// Try ISO 9660 (including Joliet and Rock Ridge extensions).
	r, isoErr := iso.OpenReaderWithSectorSize(ra, sectorSize)
	if isoErr == nil {
		return buildISOFSResult(r, vr)
	}

	res := &FSResult{Type: FSUnknown}
	// "The volume descriptor was never recovered" and "this disc carries no ISO
	// 9660" are different answers, and only the map can tell them apart.
	if errors.Is(isoErr, iso.ErrPVDUnrecovered) {
		res.addIssue("%s", isoErr)
	}

	// Try Apple Partition Map / HFS.
	sec0, err := iso.ReadUserData(ra, 0, sectorSize)
	if err != nil {
		// Returning FSUnknown silently would claim the disc has no filesystem,
		// when in fact sector 0 could not be read.
		res.addIssue("sector 0 would not read, so no partition map could be looked for: %s", err)
		return res
	}
	// A driver descriptor record in sector 0 says this is an Apple-formatted
	// disc. The map is then parsed structurally: one disc in this archive
	// carries the string "Apple_HFS" six times, only one of which is an entry.
	if len(sec0) >= 2 && bytes.Equal(sec0[0:2], apm.DriverSignature[:]) {
		out := parseAPM(ra, sectorSize, vr)
		out.Issues = append(res.Issues, out.Issues...)
		return out
	}

	return res
}

// buildISOFSResult builds an FSResult from an open iso.Reader.
func buildISOFSResult(r *iso.Reader, vr *VerifyResult) *FSResult {
	res := &FSResult{
		Type:            FSISO9660,
		VolumeLabel:     r.VolumeLabel(),
		DeclaredSectors: r.TotalSectors(),
	}
	_ = r.Walk(func(e iso.DirEntry) error {
		if e.IsDir || e.Symlink != "" {
			return nil
		}
		// StartLBA and EndLBA span every extent, as they do for an HFS fork;
		// the bad sectors are taken from the extents themselves.
		entry := FileEntry{Path: e.Path}
		for i, x := range e.Extents {
			end := x.LBA + (x.Size+iso.UserDataSize-1)/iso.UserDataSize
			if i == 0 || x.LBA < entry.StartLBA {
				entry.StartLBA = x.LBA
			}
			if i == 0 || end > entry.EndLBA {
				entry.EndLBA = end
			}
			if vr == nil {
				continue
			}
			for s := x.LBA; s < end; s++ {
				if s < int64(len(vr.Sectors)) && (vr.Sectors[s].MapBad || vr.Sectors[s].BadEDC) {
					entry.BadSectors = append(entry.BadSectors, s)
				}
			}
		}
		res.Files = append(res.Files, entry)
		return nil
	})
	// The reader's issues are only complete once Walk has run, so they are
	// collected after it rather than at open time. Walk fails only when the
	// callback asks it to stop, and this one never does.
	res.Issues = append(res.Issues, r.Issues()...)
	for i := range res.Files {
		if len(res.Files[i].BadSectors) > 0 {
			res.AffectedFiles++
		}
	}
	return res
}

// sectorReader presents the user data of raw or cooked CD sectors as a flat
// byte stream, so a parser that reads by byte offset can read a filesystem
// inside a BIN.
//
// It carries no base offset: apm.ReadMap and hfs.Open both take one, so they
// handle a partition that begins part way into a 2048-byte sector.
type sectorReader struct {
	ra         io.ReaderAt
	sectorSize int // as the image stores a sector
	dataOffset int // where that sector's user data starts
}

func (sr *sectorReader) ReadAt(p []byte, off int64) (int, error) {
	var totalRead int
	for totalRead < len(p) {
		currentOff := off + int64(totalRead)
		sector := currentOff / iso.UserDataSize
		inSector := currentOff % iso.UserDataSize
		binOffset := sector*int64(sr.sectorSize) + int64(sr.dataOffset) + inSector

		toRead := min(int64(len(p)-totalRead), iso.UserDataSize-inSector)

		n, err := sr.ra.ReadAt(p[totalRead:totalRead+int(toRead)], binOffset)
		totalRead += n
		if err != nil {
			return totalRead, err
		}
	}
	return totalRead, nil
}

// parseAPM walks an Apple partition map and looks inside the HFS volumes it
// lists.
func parseAPM(ra io.ReaderAt, sectorSize int, vr *VerifyResult) *FSResult {
	res := &FSResult{Type: FSApplePartitionMap}

	dataOffset, err := iso.DataOffset(sectorSize)
	if err != nil {
		res.addIssue("%s", err)
		return res
	}
	sr := &sectorReader{ra: ra, sectorSize: sectorSize, dataOffset: dataOffset}

	m, err := apm.ReadMap(sr, 0)
	if err != nil {
		res.addIssue("the Apple partition map would not read: %s", err)
		return res
	}
	for _, is := range m.Issues {
		res.addIssue("Apple partition map: %s", is)
	}
	// Report every entry, not only the one examined: on a hybrid disc, a
	// partition this tool did not open is worth knowing about.
	for i := range m.Entries {
		e := &m.Entries[i]
		if apm.IsHFS(e.TypeName()) || e.Unused() {
			continue
		}
		res.addIssue("Apple partition %d is %s and was not examined", e.Index, apm.TypeDescription(e.TypeName()))
	}

	// Try every HFS partition in map order and take the first that opens: a
	// disc may have more than the two entries the simplest ones carry.
	for i := range m.Entries {
		e := &m.Entries[i]
		if !apm.IsHFS(e.TypeName()) {
			continue
		}
		vol, err := hfs.Open(sr, e.Offset(), e.Size())
		if err != nil {
			// The map says HFS but the volume would not open. Report it rather
			// than quietly downgrading to a bare partition-map result.
			res.addIssue("HFS volume at byte %d would not open: %s", e.Offset(), err)
			continue
		}
		fillHFS(res, vol, vr)
		return res
	}
	return res
}

// fillHFS describes an open HFS volume and maps each file's forks to the disc
// sectors holding them.
func fillHFS(res *FSResult, vol *hfs.Volume, vr *VerifyResult) {
	res.Type = FSHFS
	res.VolumeLabel = vol.MDB.Label()
	res.DeclaredSectors = int64(vol.MDB.AllocationBlocks) * int64(vol.MDB.AllocationBlockSize) / iso.UserDataSize

	entries, catIssues, err := vol.List()
	for _, ci := range catIssues {
		res.addIssue("HFS catalog: %s", ci)
	}
	if err != nil {
		res.addIssue("HFS catalog could not be listed: %s", err)
		return
	}

	for _, e := range entries {
		if e.IsDir {
			continue
		}
		entry := FileEntry{Path: strings.Join(e.Path, "/")}
		// Check both forks: a file whose resource fork sits on a bad sector is
		// affected, and checking only the data fork would report it clean.
		first := true
		for _, ref := range []hfs.ForkRef{e.DataFork(), e.ResourceFork()} {
			if ref.Size == 0 {
				continue
			}
			ranges, err := vol.ForkRanges(ref)
			if err != nil {
				res.addIssue("HFS %s: %s", entry.Path, err)
				continue
			}
			for _, rg := range ranges {
				// The volume's own base is already in ImageOffset, so an extent
				// inside a partition that does not begin on a sector boundary
				// lands in the sector that really holds it.
				lba := rg.ImageOffset / iso.UserDataSize
				end := (rg.ImageOffset + rg.Length + iso.UserDataSize - 1) / iso.UserDataSize
				if first || lba < entry.StartLBA {
					entry.StartLBA = lba
				}
				if first || end > entry.EndLBA {
					entry.EndLBA = end
				}
				first = false
			}
		}
		for _, d := range vol.Diagnostics() {
			res.addIssue("HFS %s: %s", entry.Path, d)
		}

		if vr != nil {
			for s := entry.StartLBA; s < entry.EndLBA; s++ {
				if s < int64(len(vr.Sectors)) && (vr.Sectors[s].MapBad || vr.Sectors[s].BadEDC) {
					entry.BadSectors = append(entry.BadSectors, s)
				}
			}
		}
		if len(entry.BadSectors) > 0 {
			res.AffectedFiles++
		}
		res.Files = append(res.Files, entry)
	}
}

// DeepVerifyFiles rebuilds each FileEntry's BadSectors by actually re-reading
// every file sector through src.Checked, rather than cross-referencing the
// verify result.
func DeepVerifyFiles(src *Source, tracks []CueTrack, fsr *FSResult) {
	sectorSize, err := BinSectorSize(tracks)
	if err != nil {
		fsr.addIssue("%s", err)
		return
	}
	ra := src.Checked()
	buf := make([]byte, sectorSize)

	// A sector the map does not mark recovered is expected damage and is only
	// counted. Any other read failure means the BIN itself is failing (a
	// truncated file, a disk error), which is reported once rather than once
	// per sector.
	var ioErrs int64
	var firstIOErr error

	fsr.AffectedFiles = 0
	for i := range fsr.Files {
		fe := &fsr.Files[i]
		fe.BadSectors = fe.BadSectors[:0]
		for lba := fe.StartLBA; lba < fe.EndLBA; lba++ {
			if _, err := ra.ReadAt(buf, lba*int64(sectorSize)); err != nil {
				fe.BadSectors = append(fe.BadSectors, lba)
				if !errors.Is(err, ddrescue.ErrNotRecovered) {
					ioErrs++
					if firstIOErr == nil {
						firstIOErr = err
					}
				}
			} else if sectorSize == iso.RawSectorSize && buf[15] == 1 {
				// Byte 15 is the sector's own mode, so this is a raw MODE1
				// sector and carries an EDC to check.
				if !mode1EDCMatches(buf) {
					fe.BadSectors = append(fe.BadSectors, lba)
				}
			}
		}
		if len(fe.BadSectors) > 0 {
			fsr.AffectedFiles++
		}
	}
	if ioErrs > 0 {
		fsr.addIssue("%d file sector(s) failed to read for reasons the rescue map does not account for, first: %s",
			ioErrs, firstIOErr)
	}
}
