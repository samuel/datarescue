package hfsdrv

import (
	"encoding/binary"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"samuelks.com/datarescue/apm"
	"samuelks.com/datarescue/hfs"
	"samuelks.com/datarescue/internal/fstool"
	"samuelks.com/datarescue/internal/testapm"
	"samuelks.com/datarescue/internal/testfixture"
	"samuelks.com/datarescue/mbr"
)

const fixture = "../../../hfs/testdata/disk.img.gz"

// bareVolume is the committed fixture: an HFS volume with no partition map.
func bareVolume(t *testing.T) string {
	t.Helper()
	return testfixture.File(t, fixture)
}

// appleDisk wraps that same volume in a driver descriptor record and a
// partition map, the layout of the real Zip disks: a reserved map partition,
// then the HFS volume. It needs no fixture of its own.
func appleDisk(t *testing.T) (string, int64) {
	t.Helper()
	vol := testfixture.Bytes(t, fixture)
	img, placed := testapm.Build(
		testapm.Options{MapBlocks: 10, Fill: 0x00},
		testapm.Partition{Type: apm.TypeHFS, Name: "ISIS", Data: vol},
	)
	path := filepath.Join(t.TempDir(), "apple.img")
	if err := os.WriteFile(path, img, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, placed[1].Offset
}

func open(t *testing.T, path string) (*fstool.Context, fstool.Volume) {
	t.Helper()
	drv := New()
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

func TestDetectsABareVolume(t *testing.T) {
	c, _ := open(t, bareVolume(t))
	if c.Driver.Name() != "hfs" {
		t.Errorf("driver = %s; want hfs", c.Driver.Name())
	}
	if c.Image.Source != "offset 0 (no partition table)" {
		t.Errorf("Source = %q", c.Image.Source)
	}
}

// The layout of the target disks: an HFS volume after an Apple partition map.
// The volume does not start at byte 0, which is what hfs.Open's offset is for.
func TestDetectsAVolumeInAnApplePartition(t *testing.T) {
	path, want := appleDisk(t)
	c, vol := open(t, path)
	if c.Image.Offset != want {
		t.Errorf("Offset = %d; want %d", c.Image.Offset, want)
	}
	if !strings.Contains(c.Image.Source, "Apple partition 2") {
		t.Errorf("Source = %q; want it to name the Apple partition", c.Image.Source)
	}
	if !strings.Contains(c.Image.Source, "ISIS") {
		t.Errorf("Source = %q; want it to name the partition", c.Image.Source)
	}
	// And the volume reads the same as it does bare.
	var names []string
	if _, err := vol.Walk(func(e fstool.Entry) error {
		names = append(names, e.Path)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(names, "TwinPeaksTrivia") {
		t.Errorf("the volume in a partition listed %v", names)
	}
}

// mbrDisk wraps the same volume in an MBR partition, with trailing bytes after
// the partition, so only the partition's own extent locates the volume's spare
// master directory block. A volume bounded by the whole image would read past
// its own end and report the spare missing.
func mbrDisk(t *testing.T) (string, int64) {
	t.Helper()
	vol := testfixture.Bytes(t, fixture)
	const partLBA = 2
	partStart := int64(partLBA) * mbr.SectorSize
	disk := make([]byte, partStart+int64(len(vol))+4096)
	disk[446+4] = 0xAF // Apple HFS
	binary.LittleEndian.PutUint32(disk[446+8:], partLBA)
	binary.LittleEndian.PutUint32(disk[446+12:], uint32(len(vol)/mbr.SectorSize))
	disk[510], disk[511] = 0x55, 0xAA
	copy(disk[partStart:], vol)
	path := filepath.Join(t.TempDir(), "mbr.img")
	if err := os.WriteFile(path, disk, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, partStart
}

// An HFS volume can be in an MBR partition as well as in an Apple one.
func TestDetectsAVolumeInAnMBRPartition(t *testing.T) {
	path, want := mbrDisk(t)
	c, vol := open(t, path)
	if c.Image.Offset != want {
		t.Errorf("Offset = %d; want %d", c.Image.Offset, want)
	}
	if !strings.Contains(c.Image.Source, "partition 1") {
		t.Errorf("Source = %q; want it to name the MBR partition", c.Image.Source)
	}
	// The volume opens cleanly: the spare master directory block is found within
	// the partition, and the trailing bytes in the image are not mistaken for
	// part of the volume.
	var names []string
	if _, err := vol.Walk(func(e fstool.Entry) error {
		names = append(names, e.Path)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(names, "TwinPeaksTrivia") {
		t.Errorf("the volume in the MBR partition listed %v", names)
	}
	if ds := vol.Diagnostics(); len(ds) != 0 {
		t.Errorf("the volume reported damage it did not have: %v", ds)
	}
}

// The root folder is the volume, not something in it. Listing it puts every
// file one level down under a folder named after the disk.
func TestTheRootFolderIsNotListed(t *testing.T) {
	_, vol := open(t, bareVolume(t))
	var files, dirs int
	if _, err := vol.Walk(func(e fstool.Entry) error {
		if e.Path == "disk" {
			t.Error("the root folder was listed as an entry")
		}
		if e.Dir {
			dirs++
		} else {
			files++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// The expected counts are the ones the volume itself records.
	if files != 3 || dirs != 3 {
		t.Errorf("listed %d files and %d directories; the volume records 3 and 3", files, dirs)
	}
}

func TestReadsAFile(t *testing.T) {
	_, vol := open(t, bareVolume(t))
	var entry fstool.Entry
	if _, err := vol.Walk(func(e fstool.Entry) error {
		if e.Path == "TwinPeaksTrivia" {
			entry = e
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if entry.Path == "" {
		t.Fatal("the file is not in the fixture")
	}
	if entry.Size != 327680 {
		t.Errorf("size = %d; want 327680", entry.Size)
	}
	r, err := vol.Open(entry)
	if err != nil {
		t.Fatal(err)
	}
	n, err := io.Copy(io.Discard, r)
	if err != nil {
		t.Fatal(err)
	}
	if n != entry.Size {
		t.Errorf("read %d bytes; want %d", n, entry.Size)
	}
}

// "/" is an ordinary character in an HFS filename and ":" is the separator, so
// the two swap roles on output. Otherwise one file would split into three path
// components and an extract would build a tree out of it.
func TestASlashInANameDoesNotBecomeAPathSeparator(t *testing.T) {
	got := joinPath([]string{"Docs", "10/20/99 Budget"})
	if want := "Docs/10:20:99 Budget"; got != want {
		t.Errorf("joinPath = %q; want %q", got, want)
	}
	if strings.Count(got, "/") != 1 {
		t.Errorf("joinPath = %q; the name's own slashes became separators", got)
	}
}

// HFS records local wall-clock seconds and no zone, so UTC is the default: it
// is deterministic, where any other default would be a guess.
func TestTimestampsDefaultToUTC(t *testing.T) {
	_, vol := open(t, bareVolume(t))
	var mod string
	if _, err := vol.Walk(func(e fstool.Entry) error {
		if e.Path == "TwinPeaksTrivia" {
			mod = e.ModTime.Format("2006-01-02 15:04:05 MST")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(mod, "UTC") {
		t.Errorf("modification time = %q; want it read as UTC", mod)
	}
	if !strings.HasPrefix(mod, "1992-11-12 00:57:13") {
		t.Errorf("modification time = %q; want the date the stack records", mod)
	}
}

func TestInfoReportsTheVolume(t *testing.T) {
	_, vol := open(t, bareVolume(t))
	var b strings.Builder
	if err := vol.Info(&b); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Volume name:  disk", "Block size:", "Files:", "Directories:"} {
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
		t.Errorf("probe of zeros = %v; want No — that is not HFS, and not damage", v.Kind)
	}
}

func TestTZNamesTheZone(t *testing.T) {
	d := New()
	d.tz = "America/Los_Angeles"
	loc, err := d.location()
	if err != nil {
		t.Skipf("no zone database: %v", err)
	}
	if loc == nil || loc.String() != "America/Los_Angeles" {
		t.Errorf("location = %v; want America/Los_Angeles", loc)
	}
	bad := New()
	bad.tz = "Nowhere/Nothing"
	if _, err := bad.location(); err == nil {
		t.Error("an unknown zone was accepted")
	}
}

// A classic Mac file records its kind in the catalog rather than in its name,
// and reading the codes costs no I/O: they are in the record the walk already
// parsed.
func TestKindReportsWhatTheCatalogRecords(t *testing.T) {
	_, vol := open(t, bareVolume(t))
	tv, ok := vol.(fstool.Typed)
	if !ok {
		t.Fatal("an HFS volume does not answer Typed")
	}
	// This real 1989 volume holds a HyperCard stack and the two desktop
	// database files, and none of them gets an extension. A stack means
	// nothing outside a Mac, and giving the desktop database ".db" would
	// invite something to open it as a database. So every name is unchanged.
	want := map[string]string{
		"TwinPeaksTrivia": "STAK/WILD",
		"Desktop DB":      "BTFL/DMGR",
		"Desktop DF":      "DTFL/DMGR",
	}
	seen := map[string]string{}
	if _, err := vol.Walk(func(e fstool.Entry) error {
		if e.Dir {
			return nil
		}
		k := tv.Kind(e)
		seen[e.Path] = k.Code
		if k.Ext != "" {
			t.Errorf("%s: %s was given the extension %q", e.Path, k.Code, k.Ext)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(seen, want) {
		t.Errorf("kinds = %v; want %v", seen, want)
	}
}

// A folder has Finder information too, and its first four bytes are the
// window position the Finder opens it at. They decode as an OSType just as a
// file's do, so a listing that only skipped zero codes would print screen
// coordinates where a file's kind goes.
func TestKindIsBlankForAFolder(t *testing.T) {
	_, vol := open(t, bareVolume(t))
	tv := vol.(fstool.Typed) //nolint:forcetypeassert // pinned by TestKindReportsWhatTheCatalogRecords
	var dirs int
	if _, err := vol.Walk(func(e fstool.Entry) error {
		if !e.Dir {
			return nil
		}
		dirs++
		if k := tv.Kind(e); k != (fstool.FileKind{}) {
			t.Errorf("%s: folder reported kind %+v", e.Path, k)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if dirs == 0 {
		t.Fatal("the fixture listed no directories")
	}

	// This volume's folders were never positioned, so their Finder bytes are
	// zero and the walk above cannot distinguish the IsDir guard from a zero
	// test. A folder from a disk that was actually used holds a rectangle, so
	// one is built here: "\x00\x28\x00\x50" is a window at y=40, x=80, which
	// looks like a plausible type code and must be rejected by the guard alone.
	folder := fstool.Entry{Dir: true, Key: key{e: hfs.Entry{
		IsDir: true,
		Type:  hfs.OSType{0x00, 0x28, 0x00, 0x50},
	}}}
	if k := tv.Kind(folder); k != (fstool.FileKind{}) {
		t.Errorf("a positioned folder reported kind %+v; the guard is a zero test, not IsDir", k)
	}
}

// Only the caller knows the final name, so the volume's own spelling is
// compared against that name rather than against one this method computes.
func TestSidecarRecordsTheVolumesOwnNameWhenTheWrittenOneDiffers(t *testing.T) {
	_, vol := open(t, bareVolume(t))
	fv := vol.(fstool.Forked) //nolint:forcetypeassert // pinned by the compile-time assertion
	var e fstool.Entry
	if _, err := vol.Walk(func(w fstool.Entry) error {
		if w.Path == "TwinPeaksTrivia" {
			e = w
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := fv.Sidecar(e, "TwinPeaksTrivia").RealName; got != "" {
		t.Errorf("RealName = %q; want empty where the names agree", got)
	}
	if got := fv.Sidecar(e, "TwinPeaksTrivia.stk").RealName; got != "TwinPeaksTrivia" {
		t.Errorf("RealName = %q; want the volume's own name", got)
	}
}
