// Package testapm builds the Apple-partitioned images the tests read: a driver
// descriptor record and a partition map in front of volume images the caller
// supplies.
//
// It is a normal package rather than a test helper because apm, diskimage and
// the filesystem tool all need the same image, and because a test that pokes a
// hole in one has to know where the hole lands. Build returns each partition's
// byte range, so a ddrescue map can be written against it without asking the
// code under test where anything is.
//
// It commits no fixture of its own: the HFS volume it wraps is the real one
// under hfs/testdata. What it does have to model exactly is the shape of the
// real disks, because a builder that invents a plausible layout of its own will
// agree with a reader that shares the same misunderstanding. The two that
// matter are a map partition reserving far more blocks than it uses — 10 for 2
// on one disk here, 80 for 6 on another — and the reserved tail holding the
// medium's erase pattern rather than zeros.
package testapm

import (
	"encoding/binary"
	"fmt"
	"strings"
)

// BlockSize is the device block size Build uses unless told otherwise.
const BlockSize = 512

// EntrySize is one partition map entry.
const EntrySize = 512

// Partition describes one entry to write.
type Partition struct {
	Name, Type string
	// Data is the partition's contents; nil leaves it empty.
	Data []byte
	// Blocks overrides the length in blocks; 0 sizes it to Data.
	Blocks uint32
	Status uint32
	// DataCount is pmDataCnt. It is 0 on a working disk in this archive and
	// equal to the block count on another, which is why nothing may gate on it.
	DataCount uint32
}

// Options describes the image to build.
type Options struct {
	// BlockSize is the device block size; 0 uses BlockSize.
	BlockSize int
	// MapBlocks is how many blocks the map partition reserves. 0 reserves
	// exactly as many as there are entries; a real formatter reserves more.
	MapBlocks uint32
	// Fill is what the reserved-but-unwritten map blocks hold. Real disks
	// differ: 0x00 on one here, 0xFA on another.
	Fill byte
	// DeclaredCount overrides pmMapBlkCnt on every entry; 0 writes the truth.
	DeclaredCount uint32
	// NoDDR leaves block 0 zeroed, for the path where the stride has to be
	// found by probing.
	NoDDR bool
	// BlockCount overrides sbBlkCount; 0 writes the image's real length. Real
	// disks get this wrong — one here declares two blocks fewer than it has —
	// so nothing may use it as a bound.
	BlockCount uint32
	// DriverCount overrides sbDrvrCount without writing that many descriptors,
	// for the path where the declared count and the table disagree.
	DriverCount uint16
}

// Placed is where a partition's data landed in the image.
type Placed struct {
	Name  string
	Type  string
	Index int
	// Offset is the partition's byte offset in the image and Size its length.
	Offset int64
	Size   int64
}

// Build renders the image and reports where each partition landed. The first
// partition is always the map itself, as a real disk writes it.
func Build(opts Options, parts ...Partition) (img []byte, placed []Placed) {
	bs := opts.BlockSize
	if bs == 0 {
		bs = BlockSize
	}
	entries := make([]Partition, 0, len(parts)+1)
	mapBlocks := opts.MapBlocks
	if mapBlocks == 0 {
		//nolint:gosec // the partition list is a test's own literal, never disk data
		mapBlocks = uint32(len(parts) + 1)
	}
	entries = append(entries, Partition{Type: "Apple_partition_map", Blocks: mapBlocks})
	entries = append(entries, parts...)

	// Lay the partitions out from block 1, the map partition first.
	starts := make([]uint32, len(entries))
	counts := make([]uint32, len(entries))
	next := uint32(1)
	for i, p := range entries {
		n := p.Blocks
		if n == 0 {
			//nolint:gosec // the partition's contents are a test's own bytes
			n = uint32((len(p.Data) + bs - 1) / bs)
		}
		starts[i], counts[i] = next, n
		next += n
	}

	total := int(next) * bs
	img = make([]byte, total)
	if opts.Fill != 0 {
		// Only the reserved tail of the map partition carries the erase
		// pattern; the entries themselves overwrite the front of it.
		for i := int(starts[0]) * bs; i < int(starts[0]+counts[0])*bs; i++ {
			img[i] = opts.Fill
		}
	}

	if !opts.NoDDR {
		//nolint:gosec // total is the image a test asked for, sized from its own partitions
		writeDDR(img, bs, opts, uint32(total/bs))
	}

	declared := opts.DeclaredCount
	if declared == 0 {
		//nolint:gosec // the entry list is a test's own literal
		declared = uint32(len(entries))
	}
	for i, p := range entries {
		off := (int(starts[0]) + i) * bs
		writeEntry(img[off:off+EntrySize], p, declared, starts[i], counts[i])
	}

	placed = make([]Placed, 0, len(entries))
	for i, p := range entries {
		copy(img[int(starts[i])*bs:], p.Data)
		placed = append(placed, Placed{
			Name: p.Name, Type: p.Type, Index: i + 1,
			Offset: int64(starts[i]) * int64(bs),
			Size:   int64(counts[i]) * int64(bs),
		})
	}
	return img, placed
}

func writeDDR(img []byte, bs int, opts Options, blocks uint32) {
	copy(img[0:2], "ER")
	//nolint:gosec // bs is one of the block sizes a caller passes, all under 64 KiB
	binary.BigEndian.PutUint16(img[2:4], uint16(bs))
	count := opts.BlockCount
	if count == 0 {
		count = blocks
	}
	binary.BigEndian.PutUint32(img[4:8], count)
	binary.BigEndian.PutUint16(img[16:18], opts.DriverCount)
}

func writeEntry(dst []byte, p Partition, declared, start, count uint32) {
	copy(dst[0:2], "PM")
	binary.BigEndian.PutUint32(dst[4:8], declared)
	binary.BigEndian.PutUint32(dst[8:12], start)
	binary.BigEndian.PutUint32(dst[12:16], count)
	copy(dst[16:48], p.Name)
	copy(dst[48:80], p.Type)
	binary.BigEndian.PutUint32(dst[84:88], p.DataCount)
	binary.BigEndian.PutUint32(dst[88:92], p.Status)
}

// DamageRange marks byte ranges of the image unrecovered and returns the map
// text saying so. The damaged bytes are overwritten with a recognizable
// non-zero pattern, so a tool that ignored the map hands back 0xDB where one
// that honored it hands back zeros.
func DamageRange(img []byte, ranges ...[2]int) string {
	var b strings.Builder
	b.WriteString("# Mapfile. Created by internal/testapm.\n")
	b.WriteString("0x00000000     +               1\n")
	b.WriteString("#      pos        size  status\n")
	pos := 0
	for _, r := range ranges {
		off, n := r[0], r[1]
		for i := off; i < off+n && i < len(img); i++ {
			img[i] = 0xDB
		}
		if off > pos {
			fmt.Fprintf(&b, "0x%08x  0x%08x  +\n", pos, off-pos)
		}
		fmt.Fprintf(&b, "0x%08x  0x%08x  -\n", off, n)
		pos = off + n
	}
	if pos < len(img) {
		fmt.Fprintf(&b, "0x%08x  0x%08x  +\n", pos, len(img)-pos)
	}
	return b.String()
}
