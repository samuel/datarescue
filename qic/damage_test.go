package qic_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"samuelks.com/datarescue/ddrescue"
	"samuelks.com/datarescue/internal/testqic"
	"samuelks.com/datarescue/qic"
)

// open reads a damaged archive from memory, returning it and the error rather
// than failing, since some damage is meant to stop it opening at all.
func open(opts testqic.Options) (*qic.Archive, error) {
	data := testqic.Build(opts)
	return qic.OpenSource(qic.Source{
		Path: "backup.qic", Reader: bytes.NewReader(data), Size: int64(len(data)),
	}, qic.Options{})
}

func kinds(diags []qic.Diagnostic) map[qic.DiagnosticKind]int {
	out := map[qic.DiagnosticKind]int{}
	for _, d := range diags {
		out[d.Kind]++
	}
	return out
}

// survivors reads every file and reports how many came back whole and how many
// were lost, so a damage test can show both that something was lost and that
// something else survived.
func survivors(t *testing.T, a *qic.Archive, want []testqic.File) (whole, lost int) {
	t.Helper()
	byPath := map[string][]byte{}
	for _, f := range want {
		if !f.IsDir {
			byPath[strings.ToUpper(f.Path())] = f.Data
		}
	}
	for i := range a.Files() {
		f := &a.Files()[i]
		if f.Dir {
			continue
		}
		data, err := io.ReadAll(a.OpenFile(f))
		if err != nil || !bytes.Equal(data, byPath[strings.ToUpper(f.Path)]) {
			lost++
			continue
		}
		whole++
	}
	return whole, lost
}

// TestBadSegment checks that a segment whose stream will not decode costs the
// file overlapping it and no more. Each segment records its own offset into
// the uncompressed stream, so the files after the damage stay where they are.
func TestBadSegment(t *testing.T) {
	t.Parallel()
	files := testqic.DefaultFiles()
	a, err := open(testqic.Options{Compress: true, Files: files, BadSegment: 1})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer a.Close() //nolint:errcheck // read-only

	whole, lost := survivors(t, a, files)
	if lost == 0 {
		t.Error("a corrupt segment cost nothing, so the test proves nothing")
	}
	if whole == 0 {
		t.Error("a corrupt segment cost every file, which is what the segment offsets exist to prevent")
	}
	if k := kinds(a.Diagnostics()); k[qic.DiagSegmentDecompress]+k[qic.DiagSegmentShort] == 0 {
		t.Errorf("no diagnostic reported the corrupt segment; got %v", k)
	}
}

// TestLatchedRead checks that a read stopped by damage stays stopped, so a
// caller that retries cannot splice bytes from past the damage in at the wrong
// offset with nothing to mark the gap.
func TestLatchedRead(t *testing.T) {
	t.Parallel()
	files := testqic.DefaultFiles()
	a, err := open(testqic.Options{Compress: true, Files: files, BadSegment: 1})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer a.Close() //nolint:errcheck // read-only

	for i := range a.Files() {
		f := &a.Files()[i]
		if f.Dir {
			continue
		}
		r := a.OpenFile(f)
		if _, err := io.Copy(io.Discard, r); err == nil {
			continue
		}
		buf := make([]byte, 16)
		n1, err1 := r.Read(buf)
		n2, err2 := r.Read(buf)
		if n1 != 0 || n2 != 0 {
			t.Errorf("a latched reader delivered %d then %d bytes, want none", n1, n2)
		}
		if err1 == nil || err2 == nil || err1.Error() != err2.Error() {
			t.Errorf("a latched reader gave %v then %v, want the same error twice", err1, err2)
		}
		return
	}
	t.Error("no file was stopped by the corrupt segment")
}

// TestLostSegmentHeader checks that a segment header which cannot be read
// leaves a hole of a known size rather than shifting the rest of the archive.
func TestLostSegmentHeader(t *testing.T) {
	t.Parallel()
	files := testqic.DefaultFiles()
	a, err := open(testqic.Options{Compress: true, Files: files, LostSegmentHeader: 2})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer a.Close() //nolint:errcheck // read-only

	whole, lost := survivors(t, a, files)
	if lost == 0 {
		t.Error("a lost segment header cost nothing")
	}
	if whole == 0 {
		t.Error("a lost segment header cost every file")
	}
	_ = a.Diagnostics()
}

// TestBadCumSz checks that a segment whose recorded offset runs backwards is
// discarded rather than being allowed to break the lookup by offset.
func TestBadCumSz(t *testing.T) {
	t.Parallel()
	a, err := open(testqic.Options{Compress: true, BadCumSz: 2})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer a.Close() //nolint:errcheck // read-only

	var pos int64
	for i, s := range a.Segments()[:a.DataSegments()] {
		if s.Start < pos {
			t.Errorf("segment %d starts at %d, behind the segment before it at %d", i, s.Start, pos)
		}
		pos = s.End()
	}
	if k := kinds(a.Diagnostics()); k[qic.DiagSegmentInconsistent] == 0 {
		t.Errorf("no diagnostic reported the offset that runs backwards; got %v", k)
	}
}

// TestBadRecordLength checks that a record whose fields disagree costs itself
// and lets the parse pick up at the next sound record.
func TestBadRecordLength(t *testing.T) {
	t.Parallel()
	files := testqic.DefaultFiles()
	a, err := open(testqic.Options{Compress: true, Files: files, BadRecLen: 3})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer a.Close() //nolint:errcheck // read-only

	if len(a.Files()) == 0 {
		t.Fatal("a single bad record cost the whole catalog")
	}
	k := kinds(a.Diagnostics())
	if k[qic.DiagRecordResync]+k[qic.DiagRecordDamaged] == 0 {
		t.Errorf("no diagnostic reported the damaged record; got %v", k)
	}
}

// TestBadPathLength checks that a record disagreeing with the rebuilt tree
// about its parent is reported, since that length is the only check on the
// tree that needs nothing from the data region.
func TestBadPathLength(t *testing.T) {
	t.Parallel()
	a, err := open(testqic.Options{Compress: true, BadPathLen: 4})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer a.Close() //nolint:errcheck // read-only

	if k := kinds(a.Diagnostics()); k[qic.DiagPathMismatch] == 0 {
		t.Errorf("no diagnostic reported the parent path disagreement; got %v", k)
	}
}

// TestDroppedCatalog checks the fallback: with no catalog at all, the records
// the data region repeats inline still yield every file, with the paths they
// carry themselves.
func TestDroppedCatalog(t *testing.T) {
	t.Parallel()
	files := testqic.DefaultFiles()
	a, err := open(testqic.Options{Compress: true, Files: files, DropCatalog: true})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer a.Close() //nolint:errcheck // read-only

	if !a.FromScan() {
		t.Fatal("the file list did not come from scanning the data region")
	}
	whole, lost := survivors(t, a, files)
	if whole == 0 {
		t.Errorf("scanning recovered no file whole (%d lost)", lost)
	}
	for _, want := range []string{`C:\README.TXT`, `C:\SRC\LIB\NET\socket.c`} {
		if _, ok := a.Find(want); !ok {
			var have []string
			for _, f := range a.Files() {
				have = append(have, f.Path)
			}
			t.Errorf("%s was not recovered by scanning; got %v", want, have)
		}
	}
	if k := kinds(a.Diagnostics()); k[qic.DiagDataScan] == 0 {
		t.Error("no diagnostic recorded that the data region was scanned")
	}
}

// TestScanMatchesCatalog checks the scan against the catalog on an undamaged
// archive, since where both are available the size the scan derives has to be
// the size the catalog records.
func TestScanMatchesCatalog(t *testing.T) {
	t.Parallel()
	files := testqic.DefaultFiles()
	data := testqic.Build(testqic.Options{Compress: true, Files: files})
	src := func() qic.Source {
		return qic.Source{Path: "backup.qic", Reader: bytes.NewReader(data), Size: int64(len(data))}
	}
	fromCatalog, err := qic.OpenSource(src(), qic.Options{Mode: qic.ModeCatalog})
	if err != nil {
		t.Fatal(err)
	}
	defer fromCatalog.Close() //nolint:errcheck // read-only
	fromScan, err := qic.OpenSource(src(), qic.Options{Mode: qic.ModeScan})
	if err != nil {
		t.Fatal(err)
	}
	defer fromScan.Close() //nolint:errcheck // read-only

	for i := range fromCatalog.Files() {
		want := fromCatalog.Files()[i]
		got, ok := fromScan.Find(want.Path)
		if !ok {
			t.Errorf("%s was not found by scanning", want.Path)
			continue
		}
		if got.Size != want.Size {
			t.Errorf("%s: scanning derived a size of %d where the catalog records %d",
				want.Path, got.Size, want.Size)
		}
		if got.DataOffset() != want.DataOffset() {
			t.Errorf("%s: scanning put the data at %d where the catalog puts it at %d",
				want.Path, got.DataOffset(), want.DataOffset())
		}
	}
	_, _ = fromCatalog.Diagnostics(), fromScan.Diagnostics()
}

// TestTruncated checks that an archive with segments cut off its end still
// lists and recovers what remains.
func TestTruncated(t *testing.T) {
	t.Parallel()
	files := testqic.DefaultFiles()
	a, err := open(testqic.Options{Compress: true, Files: files, TruncateSegments: 1})
	if err != nil {
		// A truncated catalog can leave nothing to list, which is a sound
		// outcome as long as it is reported rather than mistaken for success.
		if !strings.Contains(err.Error(), "qic:") {
			t.Fatalf("open: %v", err)
		}
		return
	}
	defer a.Close() //nolint:errcheck // read-only
	whole, _ := survivors(t, a, files)
	if whole == 0 {
		t.Error("truncating the last segment cost every file")
	}
	_ = a.Diagnostics()
}

// TestWrongDataSize checks that a volume table disagreeing with the records is
// reported, and that the disagreement makes every file's position get checked
// against the data region rather than being trusted.
func TestWrongDataSize(t *testing.T) {
	t.Parallel()
	a, err := open(testqic.Options{Compress: true, WrongDataSize: 12345})
	if err != nil {
		return // refusing outright is acceptable; silently accepting is not
	}
	defer a.Close() //nolint:errcheck // read-only
	if k := kinds(a.Diagnostics()); len(k) == 0 {
		t.Error("a data region size the records disagree with was accepted without comment")
	}
}

// TestUnreadableSectors checks the ddrescue path: a map marking one segment's
// bytes unrecovered has to cost the files inside it and nothing else.
func TestUnreadableSectors(t *testing.T) {
	t.Parallel()
	files := testqic.DefaultFiles()
	dir := t.TempDir()
	path := filepath.Join(dir, "backup.qic")
	data := testqic.Build(testqic.Options{Compress: true, Files: files})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	// Mark the body of the third data segment unreadable, leaving its header
	// and both neighbors recovered, so what fails is the payload rather than
	// the index entry that places it.
	const seg = 2
	bad := int64(testqic.VTBLSize + seg*testqic.SegmentSize + 512)
	badLen := int64(testqic.SegmentSize - 1024)
	mapPath := path + ".map"
	var b strings.Builder
	b.WriteString("0x00000000     ?               1\n")
	b.WriteString(hexLine(0, bad, '+'))
	b.WriteString(hexLine(bad, badLen, '-'))
	b.WriteString(hexLine(bad+badLen, int64(len(data))-bad-badLen, '+'))
	if err := os.WriteFile(mapPath, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	a, err := qic.OpenSource(qic.Source{Path: path, Map: mapPath}, qic.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer a.Close() //nolint:errcheck // read-only

	var notRecovered int
	whole, lost := survivors(t, a, files)
	for i := range a.Files() {
		f := &a.Files()[i]
		if f.Dir {
			continue
		}
		if _, err := io.ReadAll(a.OpenFile(f)); errors.Is(err, ddrescue.ErrNotRecovered) {
			notRecovered++
		}
	}
	if notRecovered == 0 {
		t.Errorf("no file reported the unrecovered segment (%d whole, %d lost)", whole, lost)
	}
	if whole == 0 {
		t.Error("an unrecovered segment cost every file")
	}
	_ = a.Diagnostics()
}

func hexLine(off, size int64, status byte) string {
	return "0x" + hex(off) + "  0x" + hex(size) + "  " + string(status) + "\n"
}

func hex(n int64) string {
	const digits = "0123456789abcdef"
	b := make([]byte, 8)
	for i := 7; i >= 0; i-- {
		b[i] = digits[n&0xF]
		n >>= 4
	}
	return string(b)
}

// mapped writes an archive and a ddrescue map marking one byte range
// unrecovered, and opens it through that map. Only this makes a segment header
// actually fail to read, the case SegmentHeaderLost covers.
func mapped(t *testing.T, opts testqic.Options, badOff, badLen int64) *qic.Archive {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "backup.qic")
	data := testqic.Build(opts)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	mapPath := path + ".map"
	var b strings.Builder
	b.WriteString("0x00000000     ?               1\n")
	b.WriteString(hexLine(0, badOff, '+'))
	b.WriteString(hexLine(badOff, badLen, '-'))
	b.WriteString(hexLine(badOff+badLen, int64(len(data))-badOff-badLen, '+'))
	if err := os.WriteFile(mapPath, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := qic.OpenSource(qic.Source{Path: path, Map: mapPath}, qic.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { a.Close() }) //nolint:errcheck // read-only
	return a
}

// segmentAt returns the segment at index i.
func segmentAt(t *testing.T, a *qic.Archive, i int) qic.Segment {
	t.Helper()
	segs := a.Segments()
	if i >= len(segs) {
		t.Fatalf("the archive holds %d segments, not %d", len(segs), i+1)
	}
	return segs[i]
}

// A segment header that will not read places nothing: it neither bounds itself
// nor ends the segment before it. The span between the two recorded offsets
// around it belongs to the readable segment that opens it, whose own stream
// fills as much of the span as survived. Cutting that segment to a segment's
// worth instead puts the hole at a boundary nobody recorded and throws away
// the tail the readable segment did yield.
func TestUnreadableSegmentHeader(t *testing.T) {
	t.Parallel()
	const seg = 3
	opts := testqic.Options{Compress: true, Files: testqic.DefaultFiles()}
	off := int64(testqic.VTBLSize + seg*testqic.SegmentSize)
	a := mapped(t, opts, off, 10)

	lost := segmentAt(t, a, seg)
	if lost.State != qic.SegmentHeaderLost {
		t.Fatalf("segment %d is %s; want its header reported lost", seg, lost.State)
	}
	if lost.Length >= 0 {
		t.Errorf("a segment no header placed was given a length of %d; nothing measures one", lost.Length)
	}
	prev := segmentAt(t, a, seg-1)
	next := segmentAt(t, a, seg+1)
	if prev.End() != next.Start {
		t.Errorf("segment %d covers %d..%d where the next recorded offset is %d; the span between two offsets is the only measurement there is",
			seg-1, prev.Start, prev.End(), next.Start)
	}
	// The index still has to be ascending, or the binary search that finds a
	// segment by offset cannot work.
	var pos int64
	for i, s := range a.Segments()[:a.DataSegments()] {
		if s.Start < pos {
			t.Errorf("segment %d starts at %d, behind %d", i, s.Start, pos)
		}
		pos = s.Start
	}
	whole, _ := survivors(t, a, testqic.DefaultFiles())
	if whole == 0 {
		t.Error("one lost segment header cost every file")
	}
	_ = a.Diagnostics()
}

// A data segment header damaged to zeros declares no payload, which is also how
// the format spells the end of the data region. Honoring it as the terminator
// gives the zeroed segment the whole remaining span and discards the readable
// one before it, so it counts as damage until the rest of the header agrees
// that the data is complete.
func TestZeroedHeaderIsNotATerminator(t *testing.T) {
	t.Parallel()
	const seg = 2
	a, err := open(testqic.Options{Compress: true, Files: testqic.DefaultFiles(), LostSegmentHeader: seg})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer a.Close() //nolint:errcheck // read-only

	if s := segmentAt(t, a, seg); s.State != qic.SegmentHeaderLost {
		t.Errorf("a header of zeros before the end of the data is %s; want it taken as damage", s.State)
	}
	if prev := segmentAt(t, a, seg-1); prev.Length <= 0 {
		t.Errorf("the readable segment before it was cut to %d bytes", prev.Length)
	}
	whole, lost := survivors(t, a, testqic.DefaultFiles())
	if lost == 0 {
		t.Error("a zeroed segment header cost nothing")
	}
	if whole == 0 {
		t.Error("a zeroed segment header cost every file")
	}
	_ = a.Diagnostics()
}

// A record the map never recovered says nothing about where its file is: the
// catalog's arithmetic still puts it there and the segments past the damage
// are intact. Sweeping for a record that lies inside the hole finds nothing
// and would condemn the whole file, when the damage only affects the part of
// it over the bad segment.
func TestUnreadableAnchorKeepsThePosition(t *testing.T) {
	t.Parallel()
	files := testqic.DefaultFiles()
	// The first data segment's payload holds the first records and the bytes
	// they describe, so losing it is exactly the case where the anchor cannot
	// be read.
	off := int64(testqic.VTBLSize + 10)
	a := mapped(t, testqic.Options{Compress: true, Files: files}, off, int64(testqic.SegmentSize)-10)

	var lostPosition int
	for i := range a.Files() {
		f := &a.Files()[i]
		if !f.Dir && f.DataOffset() < 0 {
			lostPosition++
		}
	}
	if lostPosition > 0 {
		t.Errorf("%d file(s) were refused outright because a record could not be read", lostPosition)
	}
	whole, _ := survivors(t, a, files)
	if whole == 0 {
		t.Error("an unreadable first segment cost every file in the archive")
	}
	_ = a.Diagnostics()
}
