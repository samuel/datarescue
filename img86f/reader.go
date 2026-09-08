package img86f

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
)

// Magic is the signature an 86F image starts with.
const Magic = "86BF"

// Disk flag bits (offset 0x06, 16-bit).
const (
	diskFlagSurfaceData  uint16 = 1 << 0  //nolint:unused // surface description data follows track data
	diskFlagHoleMask     uint16 = 3 << 1  // bits 2-1: density
	diskFlagDoubleSided  uint16 = 1 << 3  // two-sided disk
	diskFlagWriteProtect uint16 = 1 << 4  //nolint:unused // write protected
	diskFlagRPMMask      uint16 = 3 << 5  // bits 6-5: RPM adjustment
	diskFlagBitcellCount uint16 = 1 << 7  // per-track bitcell count field present
	diskFlagZoned        uint16 = 1 << 8  //nolint:unused // zoned (variable RPM) disk
	diskFlagZoneTypeMask uint16 = 3 << 9  //nolint:unused // bits 10-9: zone type
	diskFlagRevEndian    uint16 = 1 << 11 // track data bytes are in reversed (big-endian) order
	diskFlagSpeedup      uint16 = 1 << 12 //nolint:unused // bitcell count is absolute total, not delta
)

// Hole type values for bits 2-1 of disk flags.
const (
	holeDD  = uint16(0 << 1) //nolint:unused // double density
	holeHD  = uint16(1 << 1) //nolint:unused // high density
	holeED  = uint16(2 << 1) //nolint:unused // extra density
	holeED2 = uint16(3 << 1) //nolint:unused // extra density + 2000 kbps
)

// RPM adjustment values for bits 6-5 of disk flags.
const (
	rpmAdj0  = uint16(0 << 5) //nolint:unused // nominal
	rpmAdj1  = uint16(1 << 5) //nolint:unused // +1%
	rpmAdj15 = uint16(2 << 5) //nolint:unused // +1.5%
	rpmAdj2  = uint16(3 << 5) //nolint:unused // +2%
)

// Zone type values for bits 10-9 of disk flags (when diskFlagZoned is set).
const (
	zonePreApple1 = uint16(0 << 9) //nolint:unused
	zonePreApple2 = uint16(1 << 9) //nolint:unused
	zoneApple     = uint16(2 << 9) //nolint:unused
	zoneC64       = uint16(3 << 9) //nolint:unused
)

// Track encoding types (bits 4-3 of track flags).
const (
	encodingFM   = 0 //nolint:unused
	encodingMFM  = 1
	encodingM2FM = 2 //nolint:unused
	encodingGCR  = 3 //nolint:unused
)

// Track bit rate values for bits 2-0 of track flags (MFM; FM uses half).
const (
	bitrate500k  = 0 //nolint:unused
	bitrate300k  = 1 //nolint:unused
	bitrate250k  = 2 //nolint:unused
	bitrate1000k = 3 //nolint:unused
	bitrate2000k = 5 //nolint:unused
)

// Track RPM values for bits 7-5 of track flags.
const (
	trackRPM300 = uint16(0 << 5) //nolint:unused
	trackRPM360 = uint16(1 << 5) //nolint:unused
)

// MFM address mark bytes.
const (
	markIDAM   = 0xFE // ID address mark
	markDAM    = 0xFB // data address mark
	markDelDAM = 0xF8 // deleted data address mark
)

// maxTracksPerSide is the fixed number of offset table entries per side; absent
// tracks have a zero offset.
const maxTracksPerSide = 256

// baseWordCounts maps hole type (bits 2-1 of disk flags) to base track word count.
// One word = 2 bytes = 16 bitcells.
var baseWordCounts = [4]int{12500, 12500, 25000, 50000}

// mfmSync is the raw 6-byte sequence for three MFM A1 sync marks with clock violation.
// A1 (0xA1) with the clock bit at the bit-3→bit-2 transition missing encodes as 0x4489.
var mfmSync = []byte{0x44, 0x89, 0x44, 0x89, 0x44, 0x89}

// fileHeader is the 8-byte file header at offset 0.
type fileHeader struct {
	Magic        [4]byte
	MinorVersion uint8
	MajorVersion uint8
	DiskFlags    uint16
}

// Sector holds a decoded MFM sector.
type Sector struct {
	Cylinder  uint8
	Head      uint8
	SectorNum uint8
	SizeCode  uint8 // data length = 128 << SizeCode bytes
	Data      []byte
	CRCError  bool
	Deleted   bool
}

// Disk is an open 86F floppy disk image.
type Disk struct {
	r            io.ReadSeeker
	closer       io.Closer // nil when the caller owns the source
	fileSize     int64
	flags        uint16
	sides        int
	trackOffsets []uint32
}

// fileHeaderSize is the 8-byte file header preceding the track offset table.
const fileHeaderSize = 8

// Open opens an 86F disk image file for reading.
func Open(path string) (*Disk, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close() //nolint:errcheck // closing on the error path, returning the stat error
		return nil, fmt.Errorf("stat image: %w", err)
	}
	d, err := OpenReader(f, fi.Size())
	if err != nil {
		f.Close() //nolint:errcheck // closing on error path, return parse error
		return nil, err
	}
	d.closer = f
	return d, nil
}

// OpenReader decodes an image the caller already has open. size is the image's
// length and bounds every offset the track table declares. The caller keeps
// ownership of r, so Close is a no-op. This lets the fuzzer work over bytes
// rather than a temporary file per iteration.
func OpenReader(r io.ReadSeeker, size int64) (*Disk, error) {
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	d := &Disk{r: r, fileSize: size}
	if err := d.readHeader(); err != nil {
		return nil, err
	}
	return d, nil
}

// Close releases the image when the Disk opened it, and does nothing when the
// caller supplied one.
func (d *Disk) Close() error {
	if d.closer == nil {
		return nil
	}
	return d.closer.Close()
}

func (d *Disk) readHeader() error {
	var hdr fileHeader
	if err := binary.Read(d.r, binary.LittleEndian, &hdr); err != nil {
		return fmt.Errorf("reading header: %w", err)
	}
	if string(hdr.Magic[:]) != Magic {
		return fmt.Errorf("invalid magic: %q", hdr.Magic)
	}
	d.flags = hdr.DiskFlags
	d.sides = 1
	if d.flags&diskFlagDoubleSided != 0 {
		d.sides = 2
	}
	d.trackOffsets = make([]uint32, maxTracksPerSide*d.sides)
	return binary.Read(d.r, binary.LittleEndian, d.trackOffsets)
}

// trackDataStart returns the first byte after the offset table, the earliest
// offset a track can legally have.
func (d *Disk) trackDataStart() int64 {
	return fileHeaderSize + int64(len(d.trackOffsets))*4
}

// Geometry returns the number of formatted tracks and sides.
func (d *Disk) Geometry() (tracks, sides int) {
	for i, v := range slices.Backward(d.trackOffsets) {
		if v != 0 {
			return i/d.sides + 1, d.sides
		}
	}
	return 0, d.sides
}

// ReadTrack decodes all sectors from the given track and side.
// Returns nil, nil for absent (unformatted) tracks.
func (d *Disk) ReadTrack(track, side int) ([]Sector, error) {
	raw, tflags, err := d.readRawTrack(track, side)
	if err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, nil
	}
	enc := (tflags >> 3) & 3
	if enc != encodingMFM {
		return nil, fmt.Errorf("track %d/%d: unsupported encoding %d", track, side, enc)
	}
	if d.flags&diskFlagRevEndian != 0 {
		raw = swapPairBytes(raw)
	}
	return decodeMFMTrack(raw), nil
}

// ReadSector reads a specific sector by CHS address.
// Returns an error wrapping ErrCRC if the sector was found but has a CRC error.
func (d *Disk) ReadSector(cyl, head, sectorNum int) ([]byte, error) {
	sectors, err := d.ReadTrack(cyl, head)
	if err != nil {
		return nil, err
	}
	for _, s := range sectors {
		if int(s.Cylinder) == cyl && int(s.Head) == head && int(s.SectorNum) == sectorNum {
			if s.CRCError {
				return s.Data, fmt.Errorf("cyl=%d head=%d sector=%d: %w", cyl, head, sectorNum, ErrCRC)
			}
			return s.Data, nil
		}
	}
	return nil, fmt.Errorf("sector not found: cyl=%d head=%d sector=%d", cyl, head, sectorNum)
}

// ErrCRC is returned when a sector is found but its CRC does not match.
var ErrCRC = errors.New("CRC error")

func (d *Disk) readRawTrack(track, side int) (data []byte, tflags uint16, err error) {
	idx := track*d.sides + side
	if idx >= len(d.trackOffsets) {
		return nil, 0, fmt.Errorf("track %d side %d out of range", track, side)
	}
	off := int64(d.trackOffsets[idx])
	if off == 0 {
		return nil, 0, nil
	}
	// The offset table comes straight from the image; a track that starts inside
	// the table or past the end of the file cannot be read.
	if off < d.trackDataStart() || off >= d.fileSize {
		return nil, 0, fmt.Errorf("track %d/%d: offset %d outside image of %d bytes", track, side, off, d.fileSize)
	}
	if _, err := d.r.Seek(off, io.SeekStart); err != nil {
		return nil, 0, err
	}
	if err := binary.Read(d.r, binary.LittleEndian, &tflags); err != nil {
		return nil, 0, fmt.Errorf("track %d/%d: reading flags: %w", track, side, err)
	}
	hdrSize := int64(6) // flags(2) + index_hole(4)
	var minDataLen int64
	if d.flags&diskFlagBitcellCount != 0 {
		var bitcellCount int32
		if err := binary.Read(d.r, binary.LittleEndian, &bitcellCount); err != nil {
			return nil, 0, fmt.Errorf("track %d/%d: reading bitcell count: %w", track, side, err)
		}
		if bitcellCount < 0 {
			return nil, 0, fmt.Errorf("track %d/%d: negative bitcell count %d", track, side, bitcellCount)
		}
		hdrSize += 4
		// The declared bitcell count is the only length figure that does not come
		// from the offset table, so use it as a floor on the data to read.
		minDataLen = (int64(bitcellCount) + 7) / 8
	}
	var indexHole uint32
	if err := binary.Read(d.r, binary.LittleEndian, &indexHole); err != nil {
		return nil, 0, fmt.Errorf("track %d/%d: reading index hole: %w", track, side, err)
	}

	dataStart := off + hdrSize
	trackDataLen := int64(d.defaultTrackDataLen())
	// The file layout gives a more accurate length than the flags do, but both
	// offsets are image-supplied: only a strictly later next track is usable,
	// otherwise the subtraction goes negative or spans most of the file.
	for nextIdx := idx + 1; nextIdx < len(d.trackOffsets); nextIdx++ {
		nextOff := int64(d.trackOffsets[nextIdx])
		if nextOff == 0 {
			continue
		}
		if nextOff > dataStart && nextOff <= d.fileSize {
			trackDataLen = nextOff - dataStart
		}
		break
	}
	trackDataLen = max(trackDataLen, minDataLen)
	// Never allocate or read past the end of the image.
	trackDataLen = min(trackDataLen, d.fileSize-dataStart)
	if trackDataLen <= 0 {
		return nil, 0, fmt.Errorf("track %d/%d: no track data at offset %d", track, side, off)
	}

	buf := make([]byte, trackDataLen)
	if _, err := io.ReadFull(d.r, buf); err != nil {
		return nil, 0, fmt.Errorf("track %d/%d: reading data: %w", track, side, err)
	}
	return buf, tflags, nil
}

// defaultTrackDataLen returns the nominal track data length in bytes based on disk flags.
func (d *Disk) defaultTrackDataLen() int {
	holeType := int((d.flags & diskFlagHoleMask) >> 1)
	rpmAdj := int((d.flags & diskFlagRPMMask) >> 5)
	base := baseWordCounts[holeType]
	var adj int
	switch rpmAdj {
	case 1:
		adj = base / 100
	case 2:
		adj = base * 3 / 200
	case 3:
		adj = base / 50
	}
	return (base + adj) * 2
}

// swapPairBytes reverses the byte order within each 16-bit word of the stream.
func swapPairBytes(data []byte) []byte {
	out := make([]byte, len(data))
	for i := 0; i+1 < len(data); i += 2 {
		out[i], out[i+1] = data[i+1], data[i]
	}
	if len(data)%2 == 1 {
		out[len(data)-1] = data[len(data)-1]
	}
	return out
}

// crc16 computes CRC-16/CCITT (polynomial 0x1021, initial value 0xFFFF).
func crc16(data []byte) uint16 {
	crc := uint16(0xFFFF)
	for _, b := range data {
		crc ^= uint16(b) << 8
		for range 8 {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

// decodeMFMByte extracts one data byte from two consecutive MFM-encoded bytes.
// Each data bit occupies the lower of its two encoded bitcell positions
// (bits 6, 4, 2, 0 of each encoded byte).
func decodeMFMByte(hi, lo byte) byte {
	return ((hi>>6)&1)<<7 | ((hi>>4)&1)<<6 | ((hi>>2)&1)<<5 | ((hi>>0)&1)<<4 |
		((lo>>6)&1)<<3 | ((lo>>4)&1)<<2 | ((lo>>2)&1)<<1 | (lo & 1)
}

// maxSizeCode is the largest sector size code the format defines. The length is
// 128 << code, so 7 is a 16 KiB sector and anything above it is not a valid
// length: 128<<56 overflows int to a negative value, and the IDAM's own CRC
// cannot catch that because it is checked after the shift.
const maxSizeCode = 7

// decodeMFMSlice decodes n data bytes from the MFM bitstream at offset,
// reporting false if the stream is too short.
func decodeMFMSlice(stream []byte, offset, n int) ([]byte, bool) {
	// Compared by division rather than by multiplying n: n is derived from a
	// disk-supplied size code, and n*2 can overflow into a value that passes
	// the bound and then panics in make.
	if offset < 0 || n < 0 || offset > len(stream) || n > (len(stream)-offset)/2 {
		return nil, false
	}
	out := make([]byte, n)
	for i := range n {
		out[i] = decodeMFMByte(stream[offset+i*2], stream[offset+i*2+1])
	}
	return out, true
}

// decodeMFMTrack scans a raw MFM bitstream and returns all decoded sectors.
func decodeMFMTrack(stream []byte) []Sector {
	var sectors []Sector
	var lastIDAM *Sector
	var pos int

	for pos <= len(stream)-len(mfmSync) {
		idx := bytes.Index(stream[pos:], mfmSync)
		if idx < 0 {
			break
		}
		syncPos := pos + idx

		// Decode A1 A1 A1 + mark byte (4 decoded bytes = 8 encoded bytes).
		hdr, ok := decodeMFMSlice(stream, syncPos, 4)
		if !ok {
			pos = syncPos + 2
			continue
		}
		mark := hdr[3]

		switch mark {
		case markIDAM:
			// IDAM: A1 A1 A1 FE cyl head sec size_code crc_hi crc_lo = 10 decoded bytes.
			idam, ok := decodeMFMSlice(stream, syncPos, 10)
			if !ok {
				pos = syncPos + 2
				continue
			}
			if idam[7] > maxSizeCode {
				// No usable length, so no sector header: keep scanning rather
				// than carry over a size the following data mark would be
				// decoded with.
				lastIDAM = nil
				pos = syncPos + 2
				continue
			}
			crcCalc := crc16(idam[:8])
			crcStored := uint16(idam[8])<<8 | uint16(idam[9])
			lastIDAM = &Sector{
				Cylinder:  idam[4],
				Head:      idam[5],
				SectorNum: idam[6],
				SizeCode:  idam[7],
				CRCError:  crcCalc != crcStored,
			}
			pos = syncPos + 20 // 10 decoded × 2 encoded bytes each

		case markDAM, markDelDAM:
			if lastIDAM == nil {
				pos = syncPos + 2
				continue
			}
			dataSize := 128 << lastIDAM.SizeCode
			// A1 A1 A1 mark + data + crc_hi crc_lo = (4 + dataSize + 2) decoded bytes.
			total := 4 + dataSize + 2
			dam, ok := decodeMFMSlice(stream, syncPos, total)
			if !ok {
				pos = syncPos + 2
				continue
			}
			crcCalc := crc16(dam[:4+dataSize])
			crcStored := uint16(dam[4+dataSize])<<8 | uint16(dam[4+dataSize+1])
			s := *lastIDAM
			s.Data = dam[4 : 4+dataSize]
			s.CRCError = s.CRCError || crcCalc != crcStored
			s.Deleted = mark == markDelDAM
			sectors = append(sectors, s)
			lastIDAM = nil
			pos = syncPos + total*2

		default:
			pos = syncPos + 2
		}
	}
	return sectors
}
