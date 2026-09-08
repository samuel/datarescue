package iso

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"
)

// DirError reports the damage found while reading one directory. The entries
// that could be read are returned alongside it.
type DirError struct {
	Path   string
	Issues []string
}

func (e *DirError) Error() string {
	return fmt.Sprintf("iso: directory %s: %s", e.Path, strings.Join(e.Issues, "; "))
}

// ReadDir returns the entries of the directory at path, in directory order,
// reading only the directories along the path. Entries are as Walk reports
// them, with full paths. When the directory itself is damaged, the entries that
// could be read are returned with a *DirError.
func (r *Reader) ReadDir(path string) ([]DirEntry, error) {
	e, err := r.Lookup(path)
	if err != nil {
		return nil, err
	}
	if !e.IsDir {
		return nil, &fs.PathError{Op: "readdir", Path: path, Err: errNotDir}
	}
	return r.readDir(e)
}

var errNotDir = errors.New("iso: not a directory")

// readDir lists one directory without descending into it.
func (r *Reader) readDir(dir DirEntry) ([]DirEntry, error) {
	var out []DirEntry
	w := r.newWalker(func(e DirEntry) error {
		out = append(out, e)
		return nil
	}, false)
	if err := w.walkDir(dir.StartLBA, dir.Size, dir.Path, 1); err != nil {
		return out, err
	}
	if len(w.issues) > 0 {
		return out, &DirError{Path: dir.Path, Issues: w.issues}
	}
	return out, nil
}

// Lookup returns the entry at path, reading only the directories along it.
// path is slash-separated; a leading or trailing slash, an empty path and "."
// all mean what they would to io/fs. Names match exactly, as Walk reports them.
// An associated file is never the match, since it shares its name with the file
// it belongs to.
//
// A path the volume does not hold returns an error wrapping fs.ErrNotExist.
// When a directory on the way is damaged and the name is not among the entries
// that could be read, the *DirError is returned instead: the path may be in
// the part that was lost.
func (r *Reader) Lookup(path string) (DirEntry, error) {
	cur := r.Root()
	for _, part := range splitPath(path) {
		if !cur.IsDir {
			return DirEntry{}, &fs.PathError{Op: "lookup", Path: path, Err: fs.ErrNotExist}
		}
		entries, err := r.readDir(cur)
		var found bool
		for _, e := range entries {
			if !e.Associated && baseName(e.Path) == part {
				cur, found = e, true
				break
			}
		}
		if !found {
			if err != nil {
				return DirEntry{}, err
			}
			return DirEntry{}, &fs.PathError{Op: "lookup", Path: path, Err: fs.ErrNotExist}
		}
	}
	return cur, nil
}

// splitPath splits path into its components, dropping empty and "."
// components.
func splitPath(path string) []string {
	var out []string
	for part := range strings.SplitSeq(path, "/") {
		if part != "" && part != "." {
			out = append(out, part)
		}
	}
	return out
}

// baseName is the last component of an entry's path.
func baseName(p string) string {
	p = strings.TrimSuffix(p, "/")
	return p[strings.LastIndex(p, "/")+1:]
}
