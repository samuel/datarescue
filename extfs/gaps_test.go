package extfs_test

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"samuelks.com/datarescue/ddrescue"
	"samuelks.com/datarescue/extfs"
	"samuelks.com/datarescue/internal/testfixture"
)

// bigName is the fixture's multi-block file. It is 400 blocks of 1 KiB, so its
// data reaches past the single indirect block (logical 12–267) and on into the
// double indirect, which is what lets a test lose one pointer block and still
// check that the blocks after the run it addressed come back correct.
const bigName = "big.dat"

// bigFile returns the fixture's large file: its inode number, its inode, and
// its contents read with nothing faulted.
func bigFile(t *testing.T, fs *extfs.FS) (uint32, *extfs.Inode, []byte) {
	t.Helper()
	root, err := fs.ReadInode(extfs.RootInodeNum)
	if err != nil {
		t.Fatalf("read root inode: %s", err)
	}
	entries, err := fs.ReadDir(root, false)
	if err != nil {
		t.Fatalf("read root directory: %s", err)
	}
	for _, e := range entries {
		if e.Name != bigName {
			continue
		}
		ino, err := fs.ReadInode(e.Inode)
		if err != nil {
			t.Fatalf("read inode %d: %s", e.Inode, err)
		}
		rd, err := fs.ReadInodeData(ino)
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(rd)
		if err != nil {
			t.Fatalf("reading %s from the sound image: %s", bigName, err)
		}
		if bytes.IndexByte(data, 0) >= 0 {
			t.Fatalf("%s holds a zero byte, so a zero-filled hole in it could not be told from its data", bigName)
		}
		return e.Inode, ino, data
	}
	t.Fatalf("fixture has no %s", bigName)
	return 0, nil, nil
}

// faulted reopens the fixture with the given byte ranges refusing to read, and
// returns a reader for the same inode alongside the faulting image, which
// counts the reads it turned down.
func faulted(t *testing.T, inoNum uint32, faults ...[2]int64) (*extfs.InodeReader, *faultReader) {
	t.Helper()
	fr := newFaultReader(testfixture.Reader(t, testImagePath), faults...)
	fs, err := extfs.Open(fr, 0)
	if err != nil {
		t.Fatalf("opening the faulted image: %s", err)
	}
	ino, err := fs.ReadInode(inoNum)
	if err != nil {
		t.Fatalf("read inode %d: %s", inoNum, err)
	}
	rd, err := fs.ReadInodeData(ino)
	if err != nil {
		t.Fatal(err)
	}
	return rd, fr
}

// fill reads r through ddrescue.Fill, which is the only thing that arms a gap
// log — a reader cannot zero-fill anywhere else, so this is also the only way
// to test that it does. It returns what came back and the holes recorded.
func fill(t *testing.T, r *extfs.InodeReader) ([]byte, ddrescue.Filled) {
	t.Helper()
	var data, mapped nopCloser
	f, err := ddrescue.Fill(&data,
		func() (io.WriteCloser, string, error) { return &mapped, "out.map", nil },
		func(l *ddrescue.GapLog) (io.Reader, error) {
			r.Fill(l)
			return r, nil
		})
	if err != nil {
		t.Fatalf("writing the map beside the data: %s", err)
	}
	return data.Bytes(), f
}

type nopCloser struct{ bytes.Buffer }

func (*nopCloser) Close() error { return nil }

// blockRange is the byte range of one filesystem block.
func blockRange(fs *extfs.FS, blockNum uint32) [2]int64 {
	bs := int64(fs.BlockSize())
	return [2]int64{int64(blockNum) * bs, (int64(blockNum) + 1) * bs}
}

// A pointer block that will not read costs every block whose address it held,
// even though that data is still sitting on the disk. The gap has to be exactly
// that run: too short and everything after it shifts, too long and blocks that
// did come back are written off.
func TestFillLostSingleIndirect(t *testing.T) {
	fs := openTestImage(t)
	num, ino, want := bigFile(t, fs)
	bs := int64(fs.BlockSize())
	p := bs / 4

	rd, _ := faulted(t, num, blockRange(fs, ino.Block[12]))
	got, filled := fill(t, rd)

	if filled.Err != nil {
		t.Fatalf("reading with a gap log: %s", filled.Err)
	}
	if int64(len(got)) != ino.Size {
		t.Fatalf("recovered %d bytes; want the file's full %d", len(got), ino.Size)
	}

	gapStart, gapLen := 12*bs, p*bs
	gaps := filled.Gaps
	if len(gaps) != 1 || gaps[0].FileOffset != gapStart || gaps[0].Length != gapLen {
		t.Fatalf("gaps = %v; want one gap of %d bytes at %d", gaps, gapLen, gapStart)
	}

	clear(want[gapStart : gapStart+gapLen])
	if !bytes.Equal(got, want) {
		t.Error("the recovered bytes are not the file's, with zeros only where the lost pointer block was")
	}
}

// The blocks after the run are addressed through a different level of the tree,
// so this is where a reader that miscounted the gap shows up: it would deliver
// the double-indirect blocks at the wrong file offset.
func TestFillStaysAlignedAcrossTheGap(t *testing.T) {
	fs := openTestImage(t)
	num, ino, want := bigFile(t, fs)
	bs := int64(fs.BlockSize())
	p := bs / 4
	after := (12 + p) * bs // the first block past the single indirect run

	if ino.Size <= after {
		t.Fatalf("%s is %d bytes; it must reach past %d for this test to mean anything", bigName, ino.Size, after)
	}

	rd, _ := faulted(t, num, blockRange(fs, ino.Block[12]))
	got, _ := fill(t, rd)

	if int64(len(got)) != ino.Size {
		t.Fatalf("recovered %d bytes; want %d", len(got), ino.Size)
	}
	if !bytes.Equal(got[after:], want[after:]) {
		t.Error("the blocks past the gap came back shifted; the zeros did not cover exactly the lost run")
	}
}

// A lost pointer block whose run reaches past the end of the file is clipped to
// the file: the blocks it would have addressed beyond EOF are not the file's,
// and reporting them lost would claim more damage than there was.
func TestFillClipsTheRunToTheFile(t *testing.T) {
	fs := openTestImage(t)
	num, ino, want := bigFile(t, fs)
	bs := int64(fs.BlockSize())
	p := bs / 4
	// The double indirect's outer block addresses p² blocks — 64 MiB — where
	// this file has only a few hundred.
	first := (12 + p) * bs

	rd, _ := faulted(t, num, blockRange(fs, ino.Block[13]))
	got, filled := fill(t, rd)

	if filled.Err != nil {
		t.Fatalf("reading with a gap log: %s", filled.Err)
	}
	if int64(len(got)) != ino.Size {
		t.Fatalf("recovered %d bytes; want %d", len(got), ino.Size)
	}
	wantLen := ino.Size - first
	gaps := filled.Gaps
	if len(gaps) != 1 || gaps[0].FileOffset != first || gaps[0].Length != wantLen {
		t.Fatalf("gaps = %v; want one gap of %d bytes at %d", gaps, wantLen, first)
	}
	if !bytes.Equal(got[:first], want[:first]) {
		t.Error("the blocks before the gap are not the file's")
	}
}

// A data block is retried in smaller units, so a bad sector costs its sector
// and not the whole block.
func TestFillDataBlockRetriesBelowTheBlock(t *testing.T) {
	fs := openTestImage(t)
	num, ino, want := bigFile(t, fs)
	bs := int64(fs.BlockSize())
	// The second half of the file's first block.
	blk := blockRange(fs, ino.Block[0])
	half := blk[0] + bs/2

	rd, _ := faulted(t, num, [2]int64{half, blk[1]})
	got, filled := fill(t, rd)

	if filled.Err != nil {
		t.Fatalf("reading with a gap log: %s", filled.Err)
	}
	gaps := filled.Gaps
	if len(gaps) != 1 || gaps[0].FileOffset != bs/2 || gaps[0].Length != bs/2 {
		t.Fatalf("gaps = %v; want one gap of %d bytes at %d — the block's readable half must survive",
			gaps, bs/2, bs/2)
	}
	clear(want[bs/2 : bs])
	if !bytes.Equal(got, want) {
		t.Error("the recovered bytes are not the file's, with zeros only in the failed half-block")
	}
}

// Without an armed log the read still latches at the damage, so a caller with
// nowhere to record a hole cannot be handed one. The error has to say how much
// the lost pointer block cost.
func TestWithoutAGapLogTheReadLatches(t *testing.T) {
	fs := openTestImage(t)
	num, ino, _ := bigFile(t, fs)
	bs := int64(fs.BlockSize())
	p := bs / 4

	rd, _ := faulted(t, num, blockRange(fs, ino.Block[12]))
	// A log the caller builds for itself is inert: only ddrescue.Fill arms one.
	rd.Fill(&ddrescue.GapLog{})

	got, err := io.ReadAll(rd)
	if err == nil {
		t.Fatal("reading over a lost pointer block succeeded; want the read to stop at it")
	}
	var bre *extfs.BlockReadError
	if !errors.As(err, &bre) {
		t.Fatalf("error = %v; want a *BlockReadError", err)
	}
	if bre.BlockNum != ino.Block[12] {
		t.Errorf("error names block %d; want the pointer block %d", bre.BlockNum, ino.Block[12])
	}
	if bre.Lost != p {
		t.Errorf("BlockReadError.Lost = %d; want the %d blocks the pointer block addressed", bre.Lost, p)
	}
	if int64(len(got)) != 12*bs {
		t.Errorf("recovered %d bytes before the damage; want the %d direct blocks", len(got), 12*bs)
	}
}

// A reader that latched must stay latched: resuming at the next block would
// splice later data in at the wrong file offset, with nothing marking the gap.
func TestLatchedReaderDoesNotResume(t *testing.T) {
	fs := openTestImage(t)
	num, ino, _ := bigFile(t, fs)

	rd, _ := faulted(t, num, blockRange(fs, ino.Block[12]))
	if _, err := io.ReadAll(rd); err == nil {
		t.Fatal("the first read succeeded")
	}
	buf := make([]byte, 512)
	for range 3 {
		if n, err := rd.Read(buf); n != 0 || err == nil {
			t.Fatalf("a retry delivered %d bytes and error %v; want the latched error", n, err)
		}
	}
}

// A lost pointer block is read once, not once per block it addressed. The span
// exists for this: the run is resolved from the one failure rather than
// rediscovered block by block. On damaged media each reissued failing read
// costs the drive's whole retry budget: 256 of them here, and 16384 for one
// lost mid-level block on a 4 KiB filesystem.
func TestLostPointerBlockIsReadOnce(t *testing.T) {
	fs := openTestImage(t)
	num, ino, _ := bigFile(t, fs)

	rd, fr := faulted(t, num, blockRange(fs, ino.Block[12]))
	if _, filled := fill(t, rd); filled.Err != nil {
		t.Fatal(filled.Err)
	}
	if fr.hits != 1 {
		t.Errorf("the lost pointer block was read %d times; want 1", fr.hits)
	}
}

// A directory whose data stops coming back must still yield the entries that
// did arrive. Entries never span a block, so the bytes that read are whole
// records. Discarding them would lose every file under the directory (the whole
// subtree under /usr) for one bad sector, while keeping them loses only the
// entries the damage actually covered.
func TestReadDirKeepsWhatCameBack(t *testing.T) {
	sound := openTestImage(t)
	root, err := sound.ReadInode(extfs.RootInodeNum)
	if err != nil {
		t.Fatal(err)
	}
	want, err := sound.ReadDir(root, false)
	if err != nil {
		t.Fatalf("reading the root directory of the sound image: %s", err)
	}
	if len(want) < 3 {
		t.Fatalf("the fixture's root holds %d entries; this test needs several", len(want))
	}

	// Fault the second half of the root directory's only block, so the read
	// comes back with the records in the first half and then fails.
	blk := blockRange(sound, root.Block[0])
	half := blk[0] + (blk[1]-blk[0])/2
	fs := faultedFS(t, [2]int64{half, blk[1]})
	root, err = fs.ReadInode(extfs.RootInodeNum)
	if err != nil {
		t.Fatal(err)
	}

	got, err := fs.ReadDir(root, false)
	if err == nil {
		t.Fatal("ReadDir on a damaged directory reported no problem")
	}
	var dre *extfs.DirReadError
	if !errors.As(err, &dre) {
		t.Fatalf("error = %v; want a *DirReadError", err)
	}
	if dre.Recovered != half-blk[0] {
		t.Errorf("DirReadError.Recovered = %d; want the %d bytes that came back", dre.Recovered, half-blk[0])
	}
	if len(got) == 0 {
		t.Fatal("no entries came back; one bad sector cost the whole directory")
	}
	if len(got) >= len(want) {
		t.Fatalf("got %d entries from half a directory block; the sound image has %d", len(got), len(want))
	}
	// What did come back must be the directory's own leading entries, in order.
	for i, e := range got {
		if e.Name != want[i].Name || e.Inode != want[i].Inode {
			t.Errorf("entry %d = %q/%d; want %q/%d", i, e.Name, e.Inode, want[i].Name, want[i].Inode)
		}
	}

	// Walk records the damage and still descends into what was recovered.
	var seen []string
	issues, walkErr := fs.Walk(func(path string, _ *extfs.DirEntry, _ *extfs.Inode) error {
		seen = append(seen, path)
		return nil
	})
	if walkErr != nil {
		t.Fatalf("Walk = %v; a damaged directory must not stop it", walkErr)
	}
	if len(issues) == 0 {
		t.Error("Walk recorded no issue for the directory that would not read")
	}
	if len(seen) == 0 {
		t.Error("Walk visited nothing; the recovered entries were thrown away")
	}
}

// faultedFS opens the fixture with the given byte ranges refusing to read.
func faultedFS(t *testing.T, faults ...[2]int64) *extfs.FS {
	t.Helper()
	fs, err := extfs.Open(newFaultReader(testfixture.Reader(t, testImagePath), faults...), 0)
	if err != nil {
		t.Fatalf("opening the faulted image: %s", err)
	}
	return fs
}

// Without a gap log the read latches, but the bytes the failing block returned
// before it stopped still belong to the file. extfstool cat has nowhere to put
// a map, so it can only print the readable head; discarding it would lose a
// block of real data at every bad sector.
func TestLatchingReadKeepsTheReadableHead(t *testing.T) {
	fs := openTestImage(t)
	num, ino, want := bigFile(t, fs)
	bs := int64(fs.BlockSize())
	// The second half of the file's first block.
	blk := blockRange(fs, ino.Block[0])
	half := blk[0] + bs/2

	rd, _ := faulted(t, num, [2]int64{half, blk[1]})
	got, err := io.ReadAll(rd)
	if err == nil {
		t.Fatal("reading over a bad sector succeeded; want the read to stop at it")
	}
	var bre *extfs.BlockReadError
	if !errors.As(err, &bre) {
		t.Fatalf("error = %v; want a *BlockReadError", err)
	}
	if bre.BlockNum != ino.Block[0] {
		t.Errorf("error names block %d; want the data block %d", bre.BlockNum, ino.Block[0])
	}
	if int64(len(got)) != bs/2 {
		t.Fatalf("recovered %d bytes; want the %d the block gave up before failing", len(got), bs/2)
	}
	if !bytes.Equal(got, want[:bs/2]) {
		t.Error("the recovered head is not the file's own bytes")
	}

	// And it stays latched: the head is handed over once, never re-delivered.
	buf := make([]byte, 512)
	for range 3 {
		if n, err := rd.Read(buf); n != 0 || err == nil {
			t.Fatalf("a retry delivered %d bytes and error %v; want the latched error", n, err)
		}
	}
}
