package diskimage

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"samuelks.com/datarescue/apm"
	"samuelks.com/datarescue/ddrescue"
	"samuelks.com/datarescue/internal/testapm"
	"samuelks.com/datarescue/mbr"
)

// writeImage renders a disk with an MBR and returns its path.
func writeImage(t *testing.T, size int, parts ...mbr.Partition) string {
	t.Helper()
	disk := make([]byte, size)
	for i, p := range parts {
		off := 446 + i*16
		disk[off+4] = p.PartitionType
		binary.LittleEndian.PutUint32(disk[off+8:], p.FirstSectorLBA)
		binary.LittleEndian.PutUint32(disk[off+12:], p.NumberOfSectors)
	}
	disk[510], disk[511] = 0x55, 0xAA
	path := filepath.Join(t.TempDir(), "disk.img")
	if err := os.WriteFile(path, disk, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// acceptAt returns an Accept predicate matching exactly one offset.
func acceptAt(want int64) func(io.ReadSeeker, int64) bool {
	return func(_ io.ReadSeeker, off int64) bool { return off == want }
}

func TestOpenExplicitOffset(t *testing.T) {
	t.Parallel()
	path := writeImage(t, 1<<20)
	im, err := Open(path, Options{Offset: 4096, Accept: acceptAt(-1)})
	if err != nil {
		t.Fatal(err)
	}
	defer im.Close() //nolint:errcheck // test cleanup
	if im.Offset != 4096 {
		t.Errorf("Offset = %d; want 4096", im.Offset)
	}
	// An explicit offset is taken as given: Accept is not consulted.
	t.Logf("source: %s", im.Source)
}

func TestOpenUnpartitioned(t *testing.T) {
	t.Parallel()
	path := writeImage(t, 1<<20)
	im, err := Open(path, Options{Offset: -1, Accept: acceptAt(0)})
	if err != nil {
		t.Fatal(err)
	}
	defer im.Close() //nolint:errcheck // test cleanup
	if im.Offset != 0 {
		t.Errorf("Offset = %d; want 0", im.Offset)
	}
}

func TestOpenAutoDetectsPartition(t *testing.T) {
	t.Parallel()
	const lba = 2048
	path := writeImage(t, 4<<20,
		mbr.Partition{PartitionType: 0x83, FirstSectorLBA: 64, NumberOfSectors: 100},
		mbr.Partition{PartitionType: 0x0C, FirstSectorLBA: lba, NumberOfSectors: 1000},
	)
	im, err := Open(path, Options{
		Offset:           -1,
		Accept:           acceptAt(lba * mbr.SectorSize),
		PartitionMatches: mbr.IsFAT,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer im.Close() //nolint:errcheck // test cleanup
	if im.Offset != lba*mbr.SectorSize {
		t.Errorf("Offset = %d; want %d", im.Offset, lba*mbr.SectorSize)
	}
	t.Logf("source: %s", im.Source)
}

func TestOpenNamedPartition(t *testing.T) {
	t.Parallel()
	path := writeImage(t, 4<<20,
		mbr.Partition{PartitionType: 0x83, FirstSectorLBA: 64, NumberOfSectors: 100},
		mbr.Partition{PartitionType: 0x0C, FirstSectorLBA: 2048, NumberOfSectors: 1000},
	)
	im, err := Open(path, Options{Offset: -1, Partition: 2, Accept: acceptAt(-1)})
	if err != nil {
		t.Fatal(err)
	}
	defer im.Close() //nolint:errcheck // test cleanup
	if im.Offset != 2048*mbr.SectorSize {
		t.Errorf("Offset = %d; want %d", im.Offset, 2048*mbr.SectorSize)
	}

	if _, err := Open(path, Options{Offset: -1, Partition: 4, Accept: acceptAt(-1)}); err == nil {
		t.Error("selecting an absent partition returned no error")
	}
}

func TestOpenNotFound(t *testing.T) {
	t.Parallel()
	// Nothing at offset 0, no partition table.
	path := filepath.Join(t.TempDir(), "blank.img")
	if err := os.WriteFile(path, make([]byte, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Open(path, Options{Offset: -1, Accept: acceptAt(-1)})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Open on a blank image = %v; want ErrNotFound", err)
	}
}

func TestOpenGPTYieldsNothing(t *testing.T) {
	t.Parallel()
	path := writeImage(t, 4<<20, mbr.Partition{
		PartitionType:   mbr.TypeGPTProtective,
		FirstSectorLBA:  1,
		NumberOfSectors: 0xFFFFFFFF,
	})
	// A GPT disk has no MBR partitions to try, so detection must report that
	// rather than parsing the protective entry as one huge partition.
	_, err := Open(path, Options{Offset: -1, Accept: acceptAt(-1)})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Open on a GPT disk = %v; want ErrNotFound", err)
	}
}

func TestOpenMissingFile(t *testing.T) {
	t.Parallel()
	if _, err := Open(filepath.Join(t.TempDir(), "nope.img"), Options{Offset: 0}); err == nil {
		t.Error("Open on a missing file returned no error")
	}
}

func TestParseGeometry(t *testing.T) {
	t.Parallel()
	spt, heads, bps, err := ParseGeometry("18:2:512")
	if err != nil || spt != 18 || heads != 2 || bps != 512 {
		t.Errorf("ParseGeometry(18:2:512) = %d,%d,%d,%v", spt, heads, bps, err)
	}
	for _, bad := range []string{"", "18:2", "18:2:512:1", "a:2:512", "0:2:512", "18:-1:512"} {
		if _, _, _, err := ParseGeometry(bad); err == nil {
			t.Errorf("ParseGeometry(%q) = nil error", bad)
		}
	}
}

// A reader that cannot report a partition table must not be mistaken for one.
func TestLocateWithoutReaderAt(t *testing.T) {
	t.Parallel()
	type seekOnly struct{ io.ReadSeeker }
	r := seekOnly{bytes.NewReader(make([]byte, 1024))}
	if _, err := locate(r, io.NopCloser(nil), 0, Options{Offset: -1, Accept: acceptAt(-1)}); !errors.Is(err, ErrNotFound) {
		t.Errorf("locate without io.ReaderAt = %v; want ErrNotFound", err)
	}
}

// writeMap writes a ddrescue map beside an image, marking one range as failed.
func writeMap(t *testing.T, image string, size int64, badOff, badSize int64) string {
	t.Helper()
	body := "# Mapfile. Created by a test.\n0x00000000     +               1\n#      Start        Size  Status\n" +
		fmt.Sprintf("0x%08x  0x%08x  +\n", 0, badOff) +
		fmt.Sprintf("0x%08x  0x%08x  -\n", badOff, badSize) +
		fmt.Sprintf("0x%08x  0x%08x  +\n", badOff+badSize, size-badOff-badSize)
	path := image + ".map"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A map beside the image is picked up without being given, and the partition
// table is still read through it: locating the filesystem needs random access,
// which the map-checked reader has to provide as well as sequential reads.
func TestOpenPicksUpSidecarMap(t *testing.T) {
	t.Parallel()
	const size = 1 << 20
	path := writeImage(t, size, mbr.Partition{PartitionType: 0x06, FirstSectorLBA: 2048, NumberOfSectors: 1024})
	mapPath := writeMap(t, path, size, 4096, 512)

	im, err := Open(path, Options{Offset: -1, Accept: acceptAt(2048 * 512)})
	if err != nil {
		t.Fatal(err)
	}
	defer im.Close() //nolint:errcheck // test cleanup
	if im.MapPath != mapPath {
		t.Errorf("MapPath = %q; want %q", im.MapPath, mapPath)
	}
	if n := len(im.Unrecovered(0, size)); n != 1 {
		t.Errorf("Unrecovered over the whole image = %d ranges; want 1", n)
	}
	if n := len(im.Unrecovered(0, 4096)); n != 0 {
		t.Errorf("Unrecovered before the damage = %d ranges; want none", n)
	}
	// Reading the failed range must fail rather than return the image bytes.
	buf := make([]byte, 16)
	if _, err := im.Reader.Seek(4096, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if _, err := im.Reader.Read(buf); !errors.Is(err, ddrescue.ErrNotRecovered) {
		t.Errorf("reading the failed range = %v; want ErrNotRecovered", err)
	}
}

func TestOpenMapOptions(t *testing.T) {
	t.Parallel()
	const size = 1 << 20
	path := writeImage(t, size)
	writeMap(t, path, size, 4096, 512)

	im, err := Open(path, Options{Offset: 0, NoMap: true})
	if err != nil {
		t.Fatal(err)
	}
	defer im.Close() //nolint:errcheck // test cleanup
	if im.MapPath != "" {
		t.Errorf("NoMap still opened through %q", im.MapPath)
	}

	if _, err := Open(path, Options{Offset: 0, Map: path + ".missing"}); err == nil {
		t.Error("a named map that is not there was accepted")
	}
	if _, err := Open(path, Options{Offset: 0, Map: path + ".map", NoMap: true}); err == nil {
		t.Error("-map together with -no-map was accepted")
	}
}

// A logical partition cannot be selected with -partition. Its index is a
// position in the extended chain rather than a slot in the table, so numbering
// it alongside the primaries would make "partition 1" mean two different things
// on one disk. Auto-detection still finds it.
func TestOpenCannotNameALogicalPartition(t *testing.T) {
	t.Parallel()
	const extLBA, logicalLBA = 2048, 2050
	disk := make([]byte, 4<<20)
	writeEntry := func(sector int64, slot int, p mbr.Partition) {
		base := sector*mbr.SectorSize + 446 + int64(slot)*16
		disk[base+4] = p.PartitionType
		binary.LittleEndian.PutUint32(disk[base+8:], p.FirstSectorLBA)
		binary.LittleEndian.PutUint32(disk[base+12:], p.NumberOfSectors)
		disk[sector*mbr.SectorSize+510] = 0x55
		disk[sector*mbr.SectorSize+511] = 0xAA
	}
	// One primary and one extended partition holding a single logical one.
	writeEntry(0, 0, mbr.Partition{PartitionType: 0x83, FirstSectorLBA: 64, NumberOfSectors: 100})
	writeEntry(0, 1, mbr.Partition{PartitionType: mbr.TypeExtendedLBA, FirstSectorLBA: extLBA, NumberOfSectors: 4000})
	writeEntry(extLBA, 0, mbr.Partition{PartitionType: 0x0C, FirstSectorLBA: logicalLBA - extLBA, NumberOfSectors: 1000})

	path := filepath.Join(t.TempDir(), "disk.img")
	if err := os.WriteFile(path, disk, 0o600); err != nil {
		t.Fatal(err)
	}

	// Auto-detection finds it.
	im, err := Open(path, Options{
		Offset:           -1,
		Accept:           acceptAt(logicalLBA * mbr.SectorSize),
		PartitionMatches: mbr.IsFAT,
	})
	if err != nil {
		t.Fatalf("auto-detection did not find the logical partition: %v", err)
	}
	defer im.Close() //nolint:errcheck // test cleanup
	if im.Offset != logicalLBA*mbr.SectorSize {
		t.Fatalf("Offset = %d; want %d", im.Offset, logicalLBA*mbr.SectorSize)
	}

	// Selecting it by number does not. Index 1 is the chain position of the
	// logical partition and the slot of the primary; the primary wins, and it
	// is not at the logical partition's offset.
	named, err := Open(path, Options{Offset: -1, Partition: 1, Accept: acceptAt(-1)})
	if err != nil {
		t.Fatal(err)
	}
	defer named.Close() //nolint:errcheck // test cleanup
	if named.Offset == logicalLBA*mbr.SectorSize {
		t.Error("-partition 1 selected the logical partition rather than the primary in slot 1")
	}
	if named.Offset != 64*mbr.SectorSize {
		t.Errorf("Offset = %d; want the primary at %d", named.Offset, 64*mbr.SectorSize)
	}
}

// Size is the byte range the located filesystem may draw on: the whole image
// for a volume at an offset, the located partition's own end for one found in a
// partition table.
func TestSizeIsTheLocatedRange(t *testing.T) {
	t.Parallel()
	const size = 4 << 20
	const lba, sectors = 2048, 1024
	path := writeImage(t, size, mbr.Partition{
		PartitionType:   0x0B,
		FirstSectorLBA:  lba,
		NumberOfSectors: sectors,
	})
	wantPart := int64(lba)*mbr.SectorSize + int64(sectors)*mbr.SectorSize
	for _, tc := range []struct {
		name string
		opts Options
		want int64
	}{
		{"explicit offset", Options{Offset: 0}, size},
		{"offset 0 auto", Options{Offset: -1, Accept: acceptAt(0)}, size},
		{"mbr partition", Options{Offset: -1, Accept: acceptAt(int64(lba) * mbr.SectorSize)}, wantPart},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			im, err := Open(path, tc.opts)
			if err != nil {
				t.Fatal(err)
			}
			defer im.Close() //nolint:errcheck // test cleanup
			if im.Size != tc.want {
				t.Errorf("Size = %d; want %d", im.Size, tc.want)
			}
		})
	}
}

// Size is the MBR partition's own end, not the image's. HFS depends on it: the
// spare master directory block sits two logical blocks from the end of the
// volume, and no field inside the volume says where that is.
func TestMBRPartitionSizeIsThePartitionsOwn(t *testing.T) {
	t.Parallel()
	const size = 4 << 20
	const partLBA, partSectors = 4096, 512
	path := writeImage(t, size, mbr.Partition{
		PartitionType:   0xAF,
		FirstSectorLBA:  partLBA,
		NumberOfSectors: partSectors,
	})
	want := int64(partLBA)*mbr.SectorSize + int64(partSectors)*mbr.SectorSize

	im, err := Open(path, Options{Offset: -1, Accept: acceptAt(int64(partLBA) * mbr.SectorSize)})
	if err != nil {
		t.Fatal(err)
	}
	defer im.Close() //nolint:errcheck // test cleanup
	if im.Size != want {
		t.Errorf("Size = %d; want %d, the partition's end and not the image's %d", im.Size, want, size)
	}
}

// A parser that reads by offset must see the filesystem at 0, not the front of
// the image. Folding the offset in here means no parser has to add it itself,
// so none can forget to.
func TestReaderAtIsPositionedAtTheFilesystem(t *testing.T) {
	t.Parallel()
	const off = 2048 * mbr.SectorSize
	path := writeImage(t, 4<<20, mbr.Partition{
		PartitionType:   0x0B,
		FirstSectorLBA:  2048,
		NumberOfSectors: 1024,
	})
	// Mark the first byte of the partition so its position is unambiguous.
	f, err := os.OpenFile(path, os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte{0xA5}, off); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	im, err := Open(path, Options{Offset: -1, Accept: acceptAt(off)})
	if err != nil {
		t.Fatal(err)
	}
	defer im.Close() //nolint:errcheck // test cleanup
	ra, err := im.ReaderAt()
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1)
	if _, err := ra.ReadAt(buf, 0); err != nil {
		t.Fatal(err)
	}
	if buf[0] != 0xA5 {
		t.Errorf("byte 0 through ReaderAt = %#x; want 0xA5", buf[0])
	}
}

func TestNoSuchPartitionIsItsOwnError(t *testing.T) {
	t.Parallel()
	path := writeImage(t, 4<<20, mbr.Partition{
		PartitionType:   0x0B,
		FirstSectorLBA:  2048,
		NumberOfSectors: 1024,
	})
	// Asking for a partition the table does not hold is the operator's mistake,
	// and the caller turns it into a usage error. It cannot be told apart from
	// an I/O failure without a sentinel of its own.
	_, err := Open(path, Options{Offset: -1, Partition: 4, Accept: acceptAt(-1)})
	if !errors.Is(err, ErrNoSuchPartition) {
		t.Errorf("selecting an absent partition = %v; want ErrNoSuchPartition", err)
	}
}

// The map can explain why no filesystem was located: a boot sector or volume
// descriptor that was never recovered is exactly the case where nothing can be
// found. Reporting only "no filesystem found" would give the least information
// in the case the map exists for.
func TestNotFoundCarriesTheMap(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "blank.img")
	if err := os.WriteFile(path, make([]byte, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	// The partition table reads and says nothing is there; the rest of the
	// image was never recovered. That is the case under test; a map over an
	// image whose table will not read at all fails earlier and more loudly.
	mapPath := path + ".map"
	const mapText = "0x00000000  0x00000200  +\n0x00000200  0x000FFE00  -\n"
	if err := os.WriteFile(mapPath, []byte(mapText), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Open(path, Options{Offset: -1, Accept: acceptAt(-1)})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Open on a blank image = %v; want ErrNotFound", err)
	}
	var nf *NotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("error %v is not a *NotFoundError", err)
	}
	if nf.MapPath != mapPath {
		t.Errorf("MapPath = %q; want %q", nf.MapPath, mapPath)
	}
	if nf.Size != 1<<20 {
		t.Errorf("Size = %d; want %d", nf.Size, 1<<20)
	}
	if len(nf.Blocks) == 0 {
		t.Error("Blocks is empty; the map's contents are what say how much was lost")
	}
}

// apmImageFile writes an Apple-partitioned image whose filesystem partition is
// recognizable by a byte at its start.
func apmImageFile(t *testing.T, opts testapm.Options, parts ...testapm.Partition) (string, []testapm.Placed) {
	t.Helper()
	img, placed := testapm.Build(opts, parts...)
	path := filepath.Join(t.TempDir(), "apple.img")
	if err := os.WriteFile(path, img, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, placed
}

func hfsPartitions() []testapm.Partition {
	return []testapm.Partition{
		{Type: apm.TypeDriver43, Blocks: 8},
		{Type: apm.TypeHFS, Name: "ISIS", Blocks: 40},
		{Type: apm.TypeFree, Blocks: 4},
	}
}

func TestOpenFindsAnApplePartition(t *testing.T) {
	t.Parallel()
	path, placed := apmImageFile(t, testapm.Options{MapBlocks: 10, Fill: 0xFA}, hfsPartitions()...)
	// placed[2] is the HFS partition: the map is always placed first.
	want := placed[2].Offset

	im, err := Open(path, Options{Offset: -1, Accept: acceptAt(want)})
	if err != nil {
		t.Fatal(err)
	}
	defer im.Close() //nolint:errcheck // test cleanup
	if im.Offset != want {
		t.Errorf("Offset = %d; want %d", im.Offset, want)
	}
	if !strings.Contains(im.Source, "Apple partition 3") {
		t.Errorf("Source = %q; want it to name the Apple partition and its index", im.Source)
	}
	if !strings.Contains(im.Source, "ISIS") {
		t.Errorf("Source = %q; want it to name the partition", im.Source)
	}
}

// Size is the partition's own end, not the image's. HFS depends on it: the
// spare master directory block sits two logical blocks from the end of the
// volume, and no field inside the volume says where that is.
func TestApplePartitionSizeIsThePartitionsOwn(t *testing.T) {
	t.Parallel()
	path, placed := apmImageFile(t, testapm.Options{}, hfsPartitions()...)
	want := placed[2].Offset + placed[2].Size

	im, err := Open(path, Options{Offset: -1, Accept: acceptAt(placed[2].Offset)})
	if err != nil {
		t.Fatal(err)
	}
	defer im.Close() //nolint:errcheck // test cleanup
	if im.Size != want {
		t.Errorf("Size = %d; want %d, the partition's end and not the image's", im.Size, want)
	}
}

// The number is the entry's position in the map, so what a listing prints is
// what an operator types. Numbering only the interesting entries would produce
// numbers the disk does not record.
func TestApplePartitionNumbersEveryWrittenEntry(t *testing.T) {
	t.Parallel()
	path, placed := apmImageFile(t, testapm.Options{}, hfsPartitions()...)

	for i, want := range placed {
		im, err := Open(path, Options{Offset: -1, Partition: i + 1})
		if err != nil {
			t.Fatalf("-partition %d: %v", i+1, err)
		}
		if im.Offset != want.Offset {
			t.Errorf("-partition %d at %d; want %d", i+1, im.Offset, want.Offset)
		}
		im.Close() //nolint:errcheck // test cleanup
	}

	_, err := Open(path, Options{Offset: -1, Partition: 99})
	if !errors.Is(err, ErrNoSuchPartition) {
		t.Errorf("-partition 99 = %v; want ErrNoSuchPartition", err)
	}
}

// Auto-detection skips what cannot hold a filesystem, even though those entries
// still have numbers.
func TestAutoDetectionSkipsTheMapAndDriverPartitions(t *testing.T) {
	t.Parallel()
	path, placed := apmImageFile(t, testapm.Options{}, hfsPartitions()...)
	// Accept any partition offset. Offset 0 is refused so the probe reaches the
	// map at all — it is tried first by design, so that a bare volume is never
	// reinterpreted as a partitioned disk. The first offer must then be the HFS
	// partition, not the map at block 1 or the driver partition after it.
	im, err := Open(path, Options{Offset: -1, Accept: func(_ io.ReadSeeker, off int64) bool { return off != 0 }})
	if err != nil {
		t.Fatal(err)
	}
	defer im.Close() //nolint:errcheck // test cleanup
	if im.Offset != placed[2].Offset {
		t.Errorf("auto-detection chose offset %d; want the HFS partition at %d", im.Offset, placed[2].Offset)
	}
}

// A disk carrying no Apple map falls through to the MBR rather than failing.
func TestAnMBRDiskIsNotMistakenForAnAppleOne(t *testing.T) {
	t.Parallel()
	path := writeImage(t, 4<<20, mbr.Partition{
		PartitionType:   0x0B,
		FirstSectorLBA:  2048,
		NumberOfSectors: 1024,
	})
	im, err := Open(path, Options{Offset: -1, Accept: acceptAt(2048 * mbr.SectorSize)})
	if err != nil {
		t.Fatal(err)
	}
	defer im.Close() //nolint:errcheck // test cleanup
	if !strings.Contains(im.Source, "partition 1") || strings.Contains(im.Source, "Apple") {
		t.Errorf("Source = %q; want the MBR partition", im.Source)
	}
}

// TestCompressedImage checks that an image in which nothing was found is
// reported as compressed when it is, while still being ErrNotFound with the map
// attached.
func TestCompressedImage(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "disk.img.gz")
	data := append([]byte{0x1f, 0x8b, 0x08, 0x00}, make([]byte, 4092)...)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Open(path, Options{Offset: -1, Accept: func(io.ReadSeeker, int64) bool { return false }})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Open returned %v, want ErrNotFound", err)
	}
	var nf *NotFoundError
	if !errors.As(err, &nf) {
		t.Errorf("error %v is not a *NotFoundError", err)
	}
	if !strings.Contains(err.Error(), "gzip-compressed") {
		t.Errorf("error %q does not say the image is gzip-compressed", err)
	}
}
