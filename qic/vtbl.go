package qic

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"samuelks.com/datarescue/internal/compressed"
)

const (
	// SegmentSize is the stride from one segment to the next. Everything past
	// the header region sits on this grid, whatever a segment header claims
	// its payload length is.
	SegmentSize = 29696

	// VTBLSize is one volume table record, and also one MDID record.
	VTBLSize = 128

	segHeadSize = 10
	rawSegFlag  = 0x8000

	// segPayload is what one segment of a compressed archive can hold once its
	// header is deducted.
	segPayload = SegmentSize - segHeadSize

	// vtblDateFudge is added to the volume's creation time on the medium: two
	// years exactly, with nothing in the format to account for it. It is
	// applied because only then do the timestamps agree with the names the
	// program gave the archives.
	vtblDateFudge = 63072000

	// maxVolumes bounds a header region before its size is used for anything.
	// Windows 98 and ME write one record per source drive.
	maxVolumes = 64

	mdidTerminator = 0xB0
)

// ErrNotQIC is returned when the file does not begin with a volume table, so a
// caller probing a file can tell that from an I/O failure.
var ErrNotQIC = errors.New("qic: not a Windows 9x MS Backup archive")

var (
	vtblTag = []byte("VTBL")
	mdidTag = []byte("MDID")
)

// Variant is which of the two incompatible dialects an archive is written in.
// They differ in where the regions start, in how many filler bytes surround a
// record's data, and in whether the root of the tree carries a name.
type Variant uint8

const (
	// VariantWin95 has one volume table record and nothing else in the header.
	VariantWin95 Variant = iota + 1
	// VariantWin98 covers Windows 98 and ME, which follow the volume table
	// records with an MDID record.
	VariantWin98
)

func (v Variant) String() string {
	switch v {
	case VariantWin95:
		return "Windows 95"
	case VariantWin98:
		return "Windows 98/ME"
	}
	return "unknown"
}

// trailerGroups is how many signature-and-word pairs follow a record: three
// after a directory and two after a file's data on Windows 95, one and none
// afterwards. Getting these wrong drifts every data offset in the archive,
// which the anchor check in scan.go catches.
func (v Variant) dirTrailers() int64 {
	if v == VariantWin98 {
		return 1
	}
	return 3
}

func (v Variant) fileTrailers() int64 {
	if v == VariantWin98 {
		return 0
	}
	return 2
}

// Volume is one decoded volume table record.
type Volume struct {
	Index       int    // 1-based position in the header region
	Label       string // the description the operator typed
	SourceLabel string // the source drive's volume label; blank on Windows 95
	Drive       string // "C:" where it can be established, else ""
	Created     time.Time
	Flags       uint8
	Sequence    int
	Revision    [2]uint16
	Compressed  bool
	Algorithm   uint8 // 1 is QIC-122; nothing else has been seen
	DataSize    int64
	DirSize     int64
	OSType      uint8
	OSVersion   [2]uint8
	LogicalDev  uint8
	Password    [8]byte
	QFAStart    uint32
	QFAEnd      uint32
	Variant     Variant

	// Segments is the first and last QFA block from the field the format
	// documents as a segment count. It is two 16-bit numbers, and their
	// difference plus one is the archive's real segment count, so it serves as
	// a cross-check and is never used to size anything.
	Segments [2]uint16
}

// Volume flag bits, from QIC-113. Bit 5 claims the directory follows the data
// section, which is always true here and which MS Backup never sets.
const (
	volVendorSpecific = 1 << 0
	volSpansMedia     = 1 << 1
	volNoVerify       = 1 << 2
	volSegmentSpan    = 1 << 4
	volDirLast        = 1 << 5
)

// FlagNames renders the volume's flag bits for a report.
func (v Volume) FlagNames() []string {
	var out []string
	for _, f := range []struct {
		bit  uint8
		name string
	}{
		{volVendorSpecific, "vendor specific"},
		{volSpansMedia, "spans multiple cartridges"},
		{volNoVerify, "written without verification"},
		{volSegmentSpan, "compressed segments span"},
		{volDirLast, "directory follows data"},
	} {
		if v.Flags&f.bit != 0 {
			out = append(out, f.name)
		}
	}
	return out
}

// OSTypeName returns the name of the operating system that wrote the volume.
func (v Volume) OSTypeName() string {
	names := []string{
		"Extended", "DOS Basic", "Unix", "OS/2", "Novell Netware",
		"Windows NT", "DOS Extended", "Windows 95",
	}
	if int(v.OSType) < len(names) {
		return names[v.OSType]
	}
	return fmt.Sprintf("unknown (%d)", v.OSType)
}

// MediumID is the MDID record Windows 98 and ME write after the volume table.
// It is a run of two-letter keys with ASCII values, terminated by 0xB0 rather
// than by a NUL.
type MediumID struct {
	MediumID string // 19 decimal digits identifying the medium
	Version  string
	Checksum string
	Format   string
	Label    string // free text the operator supplied
	Created  time.Time
	Keys     map[string]string // every key, including ones not named above
}

// parseVTBL decodes one volume table record. It takes bytes rather than a
// reader so the fuzzer needs no file.
func parseVTBL(b []byte) (Volume, error) {
	if len(b) < VTBLSize {
		return Volume{}, fmt.Errorf("%w: short volume table record", ErrNotQIC)
	}
	if !bytes.Equal(b[0:4], vtblTag) {
		return Volume{}, fmt.Errorf("%w: signature %q", ErrNotQIC, b[0:4])
	}

	v := Volume{
		Label:       strings.TrimRight(string(b[8:52]), " \x00"),
		Created:     stamp(binary.LittleEndian.Uint32(b[52:56]), vtblDateFudge),
		Flags:       b[56],
		Sequence:    int(b[57]),
		Revision:    [2]uint16{binary.LittleEndian.Uint16(b[58:60]), binary.LittleEndian.Uint16(b[60:62])},
		QFAStart:    binary.LittleEndian.Uint32(b[76:80]),
		QFAEnd:      binary.LittleEndian.Uint32(b[80:84]),
		DirSize:     int64(binary.LittleEndian.Uint32(b[92:96])),
		DataSize:    int64(binary.LittleEndian.Uint32(b[96:100])),
		OSVersion:   [2]uint8{b[104], b[105]},
		SourceLabel: strings.TrimRight(string(b[106:122]), " \x00"),
		LogicalDev:  b[122],
		OSType:      b[125],
		Segments:    [2]uint16{binary.LittleEndian.Uint16(b[4:6]), binary.LittleEndian.Uint16(b[6:8])},
	}
	copy(v.Password[:], b[84:92])

	// dataSz is a 64-bit quantity, and an archive whose high word is set is
	// past what anything here has seen.
	if hi := binary.LittleEndian.Uint32(b[100:104]); hi != 0 {
		return v, fmt.Errorf("qic: data region of at least %d GiB is not supported", int64(hi)*4)
	}

	comp := b[124]
	v.Compressed = comp&0x80 != 0
	v.Algorithm = comp & 0x3F

	// The documented drive letter is only believable when the record also
	// carries the source label it belongs to. Windows 95 leaves the label
	// empty and the device byte holding something else entirely, and the drive
	// comes from the root catalog record's name instead.
	if v.SourceLabel != "" && v.LogicalDev >= 1 && v.LogicalDev <= 26 {
		v.Drive = string(rune('A'+v.LogicalDev-1)) + ":"
	}
	return v, nil
}

// stamp converts an on-disk timestamp, which counts seconds from 1970, and
// rejects one too far out to have been written by the software.
func stamp(secs uint32, fudge int64) time.Time {
	if secs == 0 || secs == 0xFFFFFFFF {
		return time.Time{}
	}
	t := time.Unix(int64(secs)-fudge, 0).UTC()
	if t.Year() < 1980 || t.Year() > 2100 {
		return time.Time{}
	}
	return t
}

// readHeaderRegion reads the run of volume table records and the MDID that may
// follow them. The MDID's presence is the only thing distinguishing the two
// variants, so when the record where it would sit cannot be read the variant
// is left zero for the caller to settle another way.
func readHeaderRegion(r io.ReaderAt, size int64, diag diagFunc) (vols []Volume, mdid *MediumID, err error) {
	buf := make([]byte, VTBLSize)
	for len(vols) < maxVolumes {
		off := int64(len(vols)) * VTBLSize
		if off+VTBLSize > size {
			break
		}
		if _, err := r.ReadAt(buf, off); err != nil {
			if len(vols) == 0 {
				return nil, nil, fmt.Errorf("qic: reading the volume table: %w", err)
			}
			diag(DiagVTBLDamaged, "the volume table record at 0x%x could not be read: %v", off, err)
			break
		}
		if !bytes.Equal(buf[0:4], vtblTag) {
			break
		}
		v, err := parseVTBL(buf)
		if err != nil {
			diag(DiagVTBLDamaged, "the volume table record at 0x%x could not be decoded: %v", off, err)
			break
		}
		v.Index = len(vols) + 1
		vols = append(vols, v)
	}
	if len(vols) == 0 {
		return nil, nil, fmt.Errorf("%w: no volume table record at the start of the file%s", ErrNotQIC, compressed.Hint(buf))
	}

	// The MDID, where it exists, sits in the record slot right after the last
	// volume table record. Its presence is the only thing telling the two
	// dialects apart, so it is looked for and never assumed.
	if mdidOff := int64(len(vols)) * VTBLSize; mdidOff+VTBLSize <= size {
		if _, err := r.ReadAt(buf, mdidOff); err == nil && bytes.Equal(buf[0:4], mdidTag) {
			id := parseMDID(buf)
			mdid = &id
		}
	}
	return vols, mdid, nil
}

// mdidKeys are the keys an MDID record is known to carry. The medium
// identifier spells its key out where the rest abbreviate to two letters, so
// the list is matched longest first rather than assuming a fixed key width.
var mdidKeys = []string{"MediumID", "VR", "CS", "FM", "UL", "DT"}

// parseMDID decodes the key-and-value run in an MDID record. Values run to the
// next 0xB0 byte; the record ends with a group carrying no key at all, which
// is skipped rather than guessed at.
func parseMDID(b []byte) MediumID {
	id := MediumID{Keys: map[string]string{}}
	for group := range bytes.SplitSeq(b[4:], []byte{mdidTerminator}) {
		key, value, ok := splitMDIDGroup(string(group))
		if !ok || !isPrintable(value) {
			continue
		}
		id.Keys[key] = value
	}
	id.MediumID = id.Keys["MediumID"]
	id.Version = id.Keys["VR"]
	id.Checksum = id.Keys["CS"]
	id.Format = id.Keys["FM"]
	id.Label = id.Keys["UL"]
	if dt, err := parseHexTime(id.Keys["DT"]); err == nil {
		id.Created = dt
	}
	return id
}

func splitMDIDGroup(group string) (key, value string, ok bool) {
	for _, k := range mdidKeys {
		if rest, found := strings.CutPrefix(group, k); found {
			return k, strings.TrimRight(rest, "\x00 "), true
		}
	}
	return "", "", false
}

func isPrintable(s string) bool {
	for i := range len(s) {
		if s[i] < 0x20 || s[i] > 0x7E {
			return false
		}
	}
	return true
}

func parseHexTime(s string) (time.Time, error) {
	var secs uint32
	if _, err := fmt.Sscanf(s, "%8x", &secs); err != nil {
		return time.Time{}, err
	}
	t := stamp(secs, 0)
	if t.IsZero() {
		return time.Time{}, errors.New("qic: implausible MDID timestamp")
	}
	return t, nil
}
