package qic

import (
	"errors"
	"fmt"
	"io"

	"samuelks.com/datarescue/ddrescue"
)

// FileReadError is a read that stopped part way through a file. N is how much
// was delivered before it; a partly recovered file is worth more than nothing.
type FileReadError struct {
	Path string
	N    int64
	Err  error
}

func (e *FileReadError) Error() string {
	return fmt.Sprintf("qic: %s: %d bytes recovered before %s", e.Path, e.N, e.Err)
}

func (e *FileReadError) Unwrap() error { return e.Err }

// Reader streams one file out of the data region.
type Reader struct {
	a    *Archive
	file *File
	off  int64 // within the file
	// zero is how many bytes of a hole are still to be handed out in place of
	// a segment that would not read.
	zero int64
	// fill, when it is armed, makes damage deliver zeros and the read carry on
	// to the segment after it. Here the hole's length is arithmetic rather
	// than a guess: each segment records the count of uncompressed bytes
	// preceding it, so the length of what was lost is exactly the distance to
	// where the next segment says the stream resumes.
	fill *ddrescue.GapLog
	errs []error
	err  error // latched
}

// Fill hands the reader a log to record its holes in, switching it from
// latching at the damage to zero-filling past it. Call it before the first
// Read.
//
// Only ddrescue.Fill arms a log, and it writes the map in the same call. That
// is why filling is acceptable here: a file returned at its full declared size
// with invisible holes is the failure this toolkit exists to prevent, and the
// map makes the holes visible. Where the hole's length cannot be computed, the
// reader latches whatever the log says; see Read.
func (r *Reader) Fill(l *ddrescue.GapLog) {
	if !l.Filling() {
		return
	}
	l.Limit(r.file.Size)
	r.fill = l
}

// OpenFile streams a file the caller already has from Files. The entry is
// resolved back to the archive's own, so a caller reading through a copy still
// sees a position the archive has since moved.
func (a *Archive) OpenFile(f *File) *Reader {
	f = a.own(f)
	a.verifyAnchor(f)
	return &Reader{a: a, file: f}
}

// Open streams the file at the path Files reports.
func (a *Archive) Open(path string) (*Reader, error) {
	f, ok := a.Find(path)
	if !ok {
		return nil, fmt.Errorf("qic: %s: not found in the archive", path)
	}
	if f.Dir {
		return nil, fmt.Errorf("qic: %s: is a directory", path)
	}
	return a.OpenFile(f), nil
}

// Errors returns the problems that did not stop the read.
func (r *Reader) Errors() []error { return r.errs }

// Read fills p from the file.
//
// A failure latches: the same error comes back on every later call and the
// offset does not advance, so a caller that retries cannot splice bytes from
// past the damage in at the wrong place with nothing to mark the gap. Each
// segment of the archive carries its own offset into the uncompressed stream,
// so damage affects the file overlapping it and not the ones after it.
func (r *Reader) Read(p []byte) (int, error) {
	if r.file.Dir {
		return 0, io.EOF
	}
	if r.file.checked == anchorLost || r.file.checked == anchorUnresolved {
		// What was lost is the file's position, not its content. A whole file
		// of zeros with an all-bad map beside it proves nothing, so the file
		// is refused rather than filled and reported as unrecoverable.
		if r.err == nil {
			why := "the file's position in the archive was lost"
			if r.file.checked == anchorUnresolved {
				why = "the file's position in the archive was never confirmed against the data region"
			}
			r.err = &FileReadError{Path: r.file.Path, Err: errors.New(why)}
		}
		return 0, r.err
	}
	for {
		remain := r.file.Size - r.off
		if remain <= 0 {
			return 0, io.EOF
		}
		if int64(len(p)) > remain {
			p = p[:remain]
		}
		if r.zero > 0 {
			n := int(min(int64(len(p)), r.zero))
			clear(p[:n])
			r.zero -= int64(n)
			r.off += int64(n)
			return n, nil
		}
		if r.err != nil {
			return 0, r.err
		}

		n, err := r.a.data.ReadAt(p, r.file.dataOff+r.off)
		r.off += int64(n)
		switch {
		case err == nil, errors.Is(err, io.EOF) && r.off >= r.file.Size:
			return n, nil
		}
		if !r.openHole(err) {
			r.err = &FileReadError{Path: r.file.Path, N: r.off, Err: err}
			return n, r.err
		}
		if n > 0 {
			// Hand back what came before the damage; the hole waits for the
			// next call.
			return n, nil
		}
	}
}

// holeEnd is how far a failure reaches into the file: from where the reader
// stands to where the failing segment ends, clipped to the file's own bounds.
// A segment's failure is cached, so later meetings still report the segment's
// own start, which by then is behind the reader.
//
// It reports false where there is nothing to fill: an error that is not a
// segment's; a segment whose length is unknown (only the next segment's start
// bounds it, so an unbounded one gives no measurement and this must not
// guess); or a range entirely behind the reader, which would neither advance
// the read nor stop it.
func (r *Reader) holeEnd(cause error) (int64, bool) {
	var sre *SegmentReadError
	if !errors.As(cause, &sre) || sre.Length < 0 {
		return 0, false
	}
	end := min(sre.Offset+sre.Length-r.file.dataOff, r.file.Size)
	if end <= r.off {
		return 0, false
	}
	return end, true
}

// openHole turns a segment that would not read into a run of zeros, and reports
// whether it could.
func (r *Reader) openHole(cause error) bool {
	if !r.fill.Filling() {
		return false
	}
	end, ok := r.holeEnd(cause)
	if !ok {
		return false
	}
	r.fill.Add(r.off, end-r.off, cause)
	r.zero = end - r.off
	return true
}

// Check is the result of reading a file through without writing it.
type Check struct {
	File   *File
	Bytes  int64
	Errors []error
}

// OK reports whether the file came back whole.
func (c Check) OK() bool {
	return len(c.Errors) == 0 && c.File != nil && c.Bytes == c.File.Size
}

// Check reads a file through and reports what came back. Nothing is written,
// so a whole archive can be tested against a medium before an operator commits
// to an extraction.
func (a *Archive) Check(f *File) Check {
	c := Check{File: f}
	if f.Dir {
		return c
	}
	r := a.OpenFile(f)
	n, err := io.Copy(io.Discard, r)
	c.Bytes = n
	c.Errors = append(c.Errors, r.Errors()...)
	if err != nil {
		c.Errors = append(c.Errors, err)
	} else if n != f.Size {
		c.Errors = append(c.Errors,
			fmt.Errorf("qic: %s: %d bytes came back where the catalog records %d", f.Path, n, f.Size))
	}
	return c
}
