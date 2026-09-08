// Package safepath turns filenames taken from a disk image into paths that are
// safe to create under an output directory.
//
// Every name a recovery tool writes comes from the damaged medium it is
// reading: FAT short and long names, Joliet, Rock Ridge NM/SL, ext2 directory
// entries and HFS catalog names are all attacker- or corruption-controlled. A
// name may hold path separators, "..", an absolute prefix, a NUL, or a name
// Windows reserves. Sanitizing each component and re-joining under a fixed root
// keeps extraction inside the output directory the operator chose.
package safepath

import (
	"fmt"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

// maxComponent bounds one path component. Most filesystems stop at 255 bytes,
// and a corrupt length field can produce far more.
const maxComponent = 255

// reservedWindowsNames are the device names Windows refuses as filenames, with
// or without an extension. They are replaced everywhere so an archive extracted
// on one platform can be moved to another.
var reservedWindowsNames = map[string]struct{}{
	"CON": {}, "PRN": {}, "AUX": {}, "NUL": {},
	"COM1": {}, "COM2": {}, "COM3": {}, "COM4": {}, "COM5": {},
	"COM6": {}, "COM7": {}, "COM8": {}, "COM9": {},
	"LPT1": {}, "LPT2": {}, "LPT3": {}, "LPT4": {}, "LPT5": {},
	"LPT6": {}, "LPT7": {}, "LPT8": {}, "LPT9": {},
}

// Component sanitizes one path component. The result is never empty, never a
// path separator, never "." or "..", and never a reserved device name.
func Component(name string) string { return component(name, 0) }

// Stem sanitizes the last component of an image-supplied path and cuts it short
// enough that reserve more bytes still fit within the component bound. The
// leading directories are returned untouched, for the caller to sanitize with
// the rest of the path; the last component comes back on its own.
//
// The reservation matters because component cuts at the bound before anything
// else, so without it the bytes a caller appends afterwards are the ones lost:
// a ".tif" left as ".ti" claims a file type nothing recorded, and a "._Name"
// cut short no longer matches the fork it belongs to. The cut comes out of the
// name the medium supplied, the only part of the result the tool did not write
// itself.
//
// The result is stable under Component, so a sink that sanitizes the whole path
// again does not cut it further.
func Stem(p string, reserve int) (dir, stem string) {
	i := strings.LastIndexAny(p, `/\`)
	if i < 0 {
		return "", component(p, reserve)
	}
	return p[:i+1], component(p[i+1:], reserve)
}

// component is the one pipeline Component and Stem share, so the order of the
// two length-related steps cannot drift between them. reserve is bytes the
// caller will add afterwards.
func component(name string, reserve int) string {
	// Both separators are stripped: a Windows-style name can reach a Unix host
	// and vice versa.
	name = strings.Map(func(r rune) rune {
		switch {
		case r == '/' || r == '\\':
			return '_'
		// On Windows a colon marks an alternate data stream, and on both
		// Windows and HFS it would be read as a drive, so it cannot be left in
		// a component. 1-Step backups record their paths with the drive letter,
		// as in `C:\DOS`.
		case r == ':':
			return '_'
		case r == 0:
			return -1
		// Control characters have no place in a filename and confuse terminals.
		case r < 0x20 || r == 0x7f:
			return '_'
		case r == utf8.RuneError:
			return '_'
		default:
			return r
		}
	}, name)

	// Cut to length before the trailing dots and spaces are trimmed. Trimming
	// first does not fix the problem: the cut can then land on a dot the name
	// already held, and the result still ends in one.
	name = truncate(name, maxComponent-reserve)

	// Trailing dots and spaces are silently dropped by Windows, which would
	// make two distinct names collide.
	name = strings.TrimRight(name, ". ")
	name = strings.TrimSpace(name)

	if name == "" || name == "." || name == ".." {
		return "_"
	}
	stem, _, _ := strings.Cut(name, ".")
	if _, bad := reservedWindowsNames[strings.ToUpper(stem)]; bad {
		// The prefix keeps the name from matching a device, and it must not
		// push the component past the bound it was just cut to, reservation
		// included.
		return "_" + truncate(name, maxComponent-reserve-1)
	}
	return name
}

// truncate cuts name to at most n bytes without leaving a partial rune behind.
func truncate(name string, n int) string {
	if len(name) <= n {
		return name
	}
	name = name[:n]
	for len(name) > 0 && !utf8.ValidString(name) {
		name = name[:len(name)-1]
	}
	return name
}

// Slash sanitizes a path from an image into a relative slash-separated path,
// which is the form an archive member name takes on every platform. Absolute
// prefixes, "." and ".." components and empty components are removed, so the
// result can never climb out of whatever it is joined to.
func Slash(p string) string {
	parts := strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' })
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		switch part {
		case "", ".", "..":
			continue
		}
		out = append(out, Component(part))
	}
	if len(out) == 0 {
		return "_"
	}
	return strings.Join(out, "/")
}

// Path is Slash in the local path syntax, for a name being written to disk.
func Path(p string) string {
	return filepath.FromSlash(Slash(p))
}

// Join returns the local path to write an image-supplied path under root. The
// result is always inside root.
func Join(root, imagePath string) (string, error) {
	clean := Path(imagePath)
	full := filepath.Join(root, clean)

	// filepath.Join already cleans the result, so this only catches a root that
	// is itself relative in a surprising way. The check is cheap, and this
	// guarantee is the purpose of the package.
	rel, err := filepath.Rel(root, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("safepath: %q would be written outside %q", imagePath, root)
	}
	return full, nil
}

// IsPrintable reports whether every rune in s is printable, which callers use
// to decide whether to show a recovered name verbatim.
func IsPrintable(s string) bool {
	for _, r := range s {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}
