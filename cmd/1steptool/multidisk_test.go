package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"samuelks.com/datarescue/internal/subcmd"
	"samuelks.com/datarescue/internal/test1step"
	"samuelks.com/datarescue/internal/testout"
)

// TestInfoReportsEachDisk checks that info reports the chunk range on every
// disk of a set, which shows where a file's data has to be looked for.
func TestInfoReportsEachDisk(t *testing.T) {
	paths := backup(t, test1step.Options{Compress: true, Disks: 3})
	out := testout.CaptureStdout(t, func() {
		if code := runInfo(paths); code != subcmd.ExitOK {
			t.Errorf("info = %d", code)
		}
	})
	if got := strings.Count(out, "chunks "); got < 3 {
		t.Errorf("info names a chunk range %d times for a set of three disks:\n%s", got, out)
	}
	for _, want := range []string{"Disks in set: 3", "Sessions:", "disk 3:"} {
		if !strings.Contains(out, want) {
			t.Errorf("info output does not mention %q:\n%s", want, out)
		}
	}
}

// TestInfoReportsTruncatedDisk checks that a disk recovered only in part is
// called out with how much of it is gone, rather than passing for whole.
func TestInfoReportsTruncatedDisk(t *testing.T) {
	paths := backup(t, test1step.Options{Disks: 3, Truncate: map[int]int64{2: 4096}})
	out := testout.CaptureStdout(t, func() {
		if code := runInfo(paths); code != subcmd.ExitOK {
			t.Errorf("info = %d", code)
		}
	})
	if !strings.Contains(out, "truncated:") {
		t.Errorf("info does not report the short disk:\n%s", out)
	}
}

// TestExtractAcrossDisks restores a whole multi-disk set and checks the files
// against the bytes that went in, including the one split across disks.
func TestExtractAcrossDisks(t *testing.T) {
	want := test1step.DefaultFiles()
	paths := backup(t, test1step.Options{Compress: true, Disks: 3})
	out := filepath.Join(t.TempDir(), "restored")

	testout.CaptureStdout(t, func() {
		if code := runExtract(append([]string{"-o", out}, paths...)); code != subcmd.ExitOK {
			t.Errorf("extract = %d", code)
		}
	})
	// The drive letter's colon cannot appear in a path, so it is replaced.
	root := filepath.Join(out, strings.Replace(test1step.Drive, ":", "_", 1))
	for _, f := range want {
		got, err := os.ReadFile(filepath.Join(root, filepath.Join(strings.Split(f.Dir, `\`)...), f.Name))
		if err != nil {
			t.Errorf("%s: %s", f.Name, err)
			continue
		}
		if len(got) != len(f.Data) {
			t.Errorf("%s: restored %d bytes, want %d", f.Name, len(got), len(f.Data))
			continue
		}
		if string(got) != string(f.Data) {
			t.Errorf("%s: restored contents differ", f.Name)
		}
	}
}

// TestVerifyReportsUnreachableData checks the exit status and the count when a
// disk of the set is not supplied: the files on it are reported, not skipped.
func TestVerifyReportsUnreachableData(t *testing.T) {
	paths := backup(t, test1step.Options{Compress: true, Disks: 3})
	// Drop the middle disk; the catalog is on the last.
	out := testout.CaptureStdout(t, func() {
		if code := runVerify([]string{paths[0], paths[2]}); code != subcmd.ExitDegraded {
			t.Errorf("verify of an incomplete set = %d; want %d — the set is incomplete, the run was not",
				code, subcmd.ExitDegraded)
		}
	})
	if !strings.Contains(out, "on a disk that was not supplied") {
		t.Errorf("verify does not report the missing disk's files:\n%s", out)
	}
}
