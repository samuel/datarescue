package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"samuelks.com/datarescue/internal/subcmd"
	"samuelks.com/datarescue/internal/testfat"
	"samuelks.com/datarescue/internal/testfixture"
	"samuelks.com/datarescue/internal/testiso"
	"samuelks.com/datarescue/internal/testout"
)

// run invokes one subcommand the way main does.
func run(name string, args ...string) int {
	for _, c := range commands() {
		if c.Name == name {
			return c.Run(args)
		}
	}
	panic("no such command: " + name)
}

func fatImage(t *testing.T) string {
	t.Helper()
	im := testfat.Build(testfat.Options{Files: []testfat.File{{Name: "HELLO.TXT", Size: 12}}})
	path := filepath.Join(t.TempDir(), "fat.img")
	if err := os.WriteFile(path, im.Data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func isoImage(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "disc.iso")
	if err := os.WriteFile(path, testiso.Build(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func extImage(t *testing.T) string {
	t.Helper()
	return testfixture.File(t, filepath.Join("..", "..", "extfs", "testdata", "disk.img.gz"))
}

// One binary reads every format: the format is a property of the disk, and the
// operator should not have to know it to read the disk.
func TestDetectsEachFormatWithoutBeingTold(t *testing.T) {
	for _, tc := range []struct {
		name  string
		image func(*testing.T) string
		want  string
	}{
		{"fat", fatImage, "FAT12/16/32"},
		{"ext", extImage, "ext2/ext3"},
		{"iso", isoImage, "ISO 9660"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			img := tc.image(t)
			var code int
			out := testout.CaptureStdout(t, func() { code = run("info", img) })
			if code != subcmd.ExitOK {
				t.Fatalf("info = %d\n%s", code, out)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("info did not name the format %q:\n%s", tc.want, out)
			}
		})
	}
}

func TestListsEachFormat(t *testing.T) {
	for _, tc := range []struct {
		name  string
		image func(*testing.T) string
		want  string
	}{
		{"fat", fatImage, "HELLO.TXT"},
		{"ext", extImage, "hello.txt"},
		{"iso", isoImage, testiso.FileName},
	} {
		t.Run(tc.name, func(t *testing.T) {
			img := tc.image(t)
			var code int
			out := testout.CaptureStdout(t, func() { code = run("ls", img) })
			if code != subcmd.ExitOK {
				t.Fatalf("ls = %d\n%s", code, out)
			}
			if !strings.Contains(strings.ToUpper(out), strings.ToUpper(tc.want)) {
				t.Errorf("ls did not list %q:\n%s", tc.want, out)
			}
		})
	}
}

func TestExtractsEachFormat(t *testing.T) {
	for _, tc := range []struct {
		name  string
		image func(*testing.T) string
		want  string
	}{
		{"fat", fatImage, "HELLO.TXT"},
		{"ext", extImage, "hello.txt"},
		{"iso", isoImage, testiso.FileName},
	} {
		t.Run(tc.name, func(t *testing.T) {
			img := tc.image(t)
			dir := filepath.Join(t.TempDir(), "out")
			var code int
			testout.CaptureStdout(t, func() { code = run("extract", "-o", dir, img) })
			if code != subcmd.ExitOK {
				t.Fatalf("extract = %d", code)
			}
			if _, err := os.Stat(filepath.Join(dir, tc.want)); err != nil {
				t.Errorf("extract did not write %s: %v", tc.want, err)
			}
		})
	}
}

// -fs overrides detection, and it has to produce the chosen format's own error
// rather than "nothing recognized".
func TestForcedFormat(t *testing.T) {
	img := extImage(t)
	var code int
	out := testout.CaptureStdout(t, func() { code = run("info", "-fs", "ext", img) })
	if code != subcmd.ExitOK {
		t.Fatalf("info -fs ext = %d\n%s", code, out)
	}
	if !strings.Contains(out, "ext2/ext3") {
		t.Errorf("info -fs ext did not open the volume:\n%s", out)
	}

	if code := run("info", "-fs", "iso", img); code != subcmd.ExitFailure {
		t.Errorf("info -fs iso on an ext2 image = %d; want ExitFailure", code)
	}
	if code := run("info", "-fs", "ntfs", img); code != subcmd.ExitUsage {
		t.Errorf("info -fs ntfs = %d; want ExitUsage", code)
	}
}

// A flag belongs to one format. Passing another format's flag is the
// operator's mistake, however late it is noticed, the same as -geometry given
// for an image type that has no geometry.
func TestAFlagFromAnotherFormatIsAUsageError(t *testing.T) {
	ext := extImage(t)
	fat := fatImage(t)

	if code := run("ls", "-fat-policy", "copy0", ext); code != subcmd.ExitUsage {
		t.Errorf("-fat-policy on an ext2 image = %d; want ExitUsage", code)
	}
	if code := run("ls", "-sector-size", "2048", ext); code != subcmd.ExitUsage {
		t.Errorf("-sector-size on an ext2 image = %d; want ExitUsage", code)
	}
	// Its own format's flag works, and does not imply -fs.
	var code int
	testout.CaptureStdout(t, func() { code = run("ls", "-fat-policy", "copy0", fat) })
	if code != subcmd.ExitOK {
		t.Errorf("-fat-policy on a FAT image = %d; want ExitOK", code)
	}
}

// orphans is in the usage listing for every format, because the command is
// chosen before the image is opened. Against a volume that does not support it
// it is a usage error, the same as a flag that does not apply.
func TestOrphansOnlyAnswersForFAT(t *testing.T) {
	if code := run("orphans", extImage(t)); code != subcmd.ExitUsage {
		t.Errorf("orphans on an ext2 image = %d; want ExitUsage", code)
	}
	var code int
	testout.CaptureStdout(t, func() { code = run("orphans", fatImage(t)) })
	if code != subcmd.ExitOK {
		t.Errorf("orphans on a sound FAT image = %d; want ExitOK", code)
	}
}

// cat works the same way for every format.
func TestCatWorksForEveryFormat(t *testing.T) {
	for _, tc := range []struct {
		name  string
		image func(*testing.T) string
		path  string
	}{
		{"fat", fatImage, "HELLO.TXT"},
		{"ext", extImage, "hello.txt"},
		{"iso", isoImage, testiso.FileName},
	} {
		t.Run(tc.name, func(t *testing.T) {
			img := tc.image(t)
			var code int
			out := testout.CaptureStdout(t, func() { code = run("cat", img, tc.path) })
			if code != subcmd.ExitOK {
				t.Fatalf("cat = %d", code)
			}
			if out == "" {
				t.Error("cat produced no output")
			}
		})
	}
}

func TestUsageErrors(t *testing.T) {
	img := extImage(t)
	for _, tc := range []struct {
		name string
		args []string
		cmd  string
	}{
		{"info without an image", nil, "info"},
		{"info with extra arguments", []string{img, "extra"}, "info"},
		{"cat without a path", []string{img}, "cat"},
		{"verify without an image", nil, "verify"},
		{"offset with partition", []string{"-offset", "0", "-partition", "1", img}, "info"},
		{"map with no-map", []string{"-map", "x", "-no-map", img}, "info"},
		{"geometry off an 86F", []string{"-geometry", "18:2:512", img}, "info"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if code := run(tc.cmd, tc.args...); code != subcmd.ExitUsage {
				t.Errorf("%s = %d; want ExitUsage", tc.name, code)
			}
		})
	}
}

// A partition number the table does not hold is the operator asking for
// something that is not there. It can only be judged once the table has been
// read, after the flag parser has finished, and it is still a usage error.
func TestPartitionNotInTheTableIsAUsageError(t *testing.T) {
	if code := run("info", "-partition", "9", extImage(t)); code != subcmd.ExitUsage {
		t.Errorf("-partition 9 = %d; want ExitUsage", code)
	}
}

func TestVerifyEachFormat(t *testing.T) {
	for _, tc := range []struct {
		name  string
		image func(*testing.T) string
	}{
		{"fat", fatImage}, {"ext", extImage}, {"iso", isoImage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var code int
			out := testout.CaptureStdout(t, func() { code = run("verify", tc.image(t)) })
			if code != subcmd.ExitOK {
				t.Errorf("verify = %d\n%s", code, out)
			}
			if !strings.Contains(out, "Result:      OK") {
				t.Errorf("verify verdict missing:\n%s", out)
			}
		})
	}
}

// hfsImage is the committed Mac volume: a real 1989 disk whose files carry real
// Finder type and creator codes. That is better evidence than a synthetic
// builder, which would share the parser's assumptions.
func hfsImage(t *testing.T) string {
	t.Helper()
	return testfixture.File(t, filepath.Join("..", "..", "hfs", "testdata", "disk.img.gz"))
}

// Covers the path from a real catalog's bytes to the rendered line. A classic
// Mac file records its kind in the catalog rather than in its name, and the
// listing shows the codes as the volume recorded them.
func TestListNamesWhatAMacFileIs(t *testing.T) {
	img := hfsImage(t)
	var code int
	out := testout.CaptureStdout(t, func() { code = run("ls", "-l", img) })
	if code != subcmd.ExitOK {
		t.Fatalf("ls -l = %d\n%s", code, out)
	}
	if !strings.Contains(out, "STAK/WILD  TwinPeaksTrivia") {
		t.Errorf("the listing does not name what the file is:\n%s", out)
	}
	// The short listing is what a caller pipes and stays one name per line.
	plain := testout.CaptureStdout(t, func() { run("ls", img) })
	if strings.Contains(plain, "STAK") {
		t.Errorf("the short listing grew a column:\n%s", plain)
	}
}

// The table has no extension for any file kind on this volume, so even with
// -ext a whole extract leaves every name exactly as the volume spells it. This
// shows that an unknown code produces nothing rather than a guess.
func TestExtractLeavesAnUnnamedKindAlone(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "out")
	var code int
	testout.CaptureStdout(t, func() { code = run("extract", "-ext", "-o", dir, hfsImage(t)) })
	if code != subcmd.ExitOK {
		t.Fatalf("extract = %d", code)
	}
	for _, name := range []string{"TwinPeaksTrivia", "Desktop DB", "._TwinPeaksTrivia"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s was not written under the volume's own name: %v", name, err)
		}
	}
}
