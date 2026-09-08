package extfs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"
)

// FileType is the file-type byte in a rev1+ directory entry.
type FileType uint8

const (
	FileTypeUnknown  FileType = 0
	FileTypeRegular  FileType = 1
	FileTypeDir      FileType = 2
	FileTypeCharDev  FileType = 3
	FileTypeBlockDev FileType = 4
	FileTypeFIFO     FileType = 5
	FileTypeSocket   FileType = 6
	FileTypeSymlink  FileType = 7
)

// DirEntry is a parsed directory entry.
type DirEntry struct {
	Inode    uint32
	Name     string
	FileType FileType // valid only when FeatureIncompatFiletype is set
	Deleted  bool     // true when inode == 0
}

// WalkFunc is the callback type for Walk. Return a non-nil error to stop.
type WalkFunc func(path string, entry *DirEntry, ino *Inode) error

// dirEntryHeaderSize is the fixed part of a directory entry (before the name).
const dirEntryHeaderSize = 8

// ReadDir returns the entries of directory dirIno, including those with a zero
// inode number when includeDeleted is set.
func (fs *FS) ReadDir(dirIno *Inode, includeDeleted bool) ([]*DirEntry, error) {
	if !dirIno.IsDir() {
		return nil, fmt.Errorf("extfs: inode is not a directory (mode 0x%04X)", dirIno.Mode)
	}
	rd, err := fs.ReadInodeData(dirIno)
	if err != nil {
		return nil, err
	}
	// Keep what came back. Entries never span a block, so the blocks that did
	// read are whole directory blocks and their entries are real. Discarding
	// them would lose every file under the directory for one bad sector;
	// keeping them loses only the entries in that sector.
	data, readErr := io.ReadAll(rd)
	hasFiletype := fs.sb.FeatureIncompat&FeatureIncompatFiletype != 0
	entries, parseErr := parseDirData(data, fs.blkSize, hasFiletype, includeDeleted)
	if readErr != nil {
		readErr = &DirReadError{Recovered: int64(len(data)), Err: readErr}
	}
	return entries, errors.Join(readErr, parseErr)
}

// DirReadError reports that a directory's data stopped coming back. The entries
// parsed out of what did arrive are returned alongside it, so a caller can tell
// a complete directory from a partially recovered one.
type DirReadError struct {
	// Recovered is how many bytes of the directory's data came back before the
	// read failed.
	Recovered int64
	Err       error
}

func (e *DirReadError) Error() string {
	return fmt.Sprintf("extfs: read directory data: %s (%d bytes recovered)", e.Err, e.Recovered)
}

func (e *DirReadError) Unwrap() error { return e.Err }

// DirParseError reports directory blocks whose entry chain was malformed. The
// entries recovered from the intact blocks are returned alongside it, so a
// caller can tell a complete directory from a partially recovered one.
type DirParseError struct {
	// BadBlocks holds the byte offset within the directory data of each block
	// whose entry chain did not parse.
	BadBlocks []int
}

func (e *DirParseError) Error() string {
	return fmt.Sprintf("extfs: %d directory block(s) with a malformed entry chain at offsets %v",
		len(e.BadBlocks), e.BadBlocks)
}

// parseDirData parses directory entries one block at a time. Entries never span
// a block in ext2, so a corrupt rec_len damages only its own block: parsing
// resynchronizes at the next block boundary instead of desynchronizing the rest
// of the directory.
func parseDirData(data []byte, blkSize int, hasFiletype, includeDeleted bool) ([]*DirEntry, error) {
	if blkSize <= 0 {
		blkSize = len(data)
	}
	var entries []*DirEntry
	var badBlocks []int
	for base := 0; base < len(data); base += blkSize {
		end := min(base+blkSize, len(data))
		got, ok := parseDirBlock(data[base:end], hasFiletype, includeDeleted)
		entries = append(entries, got...)
		if !ok {
			badBlocks = append(badBlocks, base)
		}
	}
	if len(badBlocks) > 0 {
		return entries, &DirParseError{BadBlocks: badBlocks}
	}
	return entries, nil
}

// parseDirBlock parses one directory block, returning its entries and whether
// the entry chain covered the block exactly. A chain that stops short, oversteps
// the block, or holds a malformed record means the block is damaged.
func parseDirBlock(data []byte, hasFiletype, includeDeleted bool) ([]*DirEntry, bool) {
	var entries []*DirEntry
	var off int
	for off+dirEntryHeaderSize <= len(data) {
		inode := binary.LittleEndian.Uint32(data[off : off+4])
		recLen := int(binary.LittleEndian.Uint16(data[off+4 : off+6]))
		nameLen := int(data[off+6])
		var ft FileType
		if hasFiletype {
			ft = FileType(data[off+7])
		}
		if recLen < dirEntryHeaderSize || off+recLen > len(data) {
			// rec_len cannot be followed: the rest of this block is unparseable.
			return entries, false
		}
		// The name must fit inside its own record, or a corrupt name_len splices
		// bytes out of the entries that follow.
		if nameLen > recLen-dirEntryHeaderSize {
			off += recLen
			continue
		}
		if nameLen > 0 {
			name := string(data[off+dirEntryHeaderSize : off+dirEntryHeaderSize+nameLen])
			deleted := inode == 0
			if !deleted || includeDeleted {
				entries = append(entries, &DirEntry{
					Inode:    inode,
					Name:     name,
					FileType: ft,
					Deleted:  deleted,
				})
			}
		}
		off += recLen
	}
	// A well-formed chain lands exactly on the end of the block.
	return entries, off == len(data)
}

// Walk calls fn for each entry in the live directory tree, before descending
// into it. Entries named "." and ".." are skipped. A directory or inode that
// cannot be read is recorded in the returned issues and the rest of the tree is
// still walked; the error is non-nil only when fn asked to stop.
func (fs *FS) Walk(fn WalkFunc) ([]WalkIssue, error) {
	rootIno, err := fs.ReadInode(RootInodeNum)
	if err != nil {
		return []WalkIssue{{Path: "/", Err: fmt.Errorf("extfs: read root inode: %w", err)}}, nil
	}
	w := &walker{fs: fs, fn: fn, visited: make(map[uint32]struct{})}
	stopErr := w.walk("", rootIno)
	return w.issues, stopErr
}

// WalkIssue records a path that could not be read or traversed. The rest of the
// tree is still walked.
type WalkIssue struct {
	Path string
	Err  error
}

func (w WalkIssue) String() string {
	path := w.Path
	if path == "" {
		path = "/"
	}
	return fmt.Sprintf("%s: %s", path, w.Err)
}

type walker struct {
	fs      *FS
	fn      WalkFunc
	visited map[uint32]struct{}
	issues  []WalkIssue
}

func (w *walker) walk(path string, dirIno *Inode) error {
	// A partially parsed directory still yields the entries from its intact
	// blocks, so record the damage and carry on with what was recovered.
	entries, err := w.fs.ReadDir(dirIno, false)
	if err != nil {
		w.issues = append(w.issues, WalkIssue{Path: path, Err: err})
		if len(entries) == 0 {
			return nil
		}
	}
	for _, e := range entries {
		if e.Name == "." || e.Name == ".." {
			continue
		}
		entryPath := e.Name
		if path != "" {
			entryPath = path + "/" + e.Name
		}
		ino, err := w.fs.ReadInode(e.Inode)
		if err != nil {
			w.issues = append(w.issues, WalkIssue{
				Path: entryPath,
				Err:  fmt.Errorf("extfs: read inode %d: %w", e.Inode, err),
			})
			continue
		}
		if err := w.fn(entryPath, e, ino); err != nil {
			return err
		}
		if ino.IsDir() {
			if _, seen := w.visited[e.Inode]; seen {
				continue // directory loop guard
			}
			w.visited[e.Inode] = struct{}{}
			if err := w.walk(entryPath, ino); err != nil {
				return err
			}
		}
	}
	return nil
}

// Open walks the directory tree and returns a streaming reader for the file at
// path. It returns the concrete type so a caller that can record the holes can
// call Fill on it.
func (fs *FS) Open(path string) (*InodeReader, error) {
	rootIno, err := fs.ReadInode(RootInodeNum)
	if err != nil {
		return nil, fmt.Errorf("extfs: read root inode: %w", err)
	}
	trimmed := strings.Trim(path, "/")
	if trimmed == "" {
		return nil, errors.New("extfs: empty path")
	}
	return fs.openFrom(rootIno, strings.Split(trimmed, "/"))
}

func (fs *FS) openFrom(dirIno *Inode, parts []string) (*InodeReader, error) {
	// Search what was recovered; only report the parse failure if the name is
	// not among the entries that survived.
	entries, readErr := fs.ReadDir(dirIno, false)
	if readErr != nil && len(entries) == 0 {
		return nil, readErr
	}
	for _, e := range entries {
		if e.Name != parts[0] {
			continue
		}
		ino, err := fs.ReadInode(e.Inode)
		if err != nil {
			return nil, fmt.Errorf("extfs: read inode %d: %w", e.Inode, err)
		}
		if len(parts) == 1 {
			if !ino.IsRegular() {
				return nil, fmt.Errorf("extfs: %q is not a regular file", parts[0])
			}
			return fs.ReadInodeData(ino)
		}
		if !ino.IsDir() {
			return nil, fmt.Errorf("extfs: %q is not a directory", parts[0])
		}
		return fs.openFrom(ino, parts[1:])
	}
	if readErr != nil {
		return nil, fmt.Errorf("extfs: %q not found in the recovered part of the directory: %w", parts[0], readErr)
	}
	return nil, fmt.Errorf("extfs: %q not found", parts[0])
}
