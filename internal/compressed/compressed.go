// Package compressed recognizes a compressed file by its leading bytes, so a
// tool handed one can say so instead of reporting that it found nothing it
// knows. Images are routinely stored compressed, and every parser here reads
// the uncompressed bytes.
package compressed

import "bytes"

var signatures = []struct {
	magic []byte
	name  string
}{
	{[]byte{0x1f, 0x8b}, "gzip"},
	{[]byte("BZh"), "bzip2"},
	{[]byte{0xfd, '7', 'z', 'X', 'Z', 0x00}, "xz"},
	{[]byte{0x28, 0xb5, 0x2f, 0xfd}, "zstd"},
	{[]byte{'7', 'z', 0xbc, 0xaf, 0x27, 0x1c}, "7-Zip"},
	{[]byte("PK\x03\x04"), "zip"},
}

// Format returns the name of the compression format b begins with, or "" when
// it is not one this package knows.
func Format(b []byte) string {
	for _, s := range signatures {
		if bytes.HasPrefix(b, s.magic) {
			return s.name
		}
	}
	return ""
}

// Hint is a clause to append to an error about b, saying the file is
// compressed and must be decompressed first, or "" when it is not.
func Hint(b []byte) string {
	if f := Format(b); f != "" {
		return " (the file is " + f + "-compressed; decompress it first)"
	}
	return ""
}
