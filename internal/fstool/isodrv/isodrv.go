// Package isodrv reads ISO 9660 images for the filesystem tool.
package isodrv

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"strings"
	"time"

	"samuelks.com/datarescue/diskimage"
	"samuelks.com/datarescue/internal/fstool"
	"samuelks.com/datarescue/iso"
)

// Driver reads ISO 9660.
type Driver struct {
	sectorSize int
}

// New returns the ISO driver.
func New() *Driver { return &Driver{} }

func (*Driver) Name() string    { return "iso" }
func (*Driver) Summary() string { return "ISO 9660" }

// Flags registers the sector-size override, for an image whose sector size
// the probe cannot decide.
func (d *Driver) Flags(fs *fstool.FlagSet) {
	fs.IntVar(&d.sectorSize, "sector-size", 0,
		"bytes per sector: 2048 (cooked) or 2352 (raw); 0 probes both")
}

// Probe reports whether an ISO 9660 volume starts at off.
//
// The three answers matter most here. A primary volume descriptor that was
// never recovered is the case a rescue map exists for, and reporting it as
// "this is not an ISO" sends an operator looking for the wrong problem. iso
// makes that distinction itself with ErrPVDUnrecovered.
func (d *Driver) Probe(r io.ReadSeeker, off int64) fstool.Verdict {
	ra, ok := r.(io.ReaderAt)
	if !ok {
		// An 86F floppy image is decoded track by track, and no ISO 9660 volume
		// lives in one.
		return fstool.Rejected()
	}
	rd, err := d.open(io.NewSectionReader(ra, off, math.MaxInt64-off))
	switch {
	case err == nil:
		return fstool.Recognized(rd)
	case errors.Is(err, iso.ErrNotISO9660):
		return fstool.Rejected()
	}
	return fstool.Unrecovered(err)
}

func (d *Driver) open(ra io.ReaderAt) (*iso.Reader, error) {
	if d.sectorSize == 0 {
		return iso.OpenReader(ra)
	}
	return iso.OpenReaderWithSectorSize(ra, d.sectorSize)
}

// PartitionMatches narrows nothing. An ISO 9660 volume is normally the whole
// image, and when one is in a partition the type byte says nothing useful
// about it.
func (*Driver) PartitionMatches(byte) bool { return true }

// Open opens the volume. hint is the *iso.Reader the probe already built, so
// the sector-size probe does not run a second time. That probe reads the
// volume descriptor at each candidate size, so it is not free.
func (d *Driver) Open(im *diskimage.Image, hint any) (fstool.Volume, error) {
	if rd, ok := hint.(*iso.Reader); ok && rd != nil {
		return &volume{r: rd, im: im}, nil
	}
	ra, err := im.ReaderAt()
	if err != nil {
		return nil, err
	}
	rd, err := d.open(ra)
	if err != nil {
		return nil, err
	}
	return &volume{r: rd, im: im}, nil
}

type volume struct {
	r  *iso.Reader
	im *diskimage.Image
	// walked records that Walk has run, since the reader's issues are only
	// complete afterwards.
	walked bool
}

// Bytes is the volume's own length, in the medium's sectors rather than the
// file's, since that is the range the rescue map describes.
func (v *volume) Bytes() int64 {
	return v.r.TotalSectors() * int64(v.r.SectorSize())
}

func (v *volume) Close() error { return nil }

// key carries the reader's entry, so Open and Damage need no second lookup.
type key struct{ e iso.DirEntry }

func (v *volume) Walk(fn func(fstool.Entry) error) ([]fstool.Diagnostic, error) {
	var associated int
	err := v.r.Walk(func(e iso.DirEntry) error {
		// An associated file has the same path as the file it belongs to, so
		// it cannot be written beside it under a name the disc supports.
		if e.Associated {
			associated++
			return nil
		}
		return fn(entryFor(e))
	})
	v.walked = true
	// Collected after the walk, since that is when they are complete.
	diags := v.issues()
	if associated > 0 {
		diags = append(diags, fstool.Diagnostic{Kind: "iso", Text: fmt.Sprintf(
			"%d associated file(s) skipped; each shares its name with the file it belongs to", associated)})
	}
	return diags, err
}

func (v *volume) issues() []fstool.Diagnostic {
	raw := v.r.Issues()
	out := make([]fstool.Diagnostic, 0, len(raw))
	for _, s := range raw {
		out = append(out, fstool.Diagnostic{Kind: "iso", Damage: true, Text: s})
	}
	return out
}

func entryFor(e iso.DirEntry) fstool.Entry {
	return fstool.Entry{
		// iso returns a leading slash, and a trailing one on directories; the
		// shared path form has neither.
		Path:    strings.Trim(e.Path, "/"),
		Dir:     e.IsDir,
		Size:    e.Size,
		ModTime: e.ModTime,
		Mode:    modeString(e.Mode),
		Symlink: e.Symlink,
		// A Rock Ridge device node, pipe or socket has no data to extract.
		Special: e.Mode&(fs.ModeDevice|fs.ModeNamedPipe|fs.ModeSocket) != 0,
		Key:     key{e: e},
	}
}

// modeString renders a mode the way ls does: a type character, then the
// permission bits.
func modeString(m fs.FileMode) string {
	var b strings.Builder
	switch {
	case m.IsDir():
		b.WriteByte('d')
	case m&fs.ModeSymlink != 0:
		b.WriteByte('l')
	case m&fs.ModeCharDevice != 0:
		b.WriteByte('c')
	case m&fs.ModeDevice != 0:
		b.WriteByte('b')
	case m&fs.ModeNamedPipe != 0:
		b.WriteByte('p')
	case m&fs.ModeSocket != 0:
		b.WriteByte('s')
	default:
		b.WriteByte('-')
	}
	for i := 8; i >= 0; i-- {
		if m&(1<<i) != 0 {
			b.WriteByte("rwx"[(8-i)%3])
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

// Lookup resolves a path by reading only the directories along it. A name
// missing from a directory that is damaged is reported as damage, not as
// absent, since it may be in the part that was lost.
func (v *volume) Lookup(path string) (fstool.Entry, error) {
	e, err := v.r.Lookup(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fstool.Entry{}, fmt.Errorf("%w: %s", fstool.ErrNotFound, path)
	case err != nil:
		return fstool.Entry{}, err
	}
	return entryFor(e), nil
}

func (v *volume) Open(e fstool.Entry) (fstool.Reader, error) {
	k, ok := e.Key.(key)
	if !ok {
		return nil, fmt.Errorf("isodrv: %s has no extent", e.Path)
	}
	return v.r.OpenFile(k.e), nil
}

// Damage is how much of an entry's data lies in ranges the rescue never
// recovered.
//
// It is counted in the file's own bytes, not the image's. A raw sector is 2352
// bytes on the medium but holds only 2048 of the file's, and a loss in medium
// bytes next to a size in file bytes invites the wrong subtraction.
func (v *volume) Damage(e fstool.Entry) (int64, int) {
	k, ok := e.Key.(key)
	if !ok || v.im.MapPath == "" || e.Symlink != "" {
		return 0, 0
	}
	ss := int64(v.r.SectorSize())
	var lost int64
	var ranges int
	for _, x := range k.e.Extents {
		if x.LBA <= 0 || x.Size <= 0 {
			continue
		}
		off, length := v.r.ExtentBytes(x)
		// The extent is relative to the volume, and the map describes the image.
		off += v.im.Offset
		counted := int64(-1) // the last sector already counted; ranges can share one
		for _, b := range v.im.Unrecovered(off, length) {
			ranges++
			first := max((b.Offset-off)/ss, counted+1)
			last := (b.Offset + b.Size - 1 - off) / ss
			for sec := first; sec <= last; sec++ {
				lost += min(iso.UserDataSize, x.Size-sec*iso.UserDataSize)
			}
			if last > counted {
				counted = last
			}
		}
	}
	return lost, ranges
}

// Diagnostics drains what the reader noticed. Its issue list is only complete
// once Walk has run, so before that this reports the descriptor findings alone.
func (v *volume) Diagnostics() []fstool.Diagnostic {
	if v.walked {
		// Already reported by Walk; reporting them again would double every
		// finding in a listing.
		return nil
	}
	return v.issues()
}

func (v *volume) Info(w io.Writer) error {
	p := func(format string, args ...any) {
		fmt.Fprintf(w, format+"\n", args...) //nolint:errcheck // report output
	}
	vi := v.r.Volume()
	p("Volume label:  %s", vi.Label)
	for _, f := range []struct{ name, val string }{
		{"Publisher:", vi.Publisher},
		{"Data preparer:", vi.DataPreparer},
		{"Application:", vi.Application},
	} {
		if f.val != "" {
			p("%-14s %s", f.name, f.val)
		}
	}
	for _, f := range []struct {
		name string
		t    time.Time
	}{
		{"Created:", vi.Created},
		{"Modified:", vi.Modified},
		{"Expires:", vi.Expires},
		{"Effective:", vi.Effective},
	} {
		if !f.t.IsZero() {
			p("%-14s %s", f.name, f.t.Format("2006-01-02 15:04:05 -07:00"))
		}
	}
	p("Sector size:   %d bytes (%s)", v.r.SectorSize(), sectorKind(v.r.SectorSize()))
	p("Sectors:       %d", v.r.TotalSectors())
	p("Joliet:        %s", yesNo(v.r.HasJoliet()))
	p("Rock Ridge:    %s", yesNo(v.r.HasRockRidge()))
	return nil
}

func sectorKind(n int) string {
	if n == iso.UserDataSize {
		return "cooked"
	}
	return "raw"
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
