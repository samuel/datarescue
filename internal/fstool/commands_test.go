package fstool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"samuelks.com/datarescue/internal/subcmd"
	"samuelks.com/datarescue/internal/testout"
)

// openFake wires a driver over an image the way Dispatch does, without the flag
// parsing, so a command can be exercised directly.
func openFake(t *testing.T, d *fakeDriver, cmd string, args ...string) *Context {
	t.Helper()
	path := imageWith(t, map[int64]byte{0: d.magic})
	drv, im, hint, err := Detect(path, &CommonFlags{Offset: -1}, []Driver{d})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { im.Close() }) //nolint:errcheck // test cleanup
	vol, err := drv.Open(im, hint)
	if err != nil {
		t.Fatal(err)
	}
	return &Context{Driver: drv, Volume: vol, Image: im, Path: path,
		Args: args, Tool: "fstool", Command: cmd}
}

func TestListShowsEveryEntry(t *testing.T) {
	c := openFake(t, ext(), "ls")
	var code int
	out := testout.CaptureStdout(t, func() { code = List(c, &ListFlags{}) })
	if code != subcmd.ExitOK {
		t.Errorf("List = %d; want ExitOK", code)
	}
	for _, want := range []string{"README", "dir/", "dir/inner.txt", "link"} {
		if !strings.Contains(out, want) {
			t.Errorf("listing missing %q:\n%s", want, out)
		}
	}
}

// A path given to a listing means "show me what is under it", so it matches by
// prefix, unlike an extract, where the path is exactly what it will write.
func TestListNarrowsToANamedSubtree(t *testing.T) {
	c := openFake(t, ext(), "ls", "dir")
	out := testout.CaptureStdout(t, func() { List(c, &ListFlags{}) })
	if !strings.Contains(out, "dir/inner.txt") {
		t.Errorf("listing of a named directory missing its child:\n%s", out)
	}
	if strings.Contains(out, "README") {
		t.Errorf("listing of a named directory included a sibling:\n%s", out)
	}
}

// Every driver walks in tree order, so grouping by parent needs no tree in
// memory: the header changes exactly when the directory does.
func TestListGroupsByDirectory(t *testing.T) {
	c := openFake(t, ext(), "ls")
	out := testout.CaptureStdout(t, func() { List(c, &ListFlags{Grouped: true}) })
	if !strings.Contains(out, "dir:") {
		t.Errorf("grouped listing has no directory header:\n%s", out)
	}
}

// A listing describes the medium rather than judging it, so damage marks the
// entry and does not change the exit code.
func TestListMarksTheEntriesDamageTouched(t *testing.T) {
	d := ext()
	d.files["README"].lost = 1024
	c := openFake(t, d, "ls")
	var code int
	out := testout.CaptureStdout(t, func() { code = List(c, &ListFlags{}) })
	if code != subcmd.ExitOK {
		t.Errorf("List over damage = %d; want ExitOK — a listing does not judge", code)
	}
	if !strings.Contains(out, "unrecovered") {
		t.Errorf("listing did not mark the damaged entry:\n%s", out)
	}
	if !strings.Contains(out, "1 entry touch") {
		t.Errorf("listing did not count the damaged entries:\n%s", out)
	}
}

// Asking for a path the volume does not hold is a failure of the run, which is
// different from damage to the medium.
func TestListReportsAPathThatMatchedNothing(t *testing.T) {
	c := openFake(t, ext(), "ls", "nope")
	var code int
	testout.CaptureStdout(t, func() { code = List(c, &ListFlags{}) })
	if code != subcmd.ExitFailure {
		t.Errorf("List of an absent path = %d; want ExitFailure", code)
	}
}

func TestCatWritesTheFile(t *testing.T) {
	c := openFake(t, ext(), "cat", "README")
	var code int
	out := testout.CaptureStdout(t, func() { code = Cat(c) })
	if code != subcmd.ExitOK {
		t.Errorf("Cat = %d; want ExitOK", code)
	}
	if out != "hello" {
		t.Errorf("Cat wrote %q; want %q", out, "hello")
	}
}

// cat has nowhere to put a map, so it stops at the damage and reports how much
// was read. Unrecorded zeros would be worse than a short file.
func TestCatLatchesAtTheDamage(t *testing.T) {
	d := ext()
	d.files["README"].failAt = 3
	c := openFake(t, d, "cat", "README")
	var code int
	out := testout.CaptureStdout(t, func() { code = Cat(c) })
	if code != subcmd.ExitDegraded {
		t.Errorf("Cat over damage = %d; want ExitDegraded", code)
	}
	if out != "hel" {
		t.Errorf("Cat wrote %q; want the readable head %q", out, "hel")
	}
}

func TestCatOfAnAbsentPathFailsTheRun(t *testing.T) {
	c := openFake(t, ext(), "cat", "nope")
	var code int
	testout.CaptureStdout(t, func() { code = Cat(c) })
	if code != subcmd.ExitFailure {
		t.Errorf("Cat of an absent path = %d; want ExitFailure", code)
	}
}

// A volume with no index is selected through the walk instead, and must give
// the same result.
func TestCatWorksWithoutAnIndex(t *testing.T) {
	d := ext()
	d.noLookup = true
	c := openFake(t, d, "cat", "dir/inner.txt")
	var code int
	out := testout.CaptureStdout(t, func() { code = Cat(c) })
	if code != subcmd.ExitOK {
		t.Errorf("Cat without an index = %d; want ExitOK", code)
	}
	if out != "inner" {
		t.Errorf("Cat wrote %q; want %q", out, "inner")
	}
}

func extractTo(t *testing.T, c *Context, args ...string) (int, string) {
	t.Helper()
	dir := t.TempDir()
	c.Args = args
	xf := &ExtractFlags{}
	fs := NewFlagSet("extract")
	xf.Register(fs)
	if err := fs.Parse([]string{"-o", dir}); err != nil {
		t.Fatal(err)
	}
	var code int
	testout.CaptureStdout(t, func() { code = Extract(c, xf) })
	return code, dir
}

func TestExtractWritesEveryFile(t *testing.T) {
	c := openFake(t, ext(), "extract")
	code, dir := extractTo(t, c)
	if code != subcmd.ExitOK {
		t.Errorf("Extract = %d; want ExitOK", code)
	}
	got, err := os.ReadFile(filepath.Join(dir, "README"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello" {
		t.Errorf("README = %q; want %q", got, "hello")
	}
	if _, err := os.Stat(filepath.Join(dir, "dir", "inner.txt")); err != nil {
		t.Errorf("nested file not written: %v", err)
	}
	// A symlink is listed but never recreated, so an image cannot place a link
	// the operator did not choose.
	if _, err := os.Lstat(filepath.Join(dir, "link")); err == nil {
		t.Error("a symlink was recreated")
	}
}

// A damaged file is written at its full length with the holes zero-filled and a
// .map beside it recording where they are.
func TestExtractFillsAndRecordsTheHoles(t *testing.T) {
	d := ext()
	d.files["README"].failAt = 3
	c := openFake(t, d, "extract")
	code, dir := extractTo(t, c)
	if code != subcmd.ExitDegraded {
		t.Errorf("Extract over damage = %d; want ExitDegraded", code)
	}
	got, err := os.ReadFile(filepath.Join(dir, "README"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Errorf("README is %d bytes; want the full 5 with the hole zero-filled", len(got))
	}
	if string(got[:3]) != "hel" || got[3] != 0 || got[4] != 0 {
		t.Errorf("README = %q; want the readable head then zeros", got)
	}
	mapText, err := os.ReadFile(filepath.Join(dir, "README.map"))
	if err != nil {
		t.Fatalf("no .map beside the filled file: %v", err)
	}
	if !strings.Contains(string(mapText), "0x00000003") {
		t.Errorf("the map does not record the hole at offset 3:\n%s", mapText)
	}
}

// A directory is something extract will not write, and it says so rather than
// quietly turning one path into a subtree.
//
// The path did match, so this is not the run failure that a selection matching
// nothing is, and the exit code stays OK. The printed note is the response.
func TestExtractNotesANamedDirectory(t *testing.T) {
	c := openFake(t, ext(), "extract")
	code, dir := extractTo(t, c, "dir")
	if _, err := os.Stat(filepath.Join(dir, "dir", "inner.txt")); err == nil {
		t.Error("naming a directory extracted its contents")
	}
	if code != subcmd.ExitOK {
		t.Errorf("Extract of a named directory = %d; want ExitOK — the path matched", code)
	}
}

func TestExtractSelectionThatMatchesNothingFailsTheRun(t *testing.T) {
	c := openFake(t, ext(), "extract")
	code, _ := extractTo(t, c, "nope")
	if code != subcmd.ExitFailure {
		t.Errorf("Extract of an absent path = %d; want ExitFailure", code)
	}
}

// Asking for a file by path must not turn damage to the medium into a failure
// of the run: the same file reached through the whole-volume walk is
// ExitDegraded, and requesting it by path must not make it ExitFailure.
func TestExtractClassifiesANamedPathLikeTheWalk(t *testing.T) {
	for _, named := range []bool{false, true} {
		name := "walked"
		args := []string{}
		if named {
			name, args = "named", []string{"README"}
		}
		t.Run(name, func(t *testing.T) {
			d := ext()
			d.files["README"].failAt = 3
			c := openFake(t, d, "extract")
			code, _ := extractTo(t, c, args...)
			if code != subcmd.ExitDegraded {
				t.Errorf("Extract (%s) = %d; want ExitDegraded", name, code)
			}
		})
	}
}

// A volume with no index falls back to the walk, and the selection must behave
// the same either way.
func TestExtractNamedPathWithoutAnIndex(t *testing.T) {
	d := ext()
	d.noLookup = true
	c := openFake(t, d, "extract")
	code, dir := extractTo(t, c, "README")
	if code != subcmd.ExitOK {
		t.Errorf("Extract without an index = %d; want ExitOK", code)
	}
	if _, err := os.Stat(filepath.Join(dir, "README")); err != nil {
		t.Errorf("named file not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "dir", "inner.txt")); err == nil {
		t.Error("a file that was not named was written")
	}
}

func TestVerifyReportsWhatTheMapCost(t *testing.T) {
	d := ext()
	d.files["README"].lost = 2048
	c := openFake(t, d, "verify")
	var code int
	out := testout.CaptureStdout(t, func() { code = Verify(c, false) })
	if code != subcmd.ExitDegraded {
		t.Errorf("Verify with a finding = %d; want ExitDegraded", code)
	}
	if !strings.Contains(out, "README") || !strings.Contains(out, "never recovered") {
		t.Errorf("verify did not name the damaged file:\n%s", out)
	}
	if !strings.Contains(out, "Result:      ISSUES FOUND") {
		t.Errorf("verify verdict missing:\n%s", out)
	}
}

func TestVerifyCleanVolume(t *testing.T) {
	c := openFake(t, ext(), "verify")
	var code int
	out := testout.CaptureStdout(t, func() { code = Verify(c, false) })
	if code != subcmd.ExitOK {
		t.Errorf("Verify of a clean volume = %d; want ExitOK", code)
	}
	if !strings.Contains(out, "Result:      OK") {
		t.Errorf("verify verdict missing:\n%s", out)
	}
}

// orphans against a volume that does not support it is the operator asking for
// something this image cannot provide. That is a usage error however late it
// is noticed, the same as a flag that does not apply.
func TestOrphansAgainstAVolumeThatCannotAnswerIsUsage(t *testing.T) {
	c := openFake(t, ext(), "orphans")
	var code int
	testout.CaptureStdout(t, func() { code = Orphans(c, false) })
	if code != subcmd.ExitUsage {
		t.Errorf("Orphans against a volume with no answer = %d; want ExitUsage", code)
	}
}

// Three of the four formats record no kind outside the name, so their long
// listing must have no kind column: no empty column and no trailing padding.
func TestLongListingIsUnchangedWithoutTheInterface(t *testing.T) {
	c := openFake(t, ext(), "ls")
	if _, ok := c.Volume.(Typed); ok {
		t.Fatal("the plain fake volume records a kind; this test proves the case where none does")
	}
	out := testout.CaptureStdout(t, func() { List(c, &ListFlags{Long: true}) })
	want := "" +
		"-rw-r--r--           5                       README\n" +
		"-rw-r--r--       <DIR>                       dir/\n" +
		"-rw-r--r--           5                       dir/inner.txt\n" +
		"-rw-r--r--           0                       link -> README\n"
	if out != want {
		t.Errorf("long listing changed:\ngot:\n%q\nwant:\n%q", out, want)
	}
}

// The column holds the codes the volume recorded. An interpretation the tool
// inferred would be a different claim in the same column.
func TestLongListingShowsWhatTheVolumeRecords(t *testing.T) {
	c, _ := typed(t, "Party", FileKind{Code: "TIFF/8BIM", Ext: ".tif"})
	out := testout.CaptureStdout(t, func() { List(c, &ListFlags{Long: true}) })
	if !strings.Contains(out, "TIFF/8BIM  Party") {
		t.Errorf("the listing does not show the recorded codes beside the name:\n%q", out)
	}
}

// A code the table cannot describe is still shown: the medium recorded it, and
// a word like "unknown" in its place would say less than the code does.
func TestAnUnknownCodeStillShowsWhatTheMediumRecorded(t *testing.T) {
	c, _ := typed(t, "Address Book", FileKind{Code: "STAK/WILD"})
	out := testout.CaptureStdout(t, func() { List(c, &ListFlags{Long: true}) })
	if !strings.Contains(out, "STAK/WILD") {
		t.Errorf("the listing dropped a code the table does not carry:\n%q", out)
	}
	if strings.Contains(strings.ToLower(out), "unknown") {
		t.Errorf("the listing invented a word for a code it could not describe:\n%q", out)
	}
}

// tabwriter aligns by cell index, so a row that skipped the column would end it
// for every row after it.
func TestAnEntryWithNoKindStillWritesTheCell(t *testing.T) {
	c, tv := typed(t, "Dogs", FileKind{Code: "TIFF/8BIM", Ext: ".tif"})
	tv.d.files["Photos"] = &fakeFile{dir: true, failAt: -1}
	tv.rsrc["Photos"] = nil
	out := testout.CaptureStdout(t, func() { List(c, &ListFlags{Long: true}) })
	want := "" +
		"-rw-r--r--           8                       TIFF/8BIM  Dogs\n" +
		"-rw-r--r--       <DIR>                                  Photos/\n"
	if out != want {
		t.Errorf("a folder did not hold the column open:\ngot:\n%q\nwant:\n%q", out, want)
	}
}

// The short listing is what a caller pipes, so it stays one name per line.
func TestShortListingIsJustNames(t *testing.T) {
	c, _ := typed(t, "Short", FileKind{Code: "TIFF/8BIM", Ext: ".tif"})
	out := testout.CaptureStdout(t, func() { List(c, &ListFlags{}) })
	if out != "Short\n" {
		t.Errorf("short listing = %q; want just the name", out)
	}
}
