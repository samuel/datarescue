package extfs_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"

	"samuelks.com/datarescue/extfs"
	"samuelks.com/datarescue/internal/testfixture"
)

const testImagePath = "testdata/disk.img.gz"

func openTestImage(t *testing.T) *extfs.FS {
	t.Helper()
	fs, err := extfs.Open(testfixture.Reader(t, testImagePath), 0)
	if err != nil {
		t.Fatalf("extfs.Open: %s", err)
	}
	return fs
}

func TestOpen(t *testing.T) {
	fs := openTestImage(t)
	sb := fs.Superblock()
	if sb == nil {
		t.Fatal("nil superblock")
	}
	t.Logf("volume=%q inodes=%d blocks=%d blockSize=%d",
		sb.VolumeName, sb.InodesCount, sb.BlocksCount, fs.BlockSize())
	if fs.BlockSize() != 1024 {
		t.Errorf("BlockSize() = %d, want 1024", fs.BlockSize())
	}
	if sb.VolumeName != "testdisk" {
		t.Errorf("VolumeName = %q, want %q", sb.VolumeName, "testdisk")
	}
	if fs.InodeCount() == 0 {
		t.Error("InodeCount() == 0")
	}
}

func TestReadRootInode(t *testing.T) {
	fs := openTestImage(t)
	ino, err := fs.ReadInode(extfs.RootInodeNum)
	if err != nil {
		t.Fatalf("ReadInode(%d): %s", extfs.RootInodeNum, err)
	}
	if !ino.IsDir() {
		t.Errorf("root inode mode 0x%04X is not a directory", ino.Mode)
	}
	if ino.LinksCount < 2 {
		t.Errorf("root inode LinksCount=%d, want ≥2", ino.LinksCount)
	}
}

func TestReadDir(t *testing.T) {
	fs := openTestImage(t)
	rootIno, err := fs.ReadInode(extfs.RootInodeNum)
	if err != nil {
		t.Fatalf("ReadInode: %s", err)
	}
	entries, err := fs.ReadDir(rootIno, false)
	if err != nil {
		t.Fatalf("ReadDir: %s", err)
	}
	names := make(map[string]bool, len(entries))
	for _, e := range entries {
		names[e.Name] = true
	}
	for _, want := range []string{".", "..", "hello.txt", "testdir"} {
		if !names[want] {
			t.Errorf("missing entry %q in root directory", want)
		}
	}
}

func TestOpenFile(t *testing.T) {
	fs := openTestImage(t)
	r, err := fs.Open("/hello.txt")
	if err != nil {
		t.Fatalf("Open: %s", err)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %s", err)
	}
	if len(data) == 0 {
		t.Error("hello.txt: empty content")
	}
	t.Logf("hello.txt: %q", data)
}

func TestWalk(t *testing.T) {
	fs := openTestImage(t)
	var paths []string
	issues, err := fs.Walk(func(path string, _ *extfs.DirEntry, _ *extfs.Inode) error {
		paths = append(paths, path)
		return nil
	})
	if err != nil {
		t.Fatalf("Walk: %s", err)
	}
	for _, wi := range issues {
		t.Errorf("Walk issue: %s", wi)
	}
	t.Logf("walked: %v", paths)
	var found bool
	for _, p := range paths {
		if p == "hello.txt" {
			found = true
		}
	}
	if !found {
		t.Error("Walk did not visit hello.txt")
	}
}

func TestWalkInodes(t *testing.T) {
	fs := openTestImage(t)
	var count int
	issues, err := fs.WalkInodes(false, func(_ uint32, _ *extfs.Inode) error {
		count++
		return nil
	})
	if err != nil {
		t.Fatalf("WalkInodes: %s", err)
	}
	for _, is := range issues {
		t.Errorf("WalkInodes issue: %s: %s", is.Kind, is.Detail)
	}
	if count == 0 {
		t.Error("WalkInodes found no allocated inodes")
	}
	t.Logf("WalkInodes: %d allocated inodes", count)
}

func TestValidateFS(t *testing.T) {
	fs := openTestImage(t)
	result, err := fs.ValidateFS()
	if err != nil {
		t.Fatalf("ValidateFS: %s", err)
	}
	for _, issue := range result.Issues {
		t.Logf("issue inode=%d kind=%s: %s", issue.InodeNum, issue.Kind, issue.Detail)
	}
	if result.ReadableInodes == 0 {
		t.Error("ValidateFS: no readable inodes")
	}
	t.Logf("total=%d readable=%d files=%d issues=%d",
		result.TotalInodes, result.ReadableInodes, result.ReadableFiles, len(result.Issues))
}

func TestSuperblockFallback(t *testing.T) {
	// Inject faults over the primary superblock (bytes 1024–2047).
	fr := newFaultReader(testfixture.Reader(t, testImagePath), [2]int64{1024, 2048})
	_, err := extfs.Open(fr, 0)
	// The test image is only 4 MiB; the group-1 backup for 1 KiB blocks would
	// be at byte 8,389,632 which is past EOF. We expect an error explaining
	// that no valid superblock was found — not a panic.
	if err == nil {
		t.Log("Open succeeded (found a readable backup superblock)")
	} else {
		t.Logf("Open failed as expected with faulted primary: %s", err)
	}
}

func TestGroupHasSuperBackup(t *testing.T) {
	tests := []struct {
		group  uint32
		sparse bool
		want   bool
	}{
		{0, true, true},
		{1, true, true},
		{2, true, false},
		{3, true, true},
		{4, true, false},
		{5, true, true},
		{6, true, false},
		{7, true, true},
		{9, true, true},
		{25, true, true},
		{49, true, true},
		{125, true, true},
		{2, false, true}, // no SPARSE_SUPER: every group has a backup
		{4, false, true},
		{6, false, true},
	}
	for _, tt := range tests {
		got := extfs.GroupHasSuperBackup(tt.group, tt.sparse)
		if got != tt.want {
			t.Errorf("GroupHasSuperBackup(%d, sparse=%v) = %v, want %v",
				tt.group, tt.sparse, got, tt.want)
		}
	}
}

// errInjected stands in for whatever a damaged image reports: a rescue map
// refusing an unrecovered range, or the medium itself.
var errInjected = errors.New("injected read error")

// faultReader wraps an io.ReadSeeker and refuses the fault [start, end) byte
// ranges, with the semantics a rescue map gives: a read starting inside one
// fails outright, and one running into it returns the bytes before it, so the
// recovered head of a block is still delivered.
type faultReader struct {
	r      io.ReadSeeker
	pos    int64
	faults [][2]int64
	// hits counts the reads it refused. On damaged media a failed read is not
	// free — the drive retries it — so how often one is reissued is a property
	// worth asserting on.
	hits int
}

func newFaultReader(r io.ReadSeeker, faults ...[2]int64) *faultReader {
	return &faultReader{r: r, faults: faults}
}

func (fr *faultReader) Read(p []byte) (int, error) {
	for _, f := range fr.faults {
		if fr.pos >= f[0] && fr.pos < f[1] {
			fr.hits++
			return 0, errInjected
		}
	}
	// Stop short of the nearest fault ahead, so the next read is the one that
	// fails rather than this one reading straight through it.
	for _, f := range fr.faults {
		if f[0] > fr.pos && f[0] < fr.pos+int64(len(p)) {
			p = p[:f[0]-fr.pos]
		}
	}
	n, err := fr.r.Read(p)
	fr.pos += int64(n)
	return n, err
}

func (fr *faultReader) Seek(offset int64, whence int) (int64, error) {
	pos, err := fr.r.Seek(offset, whence)
	if err == nil {
		fr.pos = pos
	}
	return pos, err
}

// A group descriptor table that itself read as zeros points the inode bitmap at
// block 0, and block 0 is a sparse hole: the bitmap comes back all zeros, every
// inode in the group reads as free, and verify reports a clean empty
// filesystem. For a whole group of files that is wrong; treating the bitmap as
// all-allocated is only slower.
func TestZeroInodeBitmapIsTreatedAsUnreadable(t *testing.T) {
	data := testfixture.Bytes(t, testImagePath)
	sound, err := extfs.Open(bytes.NewReader(data), 0)
	if err != nil {
		t.Fatal(err)
	}
	sb := sound.Superblock()
	want, _, err := walkedInodes(sound)
	if err != nil {
		t.Fatal(err)
	}
	if want == 0 {
		t.Fatal("the sound fixture yields no allocated inodes")
	}

	// The group 0 descriptor follows the superblock's block; InodeBitmap is its
	// second word.
	gdt := (int64(sb.FirstDataBlock) + 1) * int64(sound.BlockSize())
	binary.LittleEndian.PutUint32(data[gdt+4:], 0)

	fs, err := extfs.Open(bytes.NewReader(data), 0)
	if err != nil {
		t.Fatal(err)
	}
	got, issues, err := walkedInodes(fs)
	if err != nil {
		t.Fatal(err)
	}
	var named bool
	for _, is := range issues {
		if is.Kind == extfs.IssueUnreadableBitmap {
			named = true
		}
	}
	if !named {
		t.Errorf("issues = %v; want one naming the unreadable inode bitmap", issues)
	}
	if got < want {
		t.Errorf("the walk found %d inodes; want at least the %d the sound image has", got, want)
	}
}

// walkedInodes counts the inodes WalkInodes yields with the allocation bitmap
// honored, alongside the issues it recorded.
func walkedInodes(fs *extfs.FS) (int, []extfs.ValidationIssue, error) {
	var n int
	issues, err := fs.WalkInodes(false, func(uint32, *extfs.Inode) error {
		n++
		return nil
	})
	return n, issues, err
}
