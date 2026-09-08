package outsink

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var modTime = time.Date(1994, time.May, 31, 6, 22, 0, 0, time.UTC)

// write puts one entry into a sink and returns the destination name.
func write(t *testing.T, s Sink, e Entry, data string) string {
	t.Helper()
	w, dest, err := s.Create(e)
	if err != nil {
		t.Fatalf("Create(%q): %v", e.Path, err)
	}
	if _, err := io.WriteString(w, data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return dest
}

func TestDirSink(t *testing.T) {
	root := t.TempDir()
	d, err := NewDir(root, false, true)
	if err != nil {
		t.Fatal(err)
	}
	dest := write(t, d, Entry{Path: `C:\DOS\ATTRIB.EXE`, ModTime: modTime}, "hello")
	if want := filepath.Join("C_", "DOS", "ATTRIB.EXE"); dest != want {
		t.Errorf("dest = %q; want %q", dest, want)
	}
	path := filepath.Join(root, dest)
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello" {
		t.Errorf("contents = %q", got)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !fi.ModTime().Equal(modTime) {
		t.Errorf("mtime = %s; want %s", fi.ModTime(), modTime)
	}

	// A directory has nowhere to keep a report, and finishing one is a no-op.
	if err := d.Manifest("report"); err != nil {
		t.Errorf("Manifest: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}

	// Without force, an existing file must not be truncated.
	if _, _, err := d.Create(Entry{Path: `C:\DOS\ATTRIB.EXE`}); err == nil {
		t.Error("Create over an existing file succeeded without force")
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "hello" {
		t.Errorf("existing file disturbed: %q %v", got, err)
	}

	forced, err := NewDir(root, true, true)
	if err != nil {
		t.Fatal(err)
	}
	write(t, forced, Entry{Path: `C:\DOS\ATTRIB.EXE`, ModTime: modTime}, "again")
	if got, _ := os.ReadFile(path); string(got) != "again" {
		t.Errorf("forced contents = %q", got)
	}
}

// TestDirSinkTimesOff checks that -times=false leaves the mtime alone.
func TestDirSinkTimesOff(t *testing.T) {
	root := t.TempDir()
	d, err := NewDir(root, false, false)
	if err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	dest := write(t, d, Entry{Path: "a.txt", ModTime: modTime}, "x")
	fi, err := os.Stat(filepath.Join(root, dest))
	if err != nil {
		t.Fatal(err)
	}
	if fi.ModTime().Before(before.Add(-time.Minute)) {
		t.Errorf("mtime = %s; want the time of the write, not the recorded %s", fi.ModTime(), modTime)
	}
}

func TestZipSink(t *testing.T) {
	var buf bytes.Buffer
	a, err := NewArchive(FormatZip, &buf, true)
	if err != nil {
		t.Fatal(err)
	}
	if dest := write(t, a, Entry{Path: `C:\DOS\ATTRIB.EXE`, ModTime: modTime}, "hello"); dest != "C_/DOS/ATTRIB.EXE" {
		t.Errorf("dest = %q; want C_/DOS/ATTRIB.EXE", dest)
	}
	write(t, a, Entry{Path: "/no/time.txt"}, "x")
	if err := a.Manifest("the report"); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	byName := make(map[string]*zip.File, len(zr.File))
	for _, f := range zr.File {
		byName[f.Name] = f
	}
	f := byName["C_/DOS/ATTRIB.EXE"]
	if f == nil {
		t.Fatalf("members = %v; want C_/DOS/ATTRIB.EXE", names(zr.File))
	}
	if f.Method != zip.Deflate {
		t.Errorf("method = %d; want deflate", f.Method)
	}
	if !f.Modified.Equal(modTime) {
		t.Errorf("modified = %s; want %s", f.Modified, modTime)
	}
	if got := readZip(t, f); got != "hello" {
		t.Errorf("contents = %q", got)
	}
	if m := byName[ManifestName]; m == nil {
		t.Errorf("members = %v; want %s", names(zr.File), ManifestName)
	} else if got := readZip(t, m); got != "the report" {
		t.Errorf("manifest = %q", got)
	}
	// A file the medium gave no time for records the epoch, which is what tar
	// and zip can both express, rather than a fabricated time.
	if u := byName["no/time.txt"]; u == nil {
		t.Errorf("members = %v; want no/time.txt", names(zr.File))
	} else if !u.Modified.Equal(unknownModTime) {
		t.Errorf("unknown modified = %s; want %s", u.Modified, unknownModTime)
	}
}

func names(fs []*zip.File) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.Name
	}
	return out
}

func readZip(t *testing.T, f *zip.File) string {
	t.Helper()
	rc, err := f.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close() //nolint:errcheck // read-only
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// tarMembers reads a gzipped tar back into name → contents plus the headers.
func tarMembers(t *testing.T, b []byte) (map[string]string, map[string]*tar.Header) {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	data := make(map[string]string)
	hdrs := make(map[string]*tar.Header)
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		data[h.Name] = string(body)
		hdrs[h.Name] = h
	}
	return data, hdrs
}

func TestTGZSink(t *testing.T) {
	var buf bytes.Buffer
	a, err := NewArchive(FormatTGZ, &buf, true)
	if err != nil {
		t.Fatal(err)
	}
	write(t, a, Entry{Path: `C:\DOS\ATTRIB.EXE`, ModTime: modTime}, "hello")
	if err := a.Manifest("the report"); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	data, hdrs := tarMembers(t, buf.Bytes())
	if data["C_/DOS/ATTRIB.EXE"] != "hello" {
		t.Errorf("contents = %q", data["C_/DOS/ATTRIB.EXE"])
	}
	if data[ManifestName] != "the report" {
		t.Errorf("manifest = %q", data[ManifestName])
	}
	h := hdrs["C_/DOS/ATTRIB.EXE"]
	if h.Size != 5 {
		t.Errorf("size = %d; want 5", h.Size)
	}
	if !h.ModTime.Equal(modTime) {
		t.Errorf("modtime = %s; want %s", h.ModTime, modTime)
	}
}

// shortReader delivers a prefix and then fails, the way a damaged medium does.
type shortReader struct {
	data string
	n    int
}

func (r *shortReader) Read(p []byte) (int, error) {
	if r.n >= len(r.data) {
		return 0, errors.New("unrecovered sector")
	}
	n := copy(p, r.data[r.n:])
	r.n += n
	return n, nil
}

// TestTGZSizeIsRecovered checks the header records what was recovered, not what
// the image declared. Padding a short member out to its declared size would
// make the archive claim bytes the medium never gave up.
func TestTGZSizeIsRecovered(t *testing.T) {
	var buf bytes.Buffer
	a, err := NewArchive(FormatTGZ, &buf, true)
	if err != nil {
		t.Fatal(err)
	}
	w, _, err := a.Create(Entry{Path: "short.bin", ModTime: modTime})
	if err != nil {
		t.Fatal(err)
	}
	// The caller keeps the prefix and closes, as every extract command does.
	n, copyErr := io.Copy(w, &shortReader{data: "abcdef"})
	if copyErr == nil {
		t.Fatal("copy from a failing reader succeeded")
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	data, hdrs := tarMembers(t, buf.Bytes())
	if hdrs["short.bin"].Size != n {
		t.Errorf("size = %d; want the %d bytes recovered", hdrs["short.bin"].Size, n)
	}
	if data["short.bin"] != "abcdef" {
		t.Errorf("contents = %q; want the recovered prefix", data["short.bin"])
	}
}

// TestTGZSpill drives the temporary-file path by lowering the threshold.
func TestTGZSpill(t *testing.T) {
	t.Cleanup(func(prev int64) func() {
		return func() { spillThreshold = prev }
	}(spillThreshold))
	spillThreshold = 8

	body := strings.Repeat("datarescue", 1000)
	var buf bytes.Buffer
	a, err := NewArchive(FormatTGZ, &buf, true)
	if err != nil {
		t.Fatal(err)
	}
	write(t, a, Entry{Path: "big.bin", ModTime: modTime}, body)
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	data, hdrs := tarMembers(t, buf.Bytes())
	if hdrs["big.bin"].Size != int64(len(body)) {
		t.Errorf("size = %d; want %d", hdrs["big.bin"].Size, len(body))
	}
	if data["big.bin"] != body {
		t.Error("spilled contents do not match")
	}
}

// TestDuplicateMember checks that two image paths sanitizing to one member name
// are refused rather than written twice: an archive member cannot be
// overwritten in place the way a file can.
func TestDuplicateMember(t *testing.T) {
	for _, format := range []string{FormatZip, FormatTGZ} {
		t.Run(format, func(t *testing.T) {
			var buf bytes.Buffer
			a, err := NewArchive(format, &buf, true)
			if err != nil {
				t.Fatal(err)
			}
			write(t, a, Entry{Path: "a/b.txt"}, "x")
			if _, _, err := a.Create(Entry{Path: `a\b.txt`}); !errors.Is(err, ErrDuplicate) {
				t.Errorf("Create of a duplicate = %v; want ErrDuplicate", err)
			}
			if err := a.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestManifestNameCollision checks a recovered file cannot displace the report.
func TestManifestNameCollision(t *testing.T) {
	var buf bytes.Buffer
	a, err := NewArchive(FormatZip, &buf, true)
	if err != nil {
		t.Fatal(err)
	}
	write(t, a, Entry{Path: ManifestName}, "not the report")
	if err := a.Manifest("the report"); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, f := range zr.File {
		if f.Name == ManifestName {
			if got := readZip(t, f); got != "not the report" {
				t.Errorf("recovered file was displaced: %q", got)
			}
			continue
		}
		if readZip(t, f) == "the report" {
			found = true
		}
	}
	if !found {
		t.Errorf("members = %v; want the report under another name", names(zr.File))
	}
}

func TestFlagsOpen(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name    string
		flags   Flags
		wantErr bool
	}{
		{"dir", Flags{Out: dir, Format: FormatDir}, false},
		{"zip", Flags{Out: filepath.Join(dir, "a.zip"), Format: FormatZip}, false},
		{"tgz", Flags{Out: filepath.Join(dir, "a.tgz"), Format: FormatTGZ}, false},
		{"tar.gz spelling", Flags{Out: filepath.Join(dir, "b.tgz"), Format: "tar.gz"}, false},
		{"unknown format", Flags{Out: dir, Format: "rar"}, true},
		{"archive without -o", Flags{Out: ".", Format: FormatZip}, true},
		{"archive with empty -o", Flags{Out: "", Format: FormatTGZ}, true},
		{"stdout for a directory", Flags{Out: "-", Format: FormatDir}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, err := tc.flags.Open()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Open = %v; wantErr %v", err, tc.wantErr)
			}
			if err == nil {
				if err := s.Close(); err != nil {
					t.Error(err)
				}
			}
		})
	}
}

// TestArchiveFileForce checks -f governs the archive file itself.
func TestArchiveFileForce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.zip")
	a, err := NewArchiveFile(FormatZip, path, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := NewArchiveFile(FormatZip, path, false, true); err == nil {
		t.Error("re-creating an archive succeeded without force")
	}
	b, err := NewArchiveFile(FormatZip, path, true, true)
	if err != nil {
		t.Fatalf("force: %v", err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestLog(t *testing.T) {
	var stderr bytes.Buffer
	lg := &Log{Prefix: "fattool extract", W: &stderr}
	lg.Printf("%s: %s\n", "A.TXT", "bad sector")
	lg.Plainf("A.TXT -> A.TXT (3 bytes)")

	want := "fattool extract: A.TXT: bad sector\nA.TXT -> A.TXT (3 bytes)\n"
	if stderr.String() != want {
		t.Errorf("stderr = %q; want %q", stderr.String(), want)
	}
	if lg.Text() != want {
		t.Errorf("Text = %q; want %q", lg.Text(), want)
	}
}

func TestManifestText(t *testing.T) {
	got := ManifestText("1steptool", []string{"d1.1-Step", "d2.1-Step"}, "")
	for _, want := range []string{"1steptool", "source: d1.1-Step", "source: d2.1-Step", "nothing to report"} {
		if !strings.Contains(got, want) {
			t.Errorf("manifest %q missing %q", got, want)
		}
	}
	if got := ManifestText("fattool", nil, "A.TXT: bad sector\n"); !strings.Contains(got, "A.TXT: bad sector") {
		t.Errorf("manifest dropped the report: %q", got)
	}
}

// exhaustibleWriter takes n bytes and then refuses everything, the way an
// output disk that fills up does.
type exhaustibleWriter struct{ n int }

var errNoSpace = errors.New("no space left on device")

func (w *exhaustibleWriter) Write(p []byte) (int, error) {
	if w.n <= 0 {
		return 0, errNoSpace
	}
	if len(p) > w.n {
		n := w.n
		w.n = 0
		return n, errNoSpace
	}
	w.n -= len(p)
	return len(p), nil
}

// A member's name is claimed only once the member has started. Claiming it
// first turns a failed start into a permanent refusal: the retry is reported as
// a duplicate of a member the archive does not hold, which sends an operator
// looking for a name collision that is not there.
func TestArchiveDoesNotClaimAFailedMemberName(t *testing.T) {
	a, err := NewArchive(FormatZip, &exhaustibleWriter{n: 64}, true)
	if err != nil {
		t.Fatal(err)
	}
	// Fill the destination while writing the first member, so starting the
	// second one — which flushes the first — is what fails.
	w, _, err := a.Create(Entry{Path: "first.bin"})
	if err != nil {
		t.Fatal(err)
	}
	// Pseudorandom, so deflate cannot shrink it back inside the zip writer's
	// own buffer and the destination is really reached.
	body := make([]byte, 256<<10)
	x := uint32(1)
	for i := range body {
		x = x*1664525 + 1013904223
		body[i] = byte(x >> 24)
	}
	_, _ = w.Write(body)
	_ = w.Close()

	if _, _, err := a.Create(Entry{Path: "second.bin"}); err == nil {
		t.Fatal("creating a member on a full destination succeeded")
	}
	_, _, err = a.Create(Entry{Path: "second.bin"})
	if err == nil {
		t.Fatal("the retry succeeded; the destination is still full")
	}
	if errors.Is(err, ErrDuplicate) {
		t.Errorf("the retry = %v; the name was burned by a member that never started", err)
	}
}
