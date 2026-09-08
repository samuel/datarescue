package qic

import (
	"bytes"
	"io"
	"testing"

	"samuelks.com/datarescue/internal/testqic"
)

// displaced opens an uncompressed archive with one record's inline copy in the
// data region corrupted, so the file that record belongs to is genuinely not
// where the accumulated offsets put it — the drift a sweep exists to find.
// Open checked only the first file's anchor, so the displaced file is still
// unchecked when it comes back.
func displaced(t *testing.T) (*Archive, *File) {
	t.Helper()
	data := testqic.Build(testqic.Options{Files: testqic.DefaultFiles()})
	a, err := OpenSource(Source{
		Path:   "backup.qic",
		Reader: bytes.NewReader(data),
		Size:   int64(len(data)),
	}, Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { a.Close() }) //nolint:errcheck // read-only
	for i := len(a.files) - 1; i >= 0; i-- {
		f := &a.files[i]
		if f.dataOff < 0 {
			continue
		}
		// Zeroing the signature makes the bytes at the accumulated offset read
		// as some other record, which is the genuine mismatch the sweep cap is
		// about rather than a record that could not be read.
		rec := data[VTBLSize+f.recOff : VTBLSize+f.recOff+4]
		clear(rec)
		return a, f
	}
	t.Fatal("the archive holds no file to displace")
	return nil, nil
}

// TestDriftWhereTheSearchCannotRun checks what a displaced file becomes when
// the record right where the catalog puts it reads as another's. With the
// sweep budget left, the region is searched for the record and the file is
// marked lost; with the budget gone, the position is left unresolved. Either
// way the file must not sit at the displaced position reading as if it had
// been verified.
func TestDriftWhereTheSearchCannotRun(t *testing.T) {
	t.Run("budget exhausted leaves the position unresolved", func(t *testing.T) {
		a, f := displaced(t)
		a.sweeps = maxAnchorSweeps
		a.verifyAnchor(f)

		if f.checked != anchorUnresolved {
			t.Fatalf("checked = %v, want anchorUnresolved", f.checked)
		}
		if f.DataOffset() != -1 {
			t.Errorf("DataOffset = %d, want -1 for an unresolved position", f.DataOffset())
		}
		if f.Recoverable() {
			t.Error("a file whose position is unresolved is reported recoverable")
		}
		if _, err := io.ReadAll(a.OpenFile(f)); err == nil {
			t.Error("a file known to be displaced read as if it were verified")
		}
		var drift int
		for _, d := range a.Diagnostics() {
			if d.Kind == DiagDataOffsetDrift {
				drift++
			}
		}
		if drift == 0 {
			t.Error("no diagnostic reported the unresolved position")
		}
	})

	t.Run("a sweep that finds nothing leaves the position lost", func(t *testing.T) {
		a, f := displaced(t)
		a.verifyAnchor(f)

		if f.checked != anchorLost {
			t.Fatalf("checked = %v, want anchorLost", f.checked)
		}
		if f.DataOffset() != -1 {
			t.Errorf("DataOffset = %d, want -1 for a lost position", f.DataOffset())
		}
		if _, err := io.ReadAll(a.OpenFile(f)); err == nil {
			t.Error("a file the sweep could not place read as if it were verified")
		}
		var lost int
		for _, d := range a.Diagnostics() {
			if d.Kind == DiagDataOffsetLost {
				lost++
			}
		}
		if lost == 0 {
			t.Error("no diagnostic reported the position that could not be found")
		}
	})

	t.Run("an intact record is taken as verified", func(t *testing.T) {
		data := testqic.Build(testqic.Options{})
		a, err := OpenSource(Source{
			Path:   "backup.qic",
			Reader: bytes.NewReader(data),
			Size:   int64(len(data)),
		}, Options{})
		if err != nil {
			t.Fatal(err)
		}
		defer a.Close() //nolint:errcheck // read-only
		f := firstFile(a)
		a.verifyAnchor(f)
		if f.checked != anchorOK {
			t.Errorf("checked = %v, want anchorOK for a record that is the file's", f.checked)
		}
	})
}

func firstFile(a *Archive) *File {
	for i := range a.files {
		if a.files[i].dataOff >= 0 {
			return &a.files[i]
		}
	}
	return nil
}
