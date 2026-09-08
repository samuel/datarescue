package hfs

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strings"
	"testing"
	"time"
)

// find returns the entry whose joined path matches, for tests that look one up.
func find(t *testing.T, v *Volume, want string) Entry {
	t.Helper()
	entries, issues, err := v.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, ci := range issues {
		t.Errorf("catalog issue on the sound fixture: %s", ci)
	}
	for _, e := range entries {
		if strings.Join(e.Path, "/") == want {
			return e
		}
	}
	t.Fatalf("%q is not in the fixture", want)
	return Entry{}
}

// The walk yields folders as well as files, so no decoded directory record is
// dropped.
func TestListYieldsDirectories(t *testing.T) {
	t.Parallel()
	v, err := openFixture(t)
	if err != nil {
		t.Fatal(err)
	}
	entries, _, err := v.List()
	if err != nil {
		t.Fatal(err)
	}
	var dirs, files int
	for _, e := range entries {
		if e.IsDir {
			dirs++
		} else {
			files++
		}
	}
	if dirs == 0 {
		t.Error("the listing yielded no directories")
	}
	if files == 0 {
		t.Error("the listing yielded no files")
	}
	// A directory is delivered before what is under it, so an extract can
	// create them in order.
	seen := map[string]bool{}
	for _, e := range entries {
		for i := 1; i < len(e.Path); i++ {
			parent := strings.Join(e.Path[:i], "/")
			if !seen[parent] {
				t.Errorf("%q came before its parent %q", strings.Join(e.Path, "/"), parent)
			}
		}
		if e.IsDir {
			seen[strings.Join(e.Path, "/")] = true
		}
	}
}

// The fixture carries a real classic Mac file: a HyperCard stack with a type
// and creator code, 1991 and 1992 timestamps, and, most importantly, a
// resource fork larger than its data fork.
func TestFixtureFileCarriesItsMacMetadata(t *testing.T) {
	t.Parallel()
	v, err := openFixture(t)
	if err != nil {
		t.Fatal(err)
	}
	e := find(t, v, "TwinPeaksTrivia")

	if got := e.Type.String(); got != "STAK" {
		t.Errorf("type = %q; want STAK", got)
	}
	if got := e.Creator.String(); got != "WILD" {
		t.Errorf("creator = %q; want WILD", got)
	}
	if e.DataSize != 327680 {
		t.Errorf("data fork = %d bytes; want 327680", e.DataSize)
	}
	if !e.HasResourceFork() {
		t.Fatal("the entry reports no resource fork")
	}
	if e.RsrcSize != 545069 {
		t.Errorf("resource fork = %d bytes; want 545069", e.RsrcSize)
	}
	if e.Created.IsZero() || e.Modified.IsZero() {
		t.Error("the entry carries no timestamps")
	}
	// The stack was written in 1991 and last changed in 1992.
	if y := e.Created.Time(time.UTC).Year(); y != 1991 {
		t.Errorf("created in %d; want 1991", y)
	}
	if y := e.Modified.Time(time.UTC).Year(); y != 1992 {
		t.Errorf("modified in %d; want 1992", y)
	}

	// The 32 bytes an AppleDouble sidecar carries open with the type and the
	// creator, in that order. Getting the join wrong puts the creator where the
	// flags belong and the sidecar is silently wrong.
	fi := e.FinderInfo()
	if string(fi[0:4]) != "STAK" || string(fi[4:8]) != "WILD" {
		t.Errorf("FinderInfo starts %q; want STAK then WILD", fi[:8])
	}
}

// Reading a fork through proves the extents resolve and the reader reaches the
// right offsets. Both forks of the fixture's stack are pinned by hash.
func TestBothForksReadWhole(t *testing.T) {
	t.Parallel()
	v, err := openFixture(t)
	if err != nil {
		t.Fatal(err)
	}
	e := find(t, v, "TwinPeaksTrivia")

	// Pinned, so a change to the extent walk that still returns the right
	// number of bytes is caught: a reader that lands one allocation block out
	// comes back the right length and the wrong file.
	for _, tc := range []struct {
		name string
		open func() (*ForkReader, error)
		size int64
		sum  string
	}{
		{"data", func() (*ForkReader, error) { return v.OpenFile(e) }, e.DataSize,
			"04e81f8778626b6c5383fb9d0ac8587df139320fe58350f4a99faa97423be1bc"},
		{"resource", func() (*ForkReader, error) { return v.OpenResourceFork(e) }, e.RsrcSize,
			"5edf772a42b4f75549a8d3fb3d48f9b7c8fcb64f2e12250275090c1b057e98dc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := tc.open()
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.New()
			n, err := io.Copy(sum, r)
			if err != nil {
				t.Fatal(err)
			}
			if n != tc.size {
				t.Errorf("read %d bytes; want %d", n, tc.size)
			}
			if got := hex.EncodeToString(sum.Sum(nil)); got != tc.sum {
				t.Errorf("%s fork sha256 = %s; want %s", tc.name, got, tc.sum)
			}
		})
	}
}

// ForkRanges reads nothing, so a caller can report which files a bad region of
// the medium affects before reading any of them, and a test can place a hole in
// a known file without asking the reader where anything is.
func TestForkRangesLocateTheBytesWithoutReading(t *testing.T) {
	t.Parallel()
	v, err := openFixture(t)
	if err != nil {
		t.Fatal(err)
	}
	e := find(t, v, "TwinPeaksTrivia")

	for _, tc := range []struct {
		name string
		ref  ForkRef
		size int64
	}{
		{"data", e.DataFork(), e.DataSize},
		{"resource", e.ResourceFork(), e.RsrcSize},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ranges, err := v.ForkRanges(tc.ref)
			if err != nil {
				t.Fatal(err)
			}
			if len(ranges) == 0 {
				t.Fatal("no ranges")
			}
			var total, want int64
			for _, rg := range ranges {
				if rg.FileOffset != want {
					t.Errorf("range at file offset %d; want %d — the ranges must be contiguous", rg.FileOffset, want)
				}
				if rg.ImageOffset <= 0 || rg.Length <= 0 {
					t.Errorf("implausible range %+v", rg)
				}
				total += rg.Length
				want += rg.Length
			}
			// Clipped to the fork's recorded length: the last allocation block
			// is normally only part full, and the slack is not the file's.
			if total != tc.size {
				t.Errorf("ranges cover %d bytes; want the fork's %d", total, tc.size)
			}
		})
	}
}

// The two forks live in different places, which is why a resource fork needs
// its own extents.
func TestTheTwoForksAreInDifferentPlaces(t *testing.T) {
	t.Parallel()
	v, err := openFixture(t)
	if err != nil {
		t.Fatal(err)
	}
	e := find(t, v, "TwinPeaksTrivia")
	data, err := v.ForkRanges(e.DataFork())
	if err != nil {
		t.Fatal(err)
	}
	rsrc, err := v.ForkRanges(e.ResourceFork())
	if err != nil {
		t.Fatal(err)
	}
	if data[0].ImageOffset == rsrc[0].ImageOffset {
		t.Error("both forks report the same image offset")
	}
}

// HFS timestamps are local wall-clock seconds with no zone recorded, so the
// conversion re-attaches the zone to the same wall-clock fields rather than
// shifting them. -tz sets the time zone a disk was written in.
func TestMacTimeReattachesTheZone(t *testing.T) {
	t.Parallel()
	const secs = 2690000000 // some time in 1989
	utc := MacTime(secs).Time(time.UTC)
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Skipf("no zone database: %v", err)
	}
	local := MacTime(secs).Time(la)
	if utc.Year() != local.Year() || utc.Hour() != local.Hour() || utc.Minute() != local.Minute() {
		t.Errorf("the wall-clock fields moved: %s against %s", utc, local)
	}
	if utc.Equal(local) {
		t.Error("the two instants are the same; the zone was not applied")
	}
	if !MacTime(0).Time(time.UTC).IsZero() {
		t.Error("an unrecorded time did not come back as the zero time")
	}
}
