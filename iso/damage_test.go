package iso_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"samuelks.com/datarescue/ddrescue"
	"samuelks.com/datarescue/internal/testiso"
	"samuelks.com/datarescue/iso"
)

// rawFile opens the raw test image with the given byte ranges marked
// unrecovered and returns a reader over BIG.DAT. The ranges go through a real
// ddrescue map, which is where the granularity comes from: a rescue reads in
// its own block size, and on a raw image that is not a whole 2352-byte sector.
func rawFile(t *testing.T, ranges ...[2]int) (*iso.Reader, *iso.FileReader) {
	t.Helper()
	img := testiso.BuildRaw()
	mapText := testiso.DamageRange(img, ranges...)
	dir := t.TempDir()
	path := filepath.Join(dir, "raw.iso")
	if err := os.WriteFile(path, img, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".map", []byte(mapText), 0o600); err != nil {
		t.Fatal(err)
	}
	rd, err := ddrescue.Open(path, path+".map")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rd.Close() })
	r, err := iso.OpenReaderWithSectorSize(rd, testiso.RawSectorSize)
	if err != nil {
		t.Fatalf("opening the damaged raw image: %v", err)
	}
	return r, r.OpenFile(iso.DirEntry{StartLBA: testiso.BigLBA, Size: testiso.BigSize})
}

// fillFile reads fr through ddrescue.Fill, the only thing that arms a gap log.
func fillFile(t *testing.T, fr *iso.FileReader) ([]byte, ddrescue.Filled) {
	t.Helper()
	var data, mapped nopCloser
	f, err := ddrescue.Fill(&data,
		func() (io.WriteCloser, string, error) { return &mapped, "out.map", nil },
		func(l *ddrescue.GapLog) (io.Reader, error) {
			fr.Fill(l)
			return fr, nil
		})
	if err != nil {
		t.Fatalf("writing the map beside the data: %v", err)
	}
	return data.Bytes(), f
}

type nopCloser struct{ bytes.Buffer }

func (*nopCloser) Close() error { return nil }

// eccRange is a range inside one raw sector's 288-byte EDC/ECC trailer that
// starts part way into it — which is what a rescue map whose blocks are not
// 2352-aligned produces, and the case a reader counting raw bytes gets wrong.
// The sector's whole payload is recovered; only bytes the file does not contain
// were lost.
func eccRange(lba int) [2]int {
	const start = 2112 // 2064 is where the trailer begins; this is inside it
	return [2]int{lba*testiso.RawSectorSize + start, (lba + 1) * testiso.RawSectorSize}
}

// Damage confined to a sector's EDC/ECC trailer loses none of the file: those
// bytes are the medium's, not the file's. Counting them as payload would splice
// error-correction data into the file and, on the filling path, compute a
// backwards slice and panic.
func TestRawSectorTrailerCostsTheFileNothing(t *testing.T) {
	t.Parallel()
	_, fr := rawFile(t, eccRange(testiso.BigLBA))
	got, filled := fillFile(t, fr)

	if filled.Err != nil {
		t.Fatalf("reading over a damaged sector trailer: %v", filled.Err)
	}
	if len(filled.Gaps) != 0 {
		t.Errorf("gaps = %v; the payload was fully recovered, so the file lost nothing", filled.Gaps)
	}
	if !bytes.Equal(got, testiso.BigData()) {
		t.Errorf("recovered %d bytes; want the file's own %d, unchanged", len(got), testiso.BigSize)
	}
}

// The same damage without a gap log: the reader latches, and what it hands over
// first must be the sector's payload and nothing after it.
func TestRawSectorTrailerDeliversOnlyThePayload(t *testing.T) {
	t.Parallel()
	_, fr := rawFile(t, eccRange(testiso.BigLBA))
	got, err := io.ReadAll(fr)
	if err == nil {
		t.Fatal("reading over a damaged sector succeeded; want the read to stop at it")
	}
	var sre *iso.SectorReadError
	if !errors.As(err, &sre) {
		t.Fatalf("error = %v; want a *iso.SectorReadError", err)
	}
	if sre.N != 2048 {
		t.Errorf("SectorReadError.N = %d; want the sector's 2048 payload bytes, not its raw length", sre.N)
	}
	if len(got) != 2048 {
		t.Fatalf("delivered %d bytes; want the 2048 of payload, with none of the trailer after them", len(got))
	}
	if !bytes.Equal(got, testiso.BigData()[:2048]) {
		t.Error("the bytes delivered are not the file's own")
	}
}

// Damage inside a sector's payload is a hole in the file, measured from where
// its bytes stopped coming back.
func TestRawSectorPayloadHole(t *testing.T) {
	t.Parallel()
	// 576 is 512-aligned in the image and lands 560 bytes into the payload.
	const badAt = 576
	from := testiso.BigLBA*testiso.RawSectorSize + badAt
	_, fr := rawFile(t, [2]int{from, from + 512})
	got, filled := fillFile(t, fr)

	if filled.Err != nil {
		t.Fatalf("reading with a gap log: %v", filled.Err)
	}
	if len(got) != testiso.BigSize {
		t.Fatalf("recovered %d bytes; want the declared %d", len(got), testiso.BigSize)
	}
	const have, payload = badAt - 16, 2048
	if len(filled.Gaps) != 1 || filled.Gaps[0].FileOffset != have || filled.Gaps[0].Length != payload-have {
		t.Fatalf("gaps = %v; want one of %d bytes at %d", filled.Gaps, payload-have, have)
	}
	want := testiso.BigData()
	clear(want[have:payload])
	if !bytes.Equal(got, want) {
		t.Error("the recovered bytes are not the file's, with zeros only where the payload was lost")
	}
}

// The unused tail of a file's last sector is not the file's, so damage there
// costs it nothing — and must not be recorded as a hole of negative length,
// which the log silently drops.
func TestRawDamageInTheUnusedTailOfTheLastSector(t *testing.T) {
	t.Parallel()
	last := testiso.BigLBA + testiso.BigSectors - 1
	// The file ends part way into this sector; 2016 is past that and inside the
	// payload the sector still carries.
	from := last*testiso.RawSectorSize + 2016
	_, fr := rawFile(t, [2]int{from, (last + 1) * testiso.RawSectorSize})
	got, filled := fillFile(t, fr)

	if filled.Err != nil {
		t.Fatalf("reading with a gap log: %v", filled.Err)
	}
	if len(filled.Gaps) != 0 {
		t.Errorf("gaps = %v; the bytes lost were past the end of the file", filled.Gaps)
	}
	if !bytes.Equal(got, testiso.BigData()) {
		t.Error("the recovered bytes are not the file's own")
	}
}
