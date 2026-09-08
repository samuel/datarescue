package iso

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"samuelks.com/datarescue/ddrescue"
)

// ErrNotISO9660 is returned when no ISO 9660 Primary Volume Descriptor is found.
var ErrNotISO9660 = errors.New("iso: no ISO 9660 Primary Volume Descriptor found")

// ErrIncompleteFile ends a FileReader for a multi-extent file whose chain of
// records stopped before its final extent. The bytes of the extents that were
// found are delivered first; how many are missing is unknown.
var ErrIncompleteFile = errors.New("iso: the file's later extents are missing")

// Extent is one contiguous run of a file's data.
type Extent struct {
	LBA  int64
	Size int64 // bytes of the file in this extent
}

// DirEntry describes a file or directory encountered during Walk.
type DirEntry struct {
	Path     string // slash-separated, leading "/"
	IsDir    bool
	Symlink  string // non-empty → Rock Ridge symlink; StartLBA and Size are zero
	StartLBA int64  // the first extent's LBA
	Size     int64  // declared byte length; for a file, the sum of its extents

	// Extents is where a file's data is, in file order. Most files have one;
	// a file stored across several records (ISO 9660 Level 3, used for files
	// over 4 GiB) has one per record. It is nil for a directory or symlink.
	Extents []Extent
	// Incomplete marks a multi-extent file whose chain of records ended before
	// a final extent. Size then counts only the extents found, and reading it
	// ends with ErrIncompleteFile.
	Incomplete bool
	// Associated marks an associated file, which shares its name, and so its
	// Path, with the file it belongs to. Classic Mac discs use them for
	// resource forks.
	Associated bool

	// ModTime is the Rock Ridge TF modify time when the record carries one,
	// else the directory record's recording date. It is zero when neither is
	// recorded or the one found is damaged.
	ModTime time.Time

	// Mode carries the entry's type and permissions. With a Rock Ridge PX
	// entry they are the POSIX mode it records, and HasPOSIX is set. Without
	// one, a directory is r-x and anything else r-- for everyone, which is how
	// a read-only medium with no recorded permissions is presented.
	Mode     fs.FileMode
	HasPOSIX bool
	UID, GID uint32 // from PX; zero without it

	// AccessTime, ChangeTime (attribute change) and CreateTime come from a
	// Rock Ridge TF entry and are zero when it does not record them.
	AccessTime time.Time
	ChangeTime time.Time
	CreateTime time.Time
}

// VolumeInfo is the descriptive metadata in the volume descriptors.
//
// The identifiers come from the Joliet descriptor when it is present and the
// field there is not blank, since Joliet can hold characters the primary
// descriptor cannot. The dates come from the primary descriptor and are zero
// when unset or damaged.
type VolumeInfo struct {
	Label        string
	Publisher    string
	DataPreparer string
	Application  string

	Created   time.Time
	Modified  time.Time
	Expires   time.Time
	Effective time.Time
}

// Reader reads ISO 9660 user data from a flat image file. It handles cooked
// (2048 bytes/sector) and raw (2352) images, and applies Joliet and Rock Ridge
// names when present.
//
// A Reader is safe for concurrent use when its io.ReaderAt is. Everything the
// walk and the name decoding depend on is settled when the image is opened,
// and Walk keeps its state per call. A FileReader is for one goroutine.
type Reader struct {
	ra         io.ReaderAt
	sectorSize int // as the image stores a sector
	dataOffset int // where that sector's user data starts

	// Plain ISO 9660 PVD fields
	volume       VolumeInfo
	totalSectors int64
	rootLBA      int64
	rootSize     int64
	rootTime     time.Time

	// Joliet SVD fields (zero if absent)
	joliet         VolumeInfo // identifiers only
	jolietRootLBA  int64
	jolietRootSize int64
	jolietRootTime time.Time

	// Rock Ridge state, settled at open
	rrDetected bool
	rrSkip     int // System Use skip count from SP entry

	// issues records damage found while opening the volume. Without it, a
	// supplementary descriptor that could not be read would silently lose
	// Joliet, leaving mangled 8.3 names with no explanation.
	issues []string

	// mu guards walkIssues, the damage the most recently finished Walk found.
	mu         sync.Mutex
	walkIssues []string
}

// maxVolumeDescriptors bounds the descriptor scan. The set terminator normally
// ends it, but on a damaged image that byte may never appear.
const maxVolumeDescriptors = 64

// Issues returns the damage found so far: what could not be read when the
// image was opened, and what the most recently finished Walk could not read.
// The second half is only complete once Walk has run.
func (r *Reader) Issues() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.issues)+len(r.walkIssues))
	out = append(out, r.issues...)
	return append(out, r.walkIssues...)
}

// ErrPVDUnrecovered reports that the primary volume descriptor lies in a range
// the rescue never recovered. The image cannot be identified, which is not the
// same as it not being an ISO: another rescue pass at LBA 16 would settle it.
// Reporting "no ISO 9660 descriptor found" instead would send an operator
// looking for the wrong problem.
var ErrPVDUnrecovered = errors.New("iso: the primary volume descriptor was never recovered")

// OpenReader probes the sector size of an image the caller already has open,
// such as an image read through its rescue map. The caller keeps ownership of
// ra and closes it; the Reader owns nothing.
func OpenReader(ra io.ReaderAt) (*Reader, error) {
	var probeErr error
	for _, ss := range SectorSizes {
		r := newReader(ra, ss)
		pvd, err := r.ReadSector(16)
		if err != nil {
			// Keep the first real reason. A range the rescue never recovered
			// means something different from a short image.
			if probeErr == nil || errors.Is(err, ddrescue.ErrNotRecovered) {
				probeErr = err
			}
			continue
		}
		if !isPVD(pvd) {
			continue
		}
		if err := r.init(pvd); err != nil {
			return nil, err
		}
		return r, nil
	}
	if errors.Is(probeErr, ddrescue.ErrNotRecovered) {
		return nil, fmt.Errorf("%w: %w", ErrPVDUnrecovered, probeErr)
	}
	return nil, ErrNotISO9660
}

// OpenReaderWithSectorSize is OpenReader for an image whose sector size is
// already known.
func OpenReaderWithSectorSize(ra io.ReaderAt, sectorSize int) (*Reader, error) {
	if _, err := DataOffset(sectorSize); err != nil {
		return nil, err
	}
	r := newReader(ra, sectorSize)
	pvd, err := r.ReadSector(16)
	if err != nil {
		if errors.Is(err, ddrescue.ErrNotRecovered) {
			return nil, fmt.Errorf("%w: %w", ErrPVDUnrecovered, err)
		}
		return nil, fmt.Errorf("iso: read PVD: %w", err)
	}
	if !isPVD(pvd) {
		return nil, ErrNotISO9660
	}
	if err := r.init(pvd); err != nil {
		return nil, err
	}
	return r, nil
}

// newReader assumes a sector size DataOffset accepts; every caller has already
// checked, or is probing with one of SectorSizes.
func newReader(ra io.ReaderAt, sectorSize int) *Reader {
	off, _ := DataOffset(sectorSize)
	return &Reader{ra: ra, sectorSize: sectorSize, dataOffset: off}
}

func isPVD(sec []byte) bool {
	return len(sec) >= 6 && sec[0] == 0x01 && string(sec[1:6]) == "CD001"
}

// Volume descriptor field offsets shared by the PVD and a Joliet SVD.
const (
	vdLabel        = 40
	vdPublisher    = 318
	vdDataPreparer = 446
	vdApplication  = 574
	vdIdentEnd     = 702
	vdCreated      = 813
	vdModified     = 830
	vdExpires      = 847
	vdEffective    = 864
)

// identifiers reads the four text fields of a volume descriptor, decoding them
// as UCS-2 for Joliet.
func identifiers(vd []byte, joliet bool) VolumeInfo {
	text := func(b []byte) string {
		if joliet {
			return strings.TrimRight(decodeUCS2(b), " \x00")
		}
		return strings.TrimRight(string(b), " \x00")
	}
	return VolumeInfo{
		Label:        text(vd[vdLabel:72]),
		Publisher:    text(vd[vdPublisher:vdDataPreparer]),
		DataPreparer: text(vd[vdDataPreparer:vdApplication]),
		Application:  text(vd[vdApplication:vdIdentEnd]),
	}
}

// volumeDates reads the PVD's four dates, recording a damaged one as an issue.
func (r *Reader) volumeDates(pvd []byte) {
	for _, d := range []struct {
		name string
		off  int
		dst  *time.Time
	}{
		{"creation", vdCreated, &r.volume.Created},
		{"modification", vdModified, &r.volume.Modified},
		{"expiration", vdExpires, &r.volume.Expires},
		{"effective", vdEffective, &r.volume.Effective},
	} {
		t, err := parseVolumeTime(pvd[d.off : d.off+volumeTimeSize])
		if err != nil {
			r.issues = append(r.issues, fmt.Sprintf("volume %s date is damaged: %s", d.name, err))
			continue
		}
		*d.dst = t
	}
}

// init parses the PVD and scans the supplementary VDs for Joliet.
func (r *Reader) init(pvd []byte) error {
	r.volume = identifiers(pvd, false)
	r.volumeDates(pvd)
	r.totalSectors = int64(binary.LittleEndian.Uint32(pvd[80:84]))
	rootRec := pvd[156:190]
	r.rootLBA = int64(binary.LittleEndian.Uint32(rootRec[2:6]))
	r.rootSize = int64(binary.LittleEndian.Uint32(rootRec[10:14]))
	r.rootTime = r.rootRecordTime(rootRec, "primary")

	// Scan VDs at LBA 17, 18, … until the Set Terminator (type 0xFF), bounded so
	// a missing terminator cannot run to the end of the image.
	for lba := int64(17); lba < 17+maxVolumeDescriptors; lba++ {
		vd, err := r.ReadSector(lba)
		if err != nil {
			r.issues = append(r.issues, fmt.Sprintf(
				"volume descriptor at LBA %d unreadable, Joliet names may be unavailable: %s", lba, err))
			break
		}
		if len(vd) < 190 {
			r.issues = append(r.issues, fmt.Sprintf("volume descriptor at LBA %d is short (%d bytes)", lba, len(vd)))
			break
		}
		vdType := vd[0]
		if vdType == 0xFF { // Set Terminator
			break
		}
		if vdType == 0x02 && string(vd[1:6]) == "CD001" {
			// Supplementary VD — check for Joliet escape sequences in bytes 88–120.
			esc := string(vd[88:120])
			if strings.Contains(esc, "%/@") || strings.Contains(esc, "%/C") || strings.Contains(esc, "%/E") {
				r.joliet = identifiers(vd, true)
				jolietRoot := vd[156:190]
				r.jolietRootLBA = int64(binary.LittleEndian.Uint32(jolietRoot[2:6]))
				r.jolietRootSize = int64(binary.LittleEndian.Uint32(jolietRoot[10:14]))
				r.jolietRootTime = r.rootRecordTime(jolietRoot, "Joliet")
			}
		}
	}
	r.detectRockRidge()
	return nil
}

// rootRecordTime decodes the recording date of the root directory record a
// volume descriptor embeds, recording a damaged one as an issue.
func (r *Reader) rootRecordTime(rec []byte, which string) time.Time {
	t, err := parseRecordTime(rec[18:25])
	if err != nil {
		r.issues = append(r.issues, fmt.Sprintf("the %s root directory's recording date is damaged: %s", which, err))
	}
	return t
}

// Root returns the entry for the root directory of the tree Walk reads: the
// Joliet tree when there is one and no Rock Ridge, else the primary one.
func (r *Reader) Root() DirEntry {
	e := DirEntry{Path: "/", IsDir: true, StartLBA: r.rootLBA, Size: r.rootSize, ModTime: r.rootTime}
	if r.useJoliet() {
		e.StartLBA, e.Size, e.ModTime = r.jolietRootLBA, r.jolietRootSize, r.jolietRootTime
	}
	e.Mode = entryMode(e)
	return e
}

// detectRockRidge reads the root directory's dot entry for the SP entry that
// marks Rock Ridge. It runs at open so that names are settled before any walk
// and the walk never changes state another goroutine reads.
func (r *Reader) detectRockRidge() {
	rootSec, err := r.ReadSector(r.rootLBA)
	if err != nil {
		// Without this the names would silently fall back to 8.3 with no
		// explanation.
		r.issues = append(r.issues, fmt.Sprintf(
			"the root directory sector could not be read, so Rock Ridge names are unavailable: %s", err))
		return
	}
	if len(rootSec) >= 1 {
		dotLen := int(rootSec[0])
		if dotLen >= 34 && dotLen <= len(rootSec) {
			r.rrDetected, r.rrSkip = findSP(rootSec[:dotLen])
		}
	}
}

// VolumeLabel returns the Joliet volume label if present, else the ISO 9660 one.
func (r *Reader) VolumeLabel() string {
	return r.Volume().Label
}

// Volume returns the volume descriptors' identifiers and dates.
func (r *Reader) Volume() VolumeInfo {
	v := r.volume
	prefer := func(dst *string, joliet string) {
		if joliet != "" {
			*dst = joliet
		}
	}
	prefer(&v.Label, r.joliet.Label)
	prefer(&v.Publisher, r.joliet.Publisher)
	prefer(&v.DataPreparer, r.joliet.DataPreparer)
	prefer(&v.Application, r.joliet.Application)
	return v
}

// TotalSectors returns the volume space size declared in the PVD.
func (r *Reader) TotalSectors() int64 { return r.totalSectors }

// SectorSize returns the detected physical sector size.
func (r *Reader) SectorSize() int { return r.sectorSize }

// HasJoliet reports whether the image has a Joliet SVD.
func (r *Reader) HasJoliet() bool { return r.jolietRootLBA != 0 }

// HasRockRidge reports whether the root directory carries the Rock Ridge SP
// entry.
func (r *Reader) HasRockRidge() bool { return r.rrDetected }

// ReadSector reads the 2048-byte user-data payload from lba.
//
// On a raw image the whole 2352-byte sector is read, so a hole anywhere in it
// (the sync pattern or the ECC, not just the payload) fails the read. This is
// deliberate, since an unrecovered header means the sector's identity cannot
// be checked. As a result, a raw image reports more damage than a cooked one
// of the same disc.
func (r *Reader) ReadSector(lba int64) ([]byte, error) {
	buf := make([]byte, r.sectorSize)
	n, err := r.readSectorInto(lba, buf)
	if err != nil {
		return nil, err
	}
	return buf[r.dataOffset : r.dataOffset+n], nil
}

// readSectorInto fills buf with one physical sector and reports how many
// **payload** bytes were read before any failure, so a caller can use a partly
// recovered sector.
//
// It counts payload rather than raw bytes because on a raw image the 288 bytes
// of EDC/ECC after the 2048 are not part of the file. Counting them would
// splice error-correction data into the file and make every length derived
// from the count 288 bytes too long.
func (r *Reader) readSectorInto(lba int64, buf []byte) (int, error) {
	n, err := r.ra.ReadAt(buf, lba*int64(r.sectorSize))
	if err != nil {
		return min(max(n-r.dataOffset, 0), UserDataSize), fmt.Errorf("iso: read sector %d: %w", lba, err)
	}
	return UserDataSize, nil
}

// ExtentBytes returns the image byte range holding one extent. On a raw image
// a sector is 2352 bytes on the medium but holds only 2048 bytes of the file,
// so a caller cross-referencing a rescue map must use this rather than
// multiply by 2048. A multi-extent file needs one call per extent.
func (r *Reader) ExtentBytes(x Extent) (off, length int64) {
	sectors := (x.Size + UserDataSize - 1) / UserDataSize
	if sectors <= 0 {
		sectors = 1
	}
	return x.LBA * int64(r.sectorSize), sectors * int64(r.sectorSize)
}

// OpenFile returns a streaming reader over a file's extents, in order. An
// entry with no Extents, such as one a caller built by hand, is read as the
// single extent StartLBA and Size describe.
func (r *Reader) OpenFile(e DirEntry) *FileReader {
	exts := e.Extents
	if exts == nil {
		exts = []Extent{{LBA: e.StartLBA, Size: e.Size}}
	}
	var total int64
	for _, x := range exts {
		total += x.Size
	}
	fr := &FileReader{r: r, extents: exts, remain: total, total: total,
		incomplete: e.Incomplete, sec: make([]byte, r.sectorSize)}
	fr.nextExtent()
	return fr
}

// SectorReadError reports a sector that could not be read in full. Offset is
// where the sector's data starts in the file, and N is the number of its bytes
// recovered before the failure; they are delivered rather than discarded.
type SectorReadError struct {
	LBA    int64
	Offset int64
	N      int
	Err    error
}

func (e *SectorReadError) Error() string {
	return fmt.Sprintf("iso: sector %d (file offset %d): %d bytes recovered before %s", e.LBA, e.Offset, e.N, e.Err)
}

func (e *SectorReadError) Unwrap() error { return e.Err }

// FileReader streams one file's extents.
type FileReader struct {
	r          *Reader
	extents    []Extent // the extents not yet started
	lba        int64    // the sector to read next
	extRemain  int64    // bytes of the current extent still to deliver
	off        int64    // bytes delivered, which is the file offset a gap is recorded at
	remain     int64    // bytes of the file still to deliver
	total      int64    // the file's declared length
	incomplete bool     // the file's chain of extents is cut short
	sec        []byte   // one physical sector
	left       []byte   // payload not yet delivered
	// fill, when armed, turns a sector that will not read into zeros and
	// continues. Without it the reader latches, since a caller with nowhere to
	// record a hole must not be given one.
	fill *ddrescue.GapLog
	// err is latched once a sector fails. Without it a caller that keeps
	// reading would reissue the same failing read forever, since the reader
	// does not advance past a sector it could not deliver.
	err error
}

// Fill gives the reader the log to record its holes in. Call it before the
// first Read. Only ddrescue.Fill arms a log, and it writes the map in the same
// call, so a reader cannot deliver zeros that nothing records.
func (fr *FileReader) Fill(l *ddrescue.GapLog) {
	if !l.Filling() {
		return
	}
	l.Limit(fr.total)
	fr.fill = l
}

func (fr *FileReader) Read(p []byte) (int, error) {
	for {
		if len(fr.left) > 0 {
			n := copy(p, fr.left)
			fr.left = fr.left[n:]
			fr.off += int64(n)
			return n, nil
		}
		if fr.err != nil {
			return 0, fr.err
		}
		if fr.remain <= 0 {
			if fr.incomplete {
				fr.err = ErrIncompleteFile
				return 0, fr.err
			}
			return 0, io.EOF
		}
		if fr.extRemain <= 0 {
			fr.nextExtent()
		}

		n, err := fr.r.readSectorInto(fr.lba, fr.sec)
		if err != nil {
			if !fr.fill.Filling() {
				fr.err = &SectorReadError{LBA: fr.lba, Offset: fr.off, N: n, Err: err}
				if n == 0 {
					return 0, fr.err
				}
				// Return the readable start of the sector. The latch, not the
				// position, is what stops a retrying caller from looping.
				fr.left = fr.deliverable(n)
				continue
			}
			// The payload past what was read is a hole in the file. The hole is
			// clipped to what the extent has left, so a short last sector loses
			// only the bytes it would have contributed, and a sector whose
			// payload survived (the damage was in its EDC/ECC) loses nothing.
			clear(fr.sec[fr.r.dataOffset+n : fr.r.dataOffset+UserDataSize])
			if lost := min(fr.extRemain, UserDataSize) - int64(n); lost > 0 {
				fr.fill.Add(fr.off+int64(n), lost, err)
			}
		}
		fr.lba++
		fr.left = fr.deliverable(UserDataSize)
	}
}

// deliverable returns the next run of payload bytes, clipped to what the
// extent has left, since the tail of its last sector is not part of the file.
func (fr *FileReader) deliverable(n int) []byte {
	limit := int(min(int64(n), fr.extRemain))
	fr.extRemain -= int64(limit)
	fr.remain -= int64(limit)
	return fr.sec[fr.r.dataOffset : fr.r.dataOffset+limit]
}

// nextExtent moves to the next extent that holds any bytes. The tail of an
// extent's last sector is not part of the file, so each extent starts on a
// sector of its own.
func (fr *FileReader) nextExtent() {
	for len(fr.extents) > 0 {
		x := fr.extents[0]
		fr.extents = fr.extents[1:]
		if x.Size > 0 {
			fr.lba, fr.extRemain = x.LBA, x.Size
			return
		}
	}
}

// decodeUCS2 decodes UCS-2 BE to UTF-8, trimming trailing spaces.
func decodeUCS2(b []byte) string {
	if len(b)%2 != 0 {
		b = b[:len(b)-1]
	}
	u16 := make([]uint16, len(b)/2)
	for i := range u16 {
		u16[i] = binary.BigEndian.Uint16(b[i*2:])
	}
	s := string(utf16.Decode(u16))
	return strings.TrimRight(s, " ")
}
