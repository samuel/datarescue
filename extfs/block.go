package extfs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"samuelks.com/datarescue/ddrescue"
)

// BlockReadError reports a filesystem block that could not be read.
type BlockReadError struct {
	BlockNum uint32
	// Lost is how many logical blocks of the file were lost: one for a data
	// block or, for a pointer block, every block whose address it held, since
	// those addresses are lost with it.
	Lost int64
	Err  error
}

func (e *BlockReadError) Error() string {
	if e.Lost > 1 {
		return fmt.Sprintf("extfs: read pointer block %d: %s (it held the addresses of %d file blocks)",
			e.BlockNum, e.Err, e.Lost)
	}
	return fmt.Sprintf("extfs: read block %d: %s", e.BlockNum, e.Err)
}

func (e *BlockReadError) Unwrap() error { return e.Err }

// retryUnit is how finely a block that would not read is retried, so a bad
// sector costs its sector rather than the whole block. It is capped at the
// block size, which is never smaller than 1 KiB.
const retryUnit = 512

// ReadBlock reads one filesystem block into buf, which must be BlockSize() long.
// Block 0 is a sparse hole and zero-fills buf without any I/O.
func (fs *FS) ReadBlock(blockNum uint32, buf []byte) error {
	if len(buf) != fs.blkSize {
		return fmt.Errorf("extfs: ReadBlock: buf size %d != block size %d", len(buf), fs.blkSize)
	}
	if blockNum == 0 {
		clear(buf)
		return nil
	}
	return fs.readAt(fs.blockOffset(blockNum), buf)
}

// blockOffset is where a block starts, relative to the FS start.
func (fs *FS) blockOffset(blockNum uint32) int64 {
	return int64(blockNum) * int64(fs.blkSize)
}

// readIndirBlock reads a block as a list of little-endian uint32 block pointers.
func (fs *FS) readIndirBlock(blockNum uint32) ([]uint32, error) {
	buf := make([]byte, fs.blkSize)
	if err := fs.ReadBlock(blockNum, buf); err != nil {
		return nil, err
	}
	n := fs.blkSize / 4
	ptrs := make([]uint32, n)
	for i := range n {
		ptrs[i] = binary.LittleEndian.Uint32(buf[i*4 : i*4+4])
	}
	return ptrs, nil
}

func (fs *FS) ptrsPerBlock() int { return fs.blkSize / 4 }

// InodeReader streams an inode's data blocks. Sparse blocks yield zeroes; an
// unreadable block delivers whatever came back before the failure and then
// returns a *BlockReadError, stopping the reader and leaving everything read
// before it valid, unless Fill has armed it to zero-fill instead.
type InodeReader struct {
	fs     *FS
	ino    *Inode
	remain int64  // bytes left to deliver
	total  int64  // the file's declared length
	off    int64  // bytes delivered, which is the file offset a gap is recorded at
	logBlk int64  // next logical block index within the file
	buf    []byte // per-block read buffer (reused)
	left   []byte // unconsumed tail of buf

	// zeroRun is how many logical blocks of a lost pointer block's run are
	// still to be delivered as zeros. A lost mid-level block covers p² blocks —
	// 64 MiB at 4 KiB — so the zeros are counted and handed out a blockful at a
	// time rather than buffered.
	zeroRun int64

	// cache holds parsed pointer blocks so sequential reads through a large file
	// do not re-read them.
	cache map[uint32][]uint32

	// fill, when armed, makes a failed read deliver zeros for what it could not
	// recover and continue. This is safe because the hole's length is computed,
	// not guessed: a lost pointer block leaves its data unknown, but the run of
	// logical blocks it addressed is known exactly.
	fill *ddrescue.GapLog

	// err is latched at the first unreadable block. Without it a caller that
	// retries Read would resume at the following block and splice later data in
	// at the wrong file offset, with no marker for the gap.
	err error
}

// indirCacheMax bounds the pointer-block cache. A sequential read touches at
// most one block per indirection level, so a handful of slots keeps the hit
// rate while stopping the map growing with file size.
const indirCacheMax = 8

// Fill gives the reader a log to record its holes in, switching it from
// latching at the damage to zero-filling past it. Call it before the first
// Read. Only ddrescue.Fill arms a log, and it writes the map in the same call,
// so a reader cannot deliver zeros that nothing records.
func (r *InodeReader) Fill(l *ddrescue.GapLog) {
	if !l.Filling() {
		return
	}
	l.Limit(r.total)
	r.fill = l
}

func (r *InodeReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for r.remain > 0 {
		if len(r.left) > 0 {
			n := len(p)
			if int64(n) > r.remain {
				n = int(r.remain)
			}
			copied := copy(p[:n], r.left)
			r.left = r.left[copied:]
			r.remain -= int64(copied)
			r.off += int64(copied)
			return copied, nil
		}
		if r.zeroRun > 0 {
			clear(r.buf)
			r.zeroRun--
			r.logBlk++
			r.left = r.buf
			continue
		}
		if r.err != nil {
			return 0, r.err
		}

		sp := r.resolveBlock(r.logBlk)
		if sp.err != nil {
			// A span of zero blocks is structurally impossible rather than
			// damage (a size past what the pointers can address), and there is
			// no measured length to fill.
			if sp.count == 0 || !r.fill.Filling() {
				r.err = sp.err
				return 0, r.err
			}
			r.startZeroRun(sp)
			continue
		}
		if sp.phys == 0 {
			clear(r.buf) // sparse hole
		} else if n, err := r.fs.readAtN(r.fs.blockOffset(sp.phys), r.buf); err != nil {
			if !r.fill.Filling() {
				// Do not advance logBlk: this block stays the reader's position.
				r.err = &BlockReadError{BlockNum: sp.phys, Lost: 1, Err: err}
				if n <= 0 {
					return 0, r.err
				}
				// The bytes that came back before the failure belong to the
				// file. Return them now and latch on the next call, as
				// fat.Reader does with its readable head, so cat prints what
				// the medium gave up instead of stopping one block early.
				r.left = r.buf[:n]
				continue
			}
			r.fill.Recover(ddrescue.Chunk{
				FileOffset:  r.off,
				ImageOffset: r.fs.blockOffset(sp.phys),
				Buf:         r.buf,
				Unit:        min(retryUnit, r.fs.blkSize),
				Read:        r.fs.readAtN,
			}, n, err)
		}
		r.logBlk++
		r.left = r.buf
	}
	return 0, io.EOF
}

// startZeroRun turns a pointer block that would not read into the right number
// of zero blocks.
//
// The gap is the part of the run the reader has not yet delivered: from the
// reader's position to the end of the run, not the whole run. The two match in
// the ordinary case, when the reader meets the run at its first block. They
// differ when a cached pointer block is evicted and then fails on the re-read;
// using the whole run there would re-report blocks already delivered and skip
// others twice.
func (r *InodeReader) startZeroRun(sp blockSpan) {
	// The loop must always advance, or it hangs.
	blocks := max(sp.first+sp.count-r.logBlk, 1)
	r.fill.Add(r.off, blocks*int64(r.fs.blkSize), sp.err)
	r.zeroRun = blocks
}

// blockSpan is the result of one lookup: a physical block or, when a pointer
// block would not read, the run of logical blocks whose addresses it held. The
// data is unknown but the range is not, so a filling reader can insert the
// right number of zeros and stay aligned with the rest of the file.
type blockSpan struct {
	phys  uint32 // the data block, when the lookup resolved
	indir uint32 // the pointer block that would not read
	first int64  // the first logical block that block addressed
	count int64  // how many it addressed; 0 when there is no span to measure
	err   error
}

// lostPointerBlock is the span for a pointer block that would not read.
func lostPointerBlock(indirPhys uint32, first, count int64, err error) blockSpan {
	return blockSpan{
		indir: indirPhys,
		first: first,
		count: count,
		err:   &BlockReadError{BlockNum: indirPhys, Lost: count, Err: err},
	}
}

// resolveBlock maps a logical block index to a physical block number, returning
// 0 for a sparse hole and, for an unreadable pointer block, the run of logical
// blocks that block addressed. ino.Size is disk-controlled, so logBlk can exceed
// what the block pointers can address; every level is bounded.
func (r *InodeReader) resolveBlock(logBlk int64) blockSpan {
	// Arithmetic is int64 because p*p*p overflows uint32 for any block size
	// above 1 KiB, and so does the last logical block a triple indirect reaches.
	p := int64(r.fs.ptrsPerBlock())
	// The first logical block each level of indirection addresses.
	b1 := int64(12)
	b2 := b1 + p
	b3 := b2 + p*p
	blk := logBlk

	if blk < 0 {
		return blockSpan{err: fmt.Errorf("extfs: negative logical block %d", logBlk)}
	}
	if blk < b1 {
		return blockSpan{phys: r.ino.Block[blk]}
	}
	blk -= b1

	if blk < p {
		return r.resolveIndir(r.ino.Block[12], blk, b1, p)
	}
	blk -= p

	if blk < p*p {
		outer, err := r.cachedIndir(r.ino.Block[13])
		if err != nil {
			return lostPointerBlock(r.ino.Block[13], b2, p*p, err)
		}
		i := blk / p
		return r.resolveIndir(outer[i], blk%p, b2+i*p, p)
	}
	blk -= p * p

	if blk >= p*p*p {
		return blockSpan{err: fmt.Errorf("extfs: logical block %d is beyond the triple-indirect range", logBlk)}
	}
	outer, err := r.cachedIndir(r.ino.Block[14])
	if err != nil {
		return lostPointerBlock(r.ino.Block[14], b3, p*p*p, err)
	}
	i := blk / (p * p)
	mid, err := r.cachedIndir(outer[i])
	if err != nil {
		return lostPointerBlock(outer[i], b3+i*p*p, p*p, err)
	}
	j := (blk / p) % p
	return r.resolveIndir(mid[j], blk%p, b3+i*p*p+j*p, p)
}

// resolveIndir reads an indirect pointer block (when non-zero) and returns the
// physical block pointer at index idx within it. first and count are the run of
// logical blocks that pointer block addresses; the reader needs them when the
// block will not read.
func (r *InodeReader) resolveIndir(indirPhys uint32, idx, first, count int64) blockSpan {
	if indirPhys == 0 {
		return blockSpan{} // sparse: the entire indirect range is a hole
	}
	ptrs, err := r.cachedIndir(indirPhys)
	if err != nil {
		return lostPointerBlock(indirPhys, first, count, err)
	}
	if idx < 0 || idx >= int64(len(ptrs)) {
		return blockSpan{err: fmt.Errorf("extfs: pointer index %d out of range in block %d", idx, indirPhys)}
	}
	return blockSpan{phys: ptrs[idx]}
}

func (r *InodeReader) cachedIndir(physBlk uint32) ([]uint32, error) {
	if physBlk == 0 {
		return make([]uint32, r.fs.ptrsPerBlock()), nil // sparse indirect block
	}
	if ptrs, ok := r.cache[physBlk]; ok {
		return ptrs, nil
	}
	ptrs, err := r.fs.readIndirBlock(physBlk)
	if err != nil {
		return nil, err
	}
	if len(r.cache) >= indirCacheMax {
		clear(r.cache)
	}
	r.cache[physBlk] = ptrs
	return ptrs, nil
}

// ReadInodeData streams an inode's file data. Sparse blocks produce zero bytes.
// Read hands over the readable head of an unreadable block and then returns a
// *BlockReadError, leaving everything delivered before it valid;
// InodeReader.Fill switches it to zero-filling past the damage instead. Inodes
// using ext4 extent trees or inline data are refused here rather than on first
// read, since neither holds block pointers.
func (fs *FS) ReadInodeData(ino *Inode) (*InodeReader, error) {
	if ino.Flags&InodeFlagExtents != 0 {
		return nil, errors.New("extfs: inode uses ext4 extent trees (not supported)")
	}
	// An inline-data inode stores file content in the i_block array; reading
	// those bytes as block pointers would return silent garbage.
	if ino.Flags&InodeFlagInline != 0 {
		return nil, errors.New("extfs: inode uses ext4 inline data (not supported)")
	}
	return &InodeReader{
		fs:     fs,
		ino:    ino,
		remain: ino.Size,
		total:  ino.Size,
		buf:    make([]byte, fs.blkSize),
		cache:  make(map[uint32][]uint32),
	}, nil
}
