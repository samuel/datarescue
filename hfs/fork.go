package hfs

import (
	"errors"
	"fmt"
	"io"

	"samuelks.com/datarescue/ddrescue"
)

// Fork type codes used as keys in the extents overflow file.
const (
	ForkTypeData     byte = 0x00
	ForkTypeResource byte = 0xFF
)

// Well-known catalog node IDs.
const (
	// RootParentID is the parent of the root folder. It has no catalog record,
	// so a path walk that reaches it has arrived at the volume root.
	RootParentID uint32 = 1
	// RootFolderID is the volume's root folder.
	RootFolderID uint32 = 2
	// ExtentsFileID and CatalogFileID are the CNIDs of the two B-tree files.
	ExtentsFileID uint32 = 3
	CatalogFileID uint32 = 4
)

// fork is a file's extent list: the three extents held in its catalog record
// plus any further extents recorded in the extents overflow file.
type fork struct {
	v   *Volume
	ref ForkRef
	// extents is the fork's resolved extent list.
	extents []Extent
	// size is the fork's logical length in bytes.
	size int64
	// tail records why the extent list falls short, which decides whether the
	// missing bytes may be zero-filled or must latch.
	tail forkTail
}

// forkTail classifies why a fork's extents cover less than its recorded length.
//
// This applies the toolkit's fill rule to HFS: a hole is zero-filled only when
// its length can be computed exactly, never guessed. The first two cases have a
// measurable span and the rest do not.
type forkTail int

const (
	// forkComplete means the extents cover the recorded length.
	forkComplete forkTail = iota
	// forkTailLost means the overflow map holds no record at the key the next
	// extent would have, so the missing run is the recorded length less what
	// the extents cover — an exact number — and nothing can follow it. The hole
	// is terminal and fillable.
	forkTailLost
	// forkListUnknown means the extents overflow file did not parse in full.
	// Records that exist on disk were never loaded, so a continuation may be
	// missing while a later one is present, and a later one can only be placed
	// in the file if the missing one's block count is known. Placing it at the
	// wrong offset is what the fill rule exists to prevent, so the reader
	// latches.
	forkListUnknown
	// forkSizeAbsurd means the recorded length is larger than the volume can
	// hold, so the tail would be computed from a field already known to be
	// wrong. Any length derived from it is a guess, so the reader latches.
	forkSizeAbsurd
)

// blockBytes returns the volume's allocation block size in bytes.
func (v *Volume) blockBytes() int64 { return int64(v.MDB.AllocationBlockSize) }

// ImageRange is a contiguous run of a fork's bytes and where they live in the
// image. ImageOffset is an offset into the reader the volume was opened over,
// so it can be handed straight to a rescue map.
type ImageRange struct {
	FileOffset  int64
	ImageOffset int64
	Length      int64
}

// ForkRef identifies one fork of one file: enough to resolve its extents without
// going back to the catalog.
type ForkRef struct {
	CNID    uint32
	Type    byte
	Extents ExtentsRecord
	Size    int64
}

// ForkReadError reports a run of a fork that would not read. The bytes
// recovered before the failure are delivered rather than discarded.
type ForkReadError struct {
	CNID     uint32
	ForkType byte
	// Extent is the index into the fork's resolved extent list and Block the
	// allocation block the read failed in. Both are here because they answer
	// different questions: the first says how much of the file is at risk, the
	// second says what to re-read from the medium.
	Extent     int
	Block      uint16
	FileOffset int64
	N          int
	Err        error
}

func (e *ForkReadError) Error() string {
	return fmt.Sprintf("hfs: file %d fork %#x: extent %d, allocation block %d at file offset %d: %s",
		e.CNID, e.ForkType, e.Extent, e.Block, e.FileOffset, e.Err)
}

func (e *ForkReadError) Unwrap() error { return e.Err }

// ForkRanges returns where in the image a fork keeps its bytes, clipped to its
// recorded length, with runs adjacent on disk coalesced.
//
// It reads nothing, so a caller can report which files a bad region of the
// medium affects before reading any of them, and a test can place a hole in a
// known file without asking the reader where anything is.
func (v *Volume) ForkRanges(ref ForkRef) ([]ImageRange, error) {
	f, err := v.openFork(ref)
	if err != nil {
		return nil, err
	}
	return f.ranges(), nil
}

// ranges converts the extent list to image byte ranges.
func (f *fork) ranges() []ImageRange {
	blockSize := f.v.blockBytes()
	if blockSize <= 0 {
		return nil
	}
	var out []ImageRange
	var fileOff int64
	for _, ext := range f.extents {
		if ext.BlockCount == 0 || fileOff >= f.size {
			continue
		}
		length := min(int64(ext.BlockCount)*blockSize, f.size-fileOff)
		imgOff := f.v.AllocBlockOffset(ext.StartBlock)
		// Coalesce with the previous run when the two are adjacent on disk.
		if n := len(out); n > 0 && out[n-1].ImageOffset+out[n-1].Length == imgOff {
			out[n-1].Length += length
		} else {
			out = append(out, ImageRange{FileOffset: fileOff, ImageOffset: imgOff, Length: length})
		}
		fileOff += length
	}
	return out
}

// readAt reads from the fork at a byte offset, walking its extents. The two
// B-tree files are read through it, since they need random access rather than a
// stream.
//
// A read reaching past the last extent is a short read with io.ErrUnexpectedEOF
// rather than a silent truncation.
func (f *fork) readAt(p []byte, off int64) (int, error) {
	var total int
	remaining := off
	blockSize := f.v.blockBytes()
	if blockSize <= 0 {
		return 0, fmt.Errorf("hfs: volume has an allocation block size of %d", f.v.MDB.AllocationBlockSize)
	}
	for _, ext := range f.extents {
		if ext.BlockCount == 0 {
			continue
		}
		extentSize := int64(ext.BlockCount) * blockSize
		if remaining >= extentSize {
			remaining -= extentSize
			continue
		}
		toRead := min(int64(len(p)-total), extentSize-remaining)
		startByte := f.v.AllocBlockOffset(ext.StartBlock)
		n, err := f.v.r.ReadAt(p[total:total+int(toRead)], startByte+remaining)
		total += n
		if err != nil {
			return total, err
		}
		if total == len(p) {
			return total, nil
		}
		remaining = 0
	}
	return total, io.ErrUnexpectedEOF
}

// covered is how many of the fork's bytes its extents account for.
func (f *fork) covered() int64 {
	blockSize := f.v.blockBytes()
	var n int64
	for _, ext := range f.extents {
		n += int64(ext.BlockCount) * blockSize
	}
	return min(n, f.size)
}

// ForkReader streams one fork, stopping at its recorded length.
type ForkReader struct {
	v      *Volume
	f      *fork
	ranges []ImageRange
	idx    int   // which range
	within int64 // offset into that range
	off    int64 // offset into the fork
	// total is the fork's recorded length.
	total int64
	// fill is the log to write holes down in; nil latches instead.
	fill *ddrescue.GapLog
	// base shifts every recorded hole. A reader whose bytes are written after
	// a header — an AppleDouble sidecar — has its holes at base+offset in the
	// file the map describes, not at offset.
	base int64
	// err is latched at the first failure. Without it a caller that retries
	// resumes past the damage and splices later bytes in at the wrong file
	// offset with nothing recording the gap.
	err error
	// buf holds one allocation block, and pos and held say what of it is still
	// to be delivered.
	buf  []byte
	pos  int
	held int
}

// Size is the fork's recorded length.
func (r *ForkReader) Size() int64 { return r.total }

// Ranges is where the fork's bytes live in the image.
func (r *ForkReader) Ranges() []ImageRange { return r.ranges }

// Fill gives the reader a log to record its holes in, which switches it from
// latching at the damage to zero-filling past it.
//
// Only ddrescue.Fill arms a log, and it writes the map in the same call, so a
// reader cannot deliver zeros that nothing records. A log a caller builds for
// itself is inert, and the reader still latches.
func (r *ForkReader) Fill(l *ddrescue.GapLog) { r.FillAt(l, 0) }

// FillAt is Fill for a reader whose bytes land at base in the file being
// written, because something is written in front of them.
//
// The reader is the only thing that knows the offsets it reports, so it adds
// the base itself; the limit is base+size so a hole in the tail is clipped
// against the written file's length rather than the fork's. Without this an
// AppleDouble sidecar's map would mark its header bytes as lost and put every
// hole one header length too early.
func (r *ForkReader) FillAt(l *ddrescue.GapLog, base int64) {
	if !l.Filling() {
		return
	}
	l.Limit(base + r.total)
	r.base, r.fill = base, l
}

// OpenFork returns a reader over one fork of one file.
func (v *Volume) OpenFork(ref ForkRef) (*ForkReader, error) {
	f, err := v.openFork(ref)
	if err != nil {
		return nil, err
	}
	blockSize := v.blockBytes()
	return &ForkReader{
		v: v, f: f, ranges: f.ranges(), total: f.size,
		buf: make([]byte, blockSize),
	}, nil
}

// openFork resolves a fork's extent list and classifies whatever it does not
// cover.
func (v *Volume) openFork(ref ForkRef) (*fork, error) {
	if v.blockBytes() <= 0 {
		return nil, fmt.Errorf("hfs: volume has an allocation block size of %d", v.MDB.AllocationBlockSize)
	}
	extents, truncated, err := v.forkExtents(ref.CNID, ref.Type, ref.Extents)
	if err != nil {
		return nil, err
	}
	f := &fork{v: v, ref: ref, extents: extents, size: ref.Size}
	f.tail = v.classifyTail(f, ref, truncated)
	return f, nil
}

// classifyTail decides whether what the extents do not cover may be filled.
func (v *Volume) classifyTail(f *fork, ref ForkRef, truncated bool) forkTail {
	covered := f.covered()
	if covered >= f.size {
		return forkComplete
	}
	volumeBytes := int64(v.MDB.AllocationBlocks) * v.blockBytes()
	switch {
	case f.size > volumeBytes:
		v.diag(DiagForkSizeAbsurd, ref.CNID,
			"file %d declares a %d-byte fork on a volume that holds %d bytes", ref.CNID, f.size, volumeBytes)
		return forkSizeAbsurd
	case truncated:
		v.diag(DiagOverflowTruncated, ref.CNID,
			"file %d is short by %d bytes and the extents overflow file did not parse in full, so where they are is unknown",
			ref.CNID, f.size-covered)
		return forkListUnknown
	}
	v.diag(DiagForkTailLost, ref.CNID,
		"file %d: the last %d bytes have no extent recording where they are", ref.CNID, f.size-covered)
	return forkTailLost
}

// Read delivers the fork's bytes.
//
// The medium is taken one allocation block at a time, aligned to the block, and
// served out of the reader's own buffer. Reading whatever length the caller
// asked for instead would put every retry at an arbitrary offset: the recovery
// unit is the medium's sector, and a chunk that does not start on a sector
// boundary reports a hole larger than the sector that was actually lost.
func (r *ForkReader) Read(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	if r.off >= r.total {
		return 0, io.EOF
	}
	if r.held == 0 {
		if r.idx >= len(r.ranges) {
			return r.readTail(p)
		}
		if err := r.fillBuf(); err != nil {
			return 0, err
		}
		if r.held == 0 {
			return 0, r.err
		}
	}
	n := copy(p, r.buf[r.pos:r.pos+r.held])
	r.pos += n
	r.held -= n
	r.off += int64(n)
	return n, nil
}

// fillBuf takes the next allocation block off the medium.
func (r *ForkReader) fillBuf() error {
	rng := r.ranges[r.idx]
	blockSize := int64(len(r.buf))
	want := min(blockSize, rng.Length-r.within, r.total-r.off)
	buf := r.buf[:want]

	n, err := r.v.readAtN(rng.ImageOffset+r.within, buf)
	if err != nil {
		fe := &ForkReadError{
			CNID: r.f.ref.CNID, ForkType: r.f.ref.Type, Extent: r.idx,
			Block: r.blockAt(rng, r.within), FileOffset: r.off, N: n, Err: err,
		}
		if !r.fill.Filling() {
			// Deliver what came back once, then latch. The position does not
			// advance past the bytes that did not: a caller that retries must
			// not be able to splice later data in at this offset.
			r.err = fe
			if n > 0 {
				r.hold(n, int64(n))
				return nil
			}
			return fe
		}
		// Retry a logical block at a time, so a bad sector loses only that
		// sector and not the whole allocation block, which on a 1536-byte
		// geometry is three sectors.
		r.fill.Recover(ddrescue.Chunk{
			FileOffset:  r.base + r.off,
			ImageOffset: rng.ImageOffset + r.within,
			Buf:         buf,
			Unit:        logicalBlockSize,
			Read:        r.v.readAtN,
		}, n, fe)
		r.hold(int(want), want)
		return nil
	}
	r.hold(n, int64(n))
	return nil
}

// hold makes n buffered bytes available and steps the position on by them,
// moving to the next range when the current one is used up.
func (r *ForkReader) hold(n int, advance int64) {
	r.pos, r.held = 0, n
	r.within += advance
	if r.idx < len(r.ranges) && r.within >= r.ranges[r.idx].Length {
		r.idx++
		r.within = 0
	}
}

// readTail delivers what the extent list did not cover.
func (r *ForkReader) readTail(p []byte) (int, error) {
	missing := r.total - r.off
	if r.f.tail != forkTailLost || !r.fill.Filling() {
		// No measurable span, or nothing to record the zeros in: latch. A file
		// returned at full length with unrecorded holes is the failure this
		// toolkit exists to prevent.
		r.err = &ForkReadError{
			CNID: r.f.ref.CNID, ForkType: r.f.ref.Type, Extent: r.idx, FileOffset: r.off,
			Err: fmt.Errorf("the extents account for %d of %d bytes", r.total-missing, r.total),
		}
		return 0, r.err
	}
	n := min(int64(len(p)), missing)
	clear(p[:n])
	r.fill.Add(r.base+r.off, n, errors.New("hfs: no extent records where these bytes are"))
	r.off += n
	return int(n), nil
}

// blockAt is the allocation block a position within a range falls in, for the
// error message: it is what an operator re-reads from the medium.
func (r *ForkReader) blockAt(rng ImageRange, within int64) uint16 {
	blockSize := r.v.blockBytes()
	if blockSize <= 0 {
		return 0
	}
	base := (rng.ImageOffset - r.v.base - int64(r.v.MDB.AllocationStartBlock)*logicalBlockSize) / blockSize
	//nolint:gosec // the sum is bounded by the volume's uint16 block count
	return uint16(base + within/blockSize)
}

// OpenFile returns a reader over a file's data fork.
func (v *Volume) OpenFile(e Entry) (*ForkReader, error) { return v.OpenFork(e.DataFork()) }

// OpenResourceFork returns a reader over a file's resource fork.
//
// On a classic Mac volume this is where much of the content actually is —
// applications, fonts, system suitcases and older documents keep everything
// there and leave the data fork empty — so a tool that reads only the data fork
// loses most of the disk.
func (v *Volume) OpenResourceFork(e Entry) (*ForkReader, error) { return v.OpenFork(e.ResourceFork()) }

// forkExtents returns a fork's full extent list: the record's own three extents
// followed by any continuation records from the extents overflow file. It also
// reports whether the overflow file itself did not parse in full, which decides
// whether a short fork's hole is measurable or must latch.
//
// The chain is bounded by the volume's own block count. Each continuation is
// keyed by the running total of blocks, and both that total and the counts it
// sums come off the disc: a set of records whose counts wrap the sum back to a
// key already visited walks the same records forever, appending three extents
// each time.
func (v *Volume) forkExtents(cnid uint32, forkType byte, rec ExtentsRecord) ([]Extent, bool, error) {
	extents := make([]Extent, 0, len(rec))
	var blocks int64
	for _, e := range rec {
		if e.BlockCount == 0 {
			break
		}
		extents = append(extents, e)
		blocks += int64(e.BlockCount)
	}

	overflow, err := v.extentsOverflow()
	if err != nil {
		// A volume with no overflow file at all is normal and is not damage.
		// One whose overflow file would not parse is damage, and the
		// difference decides whether a short fork may be filled, so the two
		// are reported separately.
		if errors.Is(err, errNoExtentsOverflow) {
			return extents, false, nil
		}
		return extents, true, nil
	}
	// A fork cannot span more blocks than the volume has.
	limit := int64(v.MDB.AllocationBlocks)
	for blocks < limit {
		//nolint:gosec // blocks is bounded by AllocationBlocks, itself a uint16
		more, ok := overflow[extentKey{forkType: forkType, cnid: cnid, startBlock: uint32(blocks)}]
		if !ok {
			break
		}
		var added int64
		for _, e := range more {
			if e.BlockCount == 0 {
				break
			}
			extents = append(extents, e)
			added += int64(e.BlockCount)
		}
		if added == 0 {
			break
		}
		blocks += added
	}
	return extents, v.overflowPartial, nil
}

// extentKey identifies one extents overflow record.
type extentKey struct {
	forkType   byte
	cnid       uint32
	startBlock uint32
}
