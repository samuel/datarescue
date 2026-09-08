package extdrv

import (
	"io"
	"strings"
	"testing"

	"samuelks.com/datarescue/internal/fstool"
	"samuelks.com/datarescue/internal/testfixture"
)

const fixture = "../../../extfs/testdata/disk.img.gz"

func open(t *testing.T) (*fstool.Context, fstool.Volume) {
	t.Helper()
	path := testfixture.File(t, fixture)
	drv := New()
	d, im, hint, err := fstool.Detect(path, &fstool.CommonFlags{Offset: -1}, []fstool.Driver{drv})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { im.Close() }) //nolint:errcheck // test cleanup
	vol, err := d.Open(im, hint)
	if err != nil {
		t.Fatal(err)
	}
	return &fstool.Context{Driver: d, Volume: vol, Image: im, Path: path,
		Tool: "fstool", Command: "test"}, vol
}

func TestDetectsTheExt2Fixture(t *testing.T) {
	c, _ := open(t)
	if c.Driver.Name() != "ext" {
		t.Errorf("driver = %s; want ext", c.Driver.Name())
	}
	if c.Image.Source != "offset 0 (no partition table)" {
		t.Errorf("Source = %q", c.Image.Source)
	}
}

func TestWalkYieldsFilesAndDirectories(t *testing.T) {
	_, vol := open(t)
	got := map[string]fstool.Entry{}
	diags, err := vol.Walk(func(e fstool.Entry) error {
		got[e.Path] = e
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range diags {
		t.Errorf("unexpected walk diagnostic: %s", d)
	}
	// The fixture is built with hello.txt, an empty testdir/ and a 400 KiB
	// big.dat; see the repo's testing notes.
	for _, want := range []string{"hello.txt", "testdir", "big.dat"} {
		if _, ok := got[want]; !ok {
			t.Errorf("walk did not yield %q; got %v", want, keys(got))
		}
	}
	if !got["testdir"].Dir {
		t.Error("testdir is not marked a directory")
	}
	if got["big.dat"].Size != 400*1024 {
		t.Errorf("big.dat size = %d; want %d", got["big.dat"].Size, 400*1024)
	}
	if !strings.HasPrefix(got["hello.txt"].Mode, "-") {
		t.Errorf("hello.txt mode = %q; want a regular-file mode", got["hello.txt"].Mode)
	}
}

// big.dat's bytes are (i%251)+1, never zero, so a zero-filled hole in it
// cannot be mistaken for its own data. Reading it through the driver checks
// that the reader reaches the right offsets across all three levels of the
// block tree.
func TestOpenReadsAFileWhole(t *testing.T) {
	_, vol := open(t)
	var entry fstool.Entry
	if _, err := vol.Walk(func(e fstool.Entry) error {
		if e.Path == "big.dat" {
			entry = e
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r, err := vol.Open(entry)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 400*1024 {
		t.Fatalf("read %d bytes; want %d", len(data), 400*1024)
	}
	for i, b := range data {
		if want := byte(i%251) + 1; b != want {
			t.Fatalf("byte %d = %d; want %d", i, b, want)
		}
	}
}

func TestProbeRejectsSomethingElse(t *testing.T) {
	// An image of zeros is not ext2, and reporting that is different from
	// reporting that the bytes were never recovered.
	v := New().Probe(zeroReader{}, 0)
	if v.Kind != fstool.No {
		t.Errorf("probe of zeros = %v; want No", v.Kind)
	}
}

type zeroReader struct{}

func (z zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }
func (z zeroReader) Seek(off int64, whence int) (int64, error) {
	if whence == io.SeekEnd {
		return 1 << 20, nil
	}
	return off, nil
}

func keys(m map[string]fstool.Entry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
