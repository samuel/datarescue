package iso

import (
	"fmt"
	"io"
)

// Sector geometry. A CD data sector occupies 2352 bytes on the medium: a
// 16-byte sync pattern and header, the user data, and an EDC/ECC trailer that
// belongs to the medium and not to the file. A cooked image holds only the user
// data, so an image's sector size determines where each sector's payload
// starts.
const (
	// UserDataSize is a data sector's payload, and so also the sector size of a
	// cooked image.
	UserDataSize = 2048
	// RawSectorSize is a sector as the medium carries it.
	RawSectorSize = 2352
	// rawDataOffset is the size of the sync pattern and header before the
	// payload.
	rawDataOffset = 16
)

// SectorSizes are the image shapes this package reads, in the order a probe
// should try them.
var SectorSizes = []int{UserDataSize, RawSectorSize}

// DataOffset returns where a sector's user data starts in an image whose
// sectors are sectorSize bytes. This is the only place that decision is made:
// a wrong answer shifts every byte of every file by 16, and nothing about the
// result looks wrong.
func DataOffset(sectorSize int) (int, error) {
	switch sectorSize {
	case UserDataSize:
		return 0, nil
	case RawSectorSize:
		return rawDataOffset, nil
	}
	return 0, fmt.Errorf("iso: unsupported sector size %d", sectorSize)
}

// ReadUserData reads sector lba out of an image whose sectors are sectorSize
// bytes and returns its user data. It takes no Reader because a caller probing
// an image for a filesystem needs a sector before it knows the image holds an
// ISO 9660 volume at all.
func ReadUserData(ra io.ReaderAt, lba int64, sectorSize int) ([]byte, error) {
	off, err := DataOffset(sectorSize)
	if err != nil {
		return nil, err
	}
	buf := make([]byte, sectorSize)
	if _, err := ra.ReadAt(buf, lba*int64(sectorSize)); err != nil {
		return nil, fmt.Errorf("iso: read sector %d: %w", lba, err)
	}
	return buf[off : off+UserDataSize], nil
}
