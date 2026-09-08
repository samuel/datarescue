package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"samuelks.com/datarescue/fat"
	"samuelks.com/datarescue/internal/subcmd"
	"samuelks.com/datarescue/internal/testfat"
	"samuelks.com/datarescue/internal/testiso"
	"samuelks.com/datarescue/internal/testout"
)

const subDir = "SUB"

// mappedImage writes a synthetic FAT image and a ddrescue map marking one byte
// range of it unrecovered. testfat.Build returns each file's byte range, so the
// hole is placed without asking the code under test where anything is.
func mappedImage(t *testing.T, o testfat.Options, badOff, badLen int64) (string, *testfat.Image) {
	t.Helper()
	im := testfat.Build(o)
	image := filepath.Join(t.TempDir(), "disk.img")
	if err := os.WriteFile(image, im.Data, 0o600); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString("# Mapfile. Created by a test.\n0x00000000     +               1\n#      Start        Size  Status\n")
	fmt.Fprintf(&b, "0x%08x  0x%08x  +\n", 0, badOff)
	fmt.Fprintf(&b, "0x%08x  0x%08x  -\n", badOff, badLen)
	fmt.Fprintf(&b, "0x%08x  0x%08x  +\n", badOff+badLen, int64(len(im.Data))-badOff-badLen)
	if err := os.WriteFile(image+".map", []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return image, im
}

// withSubdir is a volume whose root holds one subdirectory with one file.
func withSubdir() testfat.Options {
	return testfat.Options{
		Files: []testfat.File{{Name: "TOP.TXT", Size: 40}},
		Dirs:  []testfat.Dir{{Name: subDir, Files: []testfat.File{{Name: "INNER.BIN", Size: 100}}}},
	}
}

// The sidecar is picked up without -map, and what it leaves unrecovered is
// reported. info describes the medium rather than judging it, so it still
// exits OK.
func TestInfoReportsTheRescueMap(t *testing.T) {
	im := testfat.Build(withSubdir())
	d := im.FindDir(subDir)
	img, _ := mappedImage(t, withSubdir(), d.Offset, testfat.SectorSize)

	var code int
	out := testout.CaptureStdout(t, func() { code = run("info", img) })
	if code != subcmd.ExitOK {
		t.Fatalf("info = %d\n%s", code, out)
	}
	if !strings.Contains(out, "disk.img.map") || !strings.Contains(out, "Unrecovered:") {
		t.Errorf("info did not report the map:\n%s", out)
	}

	out = testout.CaptureStdout(t, func() { code = run("info", "-no-map", img) })
	if !strings.Contains(out, "Rescue map:      none") {
		t.Errorf("-no-map did not ignore the sidecar:\n%s", out)
	}
}

// A subdirectory that would not read affects the exit code rather than being
// reported once and ignored, and the entries that did read are still listed.
func TestListReportsADamagedSubdirectory(t *testing.T) {
	im := testfat.Build(withSubdir())
	d := im.FindDir(subDir)
	img, _ := mappedImage(t, withSubdir(), d.Offset, int64(d.Clusters)*testfat.ClusterSize)

	var code int
	out := testout.CaptureStdout(t, func() { code = run("ls", img) })
	if code != subcmd.ExitDegraded {
		t.Errorf("ls over a subdirectory that would not read = %d; want ExitDegraded\n%s", code, out)
	}
	if !strings.Contains(out, "TOP.TXT") {
		t.Errorf("the listing lost the entries it could read:\n%s", out)
	}
}

// A path the volume does not hold is a failure of the run. A path under a
// directory that would not read is damage to the medium: the tool could not
// look, so it cannot say the path is absent.
func TestListPathClassification(t *testing.T) {
	img, _ := mappedImage(t, withSubdir(), 0, 0)
	var code int
	out := testout.CaptureStdout(t, func() { code = run("ls", img, "NOWHERE") })
	if code != subcmd.ExitFailure {
		t.Errorf("ls of a path the volume does not hold = %d; want ExitFailure\n%s", code, out)
	}

	im := testfat.Build(withSubdir())
	d := im.FindDir(subDir)
	img, _ = mappedImage(t, withSubdir(), d.Offset, int64(d.Clusters)*testfat.ClusterSize)
	out = testout.CaptureStdout(t, func() { code = run("ls", img, subDir+"/DEEPER") })
	if code != subcmd.ExitDegraded {
		t.Errorf("ls through a directory that would not read = %d; want ExitDegraded\n%s", code, out)
	}
}

// Asking for the file by path must not change the exit code: damage the
// whole-volume walk counts against the medium must not become a failure of the
// run just because the file was requested by path.
func TestExtractClassifiesANamedPathLikeTheWalk(t *testing.T) {
	// The first sector of FAT copy 0 holds the entries for every early cluster.
	// With -fat-policy copy0 there is no other copy to fall back to, so no
	// chain in that region resolves.
	im := testfat.Build(withSubdir())
	sound, err := fat.Open(bytes.NewReader(im.Data), 0)
	if err != nil {
		t.Fatal(err)
	}
	img, _ := mappedImage(t, withSubdir(), sound.FATTableOffsets[0], testfat.SectorSize)

	var whole, named int
	testout.CaptureStdout(t, func() {
		whole = run("extract", "-fat-policy", "copy0", "-o", t.TempDir(), img)
		named = run("extract", "-fat-policy", "copy0", "-o", t.TempDir(), img, "TOP.TXT")
	})
	if whole != subcmd.ExitDegraded {
		t.Errorf("extract of the whole volume = %d; want ExitDegraded", whole)
	}
	if named != whole {
		t.Errorf("extract of one named path = %d but of the whole volume = %d; "+
			"the same damage must report the same way", named, whole)
	}

	var absent int
	testout.CaptureStdout(t, func() {
		absent = run("extract", "-o", t.TempDir(), img, "NOWHERE.TXT")
	})
	if absent != subcmd.ExitFailure {
		t.Errorf("extract of a path the volume does not hold = %d; want ExitFailure", absent)
	}
}

// An empty file is not a failure to open. It has no first cluster, so resolving
// a chain for it would fail, and it must not be reported as unreadable.
func TestEmptyFile(t *testing.T) {
	img, _ := mappedImage(t, testfat.Options{Files: []testfat.File{
		{Name: "EMPTY.TXT", Size: 0},
		{Name: "FULL.TXT", Size: 40},
	}}, 0, 0)

	var code int
	out := testout.CaptureStdout(t, func() { code = run("cat", img, "EMPTY.TXT") })
	if code != subcmd.ExitOK {
		t.Errorf("cat of an empty file = %d; want ExitOK", code)
	}
	if out != "" {
		t.Errorf("cat of an empty file wrote %q", out)
	}

	dir := filepath.Join(t.TempDir(), "out")
	testout.CaptureStdout(t, func() { code = run("extract", "-o", dir, img, "EMPTY.TXT") })
	if code != subcmd.ExitOK {
		t.Errorf("extract of an empty file by name = %d; want ExitOK", code)
	}
	fi, err := os.Stat(filepath.Join(dir, "EMPTY.TXT"))
	if err != nil {
		t.Fatalf("the empty file was not extracted: %v", err)
	}
	if fi.Size() != 0 {
		t.Errorf("the extracted empty file is %d bytes", fi.Size())
	}
}

// A damaged file is written at its full length with the holes zero-filled and
// the map recording them written into the same sink beside it.
func TestExtractWritesTheGapMap(t *testing.T) {
	const name = "BIG.BIN"
	size := 4*testfat.ClusterSize - 137
	im := testfat.Build(testfat.Options{Files: []testfat.File{{Name: name, Size: size}}})
	p := im.Find(name)
	holeOff := int64(testfat.ClusterSize + testfat.SectorSize)
	img, _ := mappedImage(t, testfat.Options{Files: []testfat.File{{Name: name, Size: size}}},
		p.Offset+holeOff, testfat.SectorSize)

	dir := filepath.Join(t.TempDir(), "out")
	var code int
	testout.CaptureStdout(t, func() { code = run("extract", "-o", dir, img) })
	if code != subcmd.ExitDegraded {
		t.Errorf("extract over a hole = %d; want ExitDegraded", code)
	}
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != size {
		t.Errorf("file is %d bytes; want the full %d with the hole zero-filled", len(data), size)
	}
	hole := data[holeOff : holeOff+testfat.SectorSize]
	if !bytes.Equal(hole, make([]byte, testfat.SectorSize)) {
		t.Error("the unrecovered sector was not zero-filled")
	}
	mapText, err := os.ReadFile(filepath.Join(dir, name+".map"))
	if err != nil {
		t.Fatalf("no .map beside the filled file: %v", err)
	}
	if !strings.Contains(string(mapText), fmt.Sprintf("0x%08x", holeOff)) {
		t.Errorf("the map does not record the hole at offset %d:\n%s", holeOff, mapText)
	}
}

// cat has nowhere to put a map, so it stops at the damage rather than
// delivering unrecorded zeros.
func TestCatLatchesAtTheDamage(t *testing.T) {
	const name = "BIG.BIN"
	size := 4*testfat.ClusterSize - 137
	im := testfat.Build(testfat.Options{Files: []testfat.File{{Name: name, Size: size}}})
	p := im.Find(name)
	holeOff := int64(testfat.ClusterSize + testfat.SectorSize)
	img, _ := mappedImage(t, testfat.Options{Files: []testfat.File{{Name: name, Size: size}}},
		p.Offset+holeOff, testfat.SectorSize)

	var code int
	out := testout.CaptureStdout(t, func() { code = run("cat", img, name) })
	if code != subcmd.ExitDegraded {
		t.Errorf("cat over a hole = %d; want ExitDegraded", code)
	}
	if int64(len(out)) != holeOff {
		t.Errorf("cat wrote %d bytes before the damage; want %d", len(out), holeOff)
	}
}

// What a listing reports as lost is counted in each file's own bytes, not the
// image's. A raw sector is 2352 bytes on the medium but holds only 2048 of the
// file's, and a loss in medium bytes next to a size in file bytes invites the
// wrong subtraction.
func TestRawISOCountsTheLossInFileBytes(t *testing.T) {
	img := testiso.BuildRaw()
	path := filepath.Join(t.TempDir(), "raw.iso")
	if err := os.WriteFile(path, img, 0o600); err != nil {
		t.Fatal(err)
	}
	mapText := testiso.Damage(img, testiso.RawSectorSize, testiso.BigLBA+1)
	if err := os.WriteFile(path+".map", []byte(mapText), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, args := range [][]string{{path}, {"-sector-size", "2352", path}} {
		out := testout.CaptureStdout(t, func() { run("ls", args...) })
		if !strings.Contains(out, "2.00 KiB") {
			t.Errorf("ls %v did not quote the loss in the file's own bytes:\n%s", args, out)
		}
	}
}

// A lost primary volume descriptor is not "this is not an ISO", and the map
// that explains it is still reported.
func TestUnrecoveredPVDIsSaidPlainly(t *testing.T) {
	img := testiso.Build()
	path := filepath.Join(t.TempDir(), "test.iso")
	if err := os.WriteFile(path, img, 0o600); err != nil {
		t.Fatal(err)
	}
	mapText := testiso.Damage(img, testiso.SectorSize, 16)
	if err := os.WriteFile(path+".map", []byte(mapText), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := run("info", path); code != subcmd.ExitFailure {
		t.Errorf("info over a lost PVD = %d; want ExitFailure", code)
	}
}
