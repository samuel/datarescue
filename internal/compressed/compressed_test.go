package compressed

import (
	"bytes"
	"compress/gzip"
	"testing"
)

func TestFormat(t *testing.T) {
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	if _, err := w.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		in   []byte
		want string
	}{
		{gz.Bytes(), "gzip"},
		{[]byte("BZh91AY&SY"), "bzip2"},
		{[]byte{0xfd, '7', 'z', 'X', 'Z', 0x00, 0x00}, "xz"},
		{[]byte{0x28, 0xb5, 0x2f, 0xfd, 0x00}, "zstd"},
		{[]byte{'7', 'z', 0xbc, 0xaf, 0x27, 0x1c, 0x00}, "7-Zip"},
		{[]byte("PK\x03\x04rest"), "zip"},
		{[]byte{0xeb, 0x3c, 0x90}, ""},
		{[]byte{0x1f}, ""},
		{nil, ""},
	}
	for _, c := range cases {
		if got := Format(c.in); got != c.want {
			t.Errorf("Format(% x) = %q, want %q", c.in, got, c.want)
		}
	}
	if Hint([]byte{0xeb}) != "" {
		t.Error("Hint on an uncompressed file is not empty")
	}
}
