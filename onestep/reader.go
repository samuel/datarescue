package onestep

import (
	"bytes"
	"compress/flate"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"

	"samuelks.com/datarescue/ddrescue"
)

// ChunkReadError reports a chunk whose data could not be read or decompressed
// in full. N is how many bytes of the chunk were delivered before the failure;
// those bytes are handed back alongside this error rather than discarded.
type ChunkReadError struct {
	Chunk int
	N     int64
	Err   error
}

func (e *ChunkReadError) Error() string {
	return fmt.Sprintf("onestep: chunk %d: %d bytes recovered before %s", e.Chunk, e.N, e.Err)
}

func (e *ChunkReadError) Unwrap() error { return e.Err }

// Reader streams one file's contents, chunk by chunk.
//
// A chunk that cannot be read or that inflates only partway returns the bytes
// recovered from it wrapped in a *ChunkReadError. The error is latched and the
// reader does not advance to the next chunk, since a caller that retried would
// otherwise splice a later chunk in at the wrong file offset with nothing to
// mark the gap.
//
// A chunk whose data does not match its recorded checksum is still delivered:
// the bytes may be what the file held, and the caller is told through Errors.
type Reader struct {
	a      *Archive
	file   *File
	index  int
	chunk  *Chunk
	src    io.Reader
	closer io.Closer
	sum    hash.Hash32
	read   int64 // delivered from the chunk being read
	off    int64 // delivered from the file, which is where a hole is recorded
	// filled says the chunk being read was zero-filled, so its length and
	// checksum are not worth complaining about.
	filled bool
	// fill, when it is armed, makes a chunk that will not read or decompress
	// deliver zeros for the rest of itself and the read carry on. The catalog
	// records each chunk's original size, so the hole's length is arithmetic
	// rather than a guess and the chunk after it still lands at the offset the
	// file gives it.
	fill *ddrescue.GapLog
	errs []error
	err  error
}

// Fill gives the reader a log to record its holes in, switching it from
// latching at the damage to zero-filling past it. Call it before the first
// Read. Only ddrescue.Fill arms a log, and it writes the map in the same call,
// so a reader cannot deliver zeros without a record of them.
func (r *Reader) Fill(l *ddrescue.GapLog) {
	if !l.Filling() {
		return
	}
	l.Limit(r.file.Size)
	r.fill = l
}

// OpenFile streams a file's contents.
func (a *Archive) OpenFile(f *File) *Reader {
	return &Reader{a: a, file: f}
}

// Open streams the file at path.
func (a *Archive) Open(path string) (*Reader, error) {
	f, ok := a.Find(path)
	if !ok {
		return nil, fmt.Errorf("onestep: %s is not in the backup", path)
	}
	return a.OpenFile(f), nil
}

// Errors returns the problems found in the data delivered so far: a chunk
// whose checksum did not match, or one that ended short of its recorded size.
// These do not stop the read, which is why they are collected rather than
// returned.
func (r *Reader) Errors() []error { return r.errs }

func (r *Reader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.err != nil {
		return 0, r.err
	}
	var total int
	for len(p) > 0 {
		// The file's own length bounds what it delivers, whatever the chunks
		// say: a corrupt OrigSize would otherwise write zeros or data past the
		// end of the file, which the map beside it does not describe.
		remain := r.file.Size - r.off
		if remain <= 0 {
			r.finishTail()
			break
		}
		if int64(len(p)) > remain {
			p = p[:remain]
		}
		if r.src == nil {
			if r.index >= len(r.file.Chunks) {
				if total > 0 {
					return total, nil
				}
				return 0, io.EOF
			}
			if err := r.openChunk(&r.file.Chunks[r.index]); err != nil {
				r.err = err
				return total, r.err
			}
		}
		n, err := r.src.Read(p)
		if n > 0 {
			_, _ = r.sum.Write(p[:n])
			r.read += int64(n)
			r.off += int64(n)
			total += n
			p = p[n:]
		}
		if err == nil {
			continue
		}
		if errors.Is(err, io.EOF) && r.read < r.chunk.OrigSize {
			// The stream ended cleanly but short of the length the catalog
			// records. The bytes that did not arrive are as lost as bytes that
			// would not read, and calling the chunk finished here would land
			// every chunk after it at the wrong file offset with nothing to
			// mark the gap.
			err = io.ErrUnexpectedEOF
		}
		switch {
		case r.read >= r.chunk.OrigSize:
			// The chunk is already whole. The catalog's size is the authority
			// on how much one holds, so a decompressor that runs out of input
			// just as it delivers the last byte has lost nothing: some writers
			// left the final block marker off the stream.
			r.finishChunk()
		case r.fillChunk(err):
			// The rest of the chunk is a hole; the loop reads it back as zeros.
		default:
			r.err = &ChunkReadError{Chunk: r.chunk.Serial, N: r.read, Err: err}
			r.closeChunk()
			return total, r.err
		}
	}
	if total > 0 {
		return total, nil
	}
	return 0, io.EOF
}

// finishTail settles the chunks once the file holds all the bytes it declares.
//
// A chunk with no bytes left is still finished, since a zero-length file is one
// empty chunk and its recorded length and checksum are all there is to check.
// A chunk with bytes left over means the catalog disagrees with itself. The
// file's own size decides what is delivered: the rest is dropped and reported
// rather than written past the end of the file, where the map beside it
// describes nothing.
func (r *Reader) finishTail() {
	past := func(c *Chunk, n int64) {
		r.errs = append(r.errs, fmt.Errorf("onestep: chunk %d holds %d bytes past the %d the file records",
			c.Serial, n, r.file.Size))
	}
	for r.src != nil || r.index < len(r.file.Chunks) {
		if r.src == nil {
			c := &r.file.Chunks[r.index]
			if c.OrigSize != 0 {
				past(c, c.OrigSize)
				return
			}
			if err := r.openChunk(c); err != nil {
				r.errs = append(r.errs, err)
				return
			}
		}
		if r.read < r.chunk.OrigSize {
			past(r.chunk, r.chunk.OrigSize-r.read)
			r.closeChunk()
			return
		}
		r.finishChunk()
	}
}

// fillChunk turns the rest of a chunk that would not read or decompress into
// zeros, and reports whether it could.
//
// The catalog's OrigSize makes this safe: the hole's length is the chunk's
// recorded size less what came back, so it is computed rather than guessed.
// The chunk still reaches finishChunk at its recorded length and every chunk
// after it lands where the file says. Without an armed log (only ddrescue.Fill
// arms one, writing the map in the same call) the read latches at the damage.
func (r *Reader) fillChunk(cause error) bool {
	if cause == nil || !r.fill.Filling() {
		return false
	}
	hole := r.chunk.OrigSize - r.read
	if hole <= 0 {
		return false
	}
	r.fill.Add(r.off, hole, cause)
	r.filled = true
	r.closeChunk()
	r.src = io.LimitReader(zeroes{}, hole)
	return true
}

// zeroes supplies zeros in place of bytes that would not read, as many as are
// asked for.
type zeroes struct{}

func (zeroes) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// openChunk prepares the next chunk's stream of original bytes.
func (r *Reader) openChunk(c *Chunk) error {
	r.chunk = c
	r.read = 0
	r.filled = false
	r.sum = crc32.NewIEEE()
	if c.Empty() {
		// A zero-length file is one chunk holding nothing. There is no data to
		// go looking for, so it comes back even from a disk that was not
		// supplied.
		r.src = bytes.NewReader(nil)
		return nil
	}
	if c.diskIdx < 0 {
		// The disk holding this chunk was never supplied. Its whole recorded
		// length is the hole, which is exact, so a set missing a disk restores
		// at full length with the missing chunks marked in every affected
		// file's map rather than stopping at the first of them.
		missing := errors.New("its data is not on any supplied disk")
		if r.fillChunk(missing) {
			return nil
		}
		return &ChunkReadError{Chunk: c.Serial, Err: missing}
	}
	src := r.a.chunkReader(c)
	switch c.Level {
	case LevelStored:
		r.src = io.LimitReader(src, c.OrigSize)
	case LevelDeflate:
		// Each chunk is a self-contained raw deflate stream: no zlib or gzip
		// wrapper, and no trailing checksum. The catalog's size is the
		// authority on how much a chunk holds, so the output is bounded by it
		// rather than by where the stream claims to end.
		fr := flate.NewReader(src)
		r.closer = fr
		r.src = io.LimitReader(fr, c.OrigSize)
	default:
		// The catalog still records this chunk's length, so an unrecognized
		// compression level becomes a hole of that length, the same as a chunk
		// that will not read.
		unknown := fmt.Errorf("unknown compression level %d", c.Level)
		if r.fillChunk(unknown) {
			return nil
		}
		return &ChunkReadError{Chunk: c.Serial, Err: unknown}
	}
	return nil
}

// finishChunk checks a completed chunk and moves on to the next.
//
// A chunk that was zero-filled reaches its recorded length by construction and
// its checksum is a CRC over zeros, so neither is checked: the gap already
// records what happened, and both checks would only repeat it less clearly.
func (r *Reader) finishChunk() {
	c := r.chunk
	if !r.filled {
		if r.read != c.OrigSize {
			r.errs = append(r.errs, fmt.Errorf("onestep: chunk %d holds %d bytes, %d recorded", c.Serial, r.read, c.OrigSize))
		}
		if c.HasChecksum() && c.Checksum != ^r.sum.Sum32() {
			r.errs = append(r.errs, fmt.Errorf("onestep: chunk %d checksum is 0x%08x, 0x%08x recorded",
				c.Serial, ^r.sum.Sum32(), c.Checksum))
		}
	}
	r.closeChunk()
	r.index++
}

func (r *Reader) closeChunk() {
	if r.closer != nil {
		r.closer.Close() //nolint:errcheck // a decompressor's close reports nothing a restore can act on
		r.closer = nil
	}
	r.src = nil
}

// Check is the result of reading a file's data without keeping it.
type Check struct {
	File *File
	// Bytes is how much of the file was recovered.
	Bytes int64
	// Errors holds the checksum and length problems found, and the read
	// failure that stopped the check when there was one.
	Errors []error
}

// OK reports whether the whole file was recovered intact.
func (c Check) OK() bool { return len(c.Errors) == 0 && c.Bytes == c.File.Size }

// Check reads a file's data and reports what is wrong with it, writing
// nothing. Every chunk is decompressed and checked against its recorded
// checksum, so a backup can be verified without restoring it.
func (a *Archive) Check(f *File) Check {
	r := a.OpenFile(f)
	n, err := io.Copy(io.Discard, r)
	c := Check{File: f, Bytes: n, Errors: r.Errors()}
	if err != nil {
		c.Errors = append(c.Errors, err)
	}
	return c
}

// chunkReader reads one chunk's stored bytes off the disk holding it. A chunk
// never spans disks: its offset is a position in its own disk's file, and the
// last chunk on a disk ends where that disk's data does.
type chunkReader struct {
	r      io.ReaderAt
	path   string
	off    int64
	remain int64
}

func (a *Archive) chunkReader(c *Chunk) io.Reader {
	d := a.disks[c.diskIdx]
	return &chunkReader{r: d.r, path: d.source.Path, off: c.Offset, remain: c.CompSize}
}

func (s *chunkReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if s.remain <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > s.remain {
		p = p[:s.remain]
	}
	n, err := s.r.ReadAt(p, s.off)
	s.off += int64(n)
	s.remain -= int64(n)
	if n > 0 {
		// Hand back what was read; the next call starts at the failure and
		// reports it then.
		return n, nil
	}
	switch {
	case err == nil:
		return 0, fmt.Errorf("%s: no progress reading at %d", s.path, s.off)
	case errors.Is(err, io.EOF):
		// The image stops inside the chunk, so the rest of it was never
		// recovered.
		return 0, io.ErrUnexpectedEOF
	}
	return 0, fmt.Errorf("%s: %w", s.path, err)
}
