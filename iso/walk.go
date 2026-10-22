package iso

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
)

// maxWalkIssues caps the walk's issue list. Every entry in it is built from a
// disc-supplied size or LBA, so a badly damaged image would otherwise grow the
// slice, and the report printed from it, without bound.
const maxWalkIssues = 200

// maxDirIssues caps the issues recorded for one directory. A directory whose
// every sector is unreadable would otherwise fill the list with one repetitive
// line per sector and crowd out every other directory's findings.
const maxDirIssues = 8

// maxDirDepth is the deepest directory nesting the walk follows. It only
// guards the stack: ISO 9660 Level 1 allows eight levels, but Joliet, ISO
// 9660:1999 and discs made by xorriso or hdiutil go deeper, and the visited
// set already stops a tree that loops.
const maxDirDepth = 64

// maxDirSectors bounds one directory's declared size when the volume's own
// sector count cannot (a PVD whose volume size field is zero). A corrupt
// 32-bit size would otherwise drive two million sector reads; real directories
// span a handful of sectors.
const maxDirSectors = 1 << 16

// Directory record file flags.
const (
	flagDirectory   = 0x02
	flagAssociated  = 0x04
	flagMultiExtent = 0x80
)

// walker is one Walk's state, kept apart from the Reader so concurrent walks
// do not share it. ReadDir uses it too, with recurse off.
type walker struct {
	r       *Reader
	fn      func(DirEntry) error
	joliet  bool
	rr      bool // the tree's records carry Rock Ridge entries
	rrSkip  int  // the System Use skip count the tree's SP entry gives
	recurse bool
	visited map[int64]bool
	issues  []string
}

// errSkipRest stops reading the current directory. It is what fs.SkipDir
// means when fn returns it for an entry that is not a directory.
var errSkipRest = errors.New("iso: skip the rest of the directory")

func (w *walker) issue(format string, args ...any) {
	switch {
	case len(w.issues) < maxWalkIssues:
		w.issues = append(w.issues, fmt.Sprintf(format, args...))
	case len(w.issues) == maxWalkIssues:
		w.issues = append(w.issues, fmt.Sprintf("more than %d problems found; the rest are not listed", maxWalkIssues))
	}
}

// Walk calls fn once per entry in directory order. A directory sector that
// cannot be read, a tree that loops, a size the volume cannot hold: all are
// recorded in Issues() and the walk continues. The walk's half of Issues() is
// only meaningful once Walk has run.
//
// fn controls the walk the way it does for filepath.WalkDir. fs.SkipDir
// returned for a directory skips its contents, and returned for anything else
// skips the rest of the directory holding it. fs.SkipAll ends the walk, and
// Walk returns nil. Any other error ends the walk and is returned.
func (r *Reader) Walk(fn func(DirEntry) error) error {
	w := r.newWalker(fn, true)
	root := r.Root()
	err := w.walkDir(root.StartLBA, root.Size, "/", 1)
	r.mu.Lock()
	r.walkIssues = w.issues
	r.mu.Unlock()
	if errors.Is(err, fs.SkipAll) {
		return nil
	}
	return err
}

func (r *Reader) newWalker(fn func(DirEntry) error, recurse bool) *walker {
	t := r.walkedRR()
	return &walker{r: r, fn: fn, joliet: r.UsesJoliet(), rr: t.present, rrSkip: t.skip,
		recurse: recurse, visited: make(map[int64]bool)}
}

// dirWalk is the state of reading one directory: its issue budget, and a
// multi-extent file whose records are still being collected. The records of
// one file are consecutive, but they can cross a sector boundary.
type dirWalk struct {
	w      *walker
	path   string
	depth  int
	issues int

	chain    *DirEntry
	chainRaw string // the chain's file identifier as recorded
}

func (d *dirWalk) issue(format string, args ...any) {
	d.issues++
	switch {
	case d.issues < maxDirIssues:
		d.w.issue(format, args...)
	case d.issues == maxDirIssues:
		d.w.issue("directory %s: further problems in it are not listed", d.path)
	}
}

// emit passes e to fn and reports whether to walk into it.
func (d *dirWalk) emit(e DirEntry) (descend bool, err error) {
	err = d.w.fn(e)
	switch {
	case err == nil:
		return e.IsDir && d.w.recurse, nil
	case errors.Is(err, fs.SkipDir):
		if e.IsDir {
			return false, nil
		}
		return false, errSkipRest
	}
	return false, err
}

// endChain reports the pending multi-extent file, if any, as incomplete and
// passes it on with the extents found so far.
func (d *dirWalk) endChain(why string) error {
	if d.chain == nil {
		return nil
	}
	e := *d.chain
	d.chain = nil
	e.Incomplete = true
	d.issue("%s: multi-extent file %s; listed with the %d bytes of its first %d extent(s)",
		e.Path, why, e.Size, len(e.Extents))
	_, err := d.emit(e)
	return err
}

func (w *walker) walkDir(lba, size int64, path string, depth int) error {
	// A deep acyclic tree would otherwise recurse until the Go stack runs out.
	// parseCE caps its own chain the same way (maxCEDepth).
	if depth > maxDirDepth {
		w.issue("directory tree under %s is deeper than the %d levels this reader follows; the rest is not walked",
			path, maxDirDepth)
		return nil
	}
	// A directory record on a damaged image can point at itself or at an
	// ancestor, and the LBA comes off the disc, so the walk needs its own guard
	// to terminate.
	if w.visited[lba] {
		w.issue("directory %s at LBA %d was already walked; the tree loops back on itself", path, lba)
		return nil
	}
	w.visited[lba] = true

	dirSectors := (size + UserDataSize - 1) / UserDataSize
	// The size comes off the disc. A corrupt one would drive millions of reads
	// over an image that cannot hold that many sectors, so the volume's own
	// sector count bounds it. Hitting that bound is reported, since the entries
	// past it are not missing but never described. When the volume size itself
	// is zero (the PVD region was damaged), the constant cap is used instead.
	// Clamping to zero (never negative) keeps a directory past the end of the
	// volume from silently yielding nothing.
	total := w.r.totalSectors
	switch {
	case total > 0 && dirSectors > total-lba:
		room := total - lba
		w.issue("directory %s declares %d bytes, more than the %d sector(s) of volume after it; reading what fits",
			path, size, max(room, 0))
		dirSectors = max(room, 0)
	case total == 0 && dirSectors > maxDirSectors:
		w.issue("directory %s declares %d bytes with no volume sector count to bound it, more than the %d sectors a directory can span; reading what fits",
			path, size, maxDirSectors)
		dirSectors = maxDirSectors
	}

	d := &dirWalk{w: w, path: path, depth: depth}
	err := d.read(lba, dirSectors)
	if errors.Is(err, errSkipRest) {
		return nil
	}
	return err
}

// read reads the directory's sectors and hands each record to record.
func (d *dirWalk) read(lba, dirSectors int64) error {
	w, path := d.w, d.path
	for sec := range dirSectors {
		data, err := w.r.ReadSector(lba + sec)
		if errors.Is(err, io.EOF) {
			// Past the end of the image nothing more can be read. The volume
			// size bounding the loop comes off the disc, and a damaged one
			// would otherwise drive billions of reads that all fail here.
			d.issue("directory %s runs past the end of the image at sector %d; the rest is not read", path, lba+sec)
			if err := d.endChain("is cut short by the end of the image"); err != nil {
				return err
			}
			break
		}
		if err != nil {
			// One unreadable sector loses only the entries it holds: the rest of
			// this directory and the rest of the tree are still walked.
			d.issue("directory %s: sector %d unreadable, the entries in it are lost: %s", path, lba+sec, err)
			// The rest of a multi-extent file may have been in that sector,
			// and records after it cannot be shown to continue the same file.
			if err := d.endChain("is cut short by the unreadable directory sector"); err != nil {
				return err
			}
			continue
		}
		for offset := 0; offset < len(data); {
			recLen := int(data[offset])
			if recLen == 0 || offset+recLen > len(data) {
				break
			}
			rec := data[offset : offset+recLen]
			offset += recLen
			if err := d.record(rec); err != nil {
				return err
			}
		}
	}
	return d.endChain("ends without a final extent")
}

// record handles one directory record.
func (d *dirWalk) record(rec []byte) error {
	if len(rec) < 34 {
		return nil
	}
	nameLen := int(rec[32])
	if nameLen == 0 || 33+nameLen > len(rec) {
		return nil
	}
	// Skip dot and dotdot entries.
	if nameLen == 1 && (rec[33] == 0x00 || rec[33] == 0x01) {
		return nil
	}

	w := d.w
	flags := rec[25]
	isDir := flags&flagDirectory != 0
	raw := string(rec[33 : 33+nameLen])
	ext := Extent{
		LBA:  int64(binary.LittleEndian.Uint32(rec[2:6])),
		Size: int64(binary.LittleEndian.Uint32(rec[10:14])),
	}

	// A record continuing a multi-extent file repeats its identifier.
	if d.chain != nil {
		if raw == d.chainRaw && !isDir {
			d.chain.Extents = append(d.chain.Extents, ext)
			d.chain.Size += ext.Size
			if flags&flagMultiExtent == 0 {
				e := *d.chain
				d.chain = nil
				_, err := d.emit(e)
				return err
			}
			return nil
		}
		if err := d.endChain(fmt.Sprintf("is followed by %q instead of its next extent", w.r.decodeName(rec, nameLen, w.joliet))); err != nil {
			return err
		}
	}

	name := w.r.decodeName(rec, nameLen, w.joliet)
	fullPath := d.path + name
	e := DirEntry{IsDir: isDir, StartLBA: ext.LBA, Size: ext.Size, Associated: flags&flagAssociated != 0}
	if t, err := parseRecordTime(rec[18:25]); err != nil {
		d.issue("%s: recording date is damaged: %s", fullPath, err)
	} else {
		e.ModTime = t
	}

	// Parse the Rock Ridge System Use Area: NM (alternate name), SL (symlink),
	// PX (POSIX attributes), TF (timestamps), and CL and RE (relocation).
	var rr rockRidge
	if w.rr {
		rr = w.parseRockRidge(rec, nameLen)
		if rr.name != "" {
			fullPath = d.path + rr.name
		}
		for _, msg := range rr.damage {
			d.issue("%s: %s", fullPath, msg)
		}
		rr.apply(&e)
		if isDir && rr.relocated {
			// The relocated copy, reached through its placeholder instead.
			return nil
		}
	}
	if e.Symlink != "" {
		// A symlink has no extent of its own, and is never walked into even
		// when the record is flagged as a directory.
		e.IsDir, e.StartLBA, e.Size = false, 0, 0
		e.Mode = entryMode(e)
		e.Path = fullPath
		_, err := d.emit(e)
		return err
	}
	if rr.hasChildLink && !isDir {
		return d.relocatedDir(e, fullPath, rr.childLink)
	}
	if !isDir {
		e.Extents = []Extent{ext}
		e.Mode = entryMode(e)
		e.Path = fullPath
		if flags&flagMultiExtent != 0 {
			d.chain, d.chainRaw = &e, raw
			return nil
		}
		_, err := d.emit(e)
		return err
	}

	e.Mode = entryMode(e)
	e.Path = fullPath + "/"
	descend, err := d.emit(e)
	if !descend || err != nil {
		return err
	}
	return w.walkDir(ext.LBA, ext.Size, fullPath+"/", d.depth+1)
}

// relocatedDir presents the directory a CL placeholder points at in the
// placeholder's place. Its size comes from the directory's own dot entry,
// since the placeholder's describes the placeholder.
func (d *dirWalk) relocatedDir(e DirEntry, fullPath string, lba int64) error {
	w := d.w
	e.IsDir, e.StartLBA, e.Size, e.Extents = true, lba, 0, nil
	e.Mode = entryMode(e)
	e.Path = fullPath + "/"

	sec, err := w.r.ReadSector(lba)
	switch {
	case err != nil:
		d.issue("%s: relocated directory at LBA %d unreadable, the entries in it are lost: %s", fullPath, lba, err)
		_, err := d.emit(e)
		return err
	case len(sec) < 34 || int(sec[0]) < 34 || sec[32] != 1 || sec[33] != 0x00:
		d.issue("%s: Rock Ridge CL entry points at LBA %d, which does not start with a directory", fullPath, lba)
		_, err := d.emit(e)
		return err
	}
	e.Size = int64(binary.LittleEndian.Uint32(sec[10:14]))
	descend, err := d.emit(e)
	if !descend || err != nil {
		return err
	}
	return w.walkDir(lba, e.Size, fullPath+"/", d.depth+1)
}

// decodeName returns the filename from a directory record, decoding UCS-2 BE
// for Joliet and ASCII otherwise.
func (r *Reader) decodeName(rec []byte, nameLen int, joliet bool) string {
	raw := rec[33 : 33+nameLen]
	var name string
	if joliet {
		name = decodeUCS2(raw)
		// Strip Joliet version suffix: 0x00 0x3B followed by two ASCII digits.
		if i := strings.LastIndex(name, ";"); i >= 0 {
			name = name[:i]
		}
	} else {
		name = string(raw)
		if i := strings.Index(name, ";"); i >= 0 {
			name = name[:i]
		}
		// A name with no extension still carries the separator ("README.;1"),
		// which is not part of the name.
		name = strings.TrimSuffix(name, ".")
	}
	return name
}
