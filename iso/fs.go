package iso

import (
	"errors"
	"io"
	"io/fs"
	"slices"
	"strings"
	"time"
)

// FS returns the volume as an fs.FS, for code written against io/fs. It also
// implements fs.ReadDirFS and fs.StatFS, and its files implement io.ReaderAt
// and io.Seeker. Reading a multi-extent file whose later extents are missing
// ends with ErrIncompleteFile rather than io.EOF.
//
// Associated files are left out, since each has the same name as the file it
// belongs to and io/fs allows one entry per name; so is a second entry with a
// name already listed. A FileInfo's Sys returns the DirEntry it describes.
func (r *Reader) FS() fs.FS { return isoFS{r: r} }

type isoFS struct{ r *Reader }

var (
	_ fs.ReadDirFS = isoFS{}
	_ fs.StatFS    = isoFS{}
)

func (f isoFS) lookup(op, name string) (DirEntry, error) {
	if !fs.ValidPath(name) {
		return DirEntry{}, &fs.PathError{Op: op, Path: name, Err: fs.ErrInvalid}
	}
	e, err := f.r.Lookup(name)
	if err != nil {
		var pe *fs.PathError
		if errors.As(err, &pe) {
			err = pe.Err
		}
		return DirEntry{}, &fs.PathError{Op: op, Path: name, Err: err}
	}
	return e, nil
}

func (f isoFS) Open(name string) (fs.File, error) {
	e, err := f.lookup("open", name)
	if err != nil {
		return nil, err
	}
	info := fileInfo{e: e, name: fsName(name)}
	if e.IsDir {
		return &dirFile{fs: f, info: info, path: name}, nil
	}
	return &file{info: info, ra: f.r.OpenFileAt(e)}, nil
}

func (f isoFS) Stat(name string) (fs.FileInfo, error) {
	e, err := f.lookup("stat", name)
	if err != nil {
		return nil, err
	}
	return fileInfo{e: e, name: fsName(name)}, nil
}

// ReadDir returns the directory's entries sorted by name, as io/fs requires.
// A damaged directory returns the entries that could be read with the
// *DirError.
func (f isoFS) ReadDir(name string) ([]fs.DirEntry, error) {
	e, err := f.lookup("readdir", name)
	if err != nil {
		return nil, err
	}
	if !e.IsDir {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: errNotDir}
	}
	entries, err := f.r.readDir(e)
	seen := make(map[string]bool, len(entries))
	out := make([]fs.DirEntry, 0, len(entries))
	for _, de := range entries {
		n := baseName(de.Path)
		if de.Associated || n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, fs.FileInfoToDirEntry(fileInfo{e: de, name: n}))
	}
	slices.SortFunc(out, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	if err != nil {
		return out, &fs.PathError{Op: "readdir", Path: name, Err: err}
	}
	return out, nil
}

// fsName is the base name io/fs gives a path: "." for the root.
func fsName(name string) string {
	if name == "." {
		return "."
	}
	return name[strings.LastIndex(name, "/")+1:]
}

type fileInfo struct {
	e    DirEntry
	name string
}

func (i fileInfo) Name() string       { return i.name }
func (i fileInfo) Size() int64        { return i.e.Size }
func (i fileInfo) Mode() fs.FileMode  { return i.e.Mode }
func (i fileInfo) ModTime() time.Time { return i.e.ModTime }
func (i fileInfo) IsDir() bool        { return i.e.IsDir }
func (i fileInfo) Sys() any           { return i.e }

// file reads through FileReaderAt directly rather than an io.SectionReader,
// which stops at the size it is given with io.EOF: a multi-extent file whose
// later extents are missing would then end as if it were whole.
type file struct {
	info fileInfo
	ra   *FileReaderAt
	off  int64
	// err latches a failed read, so a caller that retries does not reissue the
	// same failing read forever.
	err error
}

func (f *file) Stat() (fs.FileInfo, error) { return f.info, nil }
func (f *file) Close() error               { return nil }

func (f *file) Read(p []byte) (int, error) {
	if f.err != nil {
		return 0, f.err
	}
	n, err := f.ra.ReadAt(p, f.off)
	f.off += int64(n)
	if err != nil && !errors.Is(err, io.EOF) {
		f.err = err
	}
	return n, err
}

func (f *file) ReadAt(p []byte, off int64) (int, error) { return f.ra.ReadAt(p, off) }

func (f *file) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		offset += f.off
	case io.SeekEnd:
		offset += f.ra.Size()
	default:
		return 0, errors.New("iso: invalid whence")
	}
	if offset < 0 {
		return 0, errNegativeOffset
	}
	f.off = offset
	return offset, nil
}

type dirFile struct {
	fs      isoFS
	info    fileInfo
	path    string
	entries []fs.DirEntry
	read    bool
	err     error
}

func (d *dirFile) Stat() (fs.FileInfo, error) { return d.info, nil }
func (d *dirFile) Close() error               { return nil }

func (d *dirFile) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.path, Err: errors.New("is a directory")}
}

// ReadDir implements fs.ReadDirFile.
func (d *dirFile) ReadDir(n int) ([]fs.DirEntry, error) {
	if !d.read {
		d.entries, d.err = d.fs.ReadDir(d.path)
		d.read = true
	}
	if n <= 0 {
		out := d.entries
		d.entries = nil
		return out, d.err
	}
	if len(d.entries) == 0 {
		if d.err != nil {
			return nil, d.err
		}
		return nil, io.EOF
	}
	k := min(n, len(d.entries))
	out := d.entries[:k]
	d.entries = d.entries[k:]
	return out, nil
}
