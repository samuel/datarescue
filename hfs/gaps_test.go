package hfs

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"samuelks.com/datarescue/ddrescue"
	"samuelks.com/datarescue/internal/testfixture"
)

// damaged marks byte ranges of the image unrecovered and returns a reader that
// enforces the map, so a read of a lost range fails the way it would on the
// medium rather than handing back bytes that are still in the file.
func damaged(t *testing.T, data []byte, ranges ...[2]int64) *ddrescue.Reader {
	t.Helper()
	var b bytes.Buffer
	b.WriteString("# Mapfile. Created by a test.\n0x00000000     +               1\n#      pos        size  status\n")
	var pos int64
	for _, r := range ranges {
		if r[0] > pos {
			b.WriteString(hexLine(pos, r[0]-pos, '+'))
		}
		b.WriteString(hexLine(r[0], r[1], '-'))
		pos = r[0] + r[1]
	}
	if pos < int64(len(data)) {
		b.WriteString(hexLine(pos, int64(len(data))-pos, '+'))
	}
	path := filepath.Join(t.TempDir(), "disk.img.map")
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	blocks, err := ddrescue.ParseMap(path)
	if err != nil {
		t.Fatal(err)
	}
	return ddrescue.NewReader(bytes.NewReader(data), blocks)
}

func hexLine(off, size int64, status byte) string {
	const hex = "0123456789abcdef"
	render := func(n int64) string {
		b := []byte("0x00000000")
		for i := 9; i >= 2 && n > 0; i-- {
			b[i] = hex[n&0xF]
			n >>= 4
		}
		return string(b)
	}
	return render(off) + "  " + render(size) + "  " + string(status) + "\n"
}

// holeIn returns the fixture with one logical block inside the given fork
// marked unrecovered, and where that hole falls within the fork.
//
// ForkRanges locates the hole: the fork's extents say where its bytes are, so
// a test can put a hole in a known file without asking the reader under test
// where anything is.
func holeIn(t *testing.T, resource bool) (*Volume, Entry, int64) {
	t.Helper()
	data := testfixture.Bytes(t, testImagePath)
	v, err := openBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	e := find(t, v, "TwinPeaksTrivia")
	ref := e.DataFork()
	if resource {
		ref = e.ResourceFork()
	}
	ranges, err := v.ForkRanges(ref)
	if err != nil {
		t.Fatal(err)
	}
	// One logical block, a little way into the fork, so there are recovered
	// bytes on both sides of it.
	const into = 4 * logicalBlockSize
	hole := ranges[0].ImageOffset + into

	v2, err := Open(damaged(t, data, [2]int64{hole, logicalBlockSize}), 0, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	return v2, find(t, v2, "TwinPeaksTrivia"), into
}

// A reader handed no armed log latches at the damage: it delivers what came
// back and then refuses, rather than producing zeros nothing records.
func TestAForkLatchesAtTheDamage(t *testing.T) {
	t.Parallel()
	v, e, at := holeIn(t, false)
	r, err := v.OpenFile(e)
	if err != nil {
		t.Fatal(err)
	}
	n, err := io.Copy(io.Discard, r)
	if err == nil {
		t.Fatal("reading over a hole returned no error")
	}
	if n != at {
		t.Errorf("recovered %d bytes before the damage; want %d", n, at)
	}
	var fe *ForkReadError
	if !errors.As(err, &fe) {
		t.Fatalf("error %v is not a *ForkReadError", err)
	}
	if fe.CNID != e.CNID {
		t.Errorf("the error names file %d; want %d", fe.CNID, e.CNID)
	}

	// Latched: a caller that keeps reading cannot splice later bytes in at the
	// offset the lost ones should have occupied.
	before := err
	if _, err := r.Read(make([]byte, 512)); !errors.Is(err, before) {
		t.Errorf("a retry after the damage returned %v; want the latched error", err)
	}
}

// Handed an armed log, the reader fills the hole and writes down where it is.
// The length can be computed exactly (an extent's block count times the block
// size), so it is fillable.
func TestAForkFillsAndRecordsTheHole(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		resource bool
	}{
		{"data fork", false},
		{"resource fork", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v, e, at := holeIn(t, tc.resource)
			ref := e.DataFork()
			if tc.resource {
				ref = e.ResourceFork()
			}
			r, err := v.OpenFork(ref)
			if err != nil {
				t.Fatal(err)
			}
			log := armedLog(t)
			r.Fill(log)

			got, err := io.ReadAll(r)
			if err != nil {
				t.Fatalf("a filling read failed: %v", err)
			}
			if int64(len(got)) != ref.Size {
				t.Errorf("read %d bytes; want the fork's full %d", len(got), ref.Size)
			}
			gaps := log.Gaps()
			if len(gaps) != 1 {
				t.Fatalf("recorded %d gaps; want 1", len(gaps))
			}
			if gaps[0].FileOffset != at {
				t.Errorf("the gap is at %d; want %d", gaps[0].FileOffset, at)
			}
			if gaps[0].Length != logicalBlockSize {
				t.Errorf("the gap is %d bytes; want %d — a bad sector costs its sector",
					gaps[0].Length, logicalBlockSize)
			}
			if !bytes.Equal(got[at:at+logicalBlockSize], make([]byte, logicalBlockSize)) {
				t.Error("the hole was not zero-filled")
			}
		})
	}
}

// A companion file whose fork is written after a header needs the base: the
// holes are recorded at base+offset, so the map describes the file that was
// written rather than the fork it came from.
func TestFillAtShiftsEveryHole(t *testing.T) {
	t.Parallel()
	v, e, at := holeIn(t, true)
	r, err := v.OpenResourceFork(e)
	if err != nil {
		t.Fatal(err)
	}
	const base = 110
	log := armedLog(t)
	r.FillAt(log, base)
	if _, err := io.ReadAll(r); err != nil {
		t.Fatal(err)
	}
	gaps := log.Gaps()
	if len(gaps) != 1 {
		t.Fatalf("recorded %d gaps; want 1", len(gaps))
	}
	if gaps[0].FileOffset != base+at {
		t.Errorf("the gap is at %d; want %d — the header shifts every hole", gaps[0].FileOffset, base+at)
	}
}

// A log a caller builds for itself is inert, and the reader still latches.
// Only ddrescue.Fill arms one, and it writes the map in the same call, so zeros
// cannot be produced where nothing records them.
func TestAnUnarmedLogDoesNotFill(t *testing.T) {
	t.Parallel()
	v, e, _ := holeIn(t, false)
	r, err := v.OpenFile(e)
	if err != nil {
		t.Fatal(err)
	}
	r.Fill(&ddrescue.GapLog{})
	if _, err := io.Copy(io.Discard, r); err == nil {
		t.Error("an unarmed log made the reader fill; only ddrescue.Fill may arm one")
	}
}

// armedLog returns a log in the state ddrescue.Fill puts one in, which is the
// only thing that arms one in production.
func armedLog(t *testing.T) *ddrescue.GapLog {
	t.Helper()
	var log *ddrescue.GapLog
	dst := nopWriteCloser{io.Discard}
	mapDest := func() (io.WriteCloser, string, error) { return nopWriteCloser{io.Discard}, "", nil }
	//nolint:errcheck // the copy is a no-op; the log is what is wanted
	ddrescue.Fill(dst, mapDest, func(l *ddrescue.GapLog) (io.Reader, error) {
		log = l
		return bytes.NewReader(nil), nil
	})
	if log == nil {
		t.Fatal("ddrescue.Fill did not hand over a log")
	}
	return log
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// A volume that does not start at byte 0 must read from where it starts.
//
// Every fork offset comes from AllocBlockOffset, which has already added the
// volume's base, so a read path that adds it again lands at twice the offset —
// which is invisible on a bare volume at 0 and wrong on every disk that carries
// a partition map.
func TestAVolumeAtANonZeroOffsetReadsFromWhereItStarts(t *testing.T) {
	t.Parallel()
	const pad = 5632 // where the HFS volume sits on the real Zip disks
	vol := testfixture.Bytes(t, testImagePath)
	data := make([]byte, pad+len(vol))
	// The padding is not zeros: a reader landing in it must not be able to
	// mistake it for a volume, and a fork read landing in it must not look
	// like a run of holes.
	for i := range pad {
		data[i] = 0xAA
	}
	copy(data[pad:], vol)

	v, err := Open(bytes.NewReader(data), pad, int64(len(data)-pad))
	if err != nil {
		t.Fatal(err)
	}
	if v.MDB.Label() != "disk" {
		t.Fatalf("volume label = %q; want disk", v.MDB.Label())
	}
	e := find(t, v, "TwinPeaksTrivia")

	// The fork's ranges are image offsets, so they sit past the padding.
	ranges, err := v.ForkRanges(e.DataFork())
	if err != nil {
		t.Fatal(err)
	}
	if ranges[0].ImageOffset < pad {
		t.Errorf("the fork starts at %d, inside the padding before the volume", ranges[0].ImageOffset)
	}

	// And it reads the same bytes it does when the volume starts at 0.
	bare, err := openBytes(vol)
	if err != nil {
		t.Fatal(err)
	}
	want := readAll(t, bare, find(t, bare, "TwinPeaksTrivia"))
	got := readAll(t, v, e)
	if !bytes.Equal(got, want) {
		t.Errorf("the volume at offset %d read %d bytes differing from the same volume at 0 (%d bytes)",
			pad, len(got), len(want))
	}
}

func readAll(t *testing.T, v *Volume, e Entry) []byte {
	t.Helper()
	r, err := v.OpenFile(e)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
