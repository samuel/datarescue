package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"samuelks.com/datarescue/internal/outsink"
	"samuelks.com/datarescue/internal/subcmd"
	"samuelks.com/datarescue/internal/test1step"
	"samuelks.com/datarescue/internal/testarchive"
	"samuelks.com/datarescue/internal/testout"
)

// backup writes a synthetic set and returns the paths, catalog-carrying disk
// last, as the tool would receive them.
func backup(t *testing.T, opts test1step.Options) []string {
	t.Helper()
	paths, err := test1step.WriteSet(t.TempDir(), opts)
	if err != nil {
		t.Fatal(err)
	}
	return paths
}

func TestCommands(t *testing.T) {
	paths := backup(t, test1step.Options{Compress: true})

	out := testout.CaptureStdout(t, func() {
		if code := runInfo(paths); code != subcmd.ExitOK {
			t.Errorf("info = %d", code)
		}
	})
	for _, want := range []string{"Job:", test1step.Description, "Compressed:   yes", test1step.Drive} {
		if !strings.Contains(out, want) {
			t.Errorf("info output does not mention %q:\n%s", want, out)
		}
	}

	out = testout.CaptureStdout(t, func() {
		if code := runLS(append([]string{"-l"}, paths...)); code != subcmd.ExitOK {
			t.Errorf("ls = %d", code)
		}
	})
	for _, f := range test1step.DefaultFiles() {
		if !strings.Contains(out, f.Path()) {
			t.Errorf("ls output does not list %q:\n%s", f.Path(), out)
		}
	}
	// A compressed backup of incompressible data still stores a chunk, and the
	// listing says so.
	if !strings.Contains(out, "stored") || !strings.Contains(out, "deflated") {
		t.Errorf("ls -l does not distinguish stored from deflated chunks:\n%s", out)
	}

	out = testout.CaptureStdout(t, func() {
		if code := runVerify(paths); code != subcmd.ExitOK {
			t.Errorf("verify = %d", code)
		}
	})
	if !strings.Contains(out, "checksummed") {
		t.Errorf("verify output does not say what it checked:\n%s", out)
	}

	want := test1step.DefaultFiles()[1]
	out = testout.CaptureStdout(t, func() {
		if code := runCat(append([]string{"-path", want.Path()}, paths...)); code != subcmd.ExitOK {
			t.Errorf("cat = %d", code)
		}
	})
	if out != string(want.Data) {
		t.Errorf("cat wrote %d bytes, want %d", len(out), len(want.Data))
	}
}

func TestExtract(t *testing.T) {
	paths := backup(t, test1step.Options{Compress: true})
	dir := t.TempDir()
	if code := runExtract(append([]string{"-o", dir}, paths...)); code != subcmd.ExitOK {
		t.Fatalf("extract = %d", code)
	}
	for _, f := range test1step.DefaultFiles() {
		// The drive letter's colon cannot survive as a path component.
		parts := append([]string{dir, "C_"}, strings.Split(f.Dir, `\`)...)
		parts = append(parts, f.Name)
		var clean []string
		for _, p := range parts {
			if p != "" {
				clean = append(clean, p)
			}
		}
		path := filepath.Join(clean...)
		got, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: %s", f.Path(), err)
			continue
		}
		if string(got) != string(f.Data) {
			t.Errorf("%s: %d bytes extracted, want %d", f.Path(), len(got), len(f.Data))
		}
	}

	// Refuses to overwrite without -f.
	if code := runExtract(append([]string{"-o", dir}, paths...)); code != subcmd.ExitFailure {
		t.Errorf("re-extract = %d; want %d", code, subcmd.ExitFailure)
	}
	if code := runExtract(append([]string{"-f", "-o", dir}, paths...)); code != subcmd.ExitOK {
		t.Errorf("re-extract with -f = %d", code)
	}
}

func TestExtractPathFilter(t *testing.T) {
	paths := backup(t, test1step.Options{})
	dir := t.TempDir()
	args := append([]string{"-o", dir, "-path", `C:\DOS\SUB`}, paths...)
	if code := runExtract(args); code != subcmd.ExitOK {
		t.Fatalf("extract = %d", code)
	}
	if _, err := os.Stat(filepath.Join(dir, "C_", "DOS", "SUB", "NESTED.TXT")); err != nil {
		t.Errorf("the selected file was not extracted: %s", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "C_", "ROOT.TXT")); err == nil {
		t.Error("a file outside the selected directory was extracted")
	}
}

// TestVerifyCatchesABadChecksum checks that verify fails on a chunk whose data
// does not match the catalog, which is what the command exists for.
func TestVerifyCatchesABadChecksum(t *testing.T) {
	paths := backup(t, test1step.Options{Compress: true, BadChecksum: 2})
	out := testout.CaptureStdout(t, func() {
		if code := runVerify(paths); code != subcmd.ExitDegraded {
			t.Errorf("verify = %d; want %d", code, subcmd.ExitDegraded)
		}
	})
	if !strings.Contains(out, "did not verify") {
		t.Errorf("verify output does not report the failure:\n%s", out)
	}
}

func TestCatalogAndDBF(t *testing.T) {
	paths := backup(t, test1step.Options{})

	out := testout.CaptureStdout(t, func() {
		if code := runCatalog(append([]string{"-l"}, paths...)); code != subcmd.ExitOK {
			t.Errorf("catalog -l = %d", code)
		}
	})
	for _, want := range []string{"IOAFILE.DBF", "IOAARCF.DBF", "IOSESS.CDX"} {
		if !strings.Contains(out, want) {
			t.Errorf("catalog -l does not list %q:\n%s", want, out)
		}
	}

	dir := t.TempDir()
	if code := runCatalog(append([]string{"-o", dir}, paths...)); code != subcmd.ExitOK {
		t.Fatalf("catalog = %d", code)
	}
	table := filepath.Join(dir, "IOAFILE.DBF")
	if _, err := os.Stat(table); err != nil {
		t.Fatalf("the file table was not written: %s", err)
	}

	out = testout.CaptureStdout(t, func() {
		if code := runDBF([]string{"-rows", "3", table}); code != subcmd.ExitOK {
			t.Errorf("dbf = %d", code)
		}
	})
	for _, want := range []string{"SERIAL", "SIZE_LO", "ROOT.TXT"} {
		if !strings.Contains(out, want) {
			t.Errorf("dbf output does not mention %q:\n%s", want, out)
		}
	}
}

func TestUsageErrors(t *testing.T) {
	paths := backup(t, test1step.Options{})
	set := backup(t, test1step.Options{Disks: 2})
	tests := []struct {
		name string
		run  func([]string) int
		args []string
		want int
	}{
		{"info without a file", runInfo, nil, subcmd.ExitUsage},
		{"ls without a file", runLS, nil, subcmd.ExitUsage},
		{"cat without a path", runCat, paths, subcmd.ExitUsage},
		{"cat with two paths", runCat, append([]string{"-path", "a", "-path", "b"}, paths...), subcmd.ExitUsage},
		{"cat for a path not in the backup", runCat, append([]string{"-path", `C:\NOPE`}, paths...), subcmd.ExitFailure},
		{"ls with a path that matches nothing", runLS, append([]string{"-path", `C:\NOPE`}, paths...), subcmd.ExitFailure},
		{"info on a file that is not a backup", runInfo, []string{notABackup(t)}, subcmd.ExitFailure},
		{"map with more than one image", runInfo, append([]string{"-map", "x.map"}, set...), subcmd.ExitUsage},
		{"dbf without a file", runDBF, nil, subcmd.ExitUsage},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			testout.CaptureStdout(t, func() {
				if code := tc.run(tc.args); code != tc.want {
					t.Errorf("got %d, want %d", code, tc.want)
				}
			})
		})
	}
}

func notABackup(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "notabackup.1-Step")
	if err := os.WriteFile(path, []byte("this is not a backup"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// member is the archive member name a backed-up file lands under: the drive
// letter's colon cannot survive as a path component.
func member(f test1step.File) string {
	parts := []string{"C_"}
	if f.Dir != "" {
		parts = append(parts, strings.Split(f.Dir, `\`)...)
	}
	return strings.Join(append(parts, f.Name), "/")
}

func TestExtractArchive(t *testing.T) {
	paths := backup(t, test1step.Options{Compress: true})
	tests := []struct {
		format string
		ext    string
		read   func(testing.TB, string) map[string]testarchive.Member
	}{
		{"zip", ".zip", testarchive.Zip},
		{"tgz", ".tgz", testarchive.TGZ},
	}
	for _, tc := range tests {
		t.Run(tc.format, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "recovered"+tc.ext)
			args := append([]string{"-format", tc.format, "-o", out}, paths...)
			if code := runExtract(args); code != subcmd.ExitOK {
				t.Fatalf("extract = %d", code)
			}
			members := tc.read(t, out)
			for _, f := range test1step.DefaultFiles() {
				m, ok := members[member(f)]
				if !ok {
					t.Errorf("members = %v; want %s", testarchive.Names(members), member(f))
					continue
				}
				if m.Data != string(f.Data) {
					t.Errorf("%s: %d bytes, want %d", member(f), len(m.Data), len(f.Data))
				}
				// The catalog records a modification time, and the member keeps it.
				if got := m.ModTime.Year(); got != test1step.Created.Year() {
					t.Errorf("%s mtime = %s; want %d", member(f), m.ModTime, test1step.Created.Year())
				}
			}
			if _, ok := members[outsink.ManifestName]; !ok {
				t.Errorf("members = %v; want %s", testarchive.Names(members), outsink.ManifestName)
			}

			// A second run must refuse rather than truncate the archive.
			if code := runExtract(args); code != subcmd.ExitFailure {
				t.Errorf("re-extract = %d; want %d", code, subcmd.ExitFailure)
			}
			if code := runExtract(append([]string{"-f"}, args...)); code != subcmd.ExitOK {
				t.Errorf("re-extract with -f = %d", code)
			}
		})
	}
}

// TestExtractArchiveToStdout checks -o - so a restore can be piped straight
// into another machine without landing on this one's disk.
func TestExtractArchiveToStdout(t *testing.T) {
	paths := backup(t, test1step.Options{Compress: true})
	var code int
	out := testout.CaptureStdout(t, func() {
		code = runExtract(append([]string{"-format", "tgz", "-o", "-"}, paths...))
	})
	if code != subcmd.ExitOK {
		t.Fatalf("extract = %d", code)
	}
	path := filepath.Join(t.TempDir(), "piped.tgz")
	if err := os.WriteFile(path, []byte(out), 0o600); err != nil {
		t.Fatal(err)
	}
	members := testarchive.TGZ(t, path)
	if got := members["C_/ROOT.TXT"].Data; got == "" {
		t.Errorf("members = %v; want C_/ROOT.TXT", testarchive.Names(members))
	}
}

// TestExtractArchiveKeepsDamagedFiles checks a file whose checksum did not
// match is still in the archive, listed in the report, with a non-zero exit.
func TestExtractArchiveKeepsDamagedFiles(t *testing.T) {
	paths := backup(t, test1step.Options{Compress: true, BadChecksum: 2})
	out := filepath.Join(t.TempDir(), "recovered.zip")
	args := append([]string{"-format", "zip", "-o", out}, paths...)
	if code := runExtract(args); code != subcmd.ExitDegraded {
		t.Fatalf("extract of a damaged backup = %d; want %d", code, subcmd.ExitDegraded)
	}
	members := testarchive.Zip(t, out)
	for _, f := range test1step.DefaultFiles() {
		if _, ok := members[member(f)]; !ok {
			t.Errorf("members = %v; want %s kept despite the damage", testarchive.Names(members), member(f))
		}
	}
	report := members[outsink.ManifestName].Data
	if !strings.Contains(report, "checksum") {
		t.Errorf("%s does not name the damage:\n%s", outsink.ManifestName, report)
	}
}
