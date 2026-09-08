package fat_test

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"samuelks.com/datarescue/ddrescue"
	"samuelks.com/datarescue/fat"
	"samuelks.com/datarescue/internal/testfat"
)

// fill reads r through ddrescue.Fill, which is the only thing that arms a gap
// log — a reader cannot zero-fill anywhere else, so this is also the only way
// to test that it does. It returns what came back and the holes recorded.
func fill(t *testing.T, r *fat.Reader) ([]byte, ddrescue.Filled) {
	t.Helper()
	var data, mapped nopCloser
	f, err := ddrescue.Fill(&data,
		func() (io.WriteCloser, string, error) { return &mapped, "out.map", nil },
		func(l *ddrescue.GapLog) (io.Reader, error) {
			r.Fill(l)
			return r, nil
		})
	if err != nil {
		t.Fatalf("writing the map beside the data: %v", err)
	}
	return data.Bytes(), f
}

type nopCloser struct{ bytes.Buffer }

func (*nopCloser) Close() error { return nil }

// errBadSector stands in for whatever a damaged image reports: a rescue map
// refusing an unrecovered range, or the medium itself.
var errBadSector = errors.New("bad sector")

// holedReader is an image with a byte range that will not read, with the
// semantics a ddrescue reader has: a read starting in the hole fails outright,
// and one running into it returns the bytes before it.
type holedReader struct {
	data    []byte
	off     int64
	badOff  int64
	badSize int64
}

func (r *holedReader) Read(p []byte) (int, error) {
	if r.off >= int64(len(r.data)) {
		return 0, io.EOF
	}
	end := min(r.off+int64(len(p)), int64(len(r.data)))
	if r.off >= r.badOff && r.off < r.badOff+r.badSize {
		return 0, errBadSector
	}
	if r.badOff > r.off && r.badOff < end {
		end = r.badOff
	}
	n := copy(p, r.data[r.off:end])
	r.off += int64(n)
	return n, nil
}

func (r *holedReader) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		r.off = off
	case io.SeekCurrent:
		r.off += off
	default:
		r.off = int64(len(r.data)) + off
	}
	return r.off, nil
}

// bigFile is four clusters, so a hole can land inside one and leave the
// clusters on either side intact.
const bigFile = "BIG.BIN"

const bigSize = 4*testfat.ClusterSize - 137 // a size that does not fill its last cluster

func openHoled(t *testing.T, badOff, badSize int64) (*fat.FAT, testfat.Placed) {
	t.Helper()
	im := testfat.Build(testfat.Options{Files: []testfat.File{{Name: bigFile, Size: bigSize}}})
	r := &holedReader{data: im.Data, badOff: badOff, badSize: badSize}
	fs, err := fat.Open(r, 0)
	if err != nil {
		t.Fatalf("opening the synthetic image: %v", err)
	}
	return fs, im.Find(bigFile)
}

func TestChainExtents(t *testing.T) {
	t.Parallel()
	fs, p := openHoled(t, -1, 0)
	extents, err := fs.ChainExtents(uint32(p.Cluster), bigSize)
	if err != nil {
		t.Fatal(err)
	}
	// The clusters follow one another on disk, so they are one extent, and it
	// stops at the end of the file rather than at the end of its last cluster.
	want := []fat.Extent{{FileOffset: 0, ImageOffset: p.Offset, Length: bigSize}}
	if len(extents) != 1 || extents[0] != want[0] {
		t.Errorf("ChainExtents = %+v; want %+v", extents, want)
	}
}

// A hole inside one cluster must cost that sector and nothing else: the rest of
// the cluster is read a sector at a time and comes back.
func TestFillGapsSector(t *testing.T) {
	t.Parallel()
	holeOff := int64(testfat.ClusterSize + testfat.SectorSize) // second sector of the second cluster
	im := testfat.Build(testfat.Options{Files: []testfat.File{{Name: bigFile, Size: bigSize}}})
	p := im.Find(bigFile)
	fs, _ := openHoled(t, p.Offset+holeOff, testfat.SectorSize)

	r, err := fs.OpenClusterChain(uint32(p.Cluster), bigSize)
	if err != nil {
		t.Fatal(err)
	}
	got, filled := fill(t, r)
	if filled.Err != nil {
		t.Fatalf("reading with a gap log: %v", filled.Err)
	}
	if len(got) != bigSize {
		t.Fatalf("recovered %d bytes; want the file's full %d", len(got), bigSize)
	}

	gaps := filled.Gaps
	if len(gaps) != 1 || gaps[0].FileOffset != holeOff || gaps[0].Length != testfat.SectorSize {
		t.Fatalf("gaps = %v; want one gap of %d bytes at %d", gaps, testfat.SectorSize, holeOff)
	}
	if filled.GapBytes != testfat.SectorSize {
		t.Errorf("GapBytes = %d; want %d", filled.GapBytes, testfat.SectorSize)
	}

	want := testfat.Content(bigFile, bigSize)
	clear(want[holeOff : holeOff+testfat.SectorSize])
	if !bytes.Equal(got, want) {
		t.Error("the recovered bytes are not the file's, with zeros only where the gap is")
	}
}

// A gap in the last cluster is clipped to the file: the unused tail of the
// cluster is not the file's data and must not be reported as lost.
func TestFillGapsClippedToFileSize(t *testing.T) {
	t.Parallel()
	im := testfat.Build(testfat.Options{Files: []testfat.File{{Name: bigFile, Size: bigSize}}})
	p := im.Find(bigFile)
	lastSector := int64(4*testfat.ClusterSize - testfat.SectorSize)
	fs, _ := openHoled(t, p.Offset+lastSector, testfat.SectorSize)

	r, err := fs.OpenClusterChain(uint32(p.Cluster), bigSize)
	if err != nil {
		t.Fatal(err)
	}
	got, filled := fill(t, r)
	if filled.Err != nil {
		t.Fatal(filled.Err)
	}
	if len(got) != bigSize {
		t.Fatalf("recovered %d bytes; want %d", len(got), bigSize)
	}
	gaps := filled.Gaps
	wantLen := int64(bigSize) - lastSector
	if len(gaps) != 1 || gaps[0].FileOffset != lastSector || gaps[0].Length != wantLen {
		t.Errorf("gaps = %v; want one gap of %d bytes at %d", gaps, wantLen, lastSector)
	}
}

// Without an armed log a read still latches at the damage, so a caller that
// has nowhere to record a hole cannot be handed one.
func TestWithoutAGapLogTheReadLatches(t *testing.T) {
	t.Parallel()
	im := testfat.Build(testfat.Options{Files: []testfat.File{{Name: bigFile, Size: bigSize}}})
	p := im.Find(bigFile)
	holeOff := int64(testfat.ClusterSize)
	fs, _ := openHoled(t, p.Offset+holeOff, testfat.SectorSize)

	r, err := fs.OpenClusterChain(uint32(p.Cluster), bigSize)
	if err != nil {
		t.Fatal(err)
	}
	// A log the caller builds for itself is inert: only ddrescue.Fill arms one.
	r.Fill(&ddrescue.GapLog{})

	got, err := io.ReadAll(r)
	if err == nil {
		t.Fatal("reading over a hole succeeded; want the read to stop at it")
	}
	if _, ok := errors.AsType[*fat.ClusterReadError](err); !ok {
		t.Errorf("error = %v; want a *ClusterReadError", err)
	}
	if int64(len(got)) != holeOff {
		t.Errorf("recovered %d bytes before the hole; want %d", len(got), holeOff)
	}
}
