package outsink

import (
	"errors"
	"fmt"
	"io"

	"samuelks.com/datarescue/ddrescue"
	"samuelks.com/datarescue/internal/humanize"
)

// MapSuffix is appended to a recovered file's name to form the name of its map,
// so the tool that reads the file finds the map the same way every tool here
// finds a sidecar.
const MapSuffix = ".map"

// Written is the result of writing one entry, with the destination names for
// messages.
type Written struct {
	ddrescue.Filled
	// Dest is the destination name to show in messages: a path relative to the
	// output directory, or the archive member name.
	Dest string
}

// GapMessage is the single line reported for a damaged file, so every tool
// describes the same damage the same way.
func (w Written) GapMessage() string {
	return fmt.Sprintf("%s in %d range(s) could not be read; zero-filled and recorded in %s",
		humanize.Bytes(w.GapBytes), len(w.Gaps), w.MapName)
}

// WriteFilled writes one recovered file to the sink and, where the reader
// zero-filled anything, the ddrescue map recording where: "<name>.map", a
// member of its own inside an archive.
//
// It is the only way an extract command gets a gap-filling reader: open is
// handed the armed log, and only ddrescue.Fill arms one. So a file cannot be
// written with holes in it unless the map recording them is written too. If
// the map cannot be written, the data is taken back from the sink, leaving a
// missing file rather than a full-length one with unexplained zeros.
//
// The returned error is a failure of the run: the destination's write, its
// close, or the map. An error from the medium that stopped the read is a
// finding about the medium and comes back in Written.Err, with the bytes
// recovered before it kept.
func WriteFilled(s Sink, e Entry, open func(*ddrescue.GapLog) (io.Reader, error)) (Written, error) {
	out, dest, err := s.Create(e)
	if err != nil {
		return Written{Dest: e.Path}, err
	}
	mapDest := func() (io.WriteCloser, string, error) {
		return s.Create(Entry{Path: e.Path + MapSuffix, ModTime: e.ModTime, Origin: mapOrigin(e)})
	}
	filled, err := ddrescue.Fill(out, mapDest, open)
	w := Written{Filled: filled, Dest: dest}
	if filled.Unrecorded {
		err = errors.Join(err, s.Discard(e))
	}
	return w, err
}

// mapOrigin names the map after the file the medium holds, where the file was
// written under a name this tool derived. An empty origin stays empty: the two
// names are then the same and there is nothing to carry.
func mapOrigin(e Entry) string {
	if e.Origin == "" {
		return ""
	}
	return e.Origin + MapSuffix
}
