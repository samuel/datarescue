package fatdrv

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"samuelks.com/datarescue/fat"
	"samuelks.com/datarescue/internal/fstool"
	"samuelks.com/datarescue/internal/testfat"
)

const (
	damagedFile = "BIG.BIN"
	damagedSize = 4*testfat.ClusterSize - 137
	soundFile   = "SMALL.TXT"
	soundSize   = 20
)

// buildImage writes a synthetic FAT16 image, optionally with a ddrescue map
// marking one sector of BIG.BIN's second cluster unrecovered. testfat.Build
// returns each file's byte range, so the hole is placed without asking the code
// under test where anything is.
func buildImage(t *testing.T, damaged bool) (image string, holeOff int64) {
	t.Helper()
	im := testfat.Build(testfat.Options{
		Files: []testfat.File{
			{Name: damagedFile, Size: damagedSize},
			{Name: soundFile, Size: soundSize},
		},
		Dirs: []testfat.Dir{{Name: "SUB", Files: []testfat.File{{Name: "INNER.TXT", Size: 12}}}},
	})
	dir := t.TempDir()
	image = filepath.Join(dir, "disk.img")
	if err := os.WriteFile(image, im.Data, 0o600); err != nil {
		t.Fatal(err)
	}
	if !damaged {
		return image, 0
	}
	p := im.Find(damagedFile)
	holeOff = testfat.ClusterSize + testfat.SectorSize
	badOff := p.Offset + holeOff

	var b strings.Builder
	b.WriteString("# Mapfile. Created by a test.\n0x00000000     +               1\n#      Start        Size  Status\n")
	fmt.Fprintf(&b, "0x%08x  0x%08x  +\n", 0, badOff)
	fmt.Fprintf(&b, "0x%08x  0x%08x  -\n", badOff, testfat.SectorSize)
	fmt.Fprintf(&b, "0x%08x  0x%08x  +\n", badOff+testfat.SectorSize,
		int64(len(im.Data))-badOff-testfat.SectorSize)
	if err := os.WriteFile(image+".map", []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return image, holeOff
}

func open(t *testing.T, image string) (*fstool.Context, *volume) {
	t.Helper()
	drv := New()
	d, im, hint, err := fstool.Detect(image, &fstool.CommonFlags{Offset: -1}, []fstool.Driver{drv})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { im.Close() }) //nolint:errcheck // test cleanup
	vol, err := d.Open(im, hint)
	if err != nil {
		t.Fatal(err)
	}
	return &fstool.Context{Driver: d, Volume: vol, Image: im, Path: image,
		Tool: "fstool", Command: "test"}, vol.(*volume)
}

func TestDetectsAFATVolume(t *testing.T) {
	image, _ := buildImage(t, false)
	c, _ := open(t, image)
	if c.Driver.Name() != "fat" {
		t.Errorf("driver = %s; want fat", c.Driver.Name())
	}
}

func TestWalkYieldsFilesAndSubdirectories(t *testing.T) {
	image, _ := buildImage(t, false)
	_, vol := open(t, image)
	got := map[string]fstool.Entry{}
	if _, err := vol.Walk(func(e fstool.Entry) error {
		got[e.Path] = e
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{damagedFile, soundFile, "SUB", "SUB/INNER.TXT"} {
		if _, ok := got[want]; !ok {
			t.Errorf("walk did not yield %q; got %v", want, sortedKeys(got))
		}
	}
	if !got["SUB"].Dir {
		t.Error("SUB is not marked a directory")
	}
	if got[damagedFile].Size != damagedSize {
		t.Errorf("%s size = %d; want %d", damagedFile, got[damagedFile].Size, damagedSize)
	}
	if got["SUB"].Mode[0] != 'd' {
		t.Errorf("SUB mode = %q; want it to start with d", got["SUB"].Mode)
	}
}

// The two failures must stay apart: the volume not holding the path is a
// failure of the run, and a directory on the way that would not read is damage
// to the medium. They get different exit codes.
func TestLookupSeparatesNotFoundFromAReadFailure(t *testing.T) {
	image, _ := buildImage(t, false)
	_, vol := open(t, image)

	e, err := vol.Lookup(soundFile)
	if err != nil {
		t.Fatalf("Lookup(%s) = %v", soundFile, err)
	}
	if e.Size != soundSize {
		t.Errorf("size = %d; want %d", e.Size, soundSize)
	}

	if _, err := vol.Lookup("NOPE.TXT"); !errors.Is(err, fat.ErrNotFound) {
		t.Errorf("Lookup of an absent file = %v; want fat.ErrNotFound", err)
	}
	if _, err := vol.Lookup("SUB/NOPE.TXT"); !errors.Is(err, fat.ErrNotFound) {
		t.Errorf("Lookup of an absent nested file = %v; want fat.ErrNotFound", err)
	}
}

func TestLookupResolvesASubdirectory(t *testing.T) {
	image, _ := buildImage(t, false)
	_, vol := open(t, image)
	e, err := vol.Lookup("SUB/INNER.TXT")
	if err != nil {
		t.Fatal(err)
	}
	r, err := vol.Open(e)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 12 {
		t.Errorf("read %d bytes; want 12", len(data))
	}
}

// Damage reads nothing. The cluster chain gives where the file lives and the
// map gives what was recovered, so the files a bad region affected can be
// reported before anything reads them.
func TestDamageNamesWhatTheMapCostWithoutReading(t *testing.T) {
	image, _ := buildImage(t, true)
	_, vol := open(t, image)

	big, err := vol.Lookup(damagedFile)
	if err != nil {
		t.Fatal(err)
	}
	lost, ranges := vol.Damage(big)
	if lost != testfat.SectorSize {
		t.Errorf("%s lost = %d; want %d", damagedFile, lost, testfat.SectorSize)
	}
	if ranges != 1 {
		t.Errorf("%s ranges = %d; want 1", damagedFile, ranges)
	}

	small, err := vol.Lookup(soundFile)
	if err != nil {
		t.Fatal(err)
	}
	if lost, _ := vol.Damage(small); lost != 0 {
		t.Errorf("%s lost = %d; want 0 — the hole is not in this file", soundFile, lost)
	}
}

// Without a map nothing is known to be missing: an absent map is not an empty
// one.
func TestDamageClaimsNothingWithoutAMap(t *testing.T) {
	image, _ := buildImage(t, false)
	_, vol := open(t, image)
	e, err := vol.Lookup(damagedFile)
	if err != nil {
		t.Fatal(err)
	}
	if lost, _ := vol.Damage(e); lost != 0 {
		t.Errorf("lost = %d with no map; want 0", lost)
	}
}

// A reader given no armed log stops at the damage rather than delivering
// unrecorded zeros.
func TestReadLatchesAtTheDamage(t *testing.T) {
	image, holeOff := buildImage(t, true)
	_, vol := open(t, image)
	e, err := vol.Lookup(damagedFile)
	if err != nil {
		t.Fatal(err)
	}
	r, err := vol.Open(e)
	if err != nil {
		t.Fatal(err)
	}
	n, err := io.Copy(io.Discard, r)
	if err == nil {
		t.Fatal("reading over the hole returned no error")
	}
	if n != holeOff {
		t.Errorf("recovered %d bytes before the damage; want %d", n, holeOff)
	}
}

func TestOrphansAnswersForFAT(t *testing.T) {
	image, _ := buildImage(t, false)
	_, vol := open(t, image)
	o, ok := any(vol).(fstool.Orphaner)
	if !ok {
		t.Fatal("the FAT volume does not implement Orphaner")
	}
	var b strings.Builder
	found, err := o.Orphans(&b, false)
	if err != nil {
		t.Fatal(err)
	}
	if found != 0 {
		t.Errorf("found %d orphans on a sound volume; want 0", found)
	}
	if !strings.Contains(b.String(), "No orphaned cluster chains") {
		t.Errorf("orphans output = %q", b.String())
	}
}

func TestInfoReportsTheGeometry(t *testing.T) {
	image, _ := buildImage(t, false)
	_, vol := open(t, image)
	var b strings.Builder
	if err := vol.Info(&b); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Type:", "Sector Size:", "Cluster Size:", "FAT Copies:"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("info missing %q:\n%s", want, b.String())
		}
	}
}

func TestVerifyReportsACleanVolume(t *testing.T) {
	image, _ := buildImage(t, false)
	_, vol := open(t, image)
	var b strings.Builder
	rep := fstool.NewReport(&b)
	vol.Verify(rep, false)
	if rep.Issues() != 0 {
		t.Errorf("a sound volume reported %d issue(s):\n%s", rep.Issues(), b.String())
	}
	if !strings.Contains(b.String(), "FAT tables: consistent") {
		t.Errorf("verify did not report the tables consistent:\n%s", b.String())
	}
}

func TestProbeRejectsSomethingElse(t *testing.T) {
	v := New().Probe(zeroReader{}, 0)
	if v.Kind != fstool.No {
		t.Errorf("probe of zeros = %v; want No — that is not FAT, and not damage", v.Kind)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }
func (zeroReader) Seek(off int64, whence int) (int64, error) {
	if whence == io.SeekEnd {
		return 1 << 20, nil
	}
	return off, nil
}

func sortedKeys(m map[string]fstool.Entry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
