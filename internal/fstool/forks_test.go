package fstool

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"samuelks.com/datarescue/internal/appledouble"
	"samuelks.com/datarescue/internal/subcmd"
	"samuelks.com/datarescue/internal/testout"
)

// forkedDriver is a fake volume whose files carry a resource fork, so the fork
// policy can be exercised with no filesystem.
type forkedVolume struct {
	*fakeVolume
	rsrc map[string]*fakeFile
}

func (v *forkedVolume) ResourceSize(e Entry) int64 {
	if f := v.rsrc[e.Path]; f != nil {
		return int64(len(f.data))
	}
	return 0
}

func (v *forkedVolume) OpenResource(e Entry) (Reader, error) {
	return &fakeReader{f: v.rsrc[e.Path], v: v.fakeVolume}, nil
}

func (v *forkedVolume) Sidecar(e Entry, name string) appledouble.Header {
	var fi [32]byte
	copy(fi[0:4], "STAK")
	copy(fi[4:8], "WILD")
	h := appledouble.Header{FinderInfo: fi, RsrcSize: 0}
	if name != "" && name != e.Path {
		h.RealName = e.Path
	}
	return h
}

// forked wires a driver whose one file has both forks.
func forked(t *testing.T, rsrcFailAt int) (*Context, *forkedVolume) {
	t.Helper()
	d := ext()
	d.files = map[string]*fakeFile{"Stack": {data: []byte("datafork"), failAt: -1}}
	c := openFake(t, d, "extract")
	fv := &forkedVolume{
		fakeVolume: c.Volume.(*fakeVolume),
		rsrc:       map[string]*fakeFile{"Stack": {data: []byte("RESOURCEFORKBYTES"), failAt: rsrcFailAt}},
	}
	c.Volume = fv
	return c, fv
}

func extractForks(t *testing.T, c *Context, policy string) (int, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "out")
	xf := &ExtractFlags{Forks: policy}
	fs := NewFlagSet("extract")
	xf.Register(fs)
	if err := fs.Parse([]string{"-o", dir}); err != nil {
		t.Fatal(err)
	}
	xf.Forks = policy
	var code int
	testout.CaptureStdout(t, func() { code = Extract(c, xf) })
	return code, dir
}

// A resource fork is as much the operator's data as the data fork.
func TestAppleDoubleWritesBothForks(t *testing.T) {
	c, _ := forked(t, -1)
	code, dir := extractForks(t, c, ForksAppleDouble)
	if code != subcmd.ExitOK {
		t.Errorf("extract = %d; want ExitOK", code)
	}
	data, err := os.ReadFile(filepath.Join(dir, "Stack"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "datafork" {
		t.Errorf("data fork = %q", data)
	}
	side, err := os.ReadFile(filepath.Join(dir, "._Stack"))
	if err != nil {
		t.Fatalf("no AppleDouble companion: %v", err)
	}
	if !strings.HasSuffix(string(side), "RESOURCEFORKBYTES") {
		t.Errorf("the companion does not end with the fork's bytes")
	}
	if !strings.Contains(string(side), "STAKWILD") {
		t.Error("the companion does not carry the type and creator")
	}
}

// A file is one file however many streams it carries. Counting the companion
// separately would report every forked file twice.
func TestAForkedFileIsCountedOnce(t *testing.T) {
	c, _ := forked(t, -1)
	dir := filepath.Join(t.TempDir(), "out")
	xf := &ExtractFlags{Forks: ForksAppleDouble}
	fs := NewFlagSet("extract")
	xf.Register(fs)
	if err := fs.Parse([]string{"-o", dir}); err != nil {
		t.Fatal(err)
	}
	xf.Forks = ForksAppleDouble
	errs := captureStderr(t, func() { testout.CaptureStdout(t, func() { Extract(c, xf) }) })
	if !strings.Contains(errs, "wrote 1 file(s)") {
		t.Errorf("summary counted the companion as a second file:\n%s", errs)
	}
}

// The fork's bytes are written after the header, so every hole it records has
// to be shifted by the header's length. Recorded at the fork's own offsets, the
// map would mark the header as lost and put each hole one header length too
// early.
func TestTheCompanionsMapIsShiftedByTheHeader(t *testing.T) {
	// The resource fork stops after 4 bytes; the rest is zero-filled.
	c, fv := forked(t, 4)
	code, dir := extractForks(t, c, ForksAppleDouble)
	if code != subcmd.ExitDegraded {
		t.Errorf("extract over a damaged fork = %d; want ExitDegraded", code)
	}
	mapText, err := os.ReadFile(filepath.Join(dir, "._Stack.map"))
	if err != nil {
		t.Fatalf("no map beside the filled companion: %v", err)
	}
	hdr := fv.Sidecar(Entry{}, "")
	hdr.RsrcSize = int64(len(fv.rsrc["Stack"].data))
	// The hole starts 4 bytes into the fork, which is header+4 in the file.
	want := hdr.Size() + 4
	if !strings.Contains(string(mapText), formatOffset(want)) {
		t.Errorf("the map does not record the hole at %d (header %d + 4):\n%s",
			want, hdr.Size(), mapText)
	}
	// And it must not mark the header's bytes as lost: the header is the
	// tool's own and is always recovered.
	if strings.Contains(string(mapText), formatOffset(4)+"  0x") &&
		!strings.Contains(string(mapText), formatOffset(want)) {
		t.Error("the hole was recorded at the fork's offset, not the file's")
	}
}

func formatOffset(n int64) string {
	const hex = "0123456789abcdef"
	b := []byte("0x00000000")
	for i := 9; i >= 2 && n > 0; i-- {
		b[i] = hex[n&0xF]
		n >>= 4
	}
	return string(b)
}

// -forks data drops the fork and reports what was dropped, since a
// resource-only file otherwise extracts as a zero-byte file that looks fine.
func TestForksDataReportsWhatItDropped(t *testing.T) {
	c, _ := forked(t, -1)
	dir := filepath.Join(t.TempDir(), "out")
	xf := &ExtractFlags{Forks: ForksData}
	fs := NewFlagSet("extract")
	xf.Register(fs)
	if err := fs.Parse([]string{"-o", dir}); err != nil {
		t.Fatal(err)
	}
	xf.Forks = ForksData
	errs := captureStderr(t, func() { testout.CaptureStdout(t, func() { Extract(c, xf) }) })
	if _, err := os.Stat(filepath.Join(dir, "._Stack")); err == nil {
		t.Error("-forks data wrote a companion")
	}
	if !strings.Contains(errs, "resource fork not written") {
		t.Errorf("-forks data did not report the dropped fork:\n%s", errs)
	}
}

// captureStderr collects what a run wrote to stderr, where every tool here
// sends its operator messages.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	done := make(chan string, 1)
	go func() {
		var b strings.Builder
		io.Copy(&b, r) //nolint:errcheck // the pipe closes with the write end
		done <- b.String()
	}()
	fn()
	w.Close() //nolint:errcheck // test cleanup
	os.Stderr = orig
	return <-done
}

func TestForksSeparateWritesAPlainSecondFile(t *testing.T) {
	c, _ := forked(t, -1)
	_, dir := extractForks(t, c, ForksSeparate)
	got, err := os.ReadFile(filepath.Join(dir, "Stack.rsrc"))
	if err != nil {
		t.Fatalf("no .rsrc file: %v", err)
	}
	if string(got) != "RESOURCEFORKBYTES" {
		t.Errorf("Stack.rsrc = %q", got)
	}
}

func TestForksNoneIsSilent(t *testing.T) {
	c, _ := forked(t, -1)
	dir := filepath.Join(t.TempDir(), "out")
	xf := &ExtractFlags{Forks: ForksNone}
	fs := NewFlagSet("extract")
	xf.Register(fs)
	if err := fs.Parse([]string{"-o", dir}); err != nil {
		t.Fatal(err)
	}
	xf.Forks = ForksNone
	errs := captureStderr(t, func() { testout.CaptureStdout(t, func() { Extract(c, xf) }) })
	if strings.Contains(errs, "resource fork") {
		t.Errorf("-forks none mentioned the fork:\n%s", errs)
	}
}

func TestAnUnknownForkPolicyIsAUsageError(t *testing.T) {
	c, _ := forked(t, -1)
	code, _ := extractForks(t, c, "sideways")
	if code != subcmd.ExitUsage {
		t.Errorf("-forks sideways = %d; want ExitUsage", code)
	}
}
