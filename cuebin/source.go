package cuebin

import (
	"samuelks.com/datarescue/ddrescue"
)

// Source is a BIN opened through its rescue map. It offers the two views a
// damaged image has to be read in.
//
// Which view an entry point uses matters. VerifyBIN reads Raw, because it
// compares the bytes the map calls bad against the EDC: through a checked
// reader, EDCBadMapGood (corruption the map missed) and EDCGoodMapBad (a stale
// map) could never occur. Everything that decodes structure reads Checked, so
// a range the rescue never recovered is never parsed as a directory record or
// a partition map. Without a map the two are the same reader.
type Source struct {
	*ddrescue.Source
}

// OpenSource opens the BIN at path using the map policy in opts.
func OpenSource(path string, opts ddrescue.Options) (*Source, error) {
	s, err := ddrescue.OpenSource(path, opts)
	if err != nil {
		return nil, err
	}
	return &Source{Source: s}, nil
}

// unrecovered returns the parts of a byte range the map marks as not
// recovered. An absent map is not an empty one: with no blocks nothing is
// known to be missing, whereas ddrescue.Unrecovered would call the whole range
// non-tried.
func unrecovered(blocks []ddrescue.Block, off, size int64) []ddrescue.Block {
	if len(blocks) == 0 {
		return nil
	}
	return ddrescue.Unrecovered(blocks, off, size)
}
