package cuebin

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// MSF is a disc address in minutes/seconds/frames (75 frames per second).
type MSF struct {
	M, S, F int
}

// LBA converts the MSF address to a logical block address.
func (m MSF) LBA() int64 {
	return int64(m.M)*60*75 + int64(m.S)*75 + int64(m.F)
}

// Index is a cue INDEX directive.
type Index struct {
	Number   int
	Position MSF
}

// CueTrack is one TRACK entry in a cue sheet.
//
// Pregap and Postgap are parsed and validated but move no boundary: CDRWIN's
// PREGAP and POSTGAP are silence the writer generates and are not in the data
// file. A gap that *is* in the file is written as INDEX 00, which TrackBounds
// does honor.
type CueTrack struct {
	Number  int
	Mode    string // "AUDIO", "MODE1/2352", "MODE2/2352", etc.
	Pregap  *MSF
	Postgap *MSF
	Indices []Index
}

// StartLBA returns the LBA of INDEX 01 (the logical track start).
func (t *CueTrack) StartLBA() (int64, error) {
	for _, idx := range t.Indices {
		if idx.Number == 1 {
			return idx.Position.LBA(), nil
		}
	}
	return 0, fmt.Errorf("cuebin: track %d has no INDEX 01", t.Number)
}

// CueFile is one FILE block in a cue sheet.
type CueFile struct {
	Path   string
	Format string // "BINARY", "WAVE", "MP3", "AIFF"
	Tracks []CueTrack
}

// Sheet is a parsed cue sheet.
type Sheet struct {
	Files []CueFile
}

// ParseSheet parses a cue sheet from r.
func ParseSheet(r io.Reader) (*Sheet, error) {
	var sheet Sheet
	var curFile *CueFile
	var curTrack *CueTrack

	scanner := bufio.NewScanner(r)
	var lineNum int
	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		fields := splitCueFields(line)
		if len(fields) == 0 {
			continue
		}

		switch strings.ToUpper(fields[0]) {
		case "REM", "CATALOG", "CDTEXTFILE", "TITLE", "PERFORMER", "SONGWRITER", "ISRC":
			// informational directives — skip

		case "FILE":
			if len(fields) < 3 {
				return nil, fmt.Errorf("cuebin: line %d: FILE needs path and format", lineNum)
			}
			if curFile != nil && curTrack != nil {
				curFile.Tracks = append(curFile.Tracks, *curTrack)
				curTrack = nil
			}
			if curFile != nil {
				sheet.Files = append(sheet.Files, *curFile)
			}
			curFile = &CueFile{Path: fields[1], Format: strings.ToUpper(fields[2])}

		case "TRACK":
			if curFile == nil {
				return nil, fmt.Errorf("cuebin: line %d: TRACK outside FILE", lineNum)
			}
			if len(fields) < 3 {
				return nil, fmt.Errorf("cuebin: line %d: TRACK needs number and mode", lineNum)
			}
			if curTrack != nil {
				curFile.Tracks = append(curFile.Tracks, *curTrack)
			}
			n, err := strconv.Atoi(fields[1])
			if err != nil {
				return nil, fmt.Errorf("cuebin: line %d: invalid track number %q", lineNum, fields[1])
			}
			curTrack = &CueTrack{Number: n, Mode: strings.ToUpper(fields[2])}

		case "INDEX":
			if curTrack == nil {
				return nil, fmt.Errorf("cuebin: line %d: INDEX outside TRACK", lineNum)
			}
			if len(fields) < 3 {
				return nil, fmt.Errorf("cuebin: line %d: INDEX needs number and position", lineNum)
			}
			n, err := strconv.Atoi(fields[1])
			if err != nil {
				return nil, fmt.Errorf("cuebin: line %d: invalid index number %q", lineNum, fields[1])
			}
			msf, err := parseMSF(fields[2])
			if err != nil {
				return nil, fmt.Errorf("cuebin: line %d: %w", lineNum, err)
			}
			curTrack.Indices = append(curTrack.Indices, Index{Number: n, Position: msf})

		case "PREGAP":
			if curTrack == nil {
				return nil, fmt.Errorf("cuebin: line %d: PREGAP outside TRACK", lineNum)
			}
			if len(fields) < 2 {
				return nil, fmt.Errorf("cuebin: line %d: PREGAP needs length", lineNum)
			}
			msf, err := parseMSF(fields[1])
			if err != nil {
				return nil, fmt.Errorf("cuebin: line %d: %w", lineNum, err)
			}
			curTrack.Pregap = &msf

		case "POSTGAP":
			if curTrack == nil {
				return nil, fmt.Errorf("cuebin: line %d: POSTGAP outside TRACK", lineNum)
			}
			if len(fields) < 2 {
				return nil, fmt.Errorf("cuebin: line %d: POSTGAP needs length", lineNum)
			}
			msf, err := parseMSF(fields[1])
			if err != nil {
				return nil, fmt.Errorf("cuebin: line %d: %w", lineNum, err)
			}
			curTrack.Postgap = &msf

		case "FLAGS":
			// DCP, 4CH, PRE, SCMS — ignored
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("cuebin: scan: %w", err)
	}

	if curFile != nil {
		if curTrack != nil {
			curFile.Tracks = append(curFile.Tracks, *curTrack)
		}
		sheet.Files = append(sheet.Files, *curFile)
	}
	return &sheet, nil
}

// TrackBounds returns the [startLBA, endLBA) of each track, given the BIN's
// size in bytes: the last track runs to the end of the file, and only the
// caller holding the file knows where that is. Sheets referencing more than one
// binary file are not supported.
func TrackBounds(sheet *Sheet, binSize int64) ([][2]int64, error) {
	if len(sheet.Files) != 1 {
		return nil, errors.New("cuebin: multi-file cue sheets are not supported")
	}
	cf := sheet.Files[0]

	binSS, err := BinSectorSize(cf.Tracks)
	if err != nil {
		return nil, err
	}
	totalSectors := binSize / int64(binSS)

	bounds := make([][2]int64, len(cf.Tracks))
	for i := range cf.Tracks {
		// Track start
		if i == 0 {
			// First track always starts at LBA 0 in the BIN.
			bounds[i][0] = 0
		} else {
			// Subsequent tracks start exactly where the previous track ended.
			bounds[i][0] = bounds[i-1][1]
		}

		// Track end, which is where the next track's bytes begin in the file.
		if i+1 < len(cf.Tracks) {
			next := cf.Tracks[i+1]
			// INDEX 01 is the logical start; an INDEX 00 before it is the
			// in-file pregap and is where the next track's bytes really begin.
			//
			// PREGAP deliberately does not count. CDRWIN's PREGAP is silence
			// the writer generates and is *not* in the data file; an in-file
			// pregap is written as INDEX 00. Subtracting PREGAP would pull this
			// track's end back by its length, shift every later boundary by the
			// same amount and, with both PREGAP and INDEX 00 on one track,
			// produce a negative boundary.
			endLBA, err := next.StartLBA()
			if err != nil {
				return nil, err
			}
			for _, idx := range next.Indices {
				if idx.Number == 0 {
					endLBA = min(endLBA, idx.Position.LBA())
				}
			}
			if endLBA < bounds[i][0] {
				return nil, fmt.Errorf("cuebin: track %d ends at sector %d, before it starts at %d",
					cf.Tracks[i].Number, endLBA, bounds[i][0])
			}
			bounds[i][1] = endLBA
		} else {
			// The last track runs to the end of the file. If the BIN is shorter
			// than the cue describes, the track is empty rather than negative,
			// since truncated images are an expected input.
			bounds[i][1] = max(totalSectors, bounds[i][0])
		}
	}
	return bounds, nil
}

// modeSectorSize returns how many bytes of the BIN one sector in this mode
// occupies. For every mode CDRWIN defines this is the number in the mode
// string; AUDIO and CDG imply theirs.
func modeSectorSize(mode string) (int, error) {
	switch strings.ToUpper(mode) {
	case "AUDIO", "MODE1/2352", "MODE2/2352", "CDI/2352":
		return 2352, nil
	case "CDG":
		return 2448, nil
	case "MODE2/2336", "CDI/2336":
		return 2336, nil
	case "MODE2/2324":
		return 2324, nil
	case "MODE1/2048", "MODE2/2048":
		return 2048, nil
	}
	return 0, fmt.Errorf("cuebin: unrecognized track mode %q", mode)
}

// BinSectorSize returns the BIN file's per-sector byte size, taken from the
// track modes.
//
// An unrecognized mode is an error rather than a guess: assuming 2352 would
// make a legal MODE2/2336 sheet drift 16 bytes per sector without any warning.
// Tracks that disagree are an error for the same reason: a sector index is a
// position in the file, and two strides would give it two different positions.
func BinSectorSize(tracks []CueTrack) (int, error) {
	if len(tracks) == 0 {
		return 0, errors.New("cuebin: the cue sheet declares no tracks")
	}
	var size int
	for _, tr := range tracks {
		n, err := modeSectorSize(tr.Mode)
		if err != nil {
			return 0, fmt.Errorf("cuebin: track %d: %w", tr.Number, err)
		}
		if size != 0 && n != size {
			return 0, fmt.Errorf("cuebin: track %d is %s at %d bytes per sector where an earlier track is %d; a BIN has one stride",
				tr.Number, tr.Mode, n, size)
		}
		size = n
	}
	return size, nil
}

// IsAudio reports whether a track mode string represents an audio track.
func IsAudio(mode string) bool {
	return strings.ToUpper(mode) == "AUDIO"
}

// parseMSF parses a "MM:SS:FF" position string.
//
// Every field is bounded, since a CD tops out at 99:59:74 and all later
// address arithmetic depends on these fields. A negative or overflowing field
// yields a negative LBA, which would then index a per-sector slice.
func parseMSF(s string) (MSF, error) {
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return MSF{}, fmt.Errorf("invalid MSF %q", s)
	}
	limits := [3]int{99, 59, 74}
	var v [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return MSF{}, fmt.Errorf("invalid MSF %q", s)
		}
		if n < 0 || n > limits[i] {
			return MSF{}, fmt.Errorf("invalid MSF %q: field %d is %d, outside 0-%d", s, i+1, n, limits[i])
		}
		v[i] = n
	}
	return MSF{M: v[0], S: v[1], F: v[2]}, nil
}

// splitCueFields splits a cue line into fields, treating quoted strings as single fields.
func splitCueFields(line string) []string {
	var fields []string
	for len(line) > 0 {
		line = strings.TrimLeft(line, " \t")
		if len(line) == 0 {
			break
		}
		if line[0] == '"' {
			end := strings.Index(line[1:], "\"")
			if end < 0 {
				fields = append(fields, line[1:])
				break
			}
			fields = append(fields, line[1:1+end])
			line = line[2+end:]
		} else {
			end := strings.IndexAny(line, " \t")
			if end < 0 {
				fields = append(fields, line)
				break
			}
			fields = append(fields, line[:end])
			line = line[end:]
		}
	}
	return fields
}
