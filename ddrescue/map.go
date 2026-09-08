package ddrescue

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Every line in the list of data blocks describes a block of data.
// It contains 2 non-negative integers and a status character. The
// first integer is the starting position of the block in the input
// file, the second integer is the size (in bytes) of the block.
// The status character is one of these:
//
// Character   Meaning
// '?' non-tried block
// '*' failed block non-trimmed
// '/' failed block non-scraped
// '-' failed block bad-sector(s)
// '+' finished block

var reWS = regexp.MustCompile(`\s+`)

// Reader is a disk image paired with the map of which of its ranges were
// recovered. It is both an io.ReadSeeker and an io.ReaderAt over the same
// image: a filesystem parser seeks and reads, while a partition table is read
// at a fixed offset. Every read goes through readAt, so the sequential position
// is only bookkeeping, not a file cursor, and an interleaved ReadAt cannot move
// it.
type Reader struct {
	ra     io.ReaderAt
	closer io.Closer // nil when the caller owns the source
	end    int64     // just past the last mapped byte, where Read reports EOF
	size   int64     // the image's own length, which SeekEnd reports
	Blocks []Block

	mu     sync.Mutex
	Offset int64
}

// NewReader pairs an image already open for random access with the blocks of
// its map. The caller keeps ownership of ra, so Close is a no-op.
func NewReader(ra io.ReaderAt, blocks []Block) *Reader {
	r := &Reader{ra: ra, Blocks: blocks, end: mappedEnd(blocks)}
	r.size = imageSize(ra, r.end)
	return r
}

// imageSize is the length of the image itself, which can differ from where its
// map ends. A rescue that never reached the tail of a disk leaves a map shorter
// than the image, and a caller sizing its structures from the image must not be
// told the disk is smaller than it is. It falls back to the mapped end for a
// reader that cannot report its size.
func imageSize(ra io.ReaderAt, fallback int64) int64 {
	switch v := ra.(type) {
	case interface{ Stat() (os.FileInfo, error) }:
		if info, err := v.Stat(); err == nil {
			return info.Size()
		}
	case interface{ Size() int64 }:
		return v.Size()
	}
	return fallback
}

// mappedEnd is where the map's coverage ends, and where Read reports io.EOF.
func mappedEnd(blocks []Block) int64 {
	if len(blocks) == 0 {
		return 0
	}
	last := blocks[len(blocks)-1]
	return last.Offset + last.Size
}

// Block is one entry from a ddrescue map file: a byte range of the input and
// how far ddrescue got with reading it.
type Block struct {
	Offset int64
	Size   int64
	Status BlockStatus
}

type BlockStatus byte

// Only BlockStatusFinished means the data was recovered; every other status
// marks a range that was never read successfully.
const (
	BlockStatusNonTried              BlockStatus = '?'
	BlockStatusFailedBlockNonTrimmed BlockStatus = '*'
	BlockStatusFailedBlockNonScraped BlockStatus = '/'
	BlockStatusFailedBlockBadSectors BlockStatus = '-'
	BlockStatusFinished              BlockStatus = '+'
)

// Valid reports whether s is one of the status characters ddrescue writes.
func (s BlockStatus) Valid() bool {
	switch s {
	case BlockStatusNonTried, BlockStatusFailedBlockNonTrimmed,
		BlockStatusFailedBlockNonScraped, BlockStatusFailedBlockBadSectors,
		BlockStatusFinished:
		return true
	}
	return false
}

// ParseMap parses a ddrescue map file without opening the image it describes.
func ParseMap(mapFile string) ([]Block, error) {
	mapRaw, err := os.ReadFile(mapFile)
	if err != nil {
		return nil, fmt.Errorf("ddrescue: failed to read map file %q: %w", mapFile, err)
	}
	return parseBlocks(string(mapRaw))
}

// FindMap returns the path of the map beside an image, or "" when there is
// none. Both naming conventions ddrescue is run with are tried: "input.img.map"
// first, since it identifies the image it belongs to, then "input.map", which
// is what a run that named its map after the medium rather than the image file
// leaves. A path with no extension has only one candidate.
func FindMap(image string) string {
	candidates := []string{image + ".map"}
	if ext := filepath.Ext(image); ext != "" && ext != ".map" {
		candidates = append(candidates, strings.TrimSuffix(image, ext)+".map")
	}
	for _, c := range candidates {
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			return c
		}
	}
	return ""
}

// Open pairs a disk image with its ddrescue map. Reads that touch any range the
// map does not mark finished fail rather than returning unrecovered bytes.
func Open(imageFile, mapFile string) (*Reader, error) {
	mapRaw, err := os.ReadFile(mapFile)
	if err != nil {
		return nil, fmt.Errorf("ddrescue: failed to read map file %q: %w", mapFile, err)
	}
	imgf, err := os.Open(imageFile)
	if err != nil {
		return nil, err
	}
	blocks, err := parseBlocks(string(mapRaw))
	if err != nil {
		imgf.Close() //nolint:errcheck
		return nil, err
	}
	r := &Reader{ra: imgf, closer: imgf, Blocks: blocks, end: mappedEnd(blocks)}
	r.size = imageSize(imgf, r.end)
	return r, nil
}

func parseBlocks(raw string) ([]Block, error) {
	var blocks []Block
	for l := range strings.SplitSeq(raw, "\n") {
		// Trim first: an indented line would otherwise split into an empty
		// leading field, and a CRLF file would leave "\r" on the status field.
		l = strings.TrimSpace(l)
		if l == "" || l[0] == '#' {
			continue
		}
		p := reWS.Split(l, -1)
		if isStatusLine(p) {
			continue
		}
		if len(p) < 3 {
			// A line cut short mid-write. Skipping it would under-report
			// coverage, which is fail-safe, but a truncated map should be
			// reported rather than read around.
			return nil, fmt.Errorf("ddrescue: truncated map line %q", l)
		}
		offset, err := strconv.ParseUint(p[0], 0, 64)
		if err != nil {
			return nil, fmt.Errorf("ddrescue: invalid offset %q: %w", p[0], err)
		}
		size, err := strconv.ParseUint(p[1], 0, 64)
		if err != nil {
			return nil, fmt.Errorf("ddrescue: invalid size %q: %w", p[1], err)
		}
		if len(p[2]) != 1 || !BlockStatus(p[2][0]).Valid() {
			return nil, fmt.Errorf("ddrescue: invalid block status %q at offset %s", p[2], p[0])
		}
		if offset > math.MaxInt64 || size > math.MaxInt64 {
			return nil, fmt.Errorf("ddrescue: block at %s with size %s is too large to address", p[0], p[1])
		}
		if size > math.MaxInt64-offset {
			return nil, fmt.Errorf("ddrescue: block at %s with size %s extends past the largest addressable offset", p[0], p[1])
		}
		blocks = append(blocks, Block{
			Offset: int64(offset),
			Size:   int64(size),
			Status: BlockStatus(p[2][0]),
		})
	}
	// Read locates blocks by binary search, and callers walk them in order.
	slices.SortFunc(blocks, func(a, b Block) int { return cmp.Compare(a.Offset, b.Offset) })
	// Overlap is rejected rather than resolved. Coverage is walked forward from
	// one block to the next, so a finished block reaching into a failed one
	// would admit the failed bytes as data, which this package guarantees never
	// happens. ddrescue never writes such a map, and no other source may supply
	// one either.
	for i := 1; i < len(blocks); i++ {
		if prev := blocks[i-1]; blocks[i].Offset < prev.Offset+prev.Size {
			return nil, fmt.Errorf("ddrescue: block at 0x%08x overlaps the block at 0x%08x of size %d",
				blocks[i].Offset, prev.Offset, prev.Size)
		}
	}
	return blocks, nil
}

// isStatusLine reports whether the fields are ddrescue's current-position line
// ("current_pos current_status [current_pass]") rather than a block. It is
// recognized by its shape, not its position in the file: a block's second field
// is a size, so a lone status character there cannot be a block. A map written
// without a status line therefore keeps its first block.
func isStatusLine(p []string) bool {
	if len(p) < 2 || len(p) > 3 || len(p[1]) != 1 {
		return false
	}
	_, err := strconv.ParseUint(p[1], 0, 64)
	return err != nil
}

// Close releases the image when the Reader opened it, and does nothing when
// the caller supplied one.
func (r *Reader) Close() error {
	if r.closer == nil {
		return nil
	}
	return r.closer.Close()
}

// ErrNotRecovered is returned by Read for a range the map does not mark
// finished. It is the package's core guarantee: unrecovered bytes are never
// returned as data.
var ErrNotRecovered = errors.New("ddrescue: range was not recovered")

// Read fills p from the image at the sequential offset, stopping at the first
// byte the map does not mark finished.
func (r *Reader) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n, err := r.readAt(p, r.Offset)
	r.Offset += int64(n)
	return n, err
}

// ReadAt reads len(p) bytes at off, subject to the same map checks as Read. A
// short read keeps its byte count: the bytes before an unrecovered range are
// still data the caller can use. The sequential offset is untouched, so a
// caller reading through Read and Seek is unaffected.
func (r *Reader) ReadAt(p []byte, off int64) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var n int
	for n < len(p) {
		m, err := r.readAt(p[n:], off+int64(n))
		n += m
		if err != nil {
			if errors.Is(err, io.EOF) && n > 0 {
				return n, io.ErrUnexpectedEOF
			}
			return n, err
		}
		if m == 0 {
			return n, io.ErrNoProgress
		}
	}
	return n, nil
}

// readAt is the only place the map is enforced. Every block the read touches is
// checked, not just the one holding its first byte, so a read starting in a
// finished block and running into a failed one returns only the finished prefix
// rather than unrecovered data; the next read then begins at the failed block
// and fails. A read beginning in an unrecovered or unmapped range fails
// outright, and one past the last mapped byte reports io.EOF.
func (r *Reader) readAt(p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(r.Blocks) == 0 {
		return 0, errors.New("ddrescue: map file describes no blocks")
	}
	if off >= r.end {
		return 0, io.EOF
	}
	readEnd := off + int64(len(p))
	// A read starting near the largest addressable offset can wrap its end past
	// zero, which would make the slice below negative. Saturate the end
	// instead. The map's own blocks already have Offset+Size ≤ MaxInt64, so the
	// walk still stops where it should.
	if readEnd < off {
		readEnd = math.MaxInt64
	}

	// First block reaching past the start of the read. Blocks are sorted, so
	// this is a binary search rather than a scan of the whole map.
	i := sort.Search(len(r.Blocks), func(i int) bool {
		return r.Blocks[i].Offset+r.Blocks[i].Size > off
	})
	if i == len(r.Blocks) || r.Blocks[i].Offset > off {
		return 0, fmt.Errorf("ddrescue: offset %d is not covered by the map: %w", off, ErrNotRecovered)
	}

	// Walk forward over every block the read touches. Coverage must stay
	// contiguous and finished; the first byte that is not becomes the end of
	// this read.
	covered := off
	for j := i; j < len(r.Blocks) && r.Blocks[j].Offset < readEnd; j++ {
		b := r.Blocks[j]
		if b.Offset > covered {
			break // gap: the map says nothing about the next byte
		}
		if b.Status != BlockStatusFinished {
			if covered == off {
				return 0, fmt.Errorf("ddrescue: failed block (%c) at offset 0x%08x of size %d: %w",
					b.Status, b.Offset, b.Size, ErrNotRecovered)
			}
			break // hand back the finished prefix
		}
		covered = b.Offset + b.Size
	}
	covered = min(covered, readEnd)

	n, err := r.ra.ReadAt(p[:covered-off], off)
	if err != nil && (errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)) && n > 0 {
		// The image is shorter than its map says. The bytes that did come back
		// are still data; the next read starts past them and reports the EOF.
		err = nil
	}
	return n, err
}

// Seek moves the sequential offset. It is bookkeeping only: nothing is read
// until Read, so a seek past the end of the image is not an error here.
// SeekEnd is relative to the image's own length rather than to where the map
// stops: a caller seeking to the end is asking how big the image is, and a
// partly rescued disk is not a smaller disk.
func (r *Reader) Seek(offset int64, whence int) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = r.Offset + offset
	case io.SeekEnd:
		abs = r.size + offset
	default:
		return r.Offset, fmt.Errorf("ddrescue: invalid whence %d", whence)
	}
	if abs < 0 {
		return r.Offset, fmt.Errorf("ddrescue: seek to negative offset %d", abs)
	}
	r.Offset = abs
	return abs, nil
}

// Unrecovered returns the parts of [off, off+size) the map does not mark
// finished, coalesced and in order. A range the map does not cover is reported
// as non-tried. This reports what a file lost without reading anything.
func Unrecovered(blocks []Block, off, size int64) []Block {
	if size <= 0 {
		return nil
	}
	end := off + size
	if end < off {
		end = math.MaxInt64
	}
	var out []Block
	add := func(o, e int64, status BlockStatus) {
		if e <= o {
			return
		}
		if n := len(out); n > 0 && out[n-1].Status == status && out[n-1].Offset+out[n-1].Size == o {
			out[n-1].Size += e - o
			return
		}
		out = append(out, Block{Offset: o, Size: e - o, Status: status})
	}

	// First block reaching past the start of the range; blocks are sorted.
	i := sort.Search(len(blocks), func(i int) bool {
		return blocks[i].Offset+blocks[i].Size > off
	})
	covered := off
	for ; i < len(blocks) && blocks[i].Offset < end; i++ {
		b := blocks[i]
		add(covered, min(b.Offset, end), BlockStatusNonTried) // a hole in the map
		covered = max(covered, b.Offset)
		if b.Status != BlockStatusFinished {
			add(covered, min(b.Offset+b.Size, end), b.Status)
		}
		covered = max(covered, min(b.Offset+b.Size, end))
	}
	add(covered, end, BlockStatusNonTried)
	return out
}

// UnrecoveredBytes is how many bytes of [off, off+size) the map does not mark
// finished.
func UnrecoveredBytes(blocks []Block, off, size int64) int64 {
	var n int64
	for _, b := range Unrecovered(blocks, off, size) {
		n += b.Size
	}
	return n
}
