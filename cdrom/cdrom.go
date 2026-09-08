package cdrom

import (
	"errors"
	"slices"
	"time"
)

const (
	RawSectorSize  = 2352
	DataSectorSize = 2048
)

// maxTrackNumber is the highest track number a Red Book CD can carry.
const maxTrackNumber = 99

// retryDelay is the pause between read attempts. Retrying a marginal sector
// immediately gives the drive no chance to reseek or settle.
const retryDelay = 50 * time.Millisecond

// retryable is defined per platform: it reports whether another attempt at err
// could plausibly succeed. A rejected ioctl or a closed device fails
// identically every time, so retrying only multiplies the wait on a disc that
// is already slow to read.

type TrackType uint8

const (
	TrackTypeAudio TrackType = iota
	TrackTypeData
)

func (t TrackType) String() string {
	if t == TrackTypeAudio {
		return "audio"
	}
	return "data"
}

type Track struct {
	Number    uint8
	Session   uint8
	Type      TrackType
	StartLBA  int64 // INDEX 01 position
	Index0LBA int64 // INDEX 00 position (0 = none/unknown)
}

type TOC struct {
	FirstTrack uint8
	LastTrack  uint8
	LeadOutLBA int64
	Tracks     []Track
	// NotCD records that the platform established the medium is not a CD while
	// reading the TOC, instead of leaving it to be inferred from the lead-out.
	// macOS attaches its CD ioctls to CD media only, so a DVD's device node
	// rejects every one of them and the TOC is synthesized from the block
	// device's geometry; on Linux a single driver serves both and nothing at
	// this level tells them apart.
	NotCD bool
}

// HasAudio reports whether the TOC contains any audio tracks.
func (t *TOC) HasAudio() bool {
	for _, tr := range t.Tracks {
		if tr.Type == TrackTypeAudio {
			return true
		}
	}
	return false
}

// TrackAt returns the track containing lba. Sectors before the first track's
// start, and any LBA on a TOC with no tracks, belong to no track. Tracks are
// kept in ascending StartLBA order, which this and every other lookup here
// depend on.
func (t *TOC) TrackAt(lba int64) (Track, bool) {
	for _, v := range slices.Backward(t.Tracks) {
		if lba >= v.StartLBA {
			return v, true
		}
	}
	return Track{}, false
}

// TrackTypeAt returns the type of the track containing lba. Sectors before the
// first track's start, and any LBA on a TOC with no tracks, count as data.
func (t *TOC) TrackTypeAt(lba int64) TrackType {
	if tr, ok := t.TrackAt(lba); ok {
		return tr.Type
	}
	return TrackTypeData
}

// NextTrackAfter returns the first track starting strictly after lba.
func (t *TOC) NextTrackAfter(lba int64) (Track, bool) {
	for _, tr := range t.Tracks {
		if tr.StartLBA > lba {
			return tr, true
		}
	}
	return Track{}, false
}

// InPregap reports whether lba falls in a track's pregap. When any track has a
// known INDEX 00 the exact ranges decide; otherwise the answer falls back to
// whether the next track is audio, since a pregap belongs to the track it
// precedes and only audio pregaps are stored in the image.
func (t *TOC) InPregap(lba int64) bool {
	var haveIndex0 bool
	for _, tr := range t.Tracks {
		if tr.Index0LBA == 0 {
			continue
		}
		haveIndex0 = true
		if lba >= tr.Index0LBA && lba < tr.StartLBA {
			return true
		}
	}
	if haveIndex0 {
		return false
	}
	next, ok := t.NextTrackAfter(lba)
	return ok && next.Type == TrackTypeAudio
}

// LastDataSession returns the highest session containing a data track and the
// index of that session's first *data* track, or -1 when there is none.
//
// It returns the first data track rather than the session's first track: in a
// session that opens with audio and carries data after it, a caller ripping an
// ISO would otherwise get the audio track's LBA and read music as a
// filesystem.
func (t *TOC) LastDataSession() (session uint8, firstTrackIdx int) {
	firstTrackIdx = -1
	for i, tr := range t.Tracks {
		if tr.Type == TrackTypeData && tr.Session >= session {
			session = tr.Session
			firstTrackIdx = i
		}
	}
	// Walk back to the first data track in the winning session.
	if firstTrackIdx >= 0 {
		for i := firstTrackIdx - 1; i >= 0; i-- {
			if t.Tracks[i].Session == session && t.Tracks[i].Type == TrackTypeData {
				firstTrackIdx = i
			} else if t.Tracks[i].Session != session {
				break
			}
		}
	}
	return session, firstTrackIdx
}

type Device interface {
	TOC() (TOC, error)
	// ReadSector reads 2048 bytes of user data from a data-track sector.
	ReadSector(lba int64) ([]byte, error)
	// ReadRawSector reads a full 2352-byte raw sector. trackType must match the
	// real track type, as data and audio sectors are addressed differently.
	ReadRawSector(lba int64, trackType TrackType) ([]byte, error)
	Close() error
}

// CookedFallbackReader is implemented by a Device that can also retrieve a
// data sector's 2048 bytes through the plain OS block-read path, bypassing
// the low-level command ReadSector normally uses.
//
// It serves two different cases. When NeedsCookedReads reports that the
// low-level commands cannot address the medium at all, this is the only way to
// read it: a DVD sector has no 2352-byte raw form, so there is no alternative
// an operator could prefer. When ReadSector merely failed on a CD, using this
// gets the data but loses the raw sector's EDC, and a caller should ask before
// making that trade.
//
// The medium's block size need not be 2048: the read is a byte range, and
// every block size an optical or block device reports divides or is divided by
// a 2048-byte sector.
type CookedFallbackReader interface {
	ReadSectorCooked(lba int64) ([]byte, error)
}

// NeedsCookedReads reports whether the low-level CD read commands cannot
// address this medium at all, leaving a CookedFallbackReader as the only way
// to read it. SCSI READ CD, which every ReadSector and ReadRawSector path
// issues, is CD-only, so on a DVD it rejects every sector identically rather
// than failing only on the damaged ones. Two signals mark a medium as not a
// CD: a lead-out past what an MSF address can express, which only a DVD
// reaches, and NotCD, the only signal for a DVD small enough to fit inside CD
// range.
func (t *TOC) NeedsCookedReads() bool {
	return t.NotCD || t.LeadOutLBA > MaxLBA
}

// MSFToLBA converts a minutes/seconds/frames address to an LBA. Addresses
// inside the 150-frame lead-in have no LBA, since an LBA counts from where the
// user data starts. They clamp to 0 rather than going negative, because a
// negative start would be used to index every per-sector slice a caller builds
// from the TOC.
func MSFToLBA(m, s, f byte) int64 {
	return max(int64(m)*60*75+int64(s)*75+int64(f)-150, 0)
}

// MaxLBA is the highest LBA an MSF address can hold: 99 minutes, 59 seconds,
// 74 frames, less the 150-frame pregap.
const MaxLBA = (99*60+59)*75 + 74 - 150

// LBAToMSF converts an LBA to minutes, seconds, frames. An LBA outside the
// range MSF can express is clamped rather than wrapped, since a wrapped address
// silently points somewhere else on the disc.
func LBAToMSF(lba int64) (m, s, f byte) {
	lba = min(max(lba, 0), MaxLBA)
	lba += 150
	f = byte(lba % 75)
	lba /= 75
	s = byte(lba % 60)
	m = byte(lba / 60)
	return m, s, f
}

// ErrUnsupportedPlatform is returned on platforms other than Linux and macOS.
var ErrUnsupportedPlatform = errors.New("cdrom: unsupported platform")
