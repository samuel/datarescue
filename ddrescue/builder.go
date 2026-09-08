package ddrescue

import (
	"fmt"
	"io"
	"strings"
)

// mapHeader is the preamble GNU ddrescue writes before the block list: two
// comment lines, the current-position line, and the column header.
//
// The status line is "current_pos current_status current_pass", so its second
// field is a single status character, not a size. ddrescue's own parser
// requires that shape: "ddrescue -m" rejects the whole map if the field holds a
// number.
const mapHeader = "# Mapfile. Created by datarescue.\n" +
	"# Command line: datarescue\n" +
	"0x00000000     +               1\n" +
	"#      Start        Size  Status\n"

// MapBuilder accumulates per-sector read results into the coalesced byte ranges
// a ddrescue map file records. Sectors are recorded in increasing order and
// adjacent sectors sharing a status merge into one block.
type MapBuilder struct {
	sectorSize int64
	entries    []Block
	current    Block
}

// NewMapBuilder returns a builder for an image with the given sector size.
func NewMapBuilder(sectorSize int64) *MapBuilder {
	return &MapBuilder{sectorSize: sectorSize}
}

// Record notes the outcome of reading one sector. A failed sector is written as
// a bad-sector block, a successful one as finished.
func (b *MapBuilder) Record(lba int64, failed bool) {
	b.RecordRange(lba*b.sectorSize, b.sectorSize, failed)
}

// RecordRange notes the outcome for a byte range. It is for a caller whose
// damage is not a whole number of sectors: a file's loss ends where its data
// stopped coming back, not at a sector boundary of the medium.
func (b *MapBuilder) RecordRange(offset, size int64, failed bool) {
	if size <= 0 {
		return
	}
	status := BlockStatusFinished
	if failed {
		status = BlockStatusFailedBlockBadSectors
	}
	if b.current.Size == 0 {
		b.current = Block{Offset: offset, Size: size, Status: status}
		return
	}
	if b.current.Status == status && b.current.Offset+b.current.Size == offset {
		b.current.Size += size
		return
	}
	b.entries = append(b.entries, b.current)
	b.current = Block{Offset: offset, Size: size, Status: status}
}

// Blocks returns the coalesced blocks, including the run still being built.
func (b *MapBuilder) Blocks() []Block {
	// Appending to b.entries and keeping the result local would write the open
	// run into b.entries' spare capacity, so a later Record would overwrite it.
	// Build a fresh slice instead.
	out := make([]Block, 0, len(b.entries)+1)
	out = append(out, b.entries...)
	if b.current.Size > 0 {
		out = append(out, b.current)
	}
	return out
}

// WriteTo writes the map file. It satisfies io.WriterTo.
func (b *MapBuilder) WriteTo(w io.Writer) (int64, error) {
	var sb strings.Builder
	sb.WriteString(mapHeader)
	for _, e := range b.Blocks() {
		if e.Size == 0 {
			continue
		}
		fmt.Fprintf(&sb, "0x%08x  0x%08x  %c\n", e.Offset, e.Size, byte(e.Status))
	}
	n, err := io.WriteString(w, sb.String())
	return int64(n), err
}
