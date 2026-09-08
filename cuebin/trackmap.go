package cuebin

import (
	"errors"
	"io"

	"samuelks.com/datarescue/ddrescue"
)

// TrackMap is a ddrescue map of an exported track, in the exported file's
// offsets rather than the disc's. It records where the export holds zeros in
// place of unrecovered data, so the next tool down the chain can read the
// file through its damage.
//
// A source with no rescue map produces a nil TrackMap. Nothing is known about
// that disc's damage, so nothing can be claimed about the export's: an
// all-finished map would assert a recovery that was never measured. Every
// method is nil-safe, and the caller writes no sidecar.
type TrackMap struct {
	b    *ddrescue.MapBuilder
	pos  int64 // end of what has been recorded, in the exported file's offsets
	lost int64
}

// newTrackMap returns the map for an export from blocks, or nil when the source
// carried no map. unit is the exported file's own sector stride.
func newTrackMap(blocks []ddrescue.Block, unit int64) *TrackMap {
	if len(blocks) == 0 {
		return nil
	}
	return &TrackMap{b: ddrescue.NewMapBuilder(unit)}
}

// Damaged reports whether any of the exported bytes are zeros standing in for
// what the rescue never recovered.
func (m *TrackMap) Damaged() bool { return m.Lost() > 0 }

// Lost returns how many bytes of the exported file are such zeros.
func (m *TrackMap) Lost() int64 {
	if m == nil {
		return 0
	}
	return m.lost
}

// WriteTo writes the map file. It satisfies io.WriterTo.
func (m *TrackMap) WriteTo(w io.Writer) (int64, error) {
	if m == nil {
		return 0, errors.New("cuebin: the source carried no rescue map, so the export has none to write")
	}
	return m.b.WriteTo(w)
}

// fail records [off, off+size) of the exported file as never recovered, marking
// everything before it recovered. Ranges arrive in increasing order; one that
// reaches back into what is already recorded — a hole rounded outward to a
// sample boundary — is clipped rather than written backwards.
func (m *TrackMap) fail(off, size int64) {
	if m == nil {
		return
	}
	end := off + size
	if end <= m.pos {
		return
	}
	off = max(off, m.pos)
	m.b.RecordRange(m.pos, off-m.pos, false)
	m.b.RecordRange(off, end-off, true)
	m.lost += end - off
	m.pos = end
}

// done records everything up to end as recovered.
func (m *TrackMap) done(end int64) {
	if m == nil || end <= m.pos {
		return
	}
	m.b.RecordRange(m.pos, end-m.pos, false)
	m.pos = end
}
