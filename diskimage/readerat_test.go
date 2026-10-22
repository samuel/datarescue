package diskimage

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"samuelks.com/datarescue/internal/testapm"
	"samuelks.com/datarescue/internal/testfixture"
	"samuelks.com/datarescue/mbr"
)

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // a test's own temporary file
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// OpenReaderAt finds what Open finds: the same offset, the same bounds and the
// same description, for every layout Open handles.
func TestOpenReaderAtAgreesWithOpen(t *testing.T) {
	t.Parallel()
	apmPath, placed := apmImageFile(t, testapm.Options{MapBlocks: 10, Fill: 0xFA}, hfsPartitions()...)
	cases := []struct {
		name string
		path string
		opts Options
	}{
		{"unpartitioned", writeImage(t, 1<<20), Options{Offset: -1, Accept: acceptAt(0)}},
		{"explicit offset", writeImage(t, 1<<20), Options{Offset: 4096, Accept: acceptAt(-1)}},
		{"MBR auto-detected", writeImage(t, 1<<20,
			mbr.Partition{PartitionType: 0x06, FirstSectorLBA: 63, NumberOfSectors: 1000}),
			Options{Offset: -1, Accept: acceptAt(63 * 512)}},
		{"MBR named partition", writeImage(t, 1<<20,
			mbr.Partition{PartitionType: 0x83, FirstSectorLBA: 100, NumberOfSectors: 200},
			mbr.Partition{PartitionType: 0x06, FirstSectorLBA: 400, NumberOfSectors: 300}),
			Options{Offset: -1, Partition: 2, Accept: acceptAt(-1)}},
		{"Apple partition", apmPath, Options{Offset: -1, Accept: acceptAt(placed[2].Offset)}},
	}
	for _, c := range cases {
		want, err := Open(c.path, c.opts)
		if err != nil {
			t.Fatalf("%s: Open: %v", c.name, err)
		}
		want.Close() //nolint:errcheck // test cleanup
		img := readFile(t, c.path)
		got, err := OpenReaderAt(bytes.NewReader(img), int64(len(img)), c.opts)
		if err != nil {
			t.Fatalf("%s: OpenReaderAt: %v", c.name, err)
		}
		if got.Offset != want.Offset || got.Size != want.Size || got.Source != want.Source {
			t.Errorf("%s: OpenReaderAt = offset %d size %d %q; Open = offset %d size %d %q",
				c.name, got.Offset, got.Size, got.Source, want.Offset, want.Size, want.Source)
		}
	}
}

// The located filesystem reads through the Image the same way it does from
// Open, at the right offset and bounded by the partition.
func TestOpenReaderAtReads(t *testing.T) {
	t.Parallel()
	img := readFile(t, writeImage(t, 1<<20,
		mbr.Partition{PartitionType: 0x06, FirstSectorLBA: 63, NumberOfSectors: 8}))
	copy(img[63*512:], "volume start")
	im, err := OpenReaderAt(bytes.NewReader(img), int64(len(img)), Options{Offset: -1, Accept: acceptAt(63 * 512)})
	if err != nil {
		t.Fatal(err)
	}
	ra, err := im.ReaderAt()
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 12)
	if _, err := ra.ReadAt(b, 0); err != nil || string(b) != "volume start" {
		t.Errorf("ReadAt(0) = %q, %v", b, err)
	}
	if _, err := ra.ReadAt(b, 8*512); !errors.Is(err, io.EOF) {
		t.Errorf("a read past the partition's end = %v, want io.EOF", err)
	}
}

// size bounds the image: bytes past it are not part of the image even when the
// reader holds more, which is the case for an image stored inside another.
func TestOpenReaderAtHonorsSize(t *testing.T) {
	t.Parallel()
	img := readFile(t, writeImage(t, 1<<20))
	im, err := OpenReaderAt(bytes.NewReader(img), 4096, Options{Offset: -1, Accept: acceptAt(0)})
	if err != nil {
		t.Fatal(err)
	}
	if im.Size != 4096 {
		t.Errorf("Size = %d, want the 4096 given", im.Size)
	}
	if n, err := im.Reader.Seek(0, io.SeekEnd); err != nil || n != 4096 {
		t.Errorf("the reader ends at %d, %v; want 4096", n, err)
	}
}

func TestOpenReaderAtNotFound(t *testing.T) {
	t.Parallel()
	img := append([]byte{0x1f, 0x8b, 0x08, 0x00}, make([]byte, 4092)...)
	_, err := OpenReaderAt(bytes.NewReader(img), int64(len(img)), Options{Offset: -1, Accept: acceptAt(-1)})
	var nf *NotFoundError
	if !errors.As(err, &nf) || !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want a *NotFoundError", err)
	}
	if nf.Size != int64(len(img)) || nf.MapPath != "" {
		t.Errorf("NotFoundError = size %d map %q; want the image's size and no map", nf.Size, nf.MapPath)
	}
	if !strings.Contains(err.Error(), "gzip-compressed") {
		t.Errorf("error %q does not say the image is compressed", err)
	}
}

func TestOpenReaderAtRefusesAMapFile(t *testing.T) {
	t.Parallel()
	img := readFile(t, writeImage(t, 4096))
	if _, err := OpenReaderAt(bytes.NewReader(img), 4096, Options{Offset: -1, Map: "disk.map"}); err == nil {
		t.Error("a map file was accepted for a caller-supplied reader")
	}
	if _, err := OpenReaderAt(bytes.NewReader(img), -1, Options{Offset: -1}); err == nil {
		t.Error("a negative size was accepted")
	}
}

// closeCounter records whether anything closed the reader the caller owns.
type closeCounter struct {
	*bytes.Reader
	closed bool
}

func (c *closeCounter) Close() error {
	c.closed = true
	return nil
}

func TestOpenReaderAtLeavesTheReaderOpen(t *testing.T) {
	t.Parallel()
	img := readFile(t, writeImage(t, 4096))
	ra := &closeCounter{Reader: bytes.NewReader(img)}
	im, err := OpenReaderAt(ra, 4096, Options{Offset: -1, Accept: acceptAt(0)})
	if err != nil {
		t.Fatal(err)
	}
	if err := im.Close(); err != nil {
		t.Fatal(err)
	}
	if ra.closed {
		t.Error("Image.Close closed the caller's reader")
	}
}

// With no file name to go by, an 86F image is recognized by its signature.
func TestOpenReaderAt86F(t *testing.T) {
	t.Parallel()
	img := testfixture.Bytes(t, "../img86f/testdata/floppyout.86f.gz")
	want, err := Open(testfixture.File(t, "../img86f/testdata/floppyout.86f.gz"), Options{Offset: -1, Accept: acceptAt(0)})
	if err != nil {
		t.Fatal(err)
	}
	defer want.Close() //nolint:errcheck // test cleanup
	got, err := OpenReaderAt(bytes.NewReader(img), int64(len(img)), Options{Offset: -1, Accept: acceptAt(0)})
	if err != nil {
		t.Fatal(err)
	}
	defer got.Close() //nolint:errcheck // test cleanup
	if got.Source != want.Source || got.Size != want.Size {
		t.Errorf("OpenReaderAt = %q size %d; Open = %q size %d", got.Source, got.Size, want.Source, want.Size)
	}
	if !strings.HasPrefix(got.Source, "86F image") {
		t.Errorf("Source = %q, want the 86F decoder", got.Source)
	}
}
