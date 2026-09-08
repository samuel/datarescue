package cdrom

import (
	"cmp"
	"encoding/binary"
	"fmt"
	"slices"
)

// fullTOCDescLen is the size of one Full TOC descriptor. Both the MMC READ
// TOC/PMA/ATIP format 0x02 response and macOS's CDTOCDescriptor use it.
const fullTOCDescLen = 11

// fullTOCDescriptor is one decoded Full TOC descriptor. The on-disc layout is:
//
//	[0] session  [1] ctrlAdr  [2] tno  [3] point
//	[4-6] AMIN/ASEC/AFRAME  [7] zero  [8-10] PMIN/PSEC/PFRAME
//
// The position this package wants is P, at bytes 8-10. For a lead-in
// descriptor that refers to a track, P is where the track starts, while A is
// the descriptor's own address in the lead-in.
//
// A descriptor in the lead-in (tno 0) describes the disc: point 1-99 is a
// track number, and point 0xA0/0xA1/0xA2 the first track, last track and
// lead-out. A descriptor with a track number describes a position within that
// track, where point is the index number.
type fullTOCDescriptor struct {
	session byte
	adr     byte // high nibble of ctrlAdr
	ctrl    byte // low nibble of ctrlAdr
	tno     byte
	point   byte
	lba     int64 // from PMIN/PSEC/PFRAME, which is the position described
}

// isData reports whether the descriptor's control field marks a data track.
func (d fullTOCDescriptor) isData() bool { return d.ctrl&0x04 != 0 }

// carriesPosition reports whether the address fields hold an MSF position. ADR
// 2 and 3 put the media catalog number and ISRC there instead, so reading them
// as an address yields a position somewhere else on the disc entirely.
func (d fullTOCDescriptor) carriesPosition() bool { return d.adr != 2 && d.adr != 3 }

// parseFullTOC decodes the descriptors in a Full TOC response. Everything it
// reads is drive-supplied, so the declared data length is clamped to the buffer
// it arrived in and a length that does not describe whole descriptors is
// rejected rather than parsed up to the last complete one.
func parseFullTOC(buf []byte) ([]fullTOCDescriptor, error) {
	if len(buf) < 4 {
		return nil, fmt.Errorf("cdrom: Full TOC response %d bytes, need at least 4", len(buf))
	}
	// Header: uint16 data length (big-endian, excluding itself), then the first
	// and last session numbers.
	dataLen := int(binary.BigEndian.Uint16(buf[0:2]))
	if dataLen < 2 {
		return nil, fmt.Errorf("cdrom: Full TOC data length %d too short", dataLen)
	}
	descData := buf[4:min(2+dataLen, len(buf))]
	if len(descData)%fullTOCDescLen != 0 {
		return nil, fmt.Errorf("cdrom: Full TOC descriptor data length %d not a multiple of %d",
			len(descData), fullTOCDescLen)
	}

	descs := make([]fullTOCDescriptor, 0, len(descData)/fullTOCDescLen)
	for i := 0; i+fullTOCDescLen <= len(descData); i += fullTOCDescLen {
		desc := descData[i : i+fullTOCDescLen]
		descs = append(descs, fullTOCDescriptor{
			session: desc[0],
			adr:     desc[1] >> 4,
			ctrl:    desc[1] & 0x0f,
			tno:     desc[2],
			point:   desc[3],
			lba:     MSFToLBA(desc[8], desc[9], desc[10]),
		})
	}
	return descs, nil
}

// fullTOCEntry is what the Full TOC adds over the per-track TOC entries the
// simpler ioctls return: the INDEX 00 position and the session the track
// belongs to.
type fullTOCEntry struct {
	index0LBA int64
	session   uint8
}

// fullTOCEntries collects the per-track INDEX 00 positions and session numbers,
// for enriching a TOC assembled from another source.
func fullTOCEntries(descs []fullTOCDescriptor) map[uint8]fullTOCEntry {
	entries := make(map[uint8]fullTOCEntry)
	for _, d := range descs {
		if !d.carriesPosition() {
			continue
		}
		if d.tno != 0 {
			if d.point == 0x00 {
				e := entries[d.tno]
				e.index0LBA = d.lba
				entries[d.tno] = e
			}
			continue
		}
		// A track descriptor in the lead-in records the session its track
		// belongs to, which discs with three or more sessions need.
		if d.point >= 1 && d.point <= maxTrackNumber {
			e := entries[d.point]
			e.session = d.session
			entries[d.point] = e
		}
	}
	return entries
}

// tocFromFullTOC assembles a complete TOC from Full TOC descriptors alone, for
// platforms whose only TOC call returns this format.
//
// The tracks are returned sorted by start. The drive returns descriptors in
// any order, and TrackAt, NextTrackAfter and InPregap all assume the slice is
// ascending; out of order, a lookup returns the wrong track.
func tocFromFullTOC(descs []fullTOCDescriptor) TOC {
	var toc TOC
	for _, d := range descs {
		if !d.carriesPosition() || d.tno != 0 {
			continue
		}
		switch {
		case d.point == 0xA2:
			toc.LeadOutLBA = d.lba
		case d.point >= 1 && d.point <= maxTrackNumber:
			trackType := TrackTypeAudio
			if d.isData() {
				trackType = TrackTypeData
			}
			if d.point > toc.LastTrack {
				toc.LastTrack = d.point
			}
			if toc.FirstTrack == 0 || d.point < toc.FirstTrack {
				toc.FirstTrack = d.point
			}
			toc.Tracks = append(toc.Tracks, Track{
				Number:   d.point,
				Session:  d.session,
				Type:     trackType,
				StartLBA: d.lba,
			})
		}
	}
	// The INDEX 00 positions come from the in-track descriptors, which the
	// loop above skips.
	for tno, e := range fullTOCEntries(descs) {
		for i := range toc.Tracks {
			if toc.Tracks[i].Number == tno {
				toc.Tracks[i].Index0LBA = e.index0LBA
			}
		}
	}
	slices.SortFunc(toc.Tracks, func(a, b Track) int {
		if a.StartLBA != b.StartLBA {
			return cmp.Compare(a.StartLBA, b.StartLBA)
		}
		return cmp.Compare(a.Number, b.Number)
	})
	return toc
}
