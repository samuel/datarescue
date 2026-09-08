package fstool

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"samuelks.com/datarescue/ddrescue"
	"samuelks.com/datarescue/internal/subcmd"
	"samuelks.com/datarescue/internal/testout"
)

// imageWith writes an image whose byte at off is magic, so a fake driver's
// probe finds it there.
func imageWith(t *testing.T, at map[int64]byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "disk.img")
	if err := os.WriteFile(path, writeFakeImage(1<<16, at), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func fileSet() map[string]*fakeFile {
	return map[string]*fakeFile{
		"README":        {data: []byte("hello"), failAt: -1},
		"dir":           {dir: true, failAt: -1},
		"dir/inner.txt": {data: []byte("inner"), failAt: -1},
		"link":          {symlink: "README", failAt: -1},
	}
}

func fat() *fakeDriver {
	return &fakeDriver{name: "fat", magic: 0xFA, files: fileSet(), flag: "fat-policy-fake"}
}

func ext() *fakeDriver {
	return &fakeDriver{name: "ext", magic: 0xEE, files: fileSet(), flag: "ext-only-fake"}
}

func TestDetectFindsTheDriverAtOffsetZero(t *testing.T) {
	t.Parallel()
	path := imageWith(t, map[int64]byte{0: 0xEE})
	cf := &CommonFlags{Offset: -1}
	drv, im, hint, err := Detect(path, cf, []Driver{fat(), ext()})
	if err != nil {
		t.Fatal(err)
	}
	defer im.Close() //nolint:errcheck // test cleanup
	if drv.Name() != "ext" {
		t.Errorf("driver = %s; want ext", drv.Name())
	}
	if hint != "hint-ext" {
		t.Errorf("hint = %v; want hint-ext", hint)
	}
}

// The first offset any driver claims wins, which is diskimage's own rule. One
// diskimage.Open per driver would instead let driver order override the order
// of partitions in the table.
func TestDetectPrefersTheEarlierOffsetNotTheEarlierDriver(t *testing.T) {
	t.Parallel()
	path := imageWith(t, map[int64]byte{0: 0xEE})
	// fat is offered first and is not at offset 0; ext is.
	drv, im, _, err := Detect(path, &CommonFlags{Offset: -1}, []Driver{fat(), ext()})
	if err != nil {
		t.Fatal(err)
	}
	defer im.Close() //nolint:errcheck // test cleanup
	if drv.Name() != "ext" {
		t.Errorf("driver = %s; want ext", drv.Name())
	}
}

// A probe that cannot decide is not the same as a probe that says no.
// Reporting "no filesystem found" for an image whose boot sector was never
// recovered gives the least useful answer in exactly the case a rescue map
// exists for.
func TestDetectReportsWhatWasNeverRecovered(t *testing.T) {
	t.Parallel()
	path := imageWith(t, map[int64]byte{0: 0x00})
	sentinel := errors.New("the volume descriptor was never recovered")
	d := ext()
	d.damagedAt = func(off int64) error {
		if off == 0 {
			return sentinel
		}
		return nil
	}
	_, _, _, err := Detect(path, &CommonFlags{Offset: -1}, []Driver{d})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Detect = %v; want it to carry the sentinel", err)
	}
	if !strings.Contains(err.Error(), "never recovered") {
		t.Errorf("Detect = %q; want it to say what was never recovered", err)
	}
}

func TestDetectNothingRecognized(t *testing.T) {
	t.Parallel()
	path := imageWith(t, map[int64]byte{0: 0x00})
	_, _, _, err := Detect(path, &CommonFlags{Offset: -1}, []Driver{fat(), ext()})
	if err == nil {
		t.Fatal("Detect on an unrecognized image returned no error")
	}
	for _, want := range []string{"-fs", "-offset", "-partition"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Detect = %q; want it to mention %s", err, want)
		}
	}
}

func TestDetectForcedFormat(t *testing.T) {
	t.Parallel()
	path := imageWith(t, map[int64]byte{0: 0xEE})
	drv, im, _, err := Detect(path, &CommonFlags{Offset: -1, FS: "ext"}, []Driver{fat(), ext()})
	if err != nil {
		t.Fatal(err)
	}
	defer im.Close() //nolint:errcheck // test cleanup
	if drv.Name() != "ext" {
		t.Errorf("driver = %s; want ext", drv.Name())
	}
}

func TestDetectUnknownForcedFormatIsUsage(t *testing.T) {
	t.Parallel()
	path := imageWith(t, map[int64]byte{0: 0xEE})
	_, _, _, err := Detect(path, &CommonFlags{Offset: -1, FS: "ntfs"}, []Driver{fat(), ext()})
	if subcmd.ExitFor(err) != subcmd.ExitUsage {
		t.Errorf("exit for -fs ntfs = %d; want ExitUsage", subcmd.ExitFor(err))
	}
}

// An explicit offset skips the probe, so the format is not yet known. The
// drivers are still asked, or -fs would be the only way to use -offset.
func TestDetectWithExplicitOffsetStillIdentifiesTheFormat(t *testing.T) {
	t.Parallel()
	path := imageWith(t, map[int64]byte{512: 0xFA})
	drv, im, hint, err := Detect(path, &CommonFlags{Offset: 512}, []Driver{fat(), ext()})
	if err != nil {
		t.Fatal(err)
	}
	defer im.Close() //nolint:errcheck // test cleanup
	if drv.Name() != "fat" {
		t.Errorf("driver = %s; want fat", drv.Name())
	}
	if hint != "hint-fat" {
		t.Errorf("hint = %v; want hint-fat", hint)
	}
}

func TestValidateRejectsTheCombinationsThatCannotWork(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		image string
		cf    CommonFlags
		want  string
	}{
		{"offset and partition", "d.img", CommonFlags{Offset: 0, Partition: 1}, "mutually exclusive"},
		{"map and no-map", "d.img", CommonFlags{Offset: -1, Map: "m", NoMap: true}, "mutually exclusive"},
		{"negative partition", "d.img", CommonFlags{Offset: -1, Partition: -2}, "must be 1 or greater"},
		{"map on 86F", "d.86f", CommonFlags{Offset: -1, Map: "m"}, "86F"},
		{"offset on 86F", "d.86f", CommonFlags{Offset: 0}, "86F"},
		{"partition on 86F", "d.86f", CommonFlags{Offset: -1, Partition: 1}, "86F"},
		{"geometry off 86F", "d.img", CommonFlags{Offset: -1, Geometry: "18:2:512"}, "only to .86f"},
		{"bad geometry", "d.86f", CommonFlags{Offset: -1, Geometry: "nope"}, "SPT:HEADS:BPS"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.cf.Validate(tc.image)
			if err == nil {
				t.Fatalf("Validate(%q, %+v) = nil; want an error", tc.image, tc.cf)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Validate = %q; want it to mention %q", err, tc.want)
			}
		})
	}
}

// -partition has no fixed upper bound: an Apple partition map often holds more
// than four entries, so whether the table holds a number can only be judged
// once the table has been read.
func TestValidateAcceptsAPartitionPastFour(t *testing.T) {
	t.Parallel()
	cf := CommonFlags{Offset: -1, Partition: 9}
	if err := cf.Validate("d.img"); err != nil {
		t.Errorf("Validate with -partition 9 = %v; want nil", err)
	}
}

func TestFlagSetRejectsAnotherDriversFlag(t *testing.T) {
	t.Parallel()
	fs := NewFlagSet("ls")
	var cf CommonFlags
	cf.Register(fs, "fat|ext")
	f, e := fat(), ext()
	fs.Own(f.Name(), f.Flags)
	fs.Own(e.Name(), e.Flags)

	if err := fs.Parse([]string{"-ext-only-fake", "disk.img"}); err != nil {
		t.Fatal(err)
	}
	err := fs.Check("fat", "disk.img", "fat (fake)")
	if err == nil {
		t.Fatal("Check accepted a flag belonging to another driver")
	}
	if subcmd.ExitFor(err) != subcmd.ExitUsage {
		t.Errorf("exit for a misapplied flag = %d; want ExitUsage", subcmd.ExitFor(err))
	}
	if !strings.Contains(err.Error(), "-ext-only-fake") || !strings.Contains(err.Error(), "ext") {
		t.Errorf("Check = %q; want it to name the flag and the driver", err)
	}
}

// Setting a driver's flag does not imply -fs: the flag has to work on an image
// whose format was probed for.
func TestFlagSetAcceptsItsOwnDriversFlag(t *testing.T) {
	t.Parallel()
	fs := NewFlagSet("ls")
	var cf CommonFlags
	cf.Register(fs, "fat|ext")
	f, e := fat(), ext()
	fs.Own(f.Name(), f.Flags)
	fs.Own(e.Name(), e.Flags)
	if err := fs.Parse([]string{"-fat-policy-fake", "disk.img"}); err != nil {
		t.Fatal(err)
	}
	if err := fs.Check("fat", "disk.img", "fat (fake)"); err != nil {
		t.Errorf("Check = %v; want nil for the chosen driver's own flag", err)
	}
}

// A flag left at its default was not given, so it cannot be a mistake.
func TestFlagSetIgnoresFlagsThatWereNotGiven(t *testing.T) {
	t.Parallel()
	fs := NewFlagSet("ls")
	var cf CommonFlags
	cf.Register(fs, "fat|ext")
	f, e := fat(), ext()
	fs.Own(f.Name(), f.Flags)
	fs.Own(e.Name(), e.Flags)
	if err := fs.Parse([]string{"disk.img"}); err != nil {
		t.Fatal(err)
	}
	if err := fs.Check("fat", "disk.img", "fat (fake)"); err != nil {
		t.Errorf("Check = %v; want nil when no driver flag was given", err)
	}
}

// If two drivers register the same flag name, the flag package panics with a
// message that includes the flag. That is what a driver author needs, so
// nothing here re-wraps it.
func TestTwoDriversWantingOneFlagNamePanics(t *testing.T) {
	t.Parallel()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("registering one flag name for two drivers did not panic")
		}
		if !strings.Contains(fmt.Sprint(r), "dup") {
			t.Errorf("panic = %v; want it to name the flag", r)
		}
	}()
	fs := NewFlagSet("ls")
	a := &fakeDriver{name: "a", flag: "dup"}
	b := &fakeDriver{name: "b", flag: "dup"}
	fs.Own(a.Name(), a.Flags)
	fs.Own(b.Name(), b.Flags)
}

func TestSelectionExactVersusPrefix(t *testing.T) {
	t.Parallel()
	// A directory given to a listing means "show what is under it"; given to an
	// extract, it is something extract will not write.
	prefix := selection([]string{"dir"}, true)
	if !prefix.wants("dir/inner.txt") {
		t.Error("a prefix selection did not match a child")
	}
	exact := selection([]string{"dir"}, false)
	if exact.wants("dir/inner.txt") {
		t.Error("an exact selection matched a child")
	}
	if !exact.wants("dir") {
		t.Error("an exact selection did not match the named path")
	}
}

func TestSelectionReportsWhatMatchedNothing(t *testing.T) {
	t.Parallel()
	s := selection([]string{"nope", "also-nope", "dir"}, false)
	s.wants("dir")
	got := s.unmatched()
	want := []string{"also-nope", "nope"}
	if len(got) != len(want) {
		t.Fatalf("unmatched = %v; want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("unmatched[%d] = %q; want %q", i, got[i], want[i])
		}
	}
}

// An empty selection selects everything, so the whole-volume walk and the
// walk for requested paths share one code path.
func TestEmptySelectionWantsEverything(t *testing.T) {
	t.Parallel()
	s := selection(nil, false)
	if !s.wants("anything/at/all") {
		t.Error("an empty selection rejected a path")
	}
	if len(s.unmatched()) != 0 {
		t.Error("an empty selection reported something unmatched")
	}
}

func TestReportVerdict(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	r := NewReport(&b)
	if code := r.Verdict(); code != subcmd.ExitOK {
		t.Errorf("clean verdict = %d; want ExitOK", code)
	}
	if !strings.Contains(b.String(), "Result:      OK") {
		t.Errorf("clean report = %q", b.String())
	}

	b.Reset()
	r = NewReport(&b)
	r.Warnf("something")
	r.Infof("not a finding")
	// Every finding a verify makes is about the medium, so the code reports a
	// damaged medium and never a failed run.
	if code := r.Verdict(); code != subcmd.ExitDegraded {
		t.Errorf("verdict with a finding = %d; want ExitDegraded", code)
	}
	if r.Issues() != 1 {
		t.Errorf("Issues = %d; want 1 — Infof is not a finding", r.Issues())
	}
}

func TestPrintMapInfoSaysWhenThereIsNoMap(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	printMapInfo(&b, "", nil, 0, 4096, "")
	if !strings.Contains(b.String(), "none (the image is read as it is)") {
		t.Errorf("map info = %q", b.String())
	}
}

// The range is measured from where the filesystem starts. On a partitioned
// image, counting from the start of the image would report damage in whatever
// precedes the partition as this filesystem's.
func TestUnrecoveredIsMeasuredFromTheFilesystemStart(t *testing.T) {
	t.Parallel()
	blocks := []ddrescue.Block{
		{Offset: 0, Size: 512, Status: ddrescue.BlockStatusFailedBlockBadSectors},
		{Offset: 512, Size: 3584, Status: ddrescue.BlockStatusFinished},
	}
	if lost := unrecoveredBytes(blocks, 512, 1024); lost != 0 {
		t.Errorf("loss measured from 512 = %d; want 0", lost)
	}
	if lost := unrecoveredBytes(blocks, 0, 1024); lost != 512 {
		t.Errorf("loss measured from 0 = %d; want 512", lost)
	}
}

// Without a map nothing is known to be missing, so nothing is claimed: an
// absent map is not an empty one.
func TestUnrecoveredClaimsNothingWithoutAMap(t *testing.T) {
	t.Parallel()
	if lost := unrecoveredBytes(nil, 0, 4096); lost != 0 {
		t.Errorf("loss with no map = %d; want 0", lost)
	}
}

func TestInfoPrintsTheFormatAndWhereItWasFound(t *testing.T) {
	path := imageWith(t, map[int64]byte{0: 0xEE})
	drv, im, hint, err := Detect(path, &CommonFlags{Offset: -1}, []Driver{ext()})
	if err != nil {
		t.Fatal(err)
	}
	defer im.Close() //nolint:errcheck // test cleanup
	vol, err := drv.Open(im, hint)
	if err != nil {
		t.Fatal(err)
	}
	c := &Context{Driver: drv, Volume: vol, Image: im, Path: path, Tool: "fstool", Command: "info"}

	var code int
	out := testout.CaptureStdout(t, func() { code = Info(c) })
	if code != subcmd.ExitOK {
		t.Errorf("Info = %d; want ExitOK — info describes the medium, it does not judge it", code)
	}
	for _, want := range []string{"Filesystem:", "ext (fake)", "Located:", "offset 0"} {
		if !strings.Contains(out, want) {
			t.Errorf("info output missing %q:\n%s", want, out)
		}
	}
}
