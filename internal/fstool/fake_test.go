package fstool

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"samuelks.com/datarescue/ddrescue"
	"samuelks.com/datarescue/diskimage"
)

// fakeDriver is a driver over a map of paths, so the shared commands can be
// tested with no filesystem and no fixture. This works because the package
// deliberately imports no real driver; the registry lives in the command.
type fakeDriver struct {
	name string
	// magic is the byte a probe looks for at the offset, so detection order and
	// partition selection can be exercised.
	magic byte
	// damagedAt makes the probe report that the bytes it needed were never
	// recovered, the case a bool verdict cannot express.
	damagedAt func(off int64) error
	files     map[string]*fakeFile
	// noLookup makes the volume answer ErrNoLookup, the walk-selection path.
	noLookup bool
	flag     string
	flagVal  *bool
}

type fakeFile struct {
	data    []byte
	dir     bool
	symlink string
	special bool
	mod     time.Time
	// failAt is where a read stops, or -1 to read to the end.
	failAt int
	// lost is what Damage reports, for the listing and verify markers.
	lost int64
}

func (d *fakeDriver) Name() string    { return d.name }
func (d *fakeDriver) Summary() string { return d.name + " (fake)" }

func (d *fakeDriver) Flags(fs *FlagSet) {
	if d.flag == "" {
		return
	}
	d.flagVal = fs.Bool(d.flag, false, "fake flag for "+d.name)
}

func (d *fakeDriver) Probe(r io.ReadSeeker, off int64) Verdict {
	if d.damagedAt != nil {
		if err := d.damagedAt(off); err != nil {
			return Unrecovered(err)
		}
	}
	ra, ok := r.(io.ReaderAt)
	if !ok {
		return Rejected()
	}
	buf := make([]byte, 1)
	if _, err := ra.ReadAt(buf, off); err != nil {
		return Rejected()
	}
	if buf[0] != d.magic {
		return Rejected()
	}
	return Recognized("hint-" + d.name)
}

func (d *fakeDriver) PartitionMatches(byte) bool { return true }

func (d *fakeDriver) Open(im *diskimage.Image, hint any) (Volume, error) {
	return &fakeVolume{d: d, im: im, hint: hint}, nil
}

type fakeVolume struct {
	d     *fakeDriver
	im    *diskimage.Image
	hint  any
	diags []Diagnostic
}

func (v *fakeVolume) Bytes() int64 { return 4096 }

func (v *fakeVolume) Walk(fn func(Entry) error) ([]Diagnostic, error) {
	for _, p := range sortedKeys(v.d.files) {
		if err := fn(v.entry(p)); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

func (v *fakeVolume) entry(p string) Entry {
	f := v.d.files[p]
	size := int64(len(f.data))
	if f.dir {
		size = 0
	}
	return Entry{
		Path: p, Dir: f.dir, Size: size, ModTime: f.mod,
		Mode: "-rw-r--r--", Symlink: f.symlink, Special: f.special, Key: p,
	}
}

func (v *fakeVolume) Lookup(p string) (Entry, error) {
	if v.d.noLookup {
		return Entry{}, ErrNoLookup
	}
	if _, ok := v.d.files[p]; !ok {
		return Entry{}, fmt.Errorf("%w: %s", ErrNotFound, p)
	}
	return v.entry(p), nil
}

func (v *fakeVolume) Open(e Entry) (Reader, error) {
	f := v.d.files[e.Path]
	if f == nil {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, e.Path)
	}
	return &fakeReader{f: f, v: v}, nil
}

func (v *fakeVolume) Damage(e Entry) (int64, int) {
	f := v.d.files[e.Path]
	if f == nil || f.lost == 0 {
		return 0, 0
	}
	return f.lost, 1
}

func (v *fakeVolume) Diagnostics() []Diagnostic {
	d := v.diags
	v.diags = nil
	return d
}

func (v *fakeVolume) Info(w io.Writer) error {
	_, err := fmt.Fprintf(w, "Fake:        %s hint=%v\n", v.d.name, v.hint)
	return err
}

func (v *fakeVolume) Close() error { return nil }

// fakeReader stops at failAt and fills past it when given an armed log, which
// is the contract every real driver's reader follows.
type fakeReader struct {
	f      *fakeFile
	v      *fakeVolume
	off    int
	fill   *ddrescue.GapLog
	base   int64
	latch  error
	filled bool
}

func (r *fakeReader) Fill(l *ddrescue.GapLog) { r.FillAt(l, 0) }

// FillAt is what a reader whose bytes are written after a header needs: it adds
// the base to every hole it records, so the map describes the written file
// rather than the stream it came from.
func (r *fakeReader) FillAt(l *ddrescue.GapLog, base int64) {
	if !l.Filling() {
		return
	}
	l.Limit(base + int64(len(r.f.data)))
	r.base, r.fill = base, l
}

func (r *fakeReader) Read(p []byte) (int, error) {
	if r.latch != nil {
		return 0, r.latch
	}
	if r.off >= len(r.f.data) {
		return 0, io.EOF
	}
	stop := len(r.f.data)
	if r.f.failAt >= 0 && r.f.failAt < stop {
		stop = r.f.failAt
	}
	if r.off < stop {
		n := copy(p, r.f.data[r.off:stop])
		r.off += n
		return n, nil
	}
	// At the damage.
	err := errors.New("fake: would not read")
	if r.fill == nil {
		r.latch = err
		return 0, err
	}
	if !r.filled {
		r.fill.Add(r.base+int64(r.off), int64(len(r.f.data)-r.off), err)
		r.filled = true
	}
	n := copy(p, make([]byte, len(r.f.data)-r.off))
	r.off += n
	return n, nil
}

// sortedKeys returns the paths in tree order (a directory immediately followed
// by what is under it), which every real walk produces and the grouped listing
// relies on. A plain sort of slash-separated paths gives exactly that order.
func sortedKeys(m map[string]*fakeFile) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// writeFakeImage writes an image whose byte at each offset is the magic a
// driver probes for.
func writeFakeImage(size int, at map[int64]byte) []byte {
	img := make([]byte, size)
	for off, b := range at {
		img[off] = b
	}
	return img
}
