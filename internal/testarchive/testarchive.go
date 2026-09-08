// Package testarchive reads back an archive an extract command wrote, so each
// tool's tests can assert on the members without carrying their own zip and
// tar plumbing.
package testarchive

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"testing"
	"time"
)

// Member is one archive member: what it holds and the time it records.
type Member struct {
	Data    string
	ModTime time.Time
}

// Zip reads a zip file into its members, keyed by member name.
func Zip(t testing.TB, path string) map[string]Member {
	t.Helper()
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatalf("opening %s: %v", path, err)
	}
	defer zr.Close() //nolint:errcheck // read-only
	out := make(map[string]Member, len(zr.File))
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("opening member %s: %v", f.Name, err)
		}
		data, err := io.ReadAll(rc)
		_ = rc.Close() // the bytes are already read
		if err != nil {
			t.Fatalf("reading member %s: %v", f.Name, err)
		}
		out[f.Name] = Member{Data: string(data), ModTime: f.Modified}
	}
	return out
}

// TGZ reads a gzipped tar file into its members, keyed by member name.
func TGZ(t testing.TB, path string) map[string]Member {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("opening %s: %v", path, err)
	}
	defer f.Close() //nolint:errcheck // read-only
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gunzipping %s: %v", path, err)
	}
	out := make(map[string]Member)
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("reading member %s: %v", h.Name, err)
		}
		out[h.Name] = Member{Data: string(data), ModTime: h.ModTime}
	}
}

// Names lists the member names, for a failure message.
func Names(m map[string]Member) []string {
	out := make([]string, 0, len(m))
	for name := range m {
		out = append(out, name)
	}
	return out
}
