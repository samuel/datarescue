// Package diskimage locates a filesystem inside a disk image so the filesystem
// parsers do not each need their own copy of "where does it start".
package diskimage

import (
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"

	"samuelks.com/datarescue/apm"
	"samuelks.com/datarescue/ddrescue"
	"samuelks.com/datarescue/img86f"
	"samuelks.com/datarescue/internal/compressed"
	"samuelks.com/datarescue/mbr"
)

// Options control how the filesystem is located within an image.
type Options struct {
	// Offset is the byte offset of the filesystem. A negative value means
	// auto-detect: try offset 0, then scan the partition table.
	Offset int64
	// Partition selects a 1-based partition, or 0 to pick automatically.
	Partition int
	// Geometry is an "SPT:HEADS:BPS" override for 86F floppy images, which
	// carry no geometry of their own.
	Geometry string
	// Map is the ddrescue map recording which of the image's ranges were
	// recovered. Empty picks up the sidecar beside the image when there is one;
	// NoMap ignores even that. Both are ddrescue.Options, resolved by
	// ddrescue.Resolve so every tool finds a map the same way.
	Map string
	// NoMap opens the image bytes as they are, even when a map sits beside it.
	NoMap bool
	// Accept reports whether a filesystem starts at the given offset. It is the
	// caller's parser: whatever it accepts is what auto-detection looks for.
	Accept func(r io.ReadSeeker, offset int64) bool
	// PartitionMatches optionally narrows which MBR partition types are tried
	// during auto-detection. Nil tries every used partition.
	PartitionMatches func(partitionType byte) bool
	// APMPartitionMatches optionally narrows which Apple partition map entries
	// are tried, by pmParType. Nil tries every entry that could hold a
	// filesystem, which excludes free space, the map itself and the driver
	// partitions.
	//
	// It is a second predicate rather than a generalization of the one above
	// because the two schemes type a partition differently — a one-byte code
	// against a 32-byte string — and collapsing them behind an interface is
	// more machinery than three call sites justify.
	APMPartitionMatches func(partitionType string) bool
}

// Image is a located filesystem: a reader positioned over the whole image and
// the byte offset at which the filesystem starts.
type Image struct {
	Reader io.ReadSeeker
	Offset int64
	// Size is the byte range the located filesystem may draw on: the image's
	// own length for a volume at an offset, the located partition's own end for
	// one found in a partition table. The partition's end bounds lengths read
	// from the partition table and lets a filesystem find its own backup
	// structures: an HFS alternate master directory block sits in the volume's
	// second-to-last sector, and nothing in the volume says where that is.
	Size int64
	// Source describes where the filesystem was found, for diagnostics.
	Source string
	// MapPath is the ddrescue map the image was opened through, empty when
	// there was none. Blocks holds its contents, which tell whether a range of
	// the image is data or a hole the rescue never filled.
	MapPath string
	Blocks  []ddrescue.Block
	closer  io.Closer
}

// Unrecovered returns the parts of an image byte range the map does not mark
// finished. Without a map nothing is known to be missing, so it returns
// nothing: a read is then the only way to find damage.
func (im *Image) Unrecovered(off, size int64) []ddrescue.Block {
	if len(im.Blocks) == 0 {
		return nil
	}
	return ddrescue.Unrecovered(im.Blocks, off, size)
}

// ReaderAt is the image as an io.ReaderAt positioned at the filesystem, for a
// parser that reads by offset rather than by seeking. The offset is folded in
// here so no caller has to remember to add it.
//
// An 86F image is decoded track by track and has no ReaderAt. That loses
// nothing: no ISO 9660 or HFS volume lives in an 86Box PC floppy image.
func (im *Image) ReaderAt() (io.ReaderAt, error) {
	ra, ok := im.Reader.(io.ReaderAt)
	if !ok {
		return nil, errors.New("diskimage: this image is decoded track by track and cannot be read at a byte offset")
	}
	size := im.Size - im.Offset
	if size <= 0 {
		size = math.MaxInt64 - im.Offset
	}
	return io.NewSectionReader(ra, im.Offset, size), nil
}

func (im *Image) Close() error {
	if im.closer == nil {
		return nil
	}
	return im.closer.Close()
}

// ErrNotFound reports that no filesystem the caller accepts was located.
var ErrNotFound = errors.New("diskimage: no filesystem found in image")

// ErrNoSuchPartition reports a partition number the table does not hold. That
// is the operator asking for something that is not there, not damage to the
// medium, so a caller turns it into a usage error. It is detected here because
// the table has to be read before anyone can know.
var ErrNoSuchPartition = errors.New("diskimage: partition not present in the partition table")

// NotFoundError carries the rescue map alongside the failure to find a
// filesystem. The map often explains the failure: a volume descriptor or boot
// sector that was never recovered is exactly the case where nothing can be
// located. With the map, a caller that reports ErrNotFound can still say what
// the image lost, rather than printing the least output in the case the map
// exists for.
type NotFoundError struct {
	// MapPath is the map the image was opened through, empty when there was
	// none.
	MapPath string
	Blocks  []ddrescue.Block
	// Size is the image's length, so the caller can measure the damage over it.
	Size int64
	Err  error
}

func (e *NotFoundError) Error() string { return e.Err.Error() }
func (e *NotFoundError) Unwrap() error { return e.Err }

// Open locates a filesystem in the image at path. An ".86f" path is decoded as
// an 86Box floppy image and presented as a linear sector stream.
func Open(path string, opts Options) (*Image, error) {
	if strings.HasSuffix(strings.ToLower(path), ".86f") {
		if opts.Map != "" {
			return nil, errors.New("diskimage: a ddrescue map does not apply to an 86F image, whose sectors are decoded from track data rather than read at a byte offset")
		}
		return open86F(path, opts)
	}

	src, err := ddrescue.OpenSource(path, ddrescue.Options{Map: opts.Map, NoMap: opts.NoMap})
	if err != nil {
		return nil, err
	}
	im, err := locate(src.Reader(), src, src.Size, opts)
	if err != nil {
		// The map can explain why no filesystem was located, so it is returned
		// with the failure rather than discarded with the source.
		err = notFound(err, src.Raw(), src.MapPath, src.Blocks, src.Size)
		src.Close() //nolint:errcheck // best-effort cleanup on the error path
		return nil, err
	}
	im.MapPath, im.Blocks = src.MapPath, src.Blocks
	return im, nil
}

// OpenReaderAt locates a filesystem in an image the caller already has, such
// as one stored inside an archive or another filesystem. size is the image's
// length: the partition tables are bounded by it, and a filesystem at offset 0
// is sized by it. The caller keeps ownership of ra, and the Image's Close does
// nothing.
//
// It detects the same layouts as Open. An 86F floppy image is recognized by
// its signature, since there is no file name to go by.
//
// Options.Map names a file beside an image on disk and is refused here, and
// NoMap has nothing to ignore. A caller holding a rescue map reads through it
// by passing ddrescue.NewReader(ra, blocks) as ra; Image.Blocks is then empty,
// so Unrecovered reports nothing and the caller asks the blocks itself.
func OpenReaderAt(ra io.ReaderAt, size int64, opts Options) (*Image, error) {
	if opts.Map != "" {
		return nil, errors.New("diskimage: a map file does not apply to a caller-supplied reader; wrap the reader in ddrescue.NewReader instead")
	}
	if size < 0 {
		return nil, fmt.Errorf("diskimage: negative image size %d", size)
	}
	sr := io.NewSectionReader(ra, 0, size)
	if is86F(ra) {
		disk, err := img86f.OpenReader(sr, size)
		if err != nil {
			return nil, fmt.Errorf("diskimage: open 86F image: %w", err)
		}
		return from86F(disk, opts)
	}
	im, err := locate(sr, nil, size, opts)
	if err != nil {
		return nil, notFound(err, ra, "", nil, size)
	}
	return im, nil
}

// is86F reports whether the image starts with the 86F signature.
func is86F(ra io.ReaderAt) bool {
	var magic [4]byte
	n, _ := ra.ReadAt(magic[:], 0) // a short or failed read means it is not one
	return n == len(magic) && string(magic[:]) == img86f.Magic
}

// notFound turns a failure to find a filesystem into a *NotFoundError carrying
// the map, adding a hint when the image is a compressed file. Any other error
// is returned as it is.
func notFound(err error, raw io.ReaderAt, mapPath string, blocks []ddrescue.Block, size int64) error {
	if !errors.Is(err, ErrNotFound) {
		return err
	}
	return &NotFoundError{MapPath: mapPath, Blocks: blocks, Size: size, Err: compressedHint(raw, err)}
}

// compressedHint adds a hint to err when an image that held nothing
// recognizable is a compressed file, the likeliest reason for the failure.
func compressedHint(ra io.ReaderAt, err error) error {
	b := make([]byte, 8)
	n, _ := ra.ReadAt(b, 0) // a short or failed read just means no hint
	if h := compressed.Hint(b[:n]); h != "" {
		return fmt.Errorf("%w%s", err, h)
	}
	return err
}

func locate(r io.ReadSeeker, closer io.Closer, size int64, opts Options) (*Image, error) {
	if opts.Offset >= 0 {
		return &Image{Reader: r, Offset: opts.Offset, Size: size, Source: fmt.Sprintf("offset %d", opts.Offset), closer: closer}, nil
	}
	accept := opts.Accept
	if accept == nil {
		accept = func(io.ReadSeeker, int64) bool { return true }
	}

	// An unpartitioned image (a floppy, or a partition dumped on its own) has
	// the filesystem at offset 0.
	if opts.Partition == 0 && accept(r, 0) {
		return &Image{Reader: r, Offset: 0, Size: size, Source: "offset 0 (no partition table)", closer: closer}, nil
	}

	ra, ok := r.(io.ReaderAt)
	if !ok {
		return nil, ErrNotFound
	}

	// The Apple partition map is tried first. On a disk carrying only one
	// scheme the other's probe costs two 512-byte reads and stops, so the order
	// only decides a hybrid disc that carries both — and there the Apple map is
	// the one describing the Mac volumes.
	if im, err := locateAPM(r, ra, closer, size, accept, opts); im != nil || err != nil {
		return im, err
	}

	parts, err := mbr.Partitions(ra)
	if err != nil {
		if errors.Is(err, mbr.ErrNotMBR) {
			if opts.Partition > 0 {
				// Asking for a partition on an image with no partition table
				// is the same mistake as asking for one the table does not
				// hold, and neither can be judged before the table is read.
				return nil, fmt.Errorf("%w: partition %d, and this image has no partition table",
					ErrNoSuchPartition, opts.Partition)
			}
			return nil, fmt.Errorf("%w: nothing at offset 0 and no partition table", ErrNotFound)
		}
		return nil, fmt.Errorf("diskimage: reading partition table: %w", err)
	}
	if len(parts) == 0 {
		return nil, fmt.Errorf("%w: partition table lists no usable partitions", ErrNotFound)
	}

	if opts.Partition > 0 {
		for _, p := range parts {
			if p.Index == opts.Partition && !p.Logical {
				return &Image{
					Reader: r,
					Offset: p.Offset(),
					Size:   mbrEnd(&p),
					Source: fmt.Sprintf("partition %d (%s)", p.Index, mbr.TypeName(p.PartitionType)),
					closer: closer,
				}, nil
			}
		}
		return nil, fmt.Errorf("%w: partition %d", ErrNoSuchPartition, opts.Partition)
	}

	for _, p := range parts {
		if opts.PartitionMatches != nil && !opts.PartitionMatches(p.PartitionType) {
			continue
		}
		if accept(r, p.Offset()) {
			kind := "partition"
			if p.Logical {
				kind = "logical partition"
			}
			return &Image{
				Reader: r,
				Offset: p.Offset(),
				Size:   mbrEnd(&p),
				Source: fmt.Sprintf("%s %d (%s)", kind, p.Index, mbr.TypeName(p.PartitionType)),
				closer: closer,
			}, nil
		}
	}
	return nil, ErrNotFound
}

// mbrEnd is the byte range an MBR partition covers: the partition's own end as
// an absolute image offset. Both MBR paths bound the located filesystem by it,
// for the reason apmImage gives: a volume sized as the whole image cannot find
// backup structures that are located relative to its own end.
func mbrEnd(p *mbr.Entry) int64 {
	return p.Offset() + int64(p.NumberOfSectors)*mbr.SectorSize
}

// locateAPM looks for the filesystem in an Apple partition map. It returns a
// nil image and a nil error when the disk carries no such map, which sends the
// caller on to the MBR.
func locateAPM(r io.ReadSeeker, ra io.ReaderAt, closer io.Closer, size int64,
	accept func(io.ReadSeeker, int64) bool, opts Options,
) (*Image, error) {
	m, err := apm.ReadMap(ra, size)
	if err != nil {
		if errors.Is(err, apm.ErrNotAPM) {
			return nil, nil
		}
		// A map block that would not read is a finding about the medium and
		// not "this disk has no Apple map", so it is not swallowed.
		return nil, fmt.Errorf("diskimage: reading the Apple partition map: %w", err)
	}

	// The number is the entry's position in the map, which is its block, so
	// what a listing prints is what an operator types. Numbering only the
	// entries that could hold a filesystem would produce numbers the disk does
	// not record, and would renumber everything after an entry that could not be
	// read.
	if opts.Partition > 0 {
		for i := range m.Entries {
			e := &m.Entries[i]
			if e.Index != opts.Partition {
				continue
			}
			return apmImage(r, closer, e), nil
		}
		return nil, fmt.Errorf("%w: partition %d of %d in the Apple partition map",
			ErrNoSuchPartition, opts.Partition, len(m.Entries))
	}

	matches := opts.APMPartitionMatches
	if matches == nil {
		matches = apm.IsFilesystem
	}
	for i := range m.Entries {
		e := &m.Entries[i]
		if e.Unused() || !matches(e.TypeName()) {
			continue
		}
		if accept(r, e.Offset()) {
			return apmImage(r, closer, e), nil
		}
	}
	return nil, fmt.Errorf("%w: the Apple partition map lists no partition holding one", ErrNotFound)
}

// apmImage describes a filesystem found in an Apple partition.
//
// Size is the partition's own length and not the image's, which HFS depends on:
// its spare master directory block sits two logical blocks from the end of the
// volume, and no field inside the volume says where that is.
func apmImage(r io.ReadSeeker, closer io.Closer, e *apm.Entry) *Image {
	source := fmt.Sprintf("Apple partition %d (%s", e.Index, apm.TypeDescription(e.TypeName()))
	if name := e.PartName(); name != "" {
		source += fmt.Sprintf(", %q", name)
	}
	return &Image{
		Reader: r,
		Offset: e.Offset(),
		Size:   e.Offset() + e.Size(),
		Source: source + ")",
		closer: closer,
	}
}

func open86F(path string, opts Options) (*Image, error) {
	disk, err := img86f.Open(path)
	if err != nil {
		return nil, fmt.Errorf("diskimage: open 86F image: %w", err)
	}
	return from86F(disk, opts)
}

// from86F presents a decoded 86F image as a linear sector stream. The Image
// takes over the Disk and closes it.
func from86F(disk *img86f.Disk, opts Options) (*Image, error) {
	var err error
	var spt, heads, bps int
	if opts.Geometry != "" {
		spt, heads, bps, err = ParseGeometry(opts.Geometry)
	} else {
		spt, heads, bps, err = img86f.DetectGeometry(disk)
		if err != nil {
			err = fmt.Errorf("%w\nuse -geometry SPT:HEADS:BPS to specify geometry manually (e.g. -geometry 18:2:512)", err)
		}
	}
	if err != nil {
		disk.Close() //nolint:errcheck // best-effort cleanup on the error path
		return nil, err
	}
	lr, err := img86f.NewLinearReader(disk, spt, heads, bps)
	if err != nil {
		disk.Close() //nolint:errcheck // best-effort cleanup on the error path
		return nil, err
	}
	// The decoded length comes from the reader rather than from the geometry
	// multiplied out again, so the two cannot disagree.
	size, err := lr.Seek(0, io.SeekEnd)
	if err != nil {
		disk.Close() //nolint:errcheck // best-effort cleanup on the error path
		return nil, fmt.Errorf("diskimage: sizing 86F image: %w", err)
	}
	if _, err := lr.Seek(0, io.SeekStart); err != nil {
		disk.Close() //nolint:errcheck // best-effort cleanup on the error path
		return nil, fmt.Errorf("diskimage: rewinding 86F image: %w", err)
	}
	return &Image{
		Reader: lr,
		Offset: 0,
		Size:   size,
		Source: fmt.Sprintf("86F image, geometry %d:%d:%d", spt, heads, bps),
		closer: disk,
	}, nil
}

// ParseGeometry parses an "SPT:HEADS:BPS" geometry string.
func ParseGeometry(s string) (spt, heads, bps int, err error) {
	parts := strings.SplitN(s, ":", 3)
	if len(parts) != 3 {
		return 0, 0, 0, fmt.Errorf("geometry %q: expected SPT:HEADS:BPS (e.g. 18:2:512)", s)
	}
	vals := make([]int, 3)
	names := [3]string{"SPT", "HEADS", "BPS"}
	for i, p := range parts {
		v, err := strconv.Atoi(p)
		if err != nil || v <= 0 {
			return 0, 0, 0, fmt.Errorf("geometry %s=%q: must be a positive integer", names[i], p)
		}
		vals[i] = v
	}
	return vals[0], vals[1], vals[2], nil
}
