// Package hfsdrv reads Apple HFS volumes — "Mac OS Standard", not HFS+ — for
// the filesystem tool.
package hfsdrv

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"samuelks.com/datarescue/apm"
	"samuelks.com/datarescue/diskimage"
	"samuelks.com/datarescue/hfs"
	"samuelks.com/datarescue/internal/appledouble"
	"samuelks.com/datarescue/internal/fstool"
	"samuelks.com/datarescue/internal/humanize"
	"samuelks.com/datarescue/internal/mactype"
	"samuelks.com/datarescue/internal/safepath"
)

// Driver reads HFS.
type Driver struct {
	tz string
	// loc is the zone given by tz, resolved once.
	loc *time.Location
}

// New returns the HFS driver.
func New() *Driver { return &Driver{} }

func (*Driver) Name() string    { return "hfs" }
func (*Driver) Summary() string { return "Apple HFS" }

// Flags registers the zone override.
func (d *Driver) Flags(fs *fstool.FlagSet) {
	fs.StringVar(&d.tz, "tz", "",
		"the zone the disk's clock was set to, for its timestamps (default UTC)")
}

// location resolves the zone the timestamps are read in.
//
// HFS records local wall-clock seconds and no zone, so UTC is the default: it
// is deterministic and explicit, where any other default would be a guess. -tz
// is for an operator who knows where the disk was written.
func (d *Driver) location() (*time.Location, error) {
	if d.loc != nil {
		return d.loc, nil
	}
	if d.tz == "" {
		d.loc = time.UTC
		return d.loc, nil
	}
	loc, err := time.LoadLocation(d.tz)
	if err != nil {
		return nil, fmt.Errorf("-tz %q: %w", d.tz, err)
	}
	d.loc = loc
	return loc, nil
}

// Probe reports whether an HFS volume starts at off.
//
// A master directory block that reads cleanly but has the wrong signature is
// some other format. One that would not read is the case the rescue map exists
// for. An HFS wrapper around an HFS+ volume is a third case and is reported as
// such: it opens as an almost-empty volume, and "this disk is empty" is the
// worst wrong answer a recovery tool can give.
func (*Driver) Probe(r io.ReadSeeker, off int64) fstool.Verdict {
	ra, ok := r.(io.ReaderAt)
	if !ok {
		// An 86F image is decoded track by track, and no HFS volume lives in an
		// 86Box PC floppy image.
		return fstool.Rejected()
	}
	// The probe cannot know the volume's length, so it opens without one. That
	// loses only the spare master directory block, which Open retries with the
	// size once the partition is known.
	_, err := hfs.Open(ra, off, 0)
	switch {
	case err == nil:
		return fstool.Recognized(nil)
	case errors.Is(err, hfs.ErrNotHFS):
		return fstool.Rejected()
	}
	return fstool.Unrecovered(err)
}

// PartitionMatches narrows nothing on an MBR disk: HFS is normally found in an
// Apple partition map, and when a volume is in an MBR partition the type byte
// says little.
func (*Driver) PartitionMatches(byte) bool { return true }

// APMPartitionMatches narrows auto-detection to the Apple partition types that
// hold an HFS volume.
func (*Driver) APMPartitionMatches(t string) bool { return apm.IsHFS(t) }

// Open opens the volume. The size comes from the image, which on a partitioned
// disk is the partition's own length. The spare master directory block is two
// logical blocks from the end of the volume, and nothing inside the volume
// records where that is.
func (d *Driver) Open(im *diskimage.Image, _ any) (fstool.Volume, error) {
	loc, err := d.location()
	if err != nil {
		return nil, err
	}
	ra, err := im.ReaderAt()
	if err != nil {
		return nil, err
	}
	size := im.Size - im.Offset
	v, err := hfs.Open(ra, 0, size)
	if err != nil {
		return nil, err
	}
	return &volume{v: v, im: im, loc: loc, size: size}, nil
}

type volume struct {
	v    *hfs.Volume
	im   *diskimage.Image
	loc  *time.Location
	size int64
	// entries caches the catalog, which every operation needs and which costs a
	// walk of the whole B-tree to build.
	entries []hfs.Entry
	issues  []hfs.CatalogIssue
	loaded  bool
}

// Bytes is the volume's own length.
func (vo *volume) Bytes() int64 {
	return int64(vo.v.MDB.AllocationBlocks) * int64(vo.v.MDB.AllocationBlockSize)
}

func (vo *volume) Close() error { return nil }

// key carries the catalog entry, so Open and Damage reach the forks without
// walking the catalog again.
type key struct{ e hfs.Entry }

func (vo *volume) load() error {
	if vo.loaded {
		return nil
	}
	entries, issues, err := vo.v.List()
	if err != nil {
		return err
	}
	vo.entries, vo.issues, vo.loaded = entries, issues, true
	return nil
}

func (vo *volume) Walk(fn func(fstool.Entry) error) ([]fstool.Diagnostic, error) {
	if err := vo.load(); err != nil {
		return nil, err
	}
	for _, e := range vo.entries {
		if err := fn(vo.entryFor(e)); err != nil {
			return vo.catalogDiags(), err
		}
	}
	return vo.catalogDiags(), nil
}

func (vo *volume) catalogDiags() []fstool.Diagnostic {
	out := make([]fstool.Diagnostic, 0, len(vo.issues))
	for _, ci := range vo.issues {
		out = append(out, fstool.Diagnostic{Path: ci.Path, Kind: "catalog", Damage: true, Text: ci.String()})
	}
	vo.issues = nil
	return out
}

func (vo *volume) entryFor(e hfs.Entry) fstool.Entry {
	mode := "-rw-rw-rw-"
	if e.IsDir {
		mode = "drwxrwxrwx"
	}
	if e.FinderFlags&0x1000 != 0 { // kNameLocked
		mode = strings.ReplaceAll(mode, "w", "-")
	}
	return fstool.Entry{
		Path:    joinPath(e.Path),
		Dir:     e.IsDir,
		Size:    e.DataSize,
		ModTime: e.Modified.Time(vo.loc),
		Mode:    mode,
		Key:     key{e: e},
	}
}

// joinPath renders an HFS path as the slash-separated one every command matches
// against.
//
// A "/" inside a component is rewritten to ":" first. HFS's own separator is
// ":" and "/" is an ordinary filename character, so the two swap roles when
// converting, which is the same substitution macOS makes. Otherwise a file
// named "10/20/99 Budget" would split into three components and an extract
// would build a tree out of one file.
func joinPath(parts []string) string {
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = strings.ReplaceAll(p, "/", ":")
	}
	return strings.Join(out, "/")
}

// Lookup has no index to use: HFS resolves a path through the catalog, which
// the walk already loads whole.
func (vo *volume) Lookup(string) (fstool.Entry, error) {
	return fstool.Entry{}, fstool.ErrNoLookup
}

func (vo *volume) Open(e fstool.Entry) (fstool.Reader, error) {
	k, ok := e.Key.(key)
	if !ok {
		return nil, fmt.Errorf("hfsdrv: %s has no catalog entry", e.Path)
	}
	return vo.v.OpenFile(k.e)
}

// Damage is how much of an entry's data lies in ranges the rescue never
// recovered, counted over both forks: a file whose resource fork is on a bad
// sector is damaged, and counting only the data fork would report it clean.
func (vo *volume) Damage(e fstool.Entry) (int64, int) {
	k, ok := e.Key.(key)
	if !ok || vo.im.MapPath == "" {
		return 0, 0
	}
	var lost int64
	var ranges int
	for _, ref := range []hfs.ForkRef{k.e.DataFork(), k.e.ResourceFork()} {
		if ref.Size == 0 {
			continue
		}
		rs, err := vo.v.ForkRanges(ref)
		if err != nil {
			continue
		}
		for _, rg := range rs {
			for _, b := range vo.im.Unrecovered(vo.im.Offset+rg.ImageOffset, rg.Length) {
				lost += b.Size
				ranges++
			}
		}
	}
	return lost, ranges
}

func (vo *volume) Diagnostics() []fstool.Diagnostic {
	ds := vo.v.Diagnostics()
	out := make([]fstool.Diagnostic, 0, len(ds))
	for _, d := range ds {
		out = append(out, fstool.Diagnostic{Kind: string(d.Kind), Damage: true, Text: d.String()})
	}
	return out
}

func (vo *volume) Info(w io.Writer) error {
	m := vo.v.MDB
	p := func(format string, args ...any) {
		fmt.Fprintf(w, format+"\n", args...) //nolint:errcheck // report output
	}
	p("Volume name:  %s", m.Label())
	p("Created:      %s", stamp(m.CreatedTimestamp, vo.loc))
	p("Modified:     %s", stamp(m.ModifiedTimestamp, vo.loc))
	p("Block size:   %d bytes", m.AllocationBlockSize)
	p("Blocks:       %d", m.AllocationBlocks)
	p("Free blocks:  %d", m.UnusedBlocks)
	p("Size:         %s", humanize.Bytes(vo.Bytes()))
	p("Files:        %d", m.TotalFileCount)
	p("Directories:  %d", m.TotalDirectoryCount)
	return nil
}

func stamp(t uint32, loc *time.Location) string {
	mt := hfs.MacTime(t)
	if mt.IsZero() {
		return "not recorded"
	}
	return mt.Time(loc).Format(time.DateTime)
}

// The optional interfaces are checked dynamically, by a type assertion in the
// shared command code. If a method's signature drifts from the interface, it
// stops implementing it without any compile error: the volume would report
// "not forked" and an extract would silently drop every resource fork on the
// disk. These compile-time assertions prevent that.
var (
	_ fstool.Forked = (*volume)(nil)
	_ fstool.Typed  = (*volume)(nil)
)

// ResourceSize is the entry's resource fork length, answered from the catalog
// without reading the medium.
func (vo *volume) ResourceSize(e fstool.Entry) int64 {
	k, ok := e.Key.(key)
	if !ok {
		return 0
	}
	return k.e.RsrcSize
}

// OpenResource returns a reader over the entry's resource fork.
func (vo *volume) OpenResource(e fstool.Entry) (fstool.Reader, error) {
	k, ok := e.Key.(key)
	if !ok {
		return nil, fmt.Errorf("hfsdrv: %s has no catalog entry", e.Path)
	}
	return vo.v.OpenResourceFork(k.e)
}

// Sidecar describes the AppleDouble companion for an entry.
//
// name is the final path component the data fork is written under. The
// volume's own spelling is recorded whenever the two differ, for any reason: a
// "/" or ":" rewritten on output, a character the sink sanitized, a name
// shortened to fit, or an extension derived from the type code. Only the
// caller knows the final name, which is why it is a parameter rather than
// something this method computes.
func (vo *volume) Sidecar(e fstool.Entry, name string) appledouble.Header {
	k, ok := e.Key.(key)
	if !ok {
		return appledouble.Header{}
	}
	h := appledouble.Header{
		FinderInfo: k.e.FinderInfo(),
		Created:    k.e.Created.Time(vo.loc),
		Modified:   k.e.Modified.Time(vo.loc),
		Backup:     k.e.Backup.Time(vo.loc),
		RsrcSize:   k.e.RsrcSize,
	}
	if name != k.e.Name {
		h.RealName = k.e.Name
	}
	return h
}

// Kind is what the catalog's Finder information says the file is.
//
// A folder has Finder information too, but its first four bytes are the window
// position the Finder opens it at, not a type code. They are plausible bytes
// that a zero test would not reject, so a directory gets a blank answer here
// rather than being left for the table to decide.
//
// A file with only one of the two codes is reported with the other missing:
// "/8BIM" means the creator is recorded but the type is not. A code read from
// a damaged catalog can hold anything, so one that is not printable is
// dropped.
func (vo *volume) Kind(e fstool.Entry) fstool.FileKind {
	k, ok := e.Key.(key)
	if !ok || k.e.IsDir {
		return fstool.FileKind{}
	}
	typ, creator := printableCode(k.e.Type.String()), printableCode(k.e.Creator.String())
	if typ == "" && creator == "" {
		return fstool.FileKind{}
	}
	code := typ
	if creator != "" {
		code += "/" + creator
	}
	ext, _ := mactype.Extension(typ, creator)
	return fstool.FileKind{Code: code, Ext: ext}
}

func printableCode(code string) string {
	if !safepath.IsPrintable(code) {
		return ""
	}
	return code
}
