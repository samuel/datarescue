package ddrescue

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// TestResolve pins the map-resolution policy every tool shares.
func TestResolve(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(name string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	image := write("input.img")
	sidecar := write("input.img.map")
	stripped := write("input.map")
	named := write("elsewhere.map")

	tests := []struct {
		name    string
		opts    Options
		want    string
		wantErr bool
	}{
		{"the named map wins", Options{Map: named}, named, false},
		{"the sidecar is picked up", Options{}, sidecar, false},
		{"no-map ignores the sidecar", Options{NoMap: true}, "", false},
		{"a named map that is not there is an error", Options{Map: filepath.Join(dir, "gone.map")}, "", true},
		{"named and ignored at once is an error", Options{Map: named, NoMap: true}, "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Resolve(image, tc.opts)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Resolve = %q; want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if got != tc.want {
				t.Errorf("Resolve = %q; want %q", got, tc.want)
			}
		})
	}

	// With the sidecar gone the stripped spelling is what is left.
	if err := os.Remove(sidecar); err != nil {
		t.Fatal(err)
	}
	if got, _ := Resolve(image, Options{}); got != stripped {
		t.Errorf("Resolve without the sidecar = %q; want %q", got, stripped)
	}
}

// Source exists to provide two views: a parser must not be given unrecovered
// bytes, and a verifier must be able to read them.
func TestSourceViews(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	image := filepath.Join(dir, "disk.img")
	data := make([]byte, 0x300)
	for i := range data {
		data[i] = byte(i)
	}
	if err := os.WriteFile(image, data, 0o600); err != nil {
		t.Fatal(err)
	}
	body := "# Mapfile. Created by a test.\n0x00000000     +               1\n#      Start        Size  Status\n" +
		"0x00000000  0x00000100  +\n0x00000100  0x00000100  -\n0x00000200  0x00000100  +\n"
	if err := os.WriteFile(image+".map", []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	src, err := OpenSource(image, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close() //nolint:errcheck // read-only

	if src.MapPath != image+".map" || src.Size != int64(len(data)) {
		t.Errorf("Source = %+v; want the sidecar and %d bytes", src, len(data))
	}
	buf := make([]byte, 16)
	if _, err := src.Checked().ReadAt(buf, 0x110); !errors.Is(err, ErrNotRecovered) {
		t.Errorf("the checked view read a failed block: %v", err)
	}
	if n, err := src.Raw().ReadAt(buf, 0x110); err != nil || n != len(buf) {
		t.Errorf("the raw view = (%d, %v); a verifier has to see these bytes", n, err)
	}
	if got := src.UnrecoveredBytes(0, int64(len(data))); got != 0x100 {
		t.Errorf("UnrecoveredBytes = %d; want 256", got)
	}
}

// Without a map nothing is known to be missing. Reporting the whole image as
// non-tried would make every tool say a sound image is damaged.
func TestSourceWithoutAMapKnowsOfNoDamage(t *testing.T) {
	t.Parallel()
	image := filepath.Join(t.TempDir(), "disk.img")
	if err := os.WriteFile(image, make([]byte, 512), 0o600); err != nil {
		t.Fatal(err)
	}
	src, err := OpenSource(image, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close() //nolint:errcheck // read-only

	if src.MapPath != "" {
		t.Errorf("MapPath = %q; want none", src.MapPath)
	}
	if got := src.Unrecovered(0, 512); got != nil {
		t.Errorf("Unrecovered without a map = %v; want nothing", got)
	}
	buf := make([]byte, 512)
	if _, err := src.Checked().ReadAt(buf, 0); err != nil {
		t.Errorf("the checked view of an unmapped image failed: %v", err)
	}
}

// NewReader gives the same guarantee over a reader the caller owns, which lets
// Source offer both views over one file descriptor.
func TestNewReaderMatchesOpen(t *testing.T) {
	t.Parallel()
	opened := openTestReader(t, 0x300, goodBadGood)
	f, err := os.Open(opened.ra.(*os.File).Name())
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck // read-only
	wrapped := NewReader(f, opened.Blocks)

	buf1, buf2 := make([]byte, 0x20), make([]byte, 0x20)
	n1, err1 := opened.ReadAt(buf1, 0xF0)
	n2, err2 := wrapped.ReadAt(buf2, 0xF0)
	if n1 != n2 || (err1 == nil) != (err2 == nil) {
		t.Errorf("NewReader = (%d, %v); Open = (%d, %v)", n2, err2, n1, err1)
	}

	// Close is the caller's business when the caller supplied the reader.
	if err := wrapped.Close(); err != nil {
		t.Errorf("Close on a borrowed reader = %v; want nil", err)
	}
	if _, err := f.ReadAt(buf2, 0); err != nil {
		t.Errorf("Close on a borrowed reader closed it: %v", err)
	}
}

// Seek is bookkeeping over the map, not a syscall, so it has to agree with what
// Read then does.
func TestSeekWhence(t *testing.T) {
	t.Parallel()
	r := openTestReader(t, 0x300, goodBadGood)
	if off, err := r.Seek(0x10, io.SeekStart); err != nil || off != 0x10 {
		t.Fatalf("SeekStart = (%d, %v)", off, err)
	}
	if off, err := r.Seek(0x10, io.SeekCurrent); err != nil || off != 0x20 {
		t.Fatalf("SeekCurrent = (%d, %v)", off, err)
	}
	if off, err := r.Seek(-0x100, io.SeekEnd); err != nil || off != 0x200 {
		t.Fatalf("SeekEnd = (%d, %v); want 0x200", off, err)
	}
	buf := make([]byte, 4)
	if _, err := r.Read(buf); err != nil {
		t.Fatalf("Read after SeekEnd: %v", err)
	}
	if buf[0] != 0x00 { // the image byte at 0x200 is byte(0x200)
		t.Errorf("Read after SeekEnd started at the wrong offset: %#x", buf[0])
	}
	if _, err := r.Seek(-1, io.SeekStart); err == nil {
		t.Error("a seek to a negative offset was accepted")
	}
}

// A map that stops short of the image describes a rescue that never reached the
// tail of the disk. SeekEnd has to report the image's own length anyway: a
// caller sizing its structures from it — extfs.Open does exactly this — must
// not be told the disk is smaller than it is.
func TestSeekEndReportsTheImageNotTheMap(t *testing.T) {
	t.Parallel()
	const size = 0x300
	// The map covers only the first third.
	r := openTestReader(t, size, "0x00000000  0x00000100  +\n")
	off, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		t.Fatal(err)
	}
	if off != size {
		t.Errorf("SeekEnd = %#x; want the image's own %#x", off, size)
	}
	// Reading there still reports EOF: the map does not cover those bytes.
	if _, err := r.Read(make([]byte, 16)); !errors.Is(err, io.EOF) {
		t.Errorf("reading past the map = %v; want io.EOF", err)
	}
}
