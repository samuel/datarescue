// Package fstool is the shared body of the filesystem tool: the driver
// interface each format implements, and the commands written once over it.
//
// Most of the work is the same for every format: locating a volume in an
// image, reading it through a rescue map, listing it, and writing its files out
// with the holes recorded. What differs per format is what a volume reports
// about itself (info), what checking its structure means (verify), and a few
// flags that control format-specific behavior. So the shared commands live here
// and the drivers supply the rest.
//
// The registry of drivers lives in the command, not here: a driver package
// imports this one, so this one cannot import them back.
package fstool

import (
	"errors"
	"fmt"
	"io"
	"time"

	"samuelks.com/datarescue/ddrescue"
	"samuelks.com/datarescue/diskimage"
	"samuelks.com/datarescue/internal/appledouble"
)

// Driver is one filesystem format the tool knows: how to recognize it, how to
// open it, and which flags are only its.
//
// A Driver is stateless. A probe returns what it learns through Verdict, so the
// same instance can probe several candidate offsets in one scan without results
// from one offset leaking into the next.
type Driver interface {
	// Name is the -fs value and the word every message uses: "fat", "ext",
	// "iso", "hfs".
	Name() string
	// Summary is the one line -fs help and a rejection message show.
	Summary() string

	// Flags registers this driver's own flags. Every driver's flags are
	// registered on every subcommand, since the command line is parsed long
	// before the format is known; one that was set and turns out not to apply
	// is rejected after detection as a subcmd.UsageError.
	Flags(fs *FlagSet)

	// Probe reports whether this driver reads the volume at off within the
	// image. It must not retain the reader.
	Probe(r io.ReadSeeker, off int64) Verdict

	// PartitionMatches narrows which MBR partition types are worth trying when
	// -fs selects this driver explicitly. Nil tries every used partition, which
	// is what a probe across every driver does anyway.
	PartitionMatches(partitionType byte) bool

	// Open opens the volume diskimage located. hint is what the probe learned
	// and Open would otherwise redo; it is nil when -fs skipped the probe, so
	// Open has to work without it.
	Open(im *diskimage.Image, hint any) (Volume, error)
}

// VerdictKind is what a probe concluded.
//
// It has three values rather than being a bool. An image whose boot sector or
// volume descriptor was never recovered is not "some other format". Reporting
// it as one would answer "no filesystem found" in exactly the case a rescue map
// exists for.
type VerdictKind int

const (
	// No means these are not this format's bytes.
	No VerdictKind = iota
	// Yes means this driver reads what is here.
	Yes
	// Damaged means the bytes needed to decide were never recovered.
	Damaged
)

// Verdict is what one probe of one offset came to.
type Verdict struct {
	Kind VerdictKind
	// Hint is what the probe learned that Open would otherwise redo — the
	// sector size an ISO image turned out to have. It rides on the verdict
	// rather than on the driver so the driver stays stateless.
	Hint any
	// Err says why, for Damaged. It is reported verbatim, so a sentinel like
	// iso.ErrPVDUnrecovered still reaches the operator even when detection found
	// no filesystem.
	Err error
}

// Recognized is the verdict of a driver that reads what is at the offset.
func Recognized(hint any) Verdict { return Verdict{Kind: Yes, Hint: hint} }

// Unrecovered is the verdict of a driver that could not decide, because the
// bytes it needed were never recovered.
func Unrecovered(err error) Verdict { return Verdict{Kind: Damaged, Err: err} }

// Rejected is the verdict of a driver that reads some other format.
func Rejected() Verdict { return Verdict{Kind: No} }

// Volume is an opened filesystem in the terms the shared commands need.
// Anything a format records beyond this belongs in its own Info or Verify.
type Volume interface {
	// Bytes is the volume's own length, which damage is measured against.
	// Measuring from the start of the image would count whatever precedes the
	// partition as this filesystem's loss.
	Bytes() int64

	// Walk calls fn for every entry, directories included, in the order the
	// volume records them. That is tree order for all four formats, so a
	// recursive listing can group by directory without holding a tree.
	//
	// It returns an error only when the volume itself stopped the walk. What
	// the walk could not read comes back as a Diagnostic, so one lost directory
	// sector loses only the entries it held and not the rest of the tree.
	Walk(fn func(Entry) error) ([]Diagnostic, error)

	// Lookup resolves one path without walking, so a requested file can be
	// recovered from a volume whose tree will not enumerate.
	//
	// It returns ErrNotFound when the volume does not hold the path (a failure
	// of the run), and any other error when a directory on the way would not
	// read (damage to the medium). The two get different exit codes. A format
	// with no index returns ErrNoLookup and the caller selects during the walk
	// instead.
	Lookup(path string) (Entry, error)

	// Open returns a reader over the entry's data. The reader is the driver's
	// own concrete type, so Fill can arm the gap log: unreadable bytes become
	// zeros, and the map recording them is written in the same call.
	Open(e Entry) (Reader, error)

	// Damage is how much of the entry's data lies in ranges the rescue never
	// recovered, and in how many separate ranges.
	//
	// It reads nothing. The volume's extents give where the entry lives and the
	// map gives what was recovered, so the files a bad region affected can be
	// reported before anything reads them. Zero where the format cannot tell.
	Damage(e Entry) (lost int64, ranges int)

	// Diagnostics drains what the volume noticed. It clears the list, so a
	// caller drains it right after the read that produced it; otherwise a
	// pending one is reported against whichever file is read next.
	Diagnostics() []Diagnostic

	// Info writes the format's own field dump. There is no shared shape: FAT's
	// cluster arithmetic and ISO's extensions have nothing in common. The
	// caller prints the two lines every format shares, and the rescue map,
	// around this.
	Info(w io.Writer) error

	io.Closer
}

// Reader is a driver's file reader: what it could not read becomes zeros and a
// map recording them.
//
// This is deliberately more than io.Reader. outsink.WriteFilled is the only
// way to do a gap-filling write, and it needs a reader that accepts the log.
type Reader interface {
	io.Reader
	Fill(*ddrescue.GapLog)
}

// ShiftedFiller is a reader that can record its holes at an offset into the
// file being written, for bytes that land after a header.
//
// Only a format that writes something in front of a stream needs it, and a
// reader without it cannot be used that way. Recording the holes at the
// stream's own offsets would mark the header as lost and put every hole one
// header length too early: a map that is wrong about the file, not just
// incomplete.
type ShiftedFiller interface {
	Reader
	FillAt(l *ddrescue.GapLog, base int64)
}

// Entry is one thing in a volume.
type Entry struct {
	// Path is relative to the volume root, with no leading slash and "/"
	// separators. Every command matches against this form, and each driver
	// converts its own paths to it.
	//
	// A driver whose names may contain a slash (HFS, where "/" is an ordinary
	// filename character and ":" is the separator) must escape it or carry
	// components rather than joining, or one file becomes a tree.
	Path string
	Dir  bool
	// Size is what the volume records, or -1 where it records none.
	Size    int64
	ModTime time.Time
	// Mode is a listing's first column, spelled by the driver: "drhsaD" for
	// FAT, "drwxr-xr-x" for ext2.
	Mode string
	// Symlink is the target when the entry is a symlink. Such an entry is
	// listed but never recreated, so an image cannot place a link the operator
	// did not choose.
	Symlink string
	// Special marks an entry that is neither file, directory nor symlink — a
	// device node, a socket — which extract counts as skipped.
	Special bool
	// Deleted marks an entry the volume no longer references, which a listing
	// shows only under the driver's own flag for it.
	Deleted bool

	// Key is the driver's own handle for the entry, so Open and Damage need not
	// look it up again. It is opaque to everything but the driver that set it.
	Key any
}

// Diagnostic is something a volume noticed.
type Diagnostic struct {
	// Path is the entry it belongs to, empty for one about the volume itself.
	Path string
	// Kind groups similar diagnostics so they can be capped. One bad region
	// produces one per entry it affected, and thousands of near-identical lines
	// would bury every other finding.
	Kind string
	// Damage marks a problem with the medium, as opposed to the volume
	// recording something merely odd. Only damage leads to ExitDegraded.
	Damage bool
	Text   string
}

func (d Diagnostic) String() string {
	if d.Path == "" {
		return d.Text
	}
	return fmt.Sprintf("%s: %s", d.Path, d.Text)
}

var (
	// ErrNotFound reports that the volume does not hold the path asked for.
	// That is a failure of the run, not damage to the medium.
	ErrNotFound = errors.New("fstool: no such path in the volume")
	// ErrNoLookup reports a format with no index to resolve a path against, so
	// the caller selects during the walk instead.
	ErrNoLookup = errors.New("fstool: this format has no index; paths are selected during the walk")
)

// Verifier is a volume with structural checks of its own: FAT's table copies,
// ext2's inode validation. A volume without them is verified by the shared walk
// alone, which still reads every file and reports what the map says each one
// lost.
type Verifier interface {
	Verify(rep *Report, verbose bool)
}

// APMMatcher is a driver that can narrow which Apple partition map entries are
// worth probing, by pmParType. A driver without it is offered every entry that
// could hold a filesystem.
type APMMatcher interface {
	APMPartitionMatches(partitionType string) bool
}

// TailVerifier is a volume with findings that only make sense after the walk,
// such as an allocation summary that walks the tree again and so belongs after
// the walk's own findings.
type TailVerifier interface {
	VerifyTail(rep *Report)
}

// Orphaner is a volume that can find data allocated but reachable from no
// directory entry. Only FAT can, so the subcommand is a usage error elsewhere.
type Orphaner interface {
	Orphans(w io.Writer, verbose bool) (found int, err error)
}

// Forked is a volume whose entries carry more than one stream, which only HFS
// does. A resource fork is as much the operator's data as the data fork, and
// dropping it silently loses most of a classic Mac disk.
type Forked interface {
	// ResourceSize is the entry's resource fork length, 0 where it has none.
	// It answers without reading, so an extract can decide what to write before
	// touching the medium.
	ResourceSize(e Entry) int64
	// OpenResource returns a reader over the entry's resource fork.
	OpenResource(e Entry) (Reader, error)
	// Sidecar describes the AppleDouble companion for an entry: the Finder
	// information and dates that travel with the fork. name is the final path
	// component the data fork is written under, so the volume's own spelling
	// can be recorded whenever the two differ. Otherwise a name this tool
	// sanitized, shortened, or gave an extension from the type code would be the
	// only record of what the disk called the file.
	Sidecar(e Entry, name string) appledouble.Header
}

// FileKind is what a volume records about a file's kind outside its name.
type FileKind struct {
	// Code is what the volume recorded, verbatim, such as "TIFF/8BIM": the type
	// says what the file holds and the creator which application wrote it. A
	// listing prints the medium's own bytes, not this tool's interpretation.
	Code string
	// Ext is the extension that code implies, including the leading dot. It is
	// empty for a code the tool does not map. An extension is a claim about what
	// a file holds, so a wrong one is worse than none.
	Ext string
}

// Typed is a volume whose entries carry their kind outside their name.
//
// A classic Mac file has no extension. Its type is stored in the catalog's
// four-character Finder type code, and the application that opened it in the
// creator code. Written out under its own name, it reaches a modern system as
// "Photo" with nothing saying it holds a TIFF. HFS is the only format that
// does this, as it is the only one with two forks.
//
// Kind reads nothing: the codes are in the catalog record the walk already
// parsed, so an extract decides what to call a file before touching the
// medium. A directory has no kind. Its Finder bytes hold the window position
// the Finder opens it at, which a zero test would not reject, so a driver
// returns blank rather than letting the table decide.
type Typed interface {
	Kind(e Entry) FileKind
}

// forkReaderShifts reports whether the volume's resource-fork reader can shift
// the holes it records, which a companion written after a header needs.
func forkReaderShifts(fv Forked, e Entry) bool {
	r, err := fv.OpenResource(e)
	if err != nil {
		return false
	}
	_, ok := r.(ShiftedFiller)
	return ok
}
