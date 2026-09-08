package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"samuelks.com/datarescue/ddrescue"
	"samuelks.com/datarescue/internal/subcmd"
	"samuelks.com/datarescue/internal/testarchive"
	"samuelks.com/datarescue/internal/testout"
	"samuelks.com/datarescue/internal/testqic"
	"samuelks.com/datarescue/qic"
)

// backup writes a synthetic archive and returns its path.
func backup(t *testing.T, opts testqic.Options) string {
	t.Helper()
	path, err := testqic.Write(t.TempDir(), opts)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCommands(t *testing.T) {
	path := backup(t, testqic.Options{Compress: true})

	out := testout.CaptureStdout(t, func() {
		if code := runInfo([]string{path}); code != subcmd.ExitOK {
			t.Errorf("info = %d, want %d", code, subcmd.ExitOK)
		}
	})
	for _, want := range []string{testqic.Label, "Windows 95", "QIC-122", "Drive:", "Regions:", "Contents:"} {
		if !strings.Contains(out, want) {
			t.Errorf("info does not mention %q:\n%s", want, out)
		}
	}

	out = testout.CaptureStdout(t, func() {
		if code := runLS([]string{"-l", "-d", path}); code != subcmd.ExitOK {
			t.Errorf("ls = %d", code)
		}
	})
	for _, want := range []string{`C:\README.TXT`, `C:\SRC\LIB\NET\socket.c`, `C:\TEMP\SCRATCH`} {
		if !strings.Contains(out, want) {
			t.Errorf("ls -l does not list %q:\n%s", want, out)
		}
	}

	out = testout.CaptureStdout(t, func() {
		if code := runCat([]string{path, `C:\README.TXT`}); code != subcmd.ExitOK {
			t.Errorf("cat = %d", code)
		}
	})
	if want := "recovered from a synthetic archive"; !strings.Contains(out, want) {
		t.Errorf("cat gave %q, want it to contain %q", out, want)
	}

	out = testout.CaptureStdout(t, func() {
		if code := runVerify([]string{path}); code != subcmd.ExitOK {
			t.Errorf("verify = %d, want %d for an undamaged archive", code, subcmd.ExitOK)
		}
	})
	if !strings.Contains(out, "whole") {
		t.Errorf("verify does not report what came back whole:\n%s", out)
	}

	out = testout.CaptureStdout(t, func() {
		if code := runSegments([]string{path}); code != subcmd.ExitOK {
			t.Errorf("segments = %d", code)
		}
	})
	if !strings.Contains(out, "compressed, ok") {
		t.Errorf("segments does not describe the segments:\n%s", out)
	}

	out = testout.CaptureStdout(t, func() {
		if code := runCatalog([]string{"-l", path}); code != subcmd.ExitOK {
			t.Errorf("catalog = %d", code)
		}
	})
	if !strings.Contains(out, "flag=0x") {
		t.Errorf("catalog does not show the record fields:\n%s", out)
	}
}

// TestExtractDir checks that a restored tree carries the right bytes and that
// a name off the medium is made safe to create.
func TestExtractDir(t *testing.T) {
	files := testqic.DefaultFiles()
	path := backup(t, testqic.Options{Compress: true, Files: files})
	out := filepath.Join(t.TempDir(), "out")

	if code := runExtract([]string{"-o", out, path}); code != subcmd.ExitOK {
		t.Fatalf("extract = %d, want %d", code, subcmd.ExitOK)
	}
	for _, f := range files {
		if f.IsDir {
			continue
		}
		// The drive letter's colon cannot be a path component on Windows, so
		// safepath replaces it.
		local := filepath.Join(out, "C_", filepath.FromSlash(strings.ReplaceAll(strings.TrimPrefix(f.Path(), `C:\`), `\`, "/")))
		got, err := os.ReadFile(local)
		if err != nil {
			t.Errorf("%s: %v", f.Path(), err)
			continue
		}
		if string(got) != string(f.Data) {
			t.Errorf("%s: %d bytes on disk, want %d", f.Path(), len(got), len(f.Data))
		}
	}
}

// TestExtractArchive checks both archive formats, including that the report
// member is written and that an existing archive is not replaced without -f.
func TestExtractArchive(t *testing.T) {
	files := testqic.DefaultFiles()
	path := backup(t, testqic.Options{Compress: true, Files: files})

	for _, tt := range []struct {
		format, ext string
		read        func(testing.TB, string) map[string]testarchive.Member
	}{
		{"zip", ".zip", testarchive.Zip},
		{"tgz", ".tgz", testarchive.TGZ},
	} {
		t.Run(tt.format, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "backup"+tt.ext)
			if code := runExtract([]string{"-format", tt.format, "-o", out, path}); code != subcmd.ExitOK {
				t.Fatalf("extract = %d", code)
			}
			members := tt.read(t, out)
			if _, ok := members["datarescue-report.txt"]; !ok {
				t.Errorf("the archive carries no report; it holds %v", testarchive.Names(members))
			}
			for _, f := range files {
				if f.IsDir {
					continue
				}
				name := "C_/" + strings.ReplaceAll(strings.TrimPrefix(f.Path(), `C:\`), `\`, "/")
				m, ok := members[name]
				if !ok {
					t.Errorf("%s is missing; the archive holds %v", name, testarchive.Names(members))
					continue
				}
				if m.Data != string(f.Data) {
					t.Errorf("%s: %d bytes in the archive, want %d", name, len(m.Data), len(f.Data))
				}
			}

			if code := runExtract([]string{"-format", tt.format, "-o", out, path}); code == subcmd.ExitOK {
				t.Error("extract replaced an existing archive without -f")
			}
			if code := runExtract([]string{"-format", tt.format, "-o", out, "-f", path}); code != subcmd.ExitOK {
				t.Errorf("extract with -f = %d, want it to replace the archive", code)
			}
		})
	}
}

// TestDamagedExitStatus checks that a damaged archive is reported rather than
// passed off as a clean result, and that the files it did not damage still
// come out. The damage is the archive's, not the run's, so it is ExitDegraded:
// the files that were written are usable and the map says what is missing.
func TestDamagedExitStatus(t *testing.T) {
	path := backup(t, testqic.Options{Compress: true, BadSegment: 1})
	out := filepath.Join(t.TempDir(), "out")

	if code := runExtract([]string{"-o", out, path}); code != subcmd.ExitDegraded {
		t.Errorf("extract from a damaged archive = %d, want %d", code, subcmd.ExitDegraded)
	}
	var n int
	_ = filepath.Walk(out, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			n++
		}
		return nil
	})
	if n == 0 {
		t.Error("a corrupt segment cost every file")
	}

	code := subcmd.ExitOK
	testout.CaptureStdout(t, func() { code = runVerify([]string{path}) })
	if code != subcmd.ExitDegraded {
		t.Errorf("verify of a damaged archive = %d, want %d", code, subcmd.ExitDegraded)
	}
}

// TestDecompress checks that the whole data region can be written out and that
// what comes back is the size the segment offsets account for.
func TestDecompress(t *testing.T) {
	path := backup(t, testqic.Options{Compress: true})
	out := filepath.Join(t.TempDir(), "data.bin")
	if code := runDecompress([]string{"-o", out, path}); code != subcmd.ExitOK {
		t.Fatalf("decompress = %d", code)
	}
	a, err := qic.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close() //nolint:errcheck // read-only
	info, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != a.DataSize() {
		t.Errorf("decompress wrote %d bytes where the segment offsets account for %d", info.Size(), a.DataSize())
	}
	// A clean archive filled nothing, so there is nothing for a map to say and
	// the run is a faithful copy.
	if _, err := os.Stat(out + ".map"); err != nil {
		t.Errorf("no map was written beside the output: %s", err)
	}
}

// A segment that will not expand is zero-filled so that every later byte keeps
// the offset the archive gives it, and the map beside the output records which
// bytes those are. The archive was damaged and the run was not, so it exits
// degraded.
func TestDecompressDamaged(t *testing.T) {
	path := backup(t, testqic.Options{Compress: true, BadSegment: 1})
	out := filepath.Join(t.TempDir(), "data.bin")
	if code := runDecompress([]string{"-o", out, path}); code != subcmd.ExitDegraded {
		t.Fatalf("decompress of a damaged archive = %d; want %d", code, subcmd.ExitDegraded)
	}
	a, err := qic.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close() //nolint:errcheck // read-only

	// Filling must preserve the offsets.
	info, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != a.DataSize() {
		t.Errorf("decompress wrote %d bytes where the segment offsets account for %d", info.Size(), a.DataSize())
	}
	blocks, err := ddrescue.ParseMap(out + ".map")
	if err != nil {
		t.Fatalf("the zeros were written with no map to explain them: %s", err)
	}
	var bad int64
	for _, b := range blocks {
		if b.Status != ddrescue.BlockStatusFinished {
			bad += b.Size
		}
	}
	if bad == 0 {
		t.Error("the map records no unrecovered range, so it does not say which bytes are made up")
	}
}

// A stream has nowhere to keep the map, so it stops at the damage rather than
// emitting unrecorded zeros, the same rule that keeps cat latching.
func TestDecompressToStdoutRefusesToFill(t *testing.T) {
	path := backup(t, testqic.Options{Compress: true, BadSegment: 1})
	var code int
	testout.CaptureStdout(t, func() { code = runDecompress([]string{"-o", "-", path}) })
	if code != subcmd.ExitFailure {
		t.Errorf("decompress of a damaged archive to stdout = %d; want %d", code, subcmd.ExitFailure)
	}
}

// A run that stops part way, because the output stops taking bytes the way a
// full disk does, still leaves the map beside the output. Without it a later
// reader cannot tell the bytes that made it out from the holes that follow.
func TestDecompressStopsPartWayStillWritesTheMap(t *testing.T) {
	path := backup(t, testqic.Options{Compress: true, BadSegment: 1})
	a, err := qic.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close() //nolint:errcheck // read-only

	// Stop the output right at the first damaged segment, so the recovered
	// prefix makes it out and the zeros standing in for the damage do not.
	out := filepath.Join(t.TempDir(), "data.bin")
	restore := openOutput
	openOutput = func(string, bool) (io.WriteCloser, error) {
		return &stopWriter{at: a.Segments()[0].End()}, nil
	}
	defer func() { openOutput = restore }()

	code := subcmd.ExitOK
	testout.CaptureStdout(t, func() { code = runDecompress([]string{"-o", out, path}) })
	if code != subcmd.ExitFailure {
		t.Fatalf("decompress over an output that stopped = %d, want %d", code, subcmd.ExitFailure)
	}
	blocks, err := ddrescue.ParseMap(out + ".map")
	if err != nil {
		t.Fatalf("no map was written beside a stopped run: %s", err)
	}
	if got := ddrescue.UnrecoveredBytes(blocks, 0, a.DataSize()); got == 0 {
		t.Error("the map records nothing of what a stopped run never wrote")
	}
}

// stopWriter hands out a fixed number of bytes and then refuses any more, the
// way a full disk stops a run part way.
type stopWriter struct {
	at  int64
	got int64
}

func (w *stopWriter) Write(b []byte) (int, error) {
	left := w.at - w.got
	if left <= 0 {
		return 0, errors.New("output stopped taking bytes")
	}
	n := min(len(b), int(left))
	w.got += int64(n)
	if n < len(b) {
		return n, errors.New("output stopped taking bytes")
	}
	return n, nil
}

func (w *stopWriter) Close() error { return nil }

// TestUsageErrors covers every path that should reject the command line rather
// than do something surprising.
func TestUsageErrors(t *testing.T) {
	path := backup(t, testqic.Options{Compress: true})
	notQIC := filepath.Join(t.TempDir(), "empty.qic")
	if err := os.WriteFile(notQIC, make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct {
		name string
		run  func([]string) int
		args []string
		want int
	}{
		{"info without an archive", runInfo, nil, subcmd.ExitUsage},
		{"ls without an archive", runLS, nil, subcmd.ExitUsage},
		{"cat without a path", runCat, []string{path}, subcmd.ExitUsage},
		{"cat with two paths", runCat, []string{path, "a", "b"}, subcmd.ExitUsage},
		{"cat a path that is not there", runCat, []string{path, `C:\NOPE`}, subcmd.ExitFailure},
		{"verify with a -path that matches nothing", runVerify, []string{"-path", `C:\NOPE`, path}, subcmd.ExitFailure},
		{"an unknown source", runLS, []string{"-source", "wrong", path}, subcmd.ExitUsage},
		{"decompress without -o", runDecompress, []string{path}, subcmd.ExitUsage},
		{"a file that is not an archive", runInfo, []string{notQIC}, subcmd.ExitFailure},
		{"a volume that is not there", runInfo, []string{"-volume", "9", path}, subcmd.ExitFailure},
		{"an archive format with no -o", runExtract, []string{"-format", "zip", path}, subcmd.ExitUsage},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var code int
			testout.CaptureStdout(t, func() { code = tt.run(tt.args) })
			if code != tt.want {
				t.Errorf("%s = %d, want %d", tt.name, code, tt.want)
			}
		})
	}
}
