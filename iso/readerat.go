package iso

import (
	"errors"
	"io"
	"sort"
)

// FileReaderAt reads a file's bytes at any offset, which a format with an
// index at its end (zip, for one) needs to be opened in place. It is safe for
// concurrent use when the Reader's io.ReaderAt is.
//
// It reads only the file's payload bytes. On a raw image that means a sector
// whose EDC/ECC was lost still yields its data, as it does through FileReader.
type FileReaderAt struct {
	r          *Reader
	extents    []Extent
	starts     []int64 // the file offset each extent begins at
	size       int64
	incomplete bool
}

// OpenFileAt returns a FileReaderAt for a file's extents. An entry with no
// Extents is read as the single extent StartLBA and Size describe.
func (r *Reader) OpenFileAt(e DirEntry) *FileReaderAt {
	exts := e.Extents
	if exts == nil {
		exts = []Extent{{LBA: e.StartLBA, Size: e.Size}}
	}
	f := &FileReaderAt{r: r, incomplete: e.Incomplete}
	for _, x := range exts {
		// An empty extent holds nothing, and keeping it would put two extents
		// at one file offset.
		if x.Size <= 0 {
			continue
		}
		f.extents = append(f.extents, x)
		f.starts = append(f.starts, f.size)
		f.size += x.Size
	}
	return f
}

// Size returns the file's length: the sum of its extents.
func (f *FileReaderAt) Size() int64 { return f.size }

var errNegativeOffset = errors.New("iso: negative offset")

// ReadAt reads len(p) bytes at file offset off. A sector that will not read
// ends the read with a *SectorReadError giving its file offset, after the
// bytes before it. Reading past the end returns io.EOF, or ErrIncompleteFile
// for a multi-extent file whose later extents are missing.
func (f *FileReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errNegativeOffset
	}
	ss := int64(f.r.sectorSize)
	cooked := f.r.dataOffset == 0 && ss == UserDataSize
	var n int
	for n < len(p) {
		pos := off + int64(n)
		if pos >= f.size {
			return n, f.end()
		}
		i := sort.Search(len(f.starts), func(i int) bool { return f.starts[i] > pos }) - 1
		x := f.extents[i]
		within := pos - f.starts[i]
		inner := within % UserDataSize
		chunk := min(int64(len(p)-n), x.Size-within)
		// A cooked image holds an extent's payload contiguously. A raw one
		// wraps each sector's payload in a header and EDC/ECC, so it is read a
		// sector at a time.
		if !cooked {
			chunk = min(chunk, UserDataSize-inner)
		}
		at := (x.LBA+within/UserDataSize)*ss + int64(f.r.dataOffset) + inner
		m, err := f.r.ra.ReadAt(p[n:n+int(chunk)], at)
		n += m
		if err != nil && int64(m) < chunk {
			failed := within + int64(m) // the first byte not read, within the extent
			sec := failed / UserDataSize
			return n, &SectorReadError{
				LBA:    x.LBA + sec,
				Offset: f.starts[i] + sec*UserDataSize,
				N:      int(failed % UserDataSize),
				Err:    err,
			}
		}
	}
	return n, nil
}

func (f *FileReaderAt) end() error {
	if f.incomplete {
		return ErrIncompleteFile
	}
	return io.EOF
}
