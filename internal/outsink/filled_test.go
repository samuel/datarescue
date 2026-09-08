package outsink

import (
	"archive/zip"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"samuelks.com/datarescue/ddrescue"
)

var errLost = errors.New("never recovered")

// filling stands in for a parser's reader: it delivers size bytes with a hole
// zero-filled in the middle, and records the hole where it was told to.
func filling(size, holeOff, holeLen int64) func(*ddrescue.GapLog) (io.Reader, error) {
	return func(l *ddrescue.GapLog) (io.Reader, error) {
		l.Limit(size)
		data := make([]byte, size)
		for i := range data {
			data[i] = 'x'
		}
		if holeLen > 0 {
			clear(data[holeOff : holeOff+holeLen])
			l.Add(holeOff, holeLen, errLost)
		}
		return bytes.NewReader(data), nil
	}
}

// A damaged file lands beside its map, and an undamaged one does not get one:
// a sidecar is what says the file has holes in it.
func TestWriteFilledDir(t *testing.T) {
	root := t.TempDir()
	d, err := NewDir(root, false, true)
	if err != nil {
		t.Fatal(err)
	}

	w, err := WriteFilled(d, Entry{Path: "sub/BIG.BIN", ModTime: modTime}, filling(2048, 512, 512))
	if err != nil {
		t.Fatalf("WriteFilled: %v", err)
	}
	if w.Bytes != 2048 || w.GapBytes != 512 || !w.Damaged() {
		t.Errorf("Written = %+v; want 2048 bytes with 512 lost", w.Filled)
	}
	if w.MapName != filepath.Join("sub", "BIG.BIN.map") {
		t.Errorf("MapName = %q; want the sidecar beside the file", w.MapName)
	}
	if msg := w.GapMessage(); !strings.Contains(msg, "512 bytes") || !strings.Contains(msg, w.MapName) {
		t.Errorf("GapMessage = %q; want the loss and the map named", msg)
	}

	got, err := os.ReadFile(filepath.Join(root, "sub", "BIG.BIN"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2048 {
		t.Errorf("the file is %d bytes; want its full 2048", len(got))
	}
	blocks, err := ddrescue.ParseMap(filepath.Join(root, "sub", "BIG.BIN.map"))
	if err != nil {
		t.Fatalf("the map beside the file: %v", err)
	}
	bad := ddrescue.Unrecovered(blocks, 0, 2048)
	if len(bad) != 1 || bad[0].Offset != 512 || bad[0].Size != 512 {
		t.Errorf("the map records %v; want one range of 512 bytes at 512", bad)
	}

	if _, err := WriteFilled(d, Entry{Path: "SMALL.TXT"}, filling(20, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "SMALL.TXT.map")); err == nil {
		t.Error("an undamaged file was given a map")
	}
}

// Inside an archive the map is a member of its own, so a recovery that leaves
// the machine as one file still says which of its bytes were never read.
func TestWriteFilledArchive(t *testing.T) {
	var buf bytes.Buffer
	a, err := NewArchive(FormatZip, &buf, true)
	if err != nil {
		t.Fatal(err)
	}
	w, err := WriteFilled(a, Entry{Path: "BIG.BIN", ModTime: modTime}, filling(1000, 100, 50))
	if err != nil {
		t.Fatal(err)
	}
	if w.MapName != "BIG.BIN.map" {
		t.Errorf("MapName = %q; want BIG.BIN.map", w.MapName)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	members := zipMembers(t, buf.Bytes())
	if len(members["BIG.BIN"]) != 1000 {
		t.Errorf("the member is %d bytes; want its full 1000", len(members["BIG.BIN"]))
	}
	if !strings.Contains(members["BIG.BIN.map"], "0x00000064  0x00000032  -") {
		t.Errorf("the map member does not record the hole:\n%s", members["BIG.BIN.map"])
	}
}

// The data is written whatever happens to the map, but a map that cannot be
// written is the returned error, since unrecorded zeros are worse than a short
// file. In practice that happens when the map's name is already taken.
func TestWriteFilledReportsAnUnwritableMap(t *testing.T) {
	var buf bytes.Buffer
	a, err := NewArchive(FormatZip, &buf, true)
	if err != nil {
		t.Fatal(err)
	}
	// Take the map's name first, so creating it is refused as a duplicate.
	write(t, a, Entry{Path: "BIG.BIN.map"}, "not the map")

	w, err := WriteFilled(a, Entry{Path: "BIG.BIN"}, filling(100, 0, 10))
	if !errors.Is(err, ErrDuplicate) {
		t.Errorf("WriteFilled = %v; want the map's failure", err)
	}
	if w.Bytes != 100 {
		t.Errorf("the recovered data was not written: %d bytes", w.Bytes)
	}
	// A member cannot be taken back, so the archive says so rather than
	// pretending the zeros are accounted for.
	if !strings.Contains(err.Error(), "cannot be removed") {
		t.Errorf("WriteFilled = %v; want it to name the member it could not take back", err)
	}
}

// The same case in a directory, where the file can be taken back — and is.
// A full-length file with unrecorded zeros is worse than no file at all, so
// nothing must be left behind.
func TestWriteFilledDiscardsAFileWithNoMap(t *testing.T) {
	root := t.TempDir()
	d, err := NewDir(root, false, true)
	if err != nil {
		t.Fatal(err)
	}
	// Stand something in the map's way that -f would be needed to replace.
	if err := os.WriteFile(filepath.Join(root, "BIG.BIN.map"), []byte("in the way\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := WriteFilled(d, Entry{Path: "BIG.BIN"}, filling(2048, 512, 512))
	if err == nil {
		t.Fatal("WriteFilled = nil; want the map's failure")
	}
	if !w.Unrecorded {
		t.Error("Unrecorded is false; the zeros went unrecorded")
	}
	if _, err := os.Stat(filepath.Join(root, "BIG.BIN")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the zero-filled file is still there (%v); nothing records its holes", err)
	}
}

// refusingSink takes the entry and then refuses every byte of it, the way a
// full output disk does.
type refusingSink struct{ w refusingWriter }

func (s refusingSink) Create(Entry) (io.WriteCloser, string, error) {
	return s.w, "dest", nil
}
func (refusingSink) Discard(Entry) error   { return nil }
func (refusingSink) Manifest(string) error { return nil }
func (refusingSink) Close() error          { return nil }

type refusingWriter struct{ err error }

func (w refusingWriter) Write([]byte) (int, error) { return 0, w.err }
func (refusingWriter) Close() error                { return nil }

// A destination that will not take the file is a failure of the run, not damage
// to the medium: the two get different exit codes and one error cannot say
// both. io.Copy returned one for both, which is how a full output disk read as
// a damaged disc.
func TestWriteFilledKeepsTheSinksErrorOffTheMedium(t *testing.T) {
	full := errors.New("no space left on device")
	w, err := WriteFilled(refusingSink{refusingWriter{full}}, Entry{Path: "BIG.BIN"}, filling(2048, 512, 512))
	if !errors.Is(err, full) {
		t.Errorf("WriteFilled = %v; want the destination's error", err)
	}
	if w.Err != nil {
		t.Errorf("Written.Err = %v; a destination that refuses writes is not a damaged medium", w.Err)
	}
}

func zipMembers(t *testing.T, data []byte) map[string]string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]string)
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		out[f.Name] = string(b)
	}
	return out
}
