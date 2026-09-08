package isodrv

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"samuelks.com/datarescue/diskimage"
	"samuelks.com/datarescue/internal/fstool"
	"samuelks.com/datarescue/internal/testiso"
	"samuelks.com/datarescue/iso"
)

// damagedISO writes an image with the given sectors unrecovered and the map
// beside it recording them. The damaged sectors hold a recognizable non-zero
// pattern, so a tool that ignored the map returns 0xDB where one that honored
// it returns zeros.
func damagedISO(t *testing.T, lbas ...int) string {
	t.Helper()
	img := testiso.Build()
	path := filepath.Join(t.TempDir(), "test.iso")
	if err := os.WriteFile(path, img, 0o600); err != nil {
		t.Fatal(err)
	}
	if len(lbas) > 0 {
		mapText := testiso.Damage(img, testiso.SectorSize, lbas...)
		if err := os.WriteFile(path+".map", []byte(mapText), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func open(t *testing.T, path string, drv *Driver) (*fstool.Context, fstool.Volume) {
	t.Helper()
	if drv == nil {
		drv = New()
	}
	d, im, hint, err := fstool.Detect(path, &fstool.CommonFlags{Offset: -1}, []fstool.Driver{drv})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { im.Close() }) //nolint:errcheck // test cleanup
	vol, err := d.Open(im, hint)
	if err != nil {
		t.Fatal(err)
	}
	return &fstool.Context{Driver: d, Volume: vol, Image: im, Path: path,
		Tool: "fstool", Command: "test"}, vol
}

func TestDetectsAnISOAtOffsetZero(t *testing.T) {
	c, _ := open(t, damagedISO(t), nil)
	if c.Driver.Name() != "iso" {
		t.Errorf("driver = %s; want iso", c.Driver.Name())
	}
	if c.Image.Source != "offset 0 (no partition table)" {
		t.Errorf("Source = %q", c.Image.Source)
	}
}

// A lost primary volume descriptor is not "this is not an ISO". It is the case
// a rescue map exists for, and it has to be reported to the operator as such.
func TestUnrecoveredPVDIsADamagedVerdictNotARejection(t *testing.T) {
	path := damagedISO(t, 16)
	_, _, _, err := fstool.Detect(path, &fstool.CommonFlags{Offset: -1}, []fstool.Driver{New()})
	if err == nil {
		t.Fatal("detecting an image with a lost PVD returned no error")
	}
	if !errors.Is(err, iso.ErrPVDUnrecovered) {
		t.Errorf("Detect = %v; want it to carry iso.ErrPVDUnrecovered", err)
	}
}

// The map that explains why a volume could not be located is still returned
// with the failure, since this is the case the map exists for.
func TestTheMapTravelsWithTheFailure(t *testing.T) {
	path := damagedISO(t, 16)
	_, _, _, err := fstool.Detect(path, &fstool.CommonFlags{Offset: -1}, []fstool.Driver{New()})
	var nf *diskimage.NotFoundError
	if !errors.As(err, &nf) || nf.MapPath == "" {
		t.Fatalf("Detect = %v; want a *diskimage.NotFoundError carrying the map", err)
	}
}

func TestWalkYieldsEntries(t *testing.T) {
	_, vol := open(t, damagedISO(t), nil)
	got := map[string]fstool.Entry{}
	diags, err := vol.Walk(func(e fstool.Entry) error {
		got[e.Path] = e
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range diags {
		t.Errorf("unexpected walk diagnostic: %s", d)
	}
	if len(got) == 0 {
		t.Fatal("walk yielded nothing")
	}
	// Paths have neither a leading nor a trailing slash, whatever form the
	// reader uses.
	for p := range got {
		if strings.HasPrefix(p, "/") || strings.HasSuffix(p, "/") {
			t.Errorf("path %q is not in the shared spelling", p)
		}
	}
}

// The loss is counted in the file's own bytes, not the medium's.
func TestDamageIsCountedInFileBytes(t *testing.T) {
	_, vol := open(t, damagedISO(t, testiso.BigLBA+1), nil)
	var total int64
	if _, err := vol.Walk(func(e fstool.Entry) error {
		lost, _ := vol.Damage(e)
		total += lost
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if total != iso.UserDataSize {
		t.Errorf("loss = %d; want %d — one sector of the file's own bytes", total, iso.UserDataSize)
	}
}

// A raw image has 2352-byte sectors on the medium, each holding 2048 bytes of
// the file. The probe has to detect that without being told, and the loss is
// still counted in the file's bytes.
func TestRawImageProbesAndCountsInFileBytes(t *testing.T) {
	img := testiso.BuildRaw()
	path := filepath.Join(t.TempDir(), "raw.iso")
	if err := os.WriteFile(path, img, 0o600); err != nil {
		t.Fatal(err)
	}
	mapText := testiso.Damage(img, testiso.RawSectorSize, testiso.BigLBA+1)
	if err := os.WriteFile(path+".map", []byte(mapText), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, drv := range []*Driver{New(), {sectorSize: testiso.RawSectorSize}} {
		_, vol := open(t, path, drv)
		var total int64
		if _, err := vol.Walk(func(e fstool.Entry) error {
			lost, _ := vol.Damage(e)
			total += lost
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if total != iso.UserDataSize {
			t.Errorf("raw loss = %d; want %d — the file's bytes, not the medium's",
				total, iso.UserDataSize)
		}
	}
}

func TestOpenReadsAFile(t *testing.T) {
	_, vol := open(t, damagedISO(t), nil)
	var entry fstool.Entry
	if _, err := vol.Walk(func(e fstool.Entry) error {
		if !e.Dir && entry.Path == "" {
			entry = e
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if entry.Path == "" {
		t.Fatal("no file in the image")
	}
	r, err := vol.Open(entry)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(data)) != entry.Size {
		t.Errorf("read %d bytes; want %d", len(data), entry.Size)
	}
}

func TestInfoReportsTheVolume(t *testing.T) {
	_, vol := open(t, damagedISO(t), nil)
	var b strings.Builder
	if err := vol.Info(&b); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Volume label:", "Sector size:", "cooked", "Joliet:"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("info missing %q:\n%s", want, b.String())
		}
	}
}

func TestProbeRejectsSomethingElse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blank.img")
	if err := os.WriteFile(path, make([]byte, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck // read-only
	if v := New().Probe(f, 0); v.Kind != fstool.No {
		t.Errorf("probe of zeros = %v; want No — that is not an ISO, and not damage", v.Kind)
	}
}

func TestInfoReportsVolumeMetadata(t *testing.T) {
	_, vol := open(t, damagedISO(t), nil)
	var b strings.Builder
	if err := vol.Info(&b); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Publisher:     " + testiso.Publisher, "Created:       2001-02-03 04:05:06 +00:00"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("info missing %q:\n%s", want, b.String())
		}
	}
	if strings.Contains(b.String(), "Expires:") {
		t.Errorf("info prints an unset date:\n%s", b.String())
	}
}

func TestWalkCarriesTheRecordDate(t *testing.T) {
	_, vol := open(t, damagedISO(t), nil)
	var found bool
	if _, err := vol.Walk(func(e fstool.Entry) error {
		if e.Path == testiso.FileName {
			found = true
			if !e.ModTime.Equal(testiso.FileTime) {
				t.Errorf("ModTime = %v, want %v", e.ModTime, testiso.FileTime)
			}
			if e.Mode != "-r--r--r--" {
				t.Errorf("Mode = %q, want -r--r--r--", e.Mode)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatalf("no %s in the walk", testiso.FileName)
	}
}

func TestEntryForRockRidgeModes(t *testing.T) {
	cases := []struct {
		mode    fs.FileMode
		want    string
		special bool
	}{
		{fs.ModeDir | 0o750, "drwxr-x---", false},
		{fs.ModeSymlink | 0o777, "lrwxrwxrwx", false},
		{fs.ModeSetuid | 0o755, "-rwxr-xr-x", false},
		{fs.ModeDevice | fs.ModeCharDevice | 0o620, "crw--w----", true},
		{fs.ModeDevice | 0o660, "brw-rw----", true},
		{fs.ModeNamedPipe | 0o644, "prw-r--r--", true},
		{fs.ModeSocket | 0o755, "srwxr-xr-x", true},
	}
	for _, c := range cases {
		e := entryFor(iso.DirEntry{Path: "/x", Mode: c.mode, IsDir: c.mode.IsDir()})
		if e.Mode != c.want || e.Special != c.special {
			t.Errorf("%v: mode %q special %v, want %q %v", c.mode, e.Mode, e.Special, c.want, c.special)
		}
	}
}

// An associated file has the same path as the file it belongs to, so it is
// left out of the walk and counted in a diagnostic rather than written over
// its main file.
func TestAssociatedFilesAreSkipped(t *testing.T) {
	img := testiso.Build()
	// The root directory is at LBA 18: dot and dotdot are 34 bytes each, then
	// HELLO.TXT's record, whose flags are at byte 25.
	img[18*testiso.SectorSize+68+25] = 0x04
	path := filepath.Join(t.TempDir(), "assoc.iso")
	if err := os.WriteFile(path, img, 0o600); err != nil {
		t.Fatal(err)
	}
	_, vol := open(t, path, nil)
	diags, err := vol.Walk(func(e fstool.Entry) error {
		if e.Path == testiso.FileName {
			t.Errorf("the associated file %s was listed", e.Path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, d := range diags {
		if strings.Contains(d.Text, "1 associated file(s) skipped") {
			found = true
		}
	}
	if !found {
		t.Errorf("diagnostics = %+v, want one counting the associated file", diags)
	}
}

func TestLookup(t *testing.T) {
	_, vol := open(t, damagedISO(t), nil)
	e, err := vol.Lookup(testiso.FileName)
	if err != nil || e.Path != testiso.FileName || e.Size != int64(len(testiso.FileData)) {
		t.Fatalf("Lookup(%s) = %+v, %v", testiso.FileName, e, err)
	}
	r, err := vol.Open(e)
	if err != nil {
		t.Fatal(err)
	}
	if data, err := io.ReadAll(r); err != nil || string(data) != testiso.FileData {
		t.Errorf("read %q, %v", data, err)
	}
	if _, err := vol.Lookup("MISSING.TXT"); !errors.Is(err, fstool.ErrNotFound) {
		t.Errorf("Lookup(MISSING.TXT) error = %v, want fstool.ErrNotFound", err)
	}
}

// The root directory is unreadable, so a name that is not found may have been
// in it: that is damage to the medium, not a path the volume lacks.
func TestLookupUnderAnUnreadableDirectory(t *testing.T) {
	_, vol := open(t, damagedISO(t, 18), nil)
	_, err := vol.Lookup(testiso.FileName)
	if err == nil || errors.Is(err, fstool.ErrNotFound) {
		t.Errorf("Lookup error = %v, want damage rather than ErrNotFound", err)
	}
}
