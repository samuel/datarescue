package img86f

import (
	"encoding/binary"
	"fmt"
	"io"
)

// trackKey identifies a decoded track in the cache.
type trackKey struct{ track, side int }

// linearReader presents a Disk as a flat LBA byte stream, mapping
// sector = (LBA % sectorsPerTrack) + 1, head = (LBA / sectorsPerTrack) % heads,
// cyl = LBA / (sectorsPerTrack * heads).
type linearReader struct {
	disk            *Disk
	sectorsPerTrack int
	heads           int
	sectorSize      int
	pos             int64
	size            int64
	cache           map[trackKey][]Sector
}

// NewLinearReader wraps d as an io.ReadSeeker using the given disk geometry.
// Decoded tracks are cached so sequential reads do not re-decode MFM data.
//
// The geometry has to be positive: every LBA is divided by
// sectorsPerTrack*heads, so a zero there would panic rather than return an
// error.
func NewLinearReader(d *Disk, sectorsPerTrack, heads, sectorSize int) (io.ReadSeeker, error) {
	if sectorsPerTrack <= 0 || heads <= 0 || sectorSize <= 0 {
		return nil, fmt.Errorf("img86f: geometry %d:%d:%d must be positive in every term",
			sectorsPerTrack, heads, sectorSize)
	}
	tracks, _ := d.Geometry()
	return &linearReader{
		disk:            d,
		sectorsPerTrack: sectorsPerTrack,
		heads:           heads,
		sectorSize:      sectorSize,
		size:            int64(tracks * heads * sectorsPerTrack * sectorSize),
		cache:           make(map[trackKey][]Sector),
	}, nil
}

// DetectGeometry reads the FAT BIOS Parameter Block from the boot sector and
// returns the disk geometry encoded there.
func DetectGeometry(d *Disk) (sectorsPerTrack, heads, sectorSize int, err error) {
	data, err := d.ReadSector(0, 0, 1)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("DetectGeometry: %w", err)
	}
	if len(data) < 28 {
		return 0, 0, 0, fmt.Errorf("DetectGeometry: boot sector too short (%d bytes)", len(data))
	}
	bps := int(binary.LittleEndian.Uint16(data[11:13]))
	spt := int(binary.LittleEndian.Uint16(data[24:26]))
	nhe := int(binary.LittleEndian.Uint16(data[26:28]))
	if bps == 0 || spt == 0 || nhe == 0 {
		return 0, 0, 0, fmt.Errorf("DetectGeometry: invalid BPB (bps=%d spt=%d heads=%d)", bps, spt, nhe)
	}
	return spt, nhe, bps, nil
}

func (r *linearReader) lbaToCHS(lba int) (cyl, head, sec int) {
	return lba / (r.sectorsPerTrack * r.heads),
		(lba / r.sectorsPerTrack) % r.heads,
		lba%r.sectorsPerTrack + 1
}

func (r *linearReader) cachedTrack(cyl, side int) ([]Sector, error) {
	k := trackKey{cyl, side}
	if s, ok := r.cache[k]; ok {
		return s, nil
	}
	s, err := r.disk.ReadTrack(cyl, side)
	if err != nil {
		return nil, err
	}
	r.cache[k] = s
	return s, nil
}

func (r *linearReader) findSector(cyl, head, secNum int) (*Sector, error) {
	sectors, err := r.cachedTrack(cyl, head)
	if err != nil {
		return nil, err
	}
	for i := range sectors {
		s := &sectors[i]
		if int(s.Cylinder) == cyl && int(s.Head) == head && int(s.SectorNum) == secNum {
			return s, nil
		}
	}
	return nil, fmt.Errorf("sector not found: cyl=%d head=%d sec=%d", cyl, head, secNum)
}

func (r *linearReader) Read(p []byte) (int, error) {
	if r.pos >= r.size {
		return 0, io.EOF
	}
	var total int
	for len(p) > 0 && r.pos < r.size {
		lba := int(r.pos / int64(r.sectorSize))
		secOff := int(r.pos % int64(r.sectorSize))
		cyl, head, secNum := r.lbaToCHS(lba)

		s, err := r.findSector(cyl, head, secNum)
		if err != nil {
			return total, err
		}
		// A sector's length comes from its own IDAM size code, which need not
		// agree with the geometry the caller gave. A mismatch is a read error
		// for that sector; assuming they agree would copy past the end of the
		// decoded data.
		if len(s.Data) != r.sectorSize {
			return total, fmt.Errorf("cyl=%d head=%d sec=%d: sector holds %d bytes, not the %d the geometry declares",
				cyl, head, secNum, len(s.Data), r.sectorSize)
		}

		n := min(r.sectorSize-secOff, len(p))
		if remaining := r.size - r.pos; int64(n) > remaining {
			n = int(remaining)
		}

		copy(p[:n], s.Data[secOff:secOff+n])
		p = p[n:]
		r.pos += int64(n)
		total += n

		if s.CRCError {
			return total, fmt.Errorf("cyl=%d head=%d sec=%d: %w", cyl, head, secNum, ErrCRC)
		}
	}
	return total, nil
}

func (r *linearReader) Seek(offset int64, whence int) (int64, error) {
	var newPos int64
	switch whence {
	case io.SeekStart:
		newPos = offset
	case io.SeekCurrent:
		newPos = r.pos + offset
	case io.SeekEnd:
		newPos = r.size + offset
	default:
		return 0, fmt.Errorf("Seek: invalid whence %d", whence)
	}
	if newPos < 0 {
		return 0, fmt.Errorf("Seek: negative position %d", newPos)
	}
	r.pos = newPos
	return r.pos, nil
}
