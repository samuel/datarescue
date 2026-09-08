//go:build linux

package cdrom

import (
	"fmt"
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Linux ioctl constants from linux/cdrom.h and scsi/sg.h
const (
	ioctlReadTOCHeader = 0x5305
	ioctlReadTOCEntry  = 0x5306
	ioctlReadMode1     = 0x530d
	ioctlReadRaw       = 0x5314
	ioctlMultiSession  = 0x5310
	ioctlReadAudio     = 0x530e
	ioctlSGIO          = 0x2285 // SG_IO: raw SCSI pass-through
	cdromMSF           = 0x02
	cdromLBA           = 0x01
	cdromLeadOut       = 0xAA
	sgDxferFromDev     = -3 // SG_DXFER_FROM_DEV
)

// cdromMSF0 mirrors struct cdrom_msf0 from linux/cdrom.h.
type cdromMSF0 struct {
	Minute byte
	Second byte
	Frame  byte
}

// cdromReadAudio mirrors struct cdrom_read_audio from linux/cdrom.h.
type cdromReadAudio struct {
	Addr       cdromMSF0
	_          byte
	AddrFormat byte
	_          [3]byte
	NumFrames  int32
	_          [4]byte
	Buf        uintptr
}

type cdromTOCHeader struct {
	FirstTrack uint8
	LastTrack  uint8
}

type cdromTOCEntry struct {
	Track uint8
	// AddrCtrl is cdrom_tocentry's cdte_adr:4 and cdte_ctrl:4 bitfields.
	// They are declared adr first, which on a little-endian machine puts adr
	// in the low nibble and ctrl in the high one -- the opposite of the order
	// they are written in. Reading ctrl from the low nibble finds adr, which
	// is 1 on every position descriptor, so every track looks like audio.
	AddrCtrl uint8
	Format   uint8
	_        byte
	AddrLBA  int32
	DataMode uint8
	_        [3]byte
}

// cdromMultiSession mirrors struct cdrom_multisession: a union cdrom_addr
// (4 bytes — struct cdrom_msf0 is three, int lba is four, and the union takes
// the larger), then xa_flag and addr_format, padded to 8 by the union's
// alignment.
//
// There must be no padding between the address and XAFlag. Padding there puts
// Format at byte 9 instead of 5, so the kernel reads the format from padding
// and rejects every call with EINVAL. TOC() treats that as a single-session
// disc, so on a drive that also does not answer the Full TOC request every
// track would be in session 1, and ripping the last session of a multisession
// disc would silently rip the first.
type cdromMultiSession struct {
	AddrLBA int32
	XAFlag  uint8
	Format  uint8
	_       [2]byte
}

// sgIOHdr mirrors sg_io_hdr_t from scsi/sg.h (88 bytes on 64-bit Linux).
type sgIOHdr struct {
	InterfaceID    int32
	DxferDirection int32
	CmdLen         uint8
	MxSbLen        uint8
	IovecCount     uint16
	DxferLen       uint32
	Dxferp         uintptr
	Cmdp           uintptr
	Sbp            uintptr
	Timeout        uint32
	Flags          uint32
	PackID         int32
	_              [4]byte
	UsrPtr         uintptr
	Status         uint8
	MaskedStatus   uint8
	MsgStatus      uint8
	SbLenWr        uint8
	HostStatus     uint16
	DriverStatus   uint16
	Resid          int32
	Duration       uint32
	Info           uint32
	_              [4]byte
}

// readFullTOC returns each track's INDEX 00 LBA and session number, for the
// tracks the drive reports them for. It fails on drives that do not support
// the Full TOC format.
func readFullTOC(fd uintptr) (map[uint8]fullTOCEntry, error) {
	// The lengths are constants so they provably fit the header's fields.
	const bufLen, senseLen = 2048, 32
	buf := make([]byte, bufLen)
	sense := make([]byte, senseLen)
	// The format goes in two places. MMC-3 and later read it from byte 2, but
	// older drives read bits 7-6 of byte 9 and answer a byte-2 request with a
	// plain TOC: 8-byte descriptors carrying neither INDEX 00 nor a session
	// number. Setting both makes every drive return the Full TOC.
	cdb := [10]byte{
		0x43,                   // READ TOC/PMA/ATIP
		0x02,                   // TIME=1 (MSF addresses)
		0x02,                   // Format: Full TOC
		0x00, 0x00, 0x00, 0x00, // reserved + session number
		0x08, 0x00, // allocation length = 2048 (big-endian: 0x0800)
		0x80, // Format: Full TOC, in the pre-MMC-3 position
	}
	hdr := sgIOHdr{
		InterfaceID:    'S',
		DxferDirection: sgDxferFromDev,
		CmdLen:         uint8(len(cdb)),
		MxSbLen:        senseLen,
		DxferLen:       bufLen,
		Dxferp:         uintptr(unsafe.Pointer(&buf[0])),   //nolint:gosec // audited: the ioctl needs the address, and every buffer reached through it is held live across the call with runtime.KeepAlive
		Cmdp:           uintptr(unsafe.Pointer(&cdb[0])),   //nolint:gosec // audited: the ioctl needs the address, and every buffer reached through it is held live across the call with runtime.KeepAlive
		Sbp:            uintptr(unsafe.Pointer(&sense[0])), //nolint:gosec // audited: the ioctl needs the address, and every buffer reached through it is held live across the call with runtime.KeepAlive
		Timeout:        5000,
	}
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, fd, ioctlSGIO, uintptr(unsafe.Pointer(&hdr))) //nolint:gosec // audited: the ioctl needs the address, and every buffer reached through it is held live across the call with runtime.KeepAlive
	// The pointers above live in a struct, so nothing else references these
	// buffers while the kernel writes to them; keep them alive until it is done.
	runtime.KeepAlive(buf)
	runtime.KeepAlive(cdb)
	runtime.KeepAlive(sense)
	if errno != 0 {
		return nil, errno
	}
	if hdr.Status != 0 {
		return nil, fmt.Errorf("cdrom: Full TOC SCSI status %02x", hdr.Status)
	}
	descs, err := parseFullTOC(buf)
	if err != nil {
		return nil, err
	}
	return fullTOCEntries(descs), nil
}

type linuxDevice struct {
	f       *os.File
	retries int
}

func Open(path string) (Device, error) {
	f, err := os.Open(path) //nolint:gosec // the path is the device the operator named, opened read-only
	if err != nil {
		return nil, fmt.Errorf("cdrom: open %s: %w", path, err)
	}
	return &linuxDevice{f: f, retries: 3}, nil
}

func (d *linuxDevice) Close() error { return d.f.Close() }

func (d *linuxDevice) TOC() (TOC, error) {
	var hdr cdromTOCHeader
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, d.f.Fd(), ioctlReadTOCHeader, uintptr(unsafe.Pointer(&hdr))); errno != 0 { //nolint:gosec // audited: the ioctl needs the address, and every buffer reached through it is held live across the call with runtime.KeepAlive
		return TOC{}, fmt.Errorf("cdrom: CDROMREADTOCHDR: %w", errno)
	}

	toc := TOC{
		FirstTrack: hdr.FirstTrack,
		LastTrack:  hdr.LastTrack,
	}

	// The drive supplies both track numbers: a reversed range underflows the
	// uint8 subtraction, and LastTrack of 255 would make the uint8 loop counter
	// below wrap instead of terminating.
	if hdr.FirstTrack < 1 || hdr.LastTrack < hdr.FirstTrack || hdr.LastTrack > maxTrackNumber {
		return TOC{}, fmt.Errorf("cdrom: CDROMREADTOCHDR reported invalid track range %d-%d",
			hdr.FirstTrack, hdr.LastTrack)
	}

	// Read each track plus the lead-out.
	tracks := make([]uint8, 0, int(hdr.LastTrack)-int(hdr.FirstTrack)+2)
	for t := hdr.FirstTrack; t <= hdr.LastTrack; t++ {
		tracks = append(tracks, t)
	}
	tracks = append(tracks, cdromLeadOut)

	// First pass: read session info to assign session numbers.
	var ms cdromMultiSession
	ms.Format = cdromLBA
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, d.f.Fd(), ioctlMultiSession, uintptr(unsafe.Pointer(&ms))); errno != 0 { //nolint:gosec // audited: the ioctl needs the address, and every buffer reached through it is held live across the call with runtime.KeepAlive
		// Not fatal; single-session disc.
		ms.AddrLBA = 0
	}

	for _, tn := range tracks {
		var entry cdromTOCEntry
		entry.Track = tn
		entry.Format = cdromLBA
		if _, _, errno := unix.Syscall(unix.SYS_IOCTL, d.f.Fd(), ioctlReadTOCEntry, uintptr(unsafe.Pointer(&entry))); errno != 0 { //nolint:gosec // audited: the ioctl needs the address, and every buffer reached through it is held live across the call with runtime.KeepAlive
			return TOC{}, fmt.Errorf("cdrom: CDROMREADTOCENTRY track %d: %w", tn, errno)
		}
		lba := int64(entry.AddrLBA)
		ctrl := entry.AddrCtrl >> 4
		if tn == cdromLeadOut {
			toc.LeadOutLBA = lba
			continue
		}
		// ctrl bit 2: 0=audio, 1=data
		trackType := TrackTypeAudio
		if ctrl&0x04 != 0 {
			trackType = TrackTypeData
		}
		// CDROMMULTISESSION reports only the last session's start, so tracks
		// are split into "before it" and "at or after it" here and the real
		// per-track session numbers are filled in from the Full TOC below,
		// which carries a session number on every descriptor.
		session := uint8(1)
		if ms.AddrLBA > 0 && lba >= int64(ms.AddrLBA) {
			session = 2
		}
		toc.Tracks = append(toc.Tracks, Track{
			Number:   tn,
			Session:  session,
			Type:     trackType,
			StartLBA: lba,
		})
	}

	// Enrich with INDEX 00 positions and true session numbers from the Full TOC;
	// ignore errors, since not all drives support the command.
	if full, err := readFullTOC(d.f.Fd()); err == nil {
		for i := range toc.Tracks {
			if e, ok := full[toc.Tracks[i].Number]; ok {
				toc.Tracks[i].Index0LBA = e.index0LBA
				if e.session > 0 {
					toc.Tracks[i].Session = e.session
				}
			}
		}
	}

	return toc, nil
}

// readSectorInto issues one of the CDROMREAD* ioctls. They do not take the
// struct cdrom_read that linux/cdrom.h documents: the argument is the data
// buffer itself, with the sector's MSF address in its first three bytes, and
// the kernel writes the sector back over it. Passing a struct cdrom_read makes
// the kernel read an MSF out of the LBA field, which either fails or reads a
// different sector.
func readSectorInto(fd uintptr, req uintptr, lba int64, buf []byte) error {
	m, s, f := LBAToMSF(lba)
	buf[0], buf[1], buf[2] = m, s, f
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, fd, req, uintptr(unsafe.Pointer(&buf[0]))) //nolint:gosec // audited: the ioctl needs the address, and the buffer is held live across the call with runtime.KeepAlive
	runtime.KeepAlive(buf)
	if errno != 0 {
		return errno
	}
	return nil
}

// checkLBA rejects addresses MSF cannot express. LBAToMSF clamps them, so
// without this a read outside the disc would quietly return some other sector.
func checkLBA(lba int64) error {
	if lba < 0 || lba > MaxLBA {
		return fmt.Errorf("cdrom: LBA %d is outside the addressable range 0-%d", lba, MaxLBA)
	}
	return nil
}

func (d *linuxDevice) ReadSector(lba int64) ([]byte, error) {
	if err := checkLBA(lba); err != nil {
		return nil, err
	}
	buf := make([]byte, DataSectorSize)
	var lastErr error
	for attempt := range d.retries {
		retryWait(attempt)
		if err := readSectorInto(d.f.Fd(), ioctlReadMode1, lba, buf); err != nil {
			lastErr = fmt.Errorf("cdrom: CDROMREADMODE1 lba %d: %w", lba, err)
			if !retryable(err) {
				break
			}
			continue
		}
		return buf, nil
	}
	return nil, lastErr
}

// ReadSectorCooked reads a data sector through the plain block-read path
// (the same one mount and dd use) instead of the low-level MMC command
// ReadSector issues. It is used for media ReadSector cannot address at all
// (a DVD, which SCSI READ CD does not support) and for a sector where the
// low-level path failed and the operator has allowed this fallback. Unlike
// checkLBA, there is no upper bound here: a DVD's LBAs
// routinely exceed MaxLBA, which is a limit of MSF addressing, not of the
// medium. An out-of-range offset fails naturally through ReadAt.
func (d *linuxDevice) ReadSectorCooked(lba int64) ([]byte, error) {
	if lba < 0 {
		return nil, fmt.Errorf("cdrom: LBA %d is negative", lba)
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

func (d *linuxDevice) ReadRawSector(lba int64, trackType TrackType) ([]byte, error) {
	if trackType == TrackTypeAudio {
		return d.readAudioSector(lba)
	}
	if err := checkLBA(lba); err != nil {
		return nil, err
	}
	buf := make([]byte, RawSectorSize)
	var lastErr error
	for attempt := range d.retries {
		retryWait(attempt)
		if err := readSectorInto(d.f.Fd(), ioctlReadRaw, lba, buf); err != nil {
			lastErr = fmt.Errorf("cdrom: CDROMREADRAW lba %d: %w", lba, err)
			if !retryable(err) {
				break
			}
			continue
		}
		return buf, nil
	}
	return nil, lastErr
}

// readAudioSector reads one CDDA sector. CDROMREADRAW is a MODE1 command and
// drives reject it on an audio track, so audio goes through CDROMREADAUDIO,
// which addresses the disc in MSF.
func (d *linuxDevice) readAudioSector(lba int64) ([]byte, error) {
	if err := checkLBA(lba); err != nil {
		return nil, err
	}
	buf := make([]byte, RawSectorSize)
	m, s, f := LBAToMSF(lba)
	arg := cdromReadAudio{
		Addr:       cdromMSF0{Minute: m, Second: s, Frame: f},
		AddrFormat: cdromMSF,
		NumFrames:  1,
		Buf:        uintptr(unsafe.Pointer(&buf[0])), //nolint:gosec // audited: the ioctl needs the address, and every buffer reached through it is held live across the call with runtime.KeepAlive
	}
	var lastErr error
	for attempt := range d.retries {
		retryWait(attempt)
		_, _, errno := unix.Syscall(unix.SYS_IOCTL, d.f.Fd(), ioctlReadAudio, uintptr(unsafe.Pointer(&arg))) //nolint:gosec // audited: the ioctl needs the address, and every buffer reached through it is held live across the call with runtime.KeepAlive
		runtime.KeepAlive(buf)
		if errno != 0 {
			lastErr = fmt.Errorf("cdrom: CDROMREADAUDIO lba %d: %w", lba, errno)
			if !retryable(errno) {
				break
			}
			continue
		}
		return buf, nil
	}
	return nil, lastErr
}
