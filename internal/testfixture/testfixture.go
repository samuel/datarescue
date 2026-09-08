// Package testfixture loads the gzipped disk images the tests run against.
//
// Fixtures are stored compressed: the raw images are megabytes of mostly zeros,
// and the gzipped copies are a fraction of that. Every loader skips rather than
// fails when a fixture is missing, so a missing fixture is reported the same
// way everywhere.
package testfixture

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Bytes returns the decompressed contents of the gzipped fixture at path.
// A path without a ".gz" suffix is read verbatim.
func Bytes(t *testing.T, path string) []byte {
	t.Helper()
	f, err := os.Open(path) //nolint:gosec // the path is a test constant
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			t.Skipf("fixture %s not found", path)
		}
		t.Fatalf("open fixture %s: %s", path, err)
	}
	defer f.Close() //nolint:errcheck // read-only

	var r io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			t.Fatalf("fixture %s is not valid gzip: %s", path, err)
		}
		defer gz.Close() //nolint:errcheck // read-only
		r = gz
	}
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read fixture %s: %s", path, err)
	}
	return data
}

// Reader returns a reader over the decompressed fixture.
func Reader(t *testing.T, path string) *bytes.Reader {
	t.Helper()
	return bytes.NewReader(Bytes(t, path))
}

// File writes the decompressed fixture to a temporary file and returns its
// path, for APIs that take a filename rather than a reader.
func File(t *testing.T, path string) string {
	t.Helper()
	data := Bytes(t, path)
	name := strings.TrimSuffix(filepath.Base(path), ".gz")
	out := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(out, data, 0o600); err != nil {
		t.Fatalf("materialize fixture %s: %s", path, err)
	}
	return out
}
