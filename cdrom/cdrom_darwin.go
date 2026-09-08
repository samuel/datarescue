//go:build darwin

package cdrom

import (
	"errors"
	"fmt"
	"math"
	"os"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// macOS IOKit ioctl numbers from IOCDMediaBSDClient.h.
// Computed via _IOWR('d', n, sizeof(struct)):
//
//	dk_cd_read_t is 32 bytes     → _IOWR('d', 96,  32) = 0xC0206460
//	dk_cd_read_toc_t is 24 bytes → _IOWR('d', 100, 24) = 0xC0186464
const (
	ioctlCDRead    = 0xC0206460
	ioctlCDReadTOC = 0xC0186464
)

// Generic block-device ioctl numbers from sys/disk.h, which every disk answers
// whatever the medium:
//
//	DKIOCGETBLOCKSIZE  = _IOR('d', 24, uint32_t) = 0x40046418
//	DKIOCGETBLOCKCOUNT = _IOR('d', 25, uint64_t) = 0x40086419
const (
	ioctlGetBlockSize  = 0x40046418
	ioctlGetBlockCount = 0x40086419
)

// CDSectorArea flags (from IOCDTypes.h)
const (
	sectorAreaSync      = 0x80
	sectorAreaHeader    = 0x20
	sectorAreaUser      = 0x10
	sectorAreaAuxiliary = 0x08
	sectorAreaRawData   = sectorAreaSync | sectorAreaHeader | sectorAreaUser | sectorAreaAuxiliary
)

// Sector type constants from IOCDTypes.h.
const (
	sectorTypeCDDA  = 0x01
	sectorTypeMode1 = 0x02
)

// dkCDRead mirrors dk_cd_read_t (32 bytes on 64-bit).
type dkCDRead struct {
	Offset       uint64
	SectorArea   uint8
	SectorType   uint8
	_            [10]byte
	BufferLength uint32
	Buffer       uintptr
}

// dkCDReadTOC mirrors dk_cd_read_toc_t (24 bytes on 64-bit).
type dkCDReadTOC struct {
	Format       uint8
	FormatAsTime uint8
	_            [5]byte
	Session      uint8
	_            [6]byte
	BufferLength uint16
	Buffer       uintptr
}

func ioctl(fd uintptr, req uintptr, arg uintptr) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, req, arg)
	if errno != 0 {
		return errno
	}
	return nil
}

type darwinDevice struct {
	f       *os.File
	retries int
}

func Open(path string) (Device, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("cdrom: open %s: %w", path, err)
	}
	return &darwinDevice{f: f, retries: 3}, nil
}

func (d *darwinDevice) Close() error { return d.f.Close() }

func (d *darwinDevice) TOC() (TOC, error) {
	buf := make([]byte, 2048)

	req := dkCDReadTOC{
		Format: 0x02, // full TOC with session numbers per descriptor
		//nolint:gosec // buf is a fixed 2048-byte buffer allocated three lines above
		BufferLength: uint16(len(buf)),
		Buffer:       uintptr(unsafe.Pointer(&buf[0])), //nolint:gosec // audited: the ioctl needs the address, and every buffer reached through it is held live across the call with runtime.KeepAlive
	}
	err := ioctl(d.f.Fd(), ioctlCDReadTOC, uintptr(unsafe.Pointer(&req))) //nolint:gosec // audited: the ioctl needs the address, and every buffer reached through it is held live across the call with runtime.KeepAlive
	// req holds the buffer pointer as a uintptr, so nothing else references buf
	// while the kernel writes to it; keep it alive until the ioctl returns.
	runtime.KeepAlive(buf)
	if err != nil {
		// macOS attaches the CD ioctls to CD media only: a DVD's device node is
		// an IODVDMedia, which rejects this call -- and DKIOCCDREAD with it --
		// as ENOTTY. There is no table of contents to read there, so describe
		// the medium from the block geometry every disk reports instead. Any
		// other error is a real failure of a call the device does support.
		if errors.Is(err, unix.ENOTTY) {
			return d.blockDeviceTOC()
		}
		return TOC{}, fmt.Errorf("cdrom: DKIOCCDREADTOC: %w", err)
	}

	descs, err := parseFullTOC(buf)
	if err != nil {
		return TOC{}, err
	}
	return tocFromFullTOC(descs), nil
}

// blockDeviceTOC builds a TOC for a medium with no CD table of contents, using
// the block geometry the device reports. It identifies no tracks or sessions,
// because a DVD has neither; the rip loop only needs to know where the data
// starts and ends.
func (d *darwinDevice) blockDeviceTOC() (TOC, error) {
	var blockSize uint32
	if err := ioctl(d.f.Fd(), ioctlGetBlockSize, uintptr(unsafe.Pointer(&blockSize))); err != nil { //nolint:gosec // audited: the ioctl writes one uint32 through the address, and the variable outlives the call
		return TOC{}, fmt.Errorf("cdrom: DKIOCGETBLOCKSIZE: %w", err)
	}
	var blockCount uint64
	if err := ioctl(d.f.Fd(), ioctlGetBlockCount, uintptr(unsafe.Pointer(&blockCount))); err != nil { //nolint:gosec // audited: the ioctl writes one uint64 through the address, and the variable outlives the call
		return TOC{}, fmt.Errorf("cdrom: DKIOCGETBLOCKCOUNT: %w", err)
	}
	return tocFromBlockGeometry(blockSize, blockCount)
}

// tocFromBlockGeometry turns a block size and count into a single-data-track
// TOC covering the whole medium. The lead-out is in 2048-byte sectors, the
// unit every LBA in this package is counted in, and truncating the division
// rather than rounding up keeps it from claiming a sector the medium does not
// hold.
func tocFromBlockGeometry(blockSize uint32, blockCount uint64) (TOC, error) {
	if blockSize == 0 || blockCount == 0 {
		return TOC{}, fmt.Errorf("cdrom: block geometry is %d blocks of %d bytes: no medium, or the device reports none",
			blockCount, blockSize)
	}
	if blockCount > math.MaxInt64/uint64(blockSize) {
		return TOC{}, fmt.Errorf("cdrom: block geometry %d blocks of %d bytes exceeds an int64 byte count",
			blockCount, blockSize)
	}
	//nolint:gosec // bounded against MaxInt64/blockSize on the line above
	sectors := int64(blockCount) * int64(blockSize) / DataSectorSize
	if sectors == 0 {
		return TOC{}, fmt.Errorf("cdrom: %d blocks of %d bytes is less than one %d-byte sector",
			blockCount, blockSize, DataSectorSize)
	}
	return TOC{
		FirstTrack: 1,
		LastTrack:  1,
		LeadOutLBA: sectors,
		Tracks:     []Track{{Number: 1, Session: 1, Type: TrackTypeData, StartLBA: 0}},
		NotCD:      true,
	}, nil
}

func (d *darwinDevice) ReadSector(lba int64) ([]byte, error) {
	if lba < 0 {
		return nil, fmt.Errorf("cdrom: negative LBA %d", lba)
	}
	buf := make([]byte, DataSectorSize)
	req := dkCDRead{
		//nolint:gosec // lba was checked non-negative on entry
		Offset:       uint64(lba) * DataSectorSize,
		SectorArea:   sectorAreaUser,
		SectorType:   sectorTypeMode1,
		BufferLength: DataSectorSize,
		Buffer:       uintptr(unsafe.Pointer(&buf[0])), //nolint:gosec // audited: the ioctl needs the address, and every buffer reached through it is held live across the call with runtime.KeepAlive
	}
	var lastErr error
	for attempt := range d.retries {
		retryWait(attempt)
		err := ioctl(d.f.Fd(), ioctlCDRead, uintptr(unsafe.Pointer(&req))) //nolint:gosec // audited: the ioctl needs the address, and every buffer reached through it is held live across the call with runtime.KeepAlive
		runtime.KeepAlive(buf)
		if err != nil {
			lastErr = fmt.Errorf("cdrom: DKIOCCDREAD lba %d: %w", lba, err)
			if !retryable(err) {
				break
			}
			continue
		}
		return buf, nil
	}
	return nil, lastErr
}

// ReadSectorCooked reads a data sector through the plain block-read path (the
// same one mount and dd use) instead of the DKIOCCDREAD ioctl ReadSector
// issues. It is used for media ReadSector cannot address at all (a DVD, whose
// device node rejects every CD ioctl) and for a sector where the low-level
// path failed and the operator has allowed this fallback. Unlike ReadSector
// there is no upper bound on lba: a DVD's LBAs routinely exceed MaxLBA, which
// is a limit of MSF addressing and not of the medium, and an offset past the
// end fails naturally through ReadAt.
func (d *darwinDevice) ReadSectorCooked(lba int64) ([]byte, error) {
	if lba < 0 {
		return nil, fmt.Errorf("cdrom: negative LBA %d", lba)
	}
	buf := make([]byte, DataSectorSize)
	var lastErr error
	for attempt := range d.retries {
		retryWait(attempt)
		if _, err := d.f.ReadAt(buf, lba*DataSectorSize); err != nil {
			lastErr = fmt.Errorf("cdrom: block read lba %d: %w", lba, err)
			if !retryable(err) {
				break
			}
			continue
		}
		return buf, nil
	}
	return nil, lastErr
}

func (d *darwinDevice) ReadRawSector(lba int64, trackType TrackType) ([]byte, error) {
	if lba < 0 {
		return nil, fmt.Errorf("cdrom: negative LBA %d", lba)
	}
	buf := make([]byte, RawSectorSize)
	var req dkCDRead
	if trackType == TrackTypeAudio {
		// CDDA sectors use 2352-byte physical addressing.
		req = dkCDRead{
			//nolint:gosec // lba was checked non-negative on entry
			Offset:       uint64(lba) * RawSectorSize,
			SectorArea:   sectorAreaRawData,
			SectorType:   sectorTypeCDDA,
			BufferLength: RawSectorSize,
			Buffer:       uintptr(unsafe.Pointer(&buf[0])), //nolint:gosec // audited: the ioctl needs the address, and every buffer reached through it is held live across the call with runtime.KeepAlive
		}
	} else {
		// DKIOCCDREAD Offset is a byte offset into the virtual stream formed
		// by the requested SectorArea, so for sectorAreaRawData (2352 bytes
		// per sector) the stride is RawSectorSize, not DataSectorSize.
		req = dkCDRead{
			//nolint:gosec // lba was checked non-negative on entry
			Offset:       uint64(lba) * RawSectorSize,
			SectorArea:   sectorAreaRawData,
			SectorType:   sectorTypeMode1,
			BufferLength: RawSectorSize,
			Buffer:       uintptr(unsafe.Pointer(&buf[0])), //nolint:gosec // audited: the ioctl needs the address, and every buffer reached through it is held live across the call with runtime.KeepAlive
		}
	}
	var lastErr error
	for attempt := range d.retries {
		retryWait(attempt)
		err := ioctl(d.f.Fd(), ioctlCDRead, uintptr(unsafe.Pointer(&req))) //nolint:gosec // audited: the ioctl needs the address, and every buffer reached through it is held live across the call with runtime.KeepAlive
		runtime.KeepAlive(buf)
		if err != nil {
			lastErr = fmt.Errorf("cdrom: DKIOCCDREAD raw lba %d: %w", lba, err)
			if !retryable(err) {
				break
			}
			continue
		}
		return buf, nil
	}
	return nil, lastErr
}
