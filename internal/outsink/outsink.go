// Package outsink is the destination an extract command writes recovered files
// to: a directory of loose files, a zip, or a gzipped tar.
//
// The tools all read damaged media, so the destination has to tolerate a file
// that stops partway and record what went wrong. A directory keeps the partial
// file and reports on stderr; an archive keeps the partial member and carries
// the same report inside itself, so a recovery moved off the machine as a
// single file can still be interpreted on its own.
package outsink

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"samuelks.com/datarescue/internal/subcmd"
)

// Output formats, as spelled on the command line.
const (
	FormatDir = "dir"
	FormatZip = "zip"
	FormatTGZ = "tgz"
)

// ErrDuplicate reports two image paths that sanitize to one archive member
// name. A member cannot be overwritten in place the way a file can, so the
// second one is refused rather than written twice.
var ErrDuplicate = errors.New("duplicate archive member")

// Entry describes one recovered file about to be written.
type Entry struct {
	// Path is the path as the image records it; the sink sanitizes it.
	Path string
	// ModTime is the modification time the medium recorded, zero when it
	// recorded none or the parser does not read it.
	ModTime time.Time
	// Origin is the path as the medium records it, set when Path differs
	// because this tool derived it from other metadata on the medium, such as
	// a classic Mac file's Finder type code. Empty when the two are the same.
	//
	// Nothing is written from it. Messages about the medium use it, since an
	// operator looking for that file looks for the name the volume gives it.
	Origin string
}

// Sink is where an extract command writes recovered files. Entries are created
// and finished one at a time.
type Sink interface {
	// Create returns a writer for one entry. dest is the destination name to
	// show in messages: a path relative to the output directory, or the archive
	// member name.
	Create(e Entry) (w io.WriteCloser, dest string, err error)
	// Discard takes back an entry already written, for the case where keeping
	// it is worse than losing it: a file zero-filled at its full length whose
	// map could not be written would otherwise be decoded as data by a later
	// tool. A sink that cannot take an entry back returns an error.
	Discard(e Entry) error
	// Manifest stores the run's report in the sink, where the sink has somewhere
	// to put it. A directory does not, and keeps nothing.
	Manifest(text string) error
	// Close finishes the archive. A failure means a truncated archive and has to
	// reach the exit status.
	Close() error
}

// Flags are the destination flags an extract command registers.
type Flags struct {
	Out    string
	Format string
	Force  bool
	Times  bool
}

// Register adds the flags to fs.
func (f *Flags) Register(fs *flag.FlagSet) {
	fs.StringVar(&f.Out, "o", ".", `output directory, or the archive file when -format is not "dir" ("-" for stdout)`)
	fs.BoolVar(&f.Force, "f", false, "overwrite existing output")
	fs.StringVar(&f.Format, "format", FormatDir, `output format: "dir", "zip" or "tgz"`)
	fs.BoolVar(&f.Times, "times", true, "restore each file's recorded modification time")
}

// Open validates the combination of flags and builds the sink they describe.
//
// Flag combinations that cannot work are marked as command-line errors, so a
// tool can report them as usage errors. Errors from NewDir and NewArchiveFile
// come from the filesystem and remain failures of the run. This function is
// the only place that can tell the two apart.
func (f *Flags) Open() (Sink, error) {
	switch f.format() {
	case FormatDir:
		if f.Out == "-" {
			return nil, subcmd.Usage(errors.New(`-o - needs an archive format, not "dir"`))
		}
		return NewDir(f.Out, f.Force, f.Times)
	case FormatZip:
		return f.openArchive(FormatZip)
	case FormatTGZ:
		return f.openArchive(FormatTGZ)
	}
	return nil, subcmd.Usage(fmt.Errorf(`unknown output format %q; want "dir", "zip" or "tgz"`, f.Format))
}

// format normalizes the spelling. "tar.gz" is the same thing as "tgz".
func (f *Flags) format() string {
	if f.Format == "tar.gz" {
		return FormatTGZ
	}
	return f.Format
}

// openArchive builds an archive sink over the file at the -o path. The default
// "." is a directory, so it cannot stand in for an archive filename.
func (f *Flags) openArchive(format string) (Sink, error) {
	if f.Out == "." || f.Out == "" {
		return nil, subcmd.Usage(fmt.Errorf("-format %s needs an archive file named with -o", format))
	}
	return NewArchiveFile(format, f.Out, f.Force, f.Times)
}

// Log writes the messages an extract command produces to stderr and keeps them,
// so an archive can carry the report the operator saw.
type Log struct {
	// Prefix identifies the tool and command, as in "fattool extract".
	Prefix string
	// W receives the messages; os.Stderr when nil.
	W io.Writer

	text strings.Builder
}

// Printf records one message, prefixed with the command name.
func (l *Log) Printf(format string, args ...any) {
	l.line(l.Prefix + ": " + fmt.Sprintf(format, args...))
}

// Plainf records a line that carries no command prefix: the per-file listing -v
// produces, and the closing summary.
func (l *Log) Plainf(format string, args ...any) {
	l.line(fmt.Sprintf(format, args...))
}

func (l *Log) line(s string) {
	s = strings.TrimRight(s, "\n") + "\n"
	l.text.WriteString(s)
	w := l.W
	if w == nil {
		w = os.Stderr
	}
	_, _ = io.WriteString(w, s)
}

// Text returns everything recorded so far.
func (l *Log) Text() string { return l.text.String() }

// ManifestText renders the report an archive carries: what produced it, from
// what, when, and every message the run reported.
func ManifestText(tool string, sources []string, body string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s\n", tool, subcmd.CurrentVersion())
	for _, s := range sources {
		fmt.Fprintf(&b, "source: %s\n", s)
	}
	fmt.Fprintf(&b, "extracted: %s\n\n", time.Now().UTC().Format(time.RFC3339))
	if body == "" {
		b.WriteString("nothing to report\n")
		return b.String()
	}
	b.WriteString(body)
	return b.String()
}
