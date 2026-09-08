package qic

import (
	"strings"
	"unicode/utf16"
)

const (
	dosSeparator = `\`

	// maxDepth bounds a tree rebuilt from a damaged catalog.
	maxDepth = 64
)

// decodeUTF16 decodes a name, which is stored as UTF-16 in little-endian
// order and is not terminated. An odd length is damage: the trailing byte is
// dropped rather than paired with whatever follows it.
func decodeUTF16(b []byte) string {
	units := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		units = append(units, uint16(b[i])|uint16(b[i+1])<<8)
	}
	return string(utf16.Decode(units))
}

// decodePath splits a data region record's path into its components. The
// encoding puts a NUL unit before the first component and two between each
// pair after it, so `\0SAM\0\0CM` is SAM\CM; splitting on NUL and dropping
// what comes out empty handles both without depending on the count.
func decodePath(b []byte) []string {
	var out []string
	for part := range strings.SplitSeq(decodeUTF16(b), "\x00") {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

// encodedLen is how many bytes a name takes on the medium, which is not its
// length in Go: the format stores UTF-16 and Go strings are UTF-8.
func encodedLen(s string) int { return len(utf16.Encode([]rune(s))) * 2 }

// pathLen is the encoded byte length of a path with the given components. The
// catalog carries this length for every record even though it carries no path
// bytes, and it is the parent's path that it describes -- so it checks a tree
// rebuilt from the DIRLAST markers without needing the data region at all.
func pathLen(components []string) int {
	if len(components) == 0 {
		return 0
	}
	n := 2 // the leading separator
	for i, c := range components {
		if i > 0 {
			n += 4 // two separator units between components
		}
		n += encodedLen(c)
	}
	return n
}

// joinPath builds the path a file is reported under, which keeps the drive
// letter and the backslashes the archive was written with. Turning it into
// something safe to create belongs to internal/safepath, at the point a file
// is written.
func joinPath(drive string, components []string, name string) string {
	var b strings.Builder
	b.WriteString(drive)
	for _, c := range components {
		b.WriteString(dosSeparator)
		b.WriteString(c)
	}
	if name != "" {
		b.WriteString(dosSeparator)
		b.WriteString(name)
	}
	return b.String()
}

// normalizePath puts a caller's path in the form Find compares against. DOS
// paths are case insensitive and either separator is accepted, since a caller
// on a Unix shell will reach for the forward slash.
func normalizePath(p string) string {
	p = strings.ReplaceAll(p, "/", dosSeparator)
	return strings.Trim(strings.ToUpper(p), dosSeparator)
}
