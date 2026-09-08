package cuebin

import (
	"encoding/binary"
	"hash/fnv"
)

const silenceThreshold = 8800 // ~100ms of consecutive zero-valued 16-bit samples at 44.1 kHz

// SilentRun describes a contiguous stretch of silence in an audio track.
type SilentRun struct {
	StartLBA int64
	Samples  int64
}

// AudioResult holds analysis results for one audio track.
type AudioResult struct {
	TrackNumber  int
	StartLBA     int64
	EndLBA       int64
	TotalSectors int64
	BadSectors   int64
	// UnreadableSectors counts sectors that could not be read at all, which is
	// how a range the rescue never recovered shows up. It is kept separate from
	// BadSectors, which counts what the map reports without reading anything.
	UnreadableSectors int64
	SilentRuns        []SilentRun
	RepeatedSectors   []int64
}

// DurationSeconds returns the track's playing time.
func (a *AudioResult) DurationSeconds() float64 {
	return float64(a.TotalSectors) / 75.0
}

// AnalyzeAudioTrack scans one AUDIO track for silence and repeated sectors, and
// counts bad sectors from vr when it is non-nil. It reads src.Checked, so a
// sector the rescue never recovered is skipped rather than analyzed.
//
// A sector that will not read is counted and the scan continues, since damaged
// discs are the ones that need a report.
func AnalyzeAudioTrack(src *Source, trackNum int, startLBA, endLBA int64, sectorSize int, vr *VerifyResult) *AudioResult {
	result := &AudioResult{
		TrackNumber:  trackNum,
		StartLBA:     startLBA,
		EndLBA:       endLBA,
		TotalSectors: endLBA - startLBA,
	}

	if vr != nil {
		for sec := startLBA; sec < endLBA; sec++ {
			if sec < int64(len(vr.Sectors)) && vr.Sectors[sec].MapBad {
				result.BadSectors++
			}
		}
	}

	ra := src.Checked()
	buf := make([]byte, sectorSize)
	hashes := make(map[uint64]int64)

	var silentCount int64
	var silentStart int64

	// endRun closes an open silent run, keeping it when it is long enough.
	endRun := func() {
		if silentCount >= silenceThreshold {
			result.SilentRuns = append(result.SilentRuns, SilentRun{
				StartLBA: silentStart,
				Samples:  silentCount,
			})
		}
		silentCount = 0
	}

	h := fnv.New64a()
	for lba := startLBA; lba < endLBA; lba++ {
		if _, err := ra.ReadAt(buf, lba*int64(sectorSize)); err != nil {
			result.UnreadableSectors++
			// A skipped sector breaks the silent run: carrying the count across
			// a hole would report silence the disc never had. It is also not
			// hashed: every hole hashes alike, so a damaged track would report
			// the holes as repeated sectors and drown out real repeats.
			endRun()
			continue
		}

		// Repeated sector detection via FNV-64a hash.
		h.Reset()
		h.Write(buf[:sectorSize])
		hash := h.Sum64()
		if prev, seen := hashes[hash]; seen && prev != lba {
			result.RepeatedSectors = append(result.RepeatedSectors, lba)
		} else {
			hashes[hash] = lba
		}

		// Silence detection: count consecutive zero-valued 16-bit samples.
		for i := 0; i+1 < sectorSize; i += 2 {
			if binary.LittleEndian.Uint16(buf[i:i+2]) == 0 {
				if silentCount == 0 {
					silentStart = lba
				}
				silentCount++
			} else {
				endRun()
			}
		}
	}
	endRun()

	return result
}
