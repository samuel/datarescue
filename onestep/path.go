package onestep

import (
	"errors"
	"fmt"
	"strings"
)

// maxDepth bounds a directory chain. A backup of a Win9x drive cannot nest
// deeper than this, and the cap keeps a chain that has been corrupted into a
// long line from being walked forever.
const maxDepth = 64

const dosSeparator = `\`

// buildPaths fills File.Path from the directory table. The directory table's
// parent chain is the authoritative source: the selection table looks like it
// should serve, but a backup of 123 files can carry two selection rows, so it
// describes what the operator picked rather than where anything lives.
func (c *catalog) buildPaths(diag func(DiagnosticKind, string, ...any)) {
	dirs := make(map[int]dirRow, len(c.dirs))
	for _, d := range c.dirs {
		dirs[d.serial] = d
	}
	letters := make(map[int]string, len(c.disks))
	for _, d := range c.disks {
		letters[d.Serial] = d.Letter
	}

	for i := range c.files {
		f := &c.files[i]
		parts, drive, err := dirPath(dirs, letters, f.dirSer)
		if err != nil {
			diag(DiagPathUnresolved, "%s (file %d): %s", f.Name, f.Serial, err)
		}
		f.Path = joinDOS(drive, parts, f.Name)
	}
}

// dirPath walks from a directory row to its root, returning the names from the
// root down and the drive letter to put in front of them. A partial answer is
// returned alongside the error, since a path missing its top is still better
// than no path at all.
func dirPath(dirs map[int]dirRow, letters map[int]string, start int) (parts []string, drive string, err error) {
	if start == 0 {
		return nil, "", errors.New("no directory recorded")
	}
	seen := make(map[int]bool)
	serial := start
	for range maxDepth {
		d, ok := dirs[serial]
		if !ok {
			return reverse(parts), drive, fmt.Errorf("directory %d is not in the catalog", serial)
		}
		if seen[serial] {
			return reverse(parts), drive, errors.New("directory chain loops")
		}
		seen[serial] = true
		if drive == "" {
			drive = letters[d.diskSer]
		}
		// The root of a drive is the row with no parent, and its name is
		// blank: the drive letter takes its place.
		if d.parent == 0 {
			if d.nameSeen {
				parts = append(parts, d.name)
			}
			return reverse(parts), drive, nil
		}
		if d.nameSeen {
			parts = append(parts, d.name)
		}
		serial = d.parent
	}
	return reverse(parts), drive, errors.New("directory chain deeper than the limit")
}

func reverse(s []string) []string {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
	return s
}

// joinDOS assembles the path in the form the backup recorded, as in
// `C:\DOS\ATTRIB.EXE`.
func joinDOS(drive string, parts []string, name string) string {
	var b strings.Builder
	b.WriteString(drive)
	for _, p := range parts {
		if p == "" {
			continue
		}
		b.WriteString(dosSeparator)
		b.WriteString(p)
	}
	if name != "" {
		b.WriteString(dosSeparator)
		b.WriteString(name)
	}
	return b.String()
}
