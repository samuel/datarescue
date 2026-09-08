package outsink

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"time"

	"samuelks.com/datarescue/internal/safepath"
	"samuelks.com/datarescue/internal/xio"
)

// ManifestName is the archive member holding the run's report.
const ManifestName = "datarescue-report.txt"

// entryMode is the permission recorded for every member. The media the tools
// read either carry no Unix mode at all or carry one that says nothing useful
// about the recovered copy.
const entryMode = 0o644

// unknownModTime is what a member records when the medium gave no time. Tar
// cannot express the zero time.Time in its numeric fields and zip's DOS fields
// start at 1980, so the epoch stands in for "not recorded".
var unknownModTime = time.Unix(0, 0).UTC()

// Archive writes recovered files as members of a zip or a gzipped tar.
// Entries are created and finished one at a time.
type Archive struct {
	closer io.Closer // the archive file, nil when the caller owns the writer
	zw     *zip.Writer
	gz     *gzip.Writer
	tw     *tar.Writer
	times  bool
	names  map[string]bool
}

// NewArchiveFile writes an archive of the given format to the file at path out,
// or to stdout when out is "-".
func NewArchiveFile(format, out string, force, times bool) (*Archive, error) {
	if out == "-" {
		return NewArchive(format, os.Stdout, times)
	}
	f, err := xio.Create(out, force)
	if err != nil {
		return nil, err
	}
	a, err := NewArchive(format, f, times)
	if err != nil {
		_ = f.Close() // the archive was never started, so there is nothing to salvage
		return nil, err
	}
	a.closer = f
	return a, nil
}

// NewArchive writes an archive of the given format to w. Close finishes the
// archive without closing w.
func NewArchive(format string, w io.Writer, times bool) (*Archive, error) {
	a := &Archive{times: times, names: make(map[string]bool)}
	switch format {
	case FormatZip:
		a.zw = zip.NewWriter(w)
	case FormatTGZ:
		a.gz = gzip.NewWriter(w)
		a.tw = tar.NewWriter(a.gz)
	default:
		return nil, fmt.Errorf("unknown archive format %q", format)
	}
	return a, nil
}

// Create returns a writer for one member.
func (a *Archive) Create(e Entry) (io.WriteCloser, string, error) {
	name := safepath.Slash(e.Path)
	if a.names[name] {
		return nil, name, fmt.Errorf("%w %q (from %q)", ErrDuplicate, name, e.Path)
	}
	mod := e.ModTime
	if !a.times {
		mod = time.Time{}
	}
	w, err := a.create(name, mod)
	if err != nil {
		return nil, name, err
	}
	return w, name, nil
}

// Discard cannot take a member back: zip streams a member as it is written and
// tar has already copied it into the stream, so the name stays claimed and the
// bytes stay in the archive. The report lists it instead, and the run fails.
func (a *Archive) Discard(e Entry) error {
	return fmt.Errorf("%q was written with holes in it and stays in the archive: a member cannot be removed", safepath.Slash(e.Path))
}

// Manifest writes the report as a member of its own, under a name no recovered
// file has taken.
func (a *Archive) Manifest(text string) error {
	name := ManifestName
	for i := 1; a.names[name]; i++ {
		name = fmt.Sprintf("datarescue-report-%d.txt", i)
	}
	// The report is the tool's own output, so it carries the time it was
	// written rather than any time from the medium.
	w, err := a.create(name, time.Now())
	if err != nil {
		return err
	}
	_, err = io.WriteString(w, text)
	return xio.CloseAfter(w, err)
}

// Close finishes the archive and closes the file it was opened on.
func (a *Archive) Close() error {
	var err error
	if a.zw != nil {
		err = a.zw.Close()
	}
	if a.tw != nil {
		err = a.tw.Close()
		if gerr := a.gz.Close(); err == nil {
			err = gerr
		}
	}
	if a.closer != nil {
		if cerr := a.closer.Close(); err == nil {
			err = cerr
		}
	}
	return err
}

// create starts a member with the given time and reserves its name.
func (a *Archive) create(name string, mod time.Time) (io.WriteCloser, error) {
	if mod.IsZero() {
		mod = unknownModTime
	}
	w, err := a.startMember(name, mod)
	if err != nil {
		return nil, err
	}
	a.names[name] = true
	return w, nil
}

// startMember opens one member in whichever format the archive is.
func (a *Archive) startMember(name string, mod time.Time) (io.WriteCloser, error) {
	if a.zw != nil {
		hdr := &zip.FileHeader{Name: name, Method: zip.Deflate, Modified: mod}
		hdr.SetMode(entryMode)
		w, err := a.zw.CreateHeader(hdr)
		if err != nil {
			return nil, err
		}
		// A zip member ends when the next one begins, so there is nothing to
		// close; the wrapper is only here to satisfy the Sink contract.
		return nopWriteCloser{w}, nil
	}
	return &tarEntry{a: a, name: name, mod: mod}, nil
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// tarEntry buffers a member until its length is known. A tar header carries
// the length ahead of the data, and on damaged media the recovered length is
// only known once the read has finished, so the header records that length.
// Nothing is padded out to the size the image declared: the archive must not
// claim bytes the medium never gave up.
type tarEntry struct {
	a    *Archive
	name string
	mod  time.Time

	buf    spillBuffer
	closed bool
}

func (e *tarEntry) Write(p []byte) (int, error) {
	if e.closed {
		return 0, os.ErrClosed
	}
	return e.buf.Write(p)
}

func (e *tarEntry) Close() error {
	if e.closed {
		return nil
	}
	e.closed = true
	defer e.buf.remove()

	hdr := &tar.Header{
		Typeflag: tar.TypeReg,
		Name:     e.name,
		Size:     e.buf.n,
		Mode:     entryMode,
		ModTime:  e.mod,
		Format:   tar.FormatPAX,
	}
	if err := e.a.tw.WriteHeader(hdr); err != nil {
		return err
	}
	r, err := e.buf.reader()
	if err != nil {
		return err
	}
	_, err = io.Copy(e.a.tw, r)
	return err
}

// spillThreshold bounds how much of a tar member is held in memory before it
// goes to a temporary file. A var so a test can shrink it.
var spillThreshold int64 = 8 << 20

// spillBuffer accumulates a member's bytes: in memory until it outgrows
// spillThreshold, in a temporary file after that.
type spillBuffer struct {
	mem  bytes.Buffer
	file *os.File
	n    int64
}

func (b *spillBuffer) Write(p []byte) (int, error) {
	if b.file == nil && b.n+int64(len(p)) > spillThreshold {
		if err := b.spill(); err != nil {
			return 0, err
		}
	}
	var n int
	var err error
	if b.file != nil {
		n, err = b.file.Write(p)
	} else {
		n, err = b.mem.Write(p)
	}
	b.n += int64(n)
	return n, err
}

func (b *spillBuffer) spill() error {
	f, err := os.CreateTemp("", "datarescue-tar-*")
	if err != nil {
		return err
	}
	if _, err := f.Write(b.mem.Bytes()); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return err
	}
	b.mem.Reset()
	b.file = f
	return nil
}

// reader rewinds the buffer for reading back.
func (b *spillBuffer) reader() (io.Reader, error) {
	if b.file == nil {
		return bytes.NewReader(b.mem.Bytes()), nil
	}
	if _, err := b.file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return b.file, nil
}

func (b *spillBuffer) remove() {
	if b.file == nil {
		return
	}
	name := b.file.Name()
	_ = b.file.Close() // the data has already been copied into the archive
	_ = os.Remove(name)
	b.file = nil
}
