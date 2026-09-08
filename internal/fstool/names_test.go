package fstool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"samuelks.com/datarescue/internal/subcmd"
	"samuelks.com/datarescue/internal/testout"
)

// typedVolume is a volume that records a kind outside the name, as HFS does
// and no other format here does. It carries both forks, since the naming rule
// has to give the companion and the data fork the same stem.
type typedVolume struct {
	*forkedVolume
	kinds map[string]FileKind
}

func (v *typedVolume) Kind(e Entry) FileKind { return v.kinds[e.Path] }

// typed wires a driver whose one file carries both forks and a recorded kind.
func typed(t *testing.T, name string, kind FileKind) (*Context, *typedVolume) {
	t.Helper()
	d := ext()
	d.files = map[string]*fakeFile{name: {data: []byte("datafork"), failAt: -1}}
	c := openFake(t, d, "extract")
	tv := &typedVolume{
		forkedVolume: &forkedVolume{
			fakeVolume: c.Volume.(*fakeVolume),
			rsrc:       map[string]*fakeFile{name: {data: []byte("RESOURCEFORKBYTES"), failAt: -1}},
		},
		kinds: map[string]FileKind{name: kind},
	}
	c.Volume = tv
	return c, tv
}

func extractTyped(t *testing.T, c *Context, args ...string) (int, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "out")
	xf := &ExtractFlags{}
	fs := NewFlagSet("extract")
	xf.Register(fs)
	if err := fs.Parse(append([]string{"-o", dir}, args...)); err != nil {
		t.Fatal(err)
	}
	var code int
	testout.CaptureStdout(t, func() { code = Extract(c, xf) })
	return code, dir
}

func exists(t *testing.T, dir, name string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

// A classic Mac file carries no extension, so a bare name reaches a modern
// system with nothing saying what it holds. -ext is opt-in per run, and when
// it is given the extension is appended.
func TestExtractAppendsTheExtensionTheVolumeRecords(t *testing.T) {
	c, _ := typed(t, "Mountain Goat", FileKind{Code: "TIFF/8BIM", Ext: ".tif"})
	code, dir := extractTyped(t, c, "-ext")
	if code != subcmd.ExitOK {
		t.Errorf("extract = %d; want ExitOK", code)
	}
	if !exists(t, dir, "Mountain Goat.tif") {
		t.Error("the derived extension was not appended")
	}
	if exists(t, dir, "Mountain Goat") {
		t.Error("the bare name was written as well")
	}
}

// macOS matches a companion to its fork by exact filename, so if the stem were
// shortened to different lengths for the two, they would never be matched.
func TestTheCompanionFollowsTheDerivedName(t *testing.T) {
	c, _ := typed(t, "Mountain Goat", FileKind{Code: "TIFF/8BIM", Ext: ".tif"})
	_, dir := extractTyped(t, c, "-ext")
	if !exists(t, dir, "._Mountain Goat.tif") {
		t.Error("the companion does not pair with the name the fork was written under")
	}
	if exists(t, dir, "._Mountain Goat") {
		t.Error("the companion kept the volume's name while the fork did not")
	}
}

func TestForksSeparateFollowsTheDerivedName(t *testing.T) {
	c, _ := typed(t, "Mountain Goat", FileKind{Code: "TIFF/8BIM", Ext: ".tif"})
	_, dir := extractTyped(t, c, "-ext", "-forks", ForksSeparate)
	if !exists(t, dir, "Mountain Goat.tif.rsrc") {
		t.Error("the plain second file does not follow the derived name")
	}
}

// Otherwise the volume's own spelling would be lost. The companion keeps it,
// machine-readable and in the output.
func TestTheCompanionRecordsTheVolumesOwnName(t *testing.T) {
	c, _ := typed(t, "Mountain Goat", FileKind{Code: "TIFF/8BIM", Ext: ".tif"})
	_, dir := extractTyped(t, c, "-ext")
	side, err := os.ReadFile(filepath.Join(dir, "._Mountain Goat.tif"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(side), "Mountain Goat") {
		t.Error("the companion does not carry the name the volume gives the file")
	}
}

// README.TXT came to the Mac from a PC and is type TEXT. Appending would
// repeat the extension.
func TestExtractDoesNotDoubleAnExtensionTheNameAlreadyCarries(t *testing.T) {
	c, _ := typed(t, "README.TXT", FileKind{Code: "TEXT/ttxt", Ext: ".txt"})
	_, dir := extractTyped(t, c, "-ext")
	if !exists(t, dir, "README.TXT") {
		t.Error("the volume's own name was not kept")
	}
	if exists(t, dir, "README.TXT.txt") {
		t.Error("the extension was appended to a name that already carried it")
	}
}

// A code the table does not carry produces nothing rather than a guess: an
// extension is a claim about what a file holds.
func TestExtractWritesTheVolumesNameForAnUnknownKind(t *testing.T) {
	c, _ := typed(t, "Address Book", FileKind{Code: "STAK/WILD"})
	_, dir := extractTyped(t, c, "-ext")
	if !exists(t, dir, "Address Book") {
		t.Error("an unknown kind did not leave the volume's name alone")
	}
}

// By default the volume's own name is used unchanged: an extension is only a
// claim about what a file holds, and a name from the medium can be written
// as it is.
func TestDefaultWritesTheVolumesOwnName(t *testing.T) {
	c, _ := typed(t, "Mountain Goat", FileKind{Code: "TIFF/8BIM", Ext: ".tif"})
	_, dir := extractTyped(t, c)
	if !exists(t, dir, "Mountain Goat") || exists(t, dir, "Mountain Goat.tif") {
		t.Error("the default did not write the volume's own name")
	}
	if !exists(t, dir, "._Mountain Goat") {
		t.Error("the default parted the companion from its fork")
	}
}

// The map describes the file that was written, so its name follows the written
// name. A map using the volume's name would sit beside no file at all.
func TestTheMapFollowsTheDerivedName(t *testing.T) {
	c, _ := typed(t, "Mountain Goat", FileKind{Code: "TIFF/8BIM", Ext: ".tif"})
	c.Volume.(*typedVolume).d.files["Mountain Goat"].failAt = 4
	code, dir := extractTyped(t, c, "-ext")
	if code != subcmd.ExitDegraded {
		t.Errorf("extract over damage = %d; want ExitDegraded", code)
	}
	if !exists(t, dir, "Mountain Goat.tif.map") {
		t.Error("no map beside the file that was written")
	}
	if exists(t, dir, "Mountain Goat.map") {
		t.Error("the map was named after a file that was not written")
	}
}

// A finding about the medium is reported under the medium's name for the file:
// an operator cross-referencing it against a listing would not find the
// written name.
func TestDamageIsReportedUnderTheVolumesOwnName(t *testing.T) {
	c, _ := typed(t, "Mountain Goat", FileKind{Code: "TIFF/8BIM", Ext: ".tif"})
	c.Volume.(*typedVolume).d.files["Mountain Goat"].failAt = 4
	errs := captureStderr(t, func() { extractTyped(t, c, "-ext") })
	if !strings.Contains(errs, "Mountain Goat:") {
		t.Errorf("the finding does not name the file the volume holds:\n%s", errs)
	}
	if strings.Contains(errs, "Mountain Goat.tif:") {
		t.Errorf("the finding names a file the image does not contain:\n%s", errs)
	}
}

// Two image names that become one output name are a failure of the run,
// rejected the same way as two names that sanitize alike. The only addition is
// that the message says how to avoid it, since the tool caused the collision.
func TestACollisionTheExtensionCausedNamesTheWayOut(t *testing.T) {
	d := ext()
	d.files = map[string]*fakeFile{
		"Doc":     {data: []byte("tiff"), failAt: -1},
		"Doc.tif": {data: []byte("also"), failAt: -1},
	}
	c := openFake(t, d, "extract")
	c.Volume = &typedVolume{
		forkedVolume: &forkedVolume{fakeVolume: c.Volume.(*fakeVolume), rsrc: map[string]*fakeFile{}},
		kinds:        map[string]FileKind{"Doc": {Code: "TIFF/8BIM", Ext: ".tif"}},
	}
	var code int
	errs := captureStderr(t, func() { code, _ = extractTyped(t, c, "-ext") })
	if code != subcmd.ExitFailure {
		t.Errorf("extract over a collision = %d; want ExitFailure", code)
	}
	if !strings.Contains(errs, "-ext=false") {
		t.Errorf("the collision did not name the flag that avoids it:\n%s", errs)
	}
}

func TestAlreadyNamed(t *testing.T) {
	for _, tc := range []struct {
		name, ext string
		want      bool
	}{
		{"README.TXT", ".txt", true},
		{"Photo.tif", ".tif", true},
		{"Photo.jpg", ".tif", false},
		{"Mountain Goat", ".tif", false},
		// With no derived extension there is nothing to append, whatever the name.
		{"Address Book", "", true},
	} {
		if got := alreadyNamed(tc.name, tc.ext); got != tc.want {
			t.Errorf("alreadyNamed(%q, %q) = %v; want %v", tc.name, tc.ext, got, tc.want)
		}
	}
}
