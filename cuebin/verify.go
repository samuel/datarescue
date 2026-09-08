package cuebin

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strings"

	"samuelks.com/datarescue/cdrom"
	"samuelks.com/datarescue/ddrescue"
)

var syncPattern = [12]byte{0x00, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0x00}

// edcTable is the CD-ROM CRC-32/CD lookup table (polynomial 0xD8018001).
var edcTable = func() [256]uint32 {
	var t [256]uint32
	for i := range 256 {
		edc := uint32(i)
		for range 8 {
			if edc&1 != 0 {
				edc = (edc >> 1) ^ 0xD8018001
			} else {
				edc >>= 1
			}
		}
		t[i] = edc
	}
	return t
}()

// edcCoverage is the number of leading bytes a MODE1 sector's EDC covers, and
// where the stored four-byte value that covers them begins.
const edcCoverage = 2064

// computeEDC returns the CRC-32/CD over the first edcCoverage bytes. A short
// sector has no EDC to compute, so it returns 0.
func computeEDC(sector []byte) uint32 {
	if len(sector) < edcCoverage {
		return 0
	}
	var edc uint32
	for _, b := range sector[:edcCoverage] {
		edc = (edc >> 8) ^ edcTable[(edc^uint32(b))&0xFF]
	}
	return edc
}

// mode1EDCMatches reports whether a raw MODE1 sector's stored EDC agrees with
// the data it covers. Both callers have already established that the sector is
// a whole raw MODE1 one; a buffer too short to hold an EDC has nothing to
// compare and is not judged here.
func mode1EDCMatches(sector []byte) bool {
	if len(sector) < edcCoverage+4 {
		return true
	}
	return computeEDC(sector) == binary.LittleEndian.Uint32(sector[edcCoverage:edcCoverage+4])
}

// SectorState records the verification results for a single sector.
type SectorState struct {
	BadSync    bool // sync pattern mismatch, on a sector the map calls recovered
	BadMSF     bool // wrong position for this track, on a sector the map calls recovered
	BadEDC     bool // MODE1 EDC/CRC mismatch
	MapBad     bool // ddrescue map marks this sector bad
	Unreadable bool // the sector itself could not be read from the BIN
}

// VerifyResult holds the outcome of verifying a BIN file.
type VerifyResult struct {
	TotalSectors  int64
	SectorSize    int
	SizeUnaligned bool // file size is not a whole number of sectors
	// BadSync and BadMSF count only sectors the map calls recovered: a sector
	// that was never read has no sync pattern and no address, and reporting
	// that as a structural fault of the BIN would misstate the damage.
	BadSync int64
	BadMSF  int64
	BadEDC  int64
	BadMap  int64
	// Unreadable counts sectors that could not be read from the BIN at all.
	Unreadable int64
	// EDCBadMapGood counts sectors whose EDC fails although the map calls them
	// recovered: corruption the map did not catch. EDCGoodMapBad counts the
	// opposite, a sector the map calls bad whose EDC is intact, which usually
	// means the map is stale. The two say opposite things and are kept apart.
	EDCBadMapGood int64
	EDCGoodMapBad int64
	Sectors       []SectorState
}

// LostSectors returns how many sectors of the BIN do not hold the disc's data:
// the map's count when there is a map, and the EDC's when there is not. With a
// map, a sector the map calls recovered whose EDC still fails (EDCBadMapGood)
// is a loss the map missed, so it is added too. BadMap and EDCBadMapGood are
// disjoint by construction, so no sector is counted twice.
func (r *VerifyResult) LostSectors() int64 {
	if r.BadMap > 0 {
		return r.BadMap + r.EDCBadMapGood
	}
	return r.BadEDC
}

// VerifyBIN checks the sync pattern, MSF sequence and EDC of every sector of
// the BIN, and cross-references the rescue map when there is one.
//
// It reads src.Raw on purpose. Comparing the bytes the map calls bad against
// the EDC is what produces EDCBadMapGood (corruption the map missed) and
// EDCGoodMapBad (a stale map). Through a map-checked reader those sectors
// would never be read and both counters would always be zero.
func VerifyBIN(src *Source, tracks []CueTrack) (*VerifyResult, error) {
	sectorSize, err := BinSectorSize(tracks)
	if err != nil {
		return nil, err
	}
	blocks := src.Blocks
	totalSectors := src.Size / int64(sectorSize)

	// Map every sector to its track's mode. A failure here would leave every
	// mode empty, so no sector would be treated as MODE1 and EDC checking would
	// silently do nothing while the report still read clean.
	bounds, err := TrackBounds(&Sheet{Files: []CueFile{{Tracks: tracks}}}, src.Size)
	if err != nil {
		return nil, fmt.Errorf("cuebin: cannot determine track bounds, EDC checking would be skipped: %w", err)
	}
	if len(bounds) != len(tracks) {
		return nil, fmt.Errorf("cuebin: track bounds count %d does not match track count %d", len(bounds), len(tracks))
	}
	sectorModes := make([]string, totalSectors)
	// trackOf maps a sector to its track index so MSF continuity can restart at
	// each track boundary.
	trackOf := make([]int, totalSectors)
	for i := range trackOf {
		trackOf[i] = -1
	}
	for i, b := range bounds {
		mode := tracks[i].Mode //nolint:gosec // TrackBounds returns one entry per track, checked above
		for lba := b[0]; lba < b[1] && lba < totalSectors; lba++ {
			sectorModes[lba] = mode
			trackOf[lba] = i
		}
	}

	result := &VerifyResult{
		TotalSectors:  totalSectors,
		SectorSize:    sectorSize,
		SizeUnaligned: src.Size%int64(sectorSize) != 0,
		Sectors:       make([]SectorState, totalSectors),
	}

	ra := src.Raw()
	buf := make([]byte, sectorSize)
	// MSF continuity is judged against an anchor taken from the first readable
	// data sector of each track: sector i should carry msfAnchor+(i-anchorAt).
	// Anchoring rather than comparing with the previous sector means a single
	// corrupt MSF is reported once instead of twice, and an audio track — which
	// carries no MSF at all — cannot make the next data sector look wrong.
	var msfAnchor, anchorAt int64
	var haveAnchor bool
	curTrack := -1
	mapCursor := &mapScanner{blocks: blocks}

	for i := range totalSectors {
		state := &result.Sectors[i]
		mode := sectorModes[i]

		if len(blocks) > 0 {
			state.MapBad = mapCursor.bad(i*int64(sectorSize), int64(sectorSize))
			if state.MapBad {
				result.BadMap++
			}
		}

		if _, err := ra.ReadAt(buf, i*int64(sectorSize)); err != nil {
			// Keep going: a verifier for damaged images must not stop at the
			// first sector it cannot read.
			state.Unreadable = true
			result.Unreadable++
			continue
		}

		if sectorSize == cdrom.RawSectorSize && !IsAudio(mode) {
			// A sector the map never recovered has no sync pattern and no
			// address, since its bytes were never read off the disc. Counting
			// it would report the same damage again as a (worse) structural
			// fault of the BIN, and anchoring the track's MSF on it would make
			// every later sector look wrong. Its EDC is still compared below,
			// because that is what tells a stale map from a real loss.

			// Sync pattern: bytes 0–11 must equal 00 FF×10 00.
			if !state.MapBad && !bytes.Equal(buf[:12], syncPattern[:]) {
				state.BadSync = true
				result.BadSync++
			}

			// MSF sequence: bytes 12–14 hold this sector's absolute position in BCD.
			if trackOf[i] != curTrack {
				curTrack = trackOf[i]
				haveAnchor = false
			}
			absLBA := cdrom.MSFToLBA(bcdToDec(buf[12]), bcdToDec(buf[13]), bcdToDec(buf[14]))
			switch {
			case state.MapBad:
				// no address to read, and none to anchor the track on
			case !haveAnchor:
				msfAnchor, anchorAt, haveAnchor = absLBA, i, true
			case absLBA != msfAnchor+(i-anchorAt):
				state.BadMSF = true
				result.BadMSF++
			}

			// EDC: for MODE1 sectors recompute CRC-32/CD and compare it with
			// the value the sector stores.
			if strings.HasPrefix(strings.ToUpper(mode), "MODE1") {
				if !mode1EDCMatches(buf) {
					state.BadEDC = true
					result.BadEDC++
				}
				if len(blocks) > 0 {
					switch {
					case state.BadEDC && !state.MapBad:
						result.EDCBadMapGood++
					case !state.BadEDC && state.MapBad:
						result.EDCGoodMapBad++
					}
				}
			}
		}
	}

	return result, nil
}

// bcdToDec decodes one packed-BCD byte. The result is at most 165, so it fits
// back into a byte for the MSF helpers.
func bcdToDec(b byte) byte {
	return (b>>4)*10 + b&0x0F
}

// mapScanner answers "is this byte range covered by a non-finished block?" for
// sectors visited in increasing order. The blocks are sorted, so a cursor
// advancing alongside the sectors makes the whole scan O(sectors + blocks)
// instead of rescanning the block list for every sector.
type mapScanner struct {
	blocks []ddrescue.Block
	i      int
}

func (m *mapScanner) bad(start, size int64) bool {
	// No map at all: nothing is known to be missing, and an absent map is not
	// an empty one. This is the same guard as in unrecovered.
	if len(m.blocks) == 0 {
		return false
	}
	end := start + size
	// Drop blocks that end at or before this sector; later sectors start later
	// still, so they will never be needed again.
	for m.i < len(m.blocks) && m.blocks[m.i].Offset+m.blocks[m.i].Size <= start {
		m.i++
	}
	// Coverage has to be contiguous and finished, as in ddrescue.Unrecovered: a
	// hole in the map, or the tail past its last block, is non-tried and not
	// data. Treating it as good would count the sector's missing sync and
	// failed EDC as structural faults of the BIN and as EDCBadMapGood, while
	// ExtractDataTrack treats the same bytes as lost.
	covered := start
	for j := m.i; j < len(m.blocks) && m.blocks[j].Offset < end; j++ {
		b := m.blocks[j]
		if b.Offset > covered || b.Status != ddrescue.BlockStatusFinished {
			return true
		}
		covered = max(covered, b.Offset+b.Size)
	}
	return covered < end
}
