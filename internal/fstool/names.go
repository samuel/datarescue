package fstool

import (
	"path"
	"strings"
	"time"

	"samuelks.com/datarescue/internal/appledouble"
	"samuelks.com/datarescue/internal/outsink"
	"samuelks.com/datarescue/internal/safepath"
)

// outName is the name one entry's files are written under, alongside the name
// the volume itself gives them.
//
// The two can differ. A classic Mac file carries no extension, so the tool
// derives one from what the catalog records and appends it. One stem serves
// every file the entry produces: the data fork, its "._" companion, its
// ".rsrc", and the ".map" beside any of them. macOS matches a companion to its
// fork by exact filename, so if the stem were shortened differently for the
// two, they would never be matched.
type outName struct {
	// dir is the leading components as the image spells them, left for the sink
	// to sanitize with the rest of the path.
	dir string
	// stem is the volume's own name, sanitized and already cut short enough for
	// everything the tool will add to it.
	stem string
	// ext is what the tool derived, empty where it derived nothing.
	ext string
	// src is the volume's own path, which findings about the medium are
	// reported under.
	src string
}

func (n outName) data() string    { return n.dir + n.stem + n.ext }
func (n outName) rsrc() string    { return n.data() + ".rsrc" }
func (n outName) sidecar() string { return n.dir + appledouble.Prefix + n.stem + n.ext }

// derived reports whether the tool added anything to the volume's own name.
func (n outName) derived() bool { return n.data() != n.src }

// entry is what the sink is given: the name to write, and the medium's own
// name for messages about the medium rather than the run.
func (n outName) entry(p string, mod time.Time) outsink.Entry {
	e := outsink.Entry{Path: p, ModTime: mod}
	if n.derived() {
		e.Origin = n.src
	}
	return e
}

// nameFor decides what one entry is written as.
//
// The extension is only ever appended, never substituted, so the volume's own
// name is always a prefix of what was written and none of it is removed.
func nameFor(c *Context, xf *ExtractFlags, e Entry) outName {
	n := outName{src: e.Path}
	if tv, ok := c.Volume.(Typed); ok && xf.Ext {
		if ext := tv.Kind(e).Ext; !alreadyNamed(e.Path, ext) {
			n.ext = ext
		}
	}
	// Reserve room up front for everything the tool will add to the stem: the
	// extension, the "._" a companion takes, and the ".map" a file written with
	// holes takes. Reserving here rather than per file keeps one stem for all of
	// an entry's files. A companion two bytes shorter than its fork is never
	// matched with it, and a map must not end up with the name of the file it
	// describes.
	reserve := len(n.ext) + len(outsink.MapSuffix)
	switch xf.Forks {
	case ForksAppleDouble:
		reserve += len(appledouble.Prefix)
	case ForksSeparate:
		reserve += len(".rsrc")
	}
	n.dir, n.stem = safepath.Stem(e.Path, reserve)
	return n
}

// alreadyNamed reports whether the name already ends in the extension the
// volume's record implies. That is common for a file that reached the Mac from
// a PC: README.TXT is type TEXT, and "README.TXT.txt" would repeat the
// extension. The comparison ignores case, and the volume's own spelling is
// kept.
//
// A name ending in a different extension still gets ours appended. The catalog
// is the authority on what the file holds, and the existing suffix is part of
// the name the medium supplied.
func alreadyNamed(name, ext string) bool {
	return ext == "" || strings.EqualFold(path.Ext(name), ext)
}

// derivedNames is the set of output names this run built rather than took from
// the volume.
//
// It exists for one message. Two image names that become one output name are a
// failure of the run, rejected the same way as names that sanitize alike. When
// a derived extension caused the collision, the operator needs to know the tool
// did it and how to stop it. Which of the two entries the sink rejects depends
// on walk order, so the set is keyed on the name rather than on the entry that
// failed. Either way the message reports the collision.
type derivedNames map[string]bool

func (d derivedNames) claim(n outName) {
	if n.derived() {
		d[n.data()] = true
	}
}
