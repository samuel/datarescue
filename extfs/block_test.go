package extfs

import (
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

// The indirect arithmetic is the one part of this package a real image cannot
// prove: reaching the triple indirect at 1 KiB blocks needs a 64 GiB file. So
// it is exercised here against a hand-built pointer tree, where every level
// boundary is one block index apart and a wrong multiplier shows immediately.
const (
	tblkSize = 1024
	tp       = tblkSize / 4 // 256 pointers per block
	// The first logical block each level of indirection addresses.
	tb1 = 12
	tb2 = tb1 + tp    // 268
	tb3 = tb2 + tp*tp // 65804
	tb4 = tb3 + tp*tp*tp
)

// Physical blocks the test tree is built from. The pointer blocks are laid out
// so that each one's contents say which level it came from.
const (
	singleIndir = 100 // logical tb1..tb1+tp-1     -> 1000...
	dblOuter    = 200
	dblInner0   = 201 // logical tb2..tb2+tp-1     -> 2000...
	dblInner1   = 202 // logical tb2+tp..          -> 3000...
	tplOuter    = 300
	tplMid0     = 301
	tplInner0   = 400 // logical tb3..tb3+tp-1     -> 4000...
	imageBlocks = 401
)

var errBadBlock = errors.New("injected block read failure")

// blockFaulter is an image with a set of blocks that will not read, with the
// semantics a rescue map gives: a read starting in a bad block fails outright,
// and one running into it returns the bytes before it.
type blockFaulter struct {
	data []byte
	off  int64
	bad  map[int64]bool
}

func (b *blockFaulter) Read(p []byte) (int, error) {
	if b.off >= int64(len(b.data)) {
		return 0, io.EOF
	}
	if b.bad[b.off/tblkSize] {
		return 0, errBadBlock
	}
	end := min(b.off+int64(len(p)), int64(len(b.data)))
	for blk := b.off/tblkSize + 1; blk*tblkSize < end; blk++ {
		if b.bad[blk] {
			end = blk * tblkSize
			break
		}
	}
	n := copy(p, b.data[b.off:end])
	b.off += int64(n)
	return n, nil
}

func (b *blockFaulter) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		b.off = off
	case io.SeekCurrent:
		b.off += off
	default:
		b.off = int64(len(b.data)) + off
	}
	return b.off, nil
}

// testTree builds an image holding one inode's pointer tree, and a reader over
// it with the given physical blocks refusing to read.
func testTree(bad ...int64) *InodeReader {
	img := make([]byte, imageBlocks*tblkSize)
	putPtrs := func(blk int, base uint32) {
		for i := range tp {
			binary.LittleEndian.PutUint32(img[blk*tblkSize+i*4:], base+uint32(i))
		}
	}
	putPtrs(singleIndir, 1000)
	putPtrs(dblOuter, dblInner0) // inner blocks 201, 202, ...
	putPtrs(dblInner0, 2000)
	putPtrs(dblInner1, 3000)
	putPtrs(tplOuter, tplMid0)  // mid blocks 301, 302, ...
	putPtrs(tplMid0, tplInner0) // inner blocks 400, 401, ...
	putPtrs(tplInner0, 4000)

	faults := make(map[int64]bool, len(bad))
	for _, b := range bad {
		faults[b] = true
	}
	fs := &FS{r: &blockFaulter{data: img, bad: faults}, blkSize: tblkSize}

	ino := &Inode{}
	for i := range 12 {
		ino.Block[i] = uint32(10 + i)
	}
	ino.Block[12], ino.Block[13], ino.Block[14] = singleIndir, dblOuter, tplOuter
	return &InodeReader{fs: fs, ino: ino, buf: make([]byte, tblkSize), cache: map[uint32][]uint32{}}
}

// Every level boundary, resolved. A wrong multiplier at any level lands one
// block out here rather than 64 MiB out on a real filesystem.
func TestResolveBlockLevels(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		logBlk int64
		want   uint32
	}{
		{"first direct", 0, 10},
		{"last direct", 11, 21},
		{"first single indirect", tb1, 1000},
		{"last single indirect", tb2 - 1, 1000 + tp - 1},
		{"first double indirect", tb2, 2000},
		{"last of the first inner block", tb2 + tp - 1, 2000 + tp - 1},
		{"first of the second inner block", tb2 + tp, 3000},
		{"first triple indirect", tb3, 4000},
		{"last of the first triple inner block", tb3 + tp - 1, 4000 + tp - 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sp := testTree().resolveBlock(tc.logBlk)
			if sp.err != nil {
				t.Fatalf("logical block %d: %s", tc.logBlk, sp.err)
			}
			if sp.phys != tc.want {
				t.Errorf("logical block %d resolved to physical %d; want %d", tc.logBlk, sp.phys, tc.want)
			}
		})
	}
}

// A pointer block that will not read loses the addresses of every block it
// held. The run it covered is still known exactly, and a filling reader puts
// zeros in that run.
func TestResolveBlockSpans(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		bad       int64
		logBlk    int64
		wantIndir uint32
		wantFirst int64
		wantCount int64
	}{
		{"single indirect", singleIndir, tb1, singleIndir, tb1, tp},
		// Met part way along, the run is still the whole run: where it starts
		// does not depend on where the reader happened to reach it.
		{"single indirect, met at its last block", singleIndir, tb2 - 1, singleIndir, tb1, tp},
		{"double outer", dblOuter, tb2, dblOuter, tb2, tp * tp},
		{"double outer, met at its last block", dblOuter, tb3 - 1, dblOuter, tb2, tp * tp},
		{"double inner", dblInner0, tb2, dblInner0, tb2, tp},
		{"second double inner", dblInner1, tb2 + tp, dblInner1, tb2 + tp, tp},
		{"triple outer", tplOuter, tb3, tplOuter, tb3, tp * tp * tp},
		{"triple mid", tplMid0, tb3, tplMid0, tb3, tp * tp},
		{"triple inner", tplInner0, tb3, tplInner0, tb3, tp},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sp := testTree(tc.bad).resolveBlock(tc.logBlk)
			if sp.err == nil {
				t.Fatalf("logical block %d resolved although block %d would not read", tc.logBlk, tc.bad)
			}
			if sp.indir != tc.wantIndir || sp.first != tc.wantFirst || sp.count != tc.wantCount {
				t.Errorf("span = {indir %d, first %d, count %d}; want {indir %d, first %d, count %d}",
					sp.indir, sp.first, sp.count, tc.wantIndir, tc.wantFirst, tc.wantCount)
			}
			// The error has to be able to say what the loss was.
			var bre *BlockReadError
			if !errors.As(sp.err, &bre) {
				t.Fatalf("error = %v; want a *BlockReadError", sp.err)
			}
			if bre.Lost != tc.wantCount {
				t.Errorf("BlockReadError.Lost = %d; want %d", bre.Lost, tc.wantCount)
			}
		})
	}
}

// A size past what the pointers can address is not damage a reader can measure,
// so there is no span and the read latches instead of inventing zeros.
func TestResolveBlockStructuralFailuresHaveNoSpan(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		logBlk int64
	}{
		{"past the triple-indirect range", tb4},
		{"far past it", tb4 * 4},
		{"negative", -1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sp := testTree().resolveBlock(tc.logBlk)
			if sp.err == nil {
				t.Fatalf("logical block %d resolved", tc.logBlk)
			}
			if sp.count != 0 {
				t.Errorf("span covers %d blocks; want none, so the reader latches", sp.count)
			}
		})
	}
}

// A zero pointer is a sparse hole, not damage: it resolves to block 0 with no
// error, and the reader delivers zeros for it without recording a gap.
func TestResolveBlockSparse(t *testing.T) {
	t.Parallel()
	r := testTree()
	r.ino.Block[12] = 0
	sp := r.resolveBlock(tb1)
	if sp.err != nil || sp.phys != 0 {
		t.Errorf("sparse single indirect resolved to {phys %d, err %v}; want {0, nil}", sp.phys, sp.err)
	}
}

// An inode whose i_block array is not a list of block pointers is refused when
// the reader is asked for, not on the first read: there is nothing to stream.
func TestReadInodeDataRefusesExtentAndInline(t *testing.T) {
	t.Parallel()
	fs := &FS{blkSize: tblkSize}
	for _, flag := range []uint32{InodeFlagExtents, InodeFlagInline} {
		if _, err := fs.ReadInodeData(&Inode{Flags: flag}); err == nil {
			t.Errorf("ReadInodeData accepted an inode with flag 0x%X", flag)
		}
	}
}

// A run met part way must be filled from where the reader stands, not from
// where the run starts. The two are the same when the reader reaches a lost
// pointer block at its first block, which is the ordinary case; they differ when
// one served from the cache is evicted and then fails on the re-read. Filling
// the whole run there would re-report blocks already delivered and run the same
// count again past the end of it.
func TestStartZeroRunFillsOnlyWhatIsLeft(t *testing.T) {
	t.Parallel()
	const met = 100 // blocks into the run the reader had already delivered
	r := testTree()
	r.logBlk = tb1 + met
	r.off = r.logBlk * tblkSize
	// The log is nil, so nothing is recorded and nothing is filled; zeroRun is
	// the arithmetic under test.
	r.startZeroRun(blockSpan{first: tb1, count: tp})

	if r.zeroRun != tp-met {
		t.Errorf("zeroRun = %d; want %d, the part of the run still to come", r.zeroRun, tp-met)
	}
	if end := r.logBlk + r.zeroRun; end != tb1+tp {
		t.Errorf("the run would end at logical block %d; want %d, where the pointer block's reach ends", end, tb1+tp)
	}
}

// The loop has to advance whatever the span says, or it hangs.
func TestStartZeroRunAlwaysAdvances(t *testing.T) {
	t.Parallel()
	r := testTree()
	r.logBlk = tb1 + tp + 5 // already past the run the span describes
	r.startZeroRun(blockSpan{first: tb1, count: tp})
	if r.zeroRun < 1 {
		t.Errorf("zeroRun = %d; want at least one block, so the read cannot stall", r.zeroRun)
	}
}
