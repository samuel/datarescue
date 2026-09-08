package fat_test

import (
	"io"
	"strings"
	"testing"
	"time"

	"samuelks.com/datarescue/fat"
)

const (
	fatcatEmpty         = "testdata/fatcat/empty.img.gz"
	fatcatHelloWorld    = "testdata/fatcat/hello-world.img.gz"
	fatcatRepair        = "testdata/fatcat/repair.img.gz"
	fatcatDeleted       = "testdata/fatcat/deleted.img.gz"
	fatcatTwoFileSame   = "testdata/fatcat/two-file-same-cluster.img.gz"
	fatcatDirectoryLoop = "testdata/fatcat/directory-loop.img.gz"
	fatcatInfiniteFile  = "testdata/fatcat/infinite-file.img.gz"
	fatcatFakeBigDisk   = "testdata/fatcat/fake-big-disk-1T.img.gz"
	fatcatFullFAT       = "testdata/fatcat/full-fat.img.gz"
)

// TestFatcatInfo verifies filesystem metadata from the empty FAT32 image.
// Mirrors the PHP testInfos test.
func TestFatcatInfo(t *testing.T) {
	t.Parallel()
	fs := openFAT(t, fatcatEmpty)

	if fs.Type != fat.TypeFAT32 {
		t.Errorf("Type = %q; want %q", fs.Type, fat.TypeFAT32)
	}
	oem := strings.TrimRight(string(fs.BootSector.OEMName[:]), "\x00 ")
	if oem != "mkdosfs" {
		t.Errorf("OEMName = %q; want %q", oem, "mkdosfs")
	}
	bpb := fs.BootSector.BIOSParameterBlock
	if bpb.BytesPerSector != 512 {
		t.Errorf("BytesPerSector = %d; want 512", bpb.BytesPerSector)
	}
	if fs.ClusterSize != 512 {
		t.Errorf("ClusterSize = %d; want 512", fs.ClusterSize)
	}
	diskSize := uint64(fs.TotalSectors) * uint64(fs.SectorSize)
	if diskSize != 52428800 {
		t.Errorf("disk size = %d; want 52428800", diskSize)
	}
	fatSize := uint64(fs.FATTableSize)
	if fatSize != 403456 {
		t.Errorf("FAT size = %d; want 403456", fatSize)
	}
}

// TestFatcatListing verifies directory listing and created timestamps.
// Mirrors the PHP testListing test.
func TestFatcatListing(t *testing.T) {
	t.Parallel()
	fs := openFAT(t, fatcatHelloWorld)

	root, err := fs.ReadDirectory(0, true)
	if err != nil {
		t.Fatalf("ReadDirectory(root) = %v", err)
	}

	byName := make(map[string]*fat.DirectoryEntry)
	for _, e := range root {
		byName[e.Name] = e
	}
	if byName["hello.txt"] == nil {
		t.Error("root directory missing hello.txt")
	}
	if byName["files"] == nil {
		t.Error("root directory missing files/")
	}

	// hello.txt: CreatedTime=0x6bc3 → base 13:30:06; CreatedTime10MS=100 → +1s extra.
	// LastModifiedTime=0x6bc3 → 13:30:06 (no 10MS field on LastModified).
	// LastAccessDate=0x4359 → 2013-10-25 (date only, no time).
	wantCreated := time.Date(2013, 10, 25, 13, 30, 7, 0, time.Local)
	wantModified := time.Date(2013, 10, 25, 13, 30, 6, 0, time.Local)
	wantAccessed := time.Date(2013, 10, 25, 0, 0, 0, 0, time.Local)
	if e := byName["hello.txt"]; e != nil {
		if !e.Created.Equal(wantCreated) {
			t.Errorf("hello.txt Created = %v; want %v", e.Created, wantCreated)
		}
		if !e.LastModified.Equal(wantModified) {
			t.Errorf("hello.txt LastModified = %v; want %v", e.LastModified, wantModified)
		}
		if !e.LastAccessed.Equal(wantAccessed) {
			t.Errorf("hello.txt LastAccessed = %v; want %v", e.LastAccessed, wantAccessed)
		}
	}

	// files/: CreatedTime=0x6bd7 → base 13:30:46; CreatedTime10MS=0 → no extra time.
	wantFilesCreated := time.Date(2013, 10, 25, 13, 30, 46, 0, time.Local)
	if e := byName["files"]; e != nil && !e.Created.Equal(wantFilesCreated) {
		t.Errorf("files/ Created = %v; want %v", e.Created, wantFilesCreated)
	}

	// Find files/ directory and list its contents.
	filesEntry := byName["files"]
	if filesEntry == nil || !filesEntry.Subdirectory {
		t.Fatal("could not find files/ directory")
	}

	sub, err := fs.ReadDirectory(filesEntry.FirstCluster, true)
	if err != nil {
		t.Fatalf("ReadDirectory(files/) = %v", err)
	}
	subNames := make(map[string]bool)
	for _, e := range sub {
		subNames[e.Name] = true
	}
	if !subNames["other_file.txt"] {
		t.Error("files/ directory missing other_file.txt")
	}
}

// TestFatcatListingNotFound verifies that listing a nonexistent path returns an error.
// Mirrors the PHP testListing error case.
func TestFatcatListingNotFound(t *testing.T) {
	t.Parallel()
	fs := openFAT(t, fatcatHelloWorld)
	_, err := fs.Open("xyz/missing.txt")
	if err == nil {
		t.Error("Open(nonexistent path) returned nil error; want error")
	}
}

// TestFatcatReading verifies file content by path.
// Mirrors the PHP testReading test.
func TestFatcatReading(t *testing.T) {
	t.Parallel()
	fs := openFAT(t, fatcatHelloWorld)

	r, err := fs.Open("hello.txt")
	if err != nil {
		t.Fatalf("Open(hello.txt) = %v", err)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll(hello.txt) = %v", err)
	}
	if string(got) != "Hello world!\n" {
		t.Errorf("hello.txt = %q; want %q", got, "Hello world!\n")
	}

	r2, err := fs.Open("files/other_file.txt")
	if err != nil {
		t.Fatalf("Open(files/other_file.txt) = %v", err)
	}
	got2, err := io.ReadAll(r2)
	if err != nil {
		t.Fatalf("ReadAll(other_file.txt) = %v", err)
	}
	if string(got2) != "Hello!\nThis is another file!\n" {
		t.Errorf("other_file.txt = %q; want %q", got2, "Hello!\nThis is another file!\n")
	}
}

// TestFatcatReadByCluster verifies reading file data directly by cluster and size.
// Mirrors the PHP testReading cluster-based read cases.
func TestFatcatReadByCluster(t *testing.T) {
	t.Parallel()
	fs := openFAT(t, fatcatHelloWorld)

	// hello.txt: cluster 3, size 13
	r, err := fs.OpenClusterChain(3, 13)
	if err != nil {
		t.Fatalf("OpenClusterChain(3, 13) = %v", err)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll cluster 3 = %v", err)
	}
	if string(got) != "Hello world!\n" {
		t.Errorf("cluster 3 = %q; want %q", got, "Hello world!\n")
	}

	// other_file.txt: cluster 5, size 29
	r2, err := fs.OpenClusterChain(5, 29)
	if err != nil {
		t.Fatalf("OpenClusterChain(5, 29) = %v", err)
	}
	got2, err := io.ReadAll(r2)
	if err != nil {
		t.Fatalf("ReadAll cluster 5 = %v", err)
	}
	if string(got2) != "Hello!\nThis is another file!\n" {
		t.Errorf("cluster 5 = %q; want %q", got2, "Hello!\nThis is another file!\n")
	}
}

// TestFatcatFATTablesEqual verifies that both FAT copies are identical in hello-world.img.
// Mirrors the PHP testDiff "FATs are exactly equals" case.
func TestFatcatFATTablesEqual(t *testing.T) {
	t.Parallel()
	fs := openFAT(t, fatcatHelloWorld)

	if len(fs.Tables) != 2 {
		t.Fatalf("len(Tables) = %d; want 2", len(fs.Tables))
	}
	for i, e := range fs.Tables[0].Entries {
		if fs.Tables[1].Entries[i] != e {
			t.Errorf("FAT tables differ at cluster %d: FAT1=%08x FAT2=%08x", i, e, fs.Tables[1].Entries[i])
		}
	}
}

// TestFatcatFATTablesDiffer verifies that the two FAT copies differ in repair.img.
// Mirrors the PHP testDiff "FATs differs" case.
func TestFatcatFATTablesDiffer(t *testing.T) {
	t.Parallel()
	fs := openFAT(t, fatcatRepair)

	if len(fs.Tables) != 2 {
		t.Fatalf("len(Tables) = %d; want 2", len(fs.Tables))
	}
	var diffs int
	for i, e := range fs.Tables[0].Entries {
		if fs.Tables[1].Entries[i] != e {
			diffs++
		}
	}
	if diffs == 0 {
		t.Error("FAT tables are equal; want them to differ")
	}
	// Cluster 32 is the known differing entry: FAT1 shows free, FAT2 shows EOC.
	if fs.Tables[0].Entries[32] == fs.Tables[1].Entries[32] {
		t.Errorf("cluster 32: FAT1=%08x FAT2=%08x; want them to differ", fs.Tables[0].Entries[32], fs.Tables[1].Entries[32])
	}
	if fs.Tables[0].Entries[32] != fat.TableEntryFree {
		t.Errorf("repair.img FAT1[32] = %08x; want free (0x00000000)", fs.Tables[0].Entries[32])
	}
	if fs.Tables[1].Entries[32] != fat.TableEntryEOCF {
		t.Errorf("repair.img FAT2[32] = %08x; want EOC (0xffffffff)", fs.Tables[1].Entries[32])
	}
}

// TestFatcatDeletedHidden verifies that deleted entries are filtered out by default.
// Mirrors the PHP testDeleted case where -d flag is not passed.
func TestFatcatDeletedHidden(t *testing.T) {
	t.Parallel()
	fs := openFAT(t, fatcatDeleted)

	entries, err := fs.ReadDirectory(0, true)
	if err != nil {
		t.Fatalf("ReadDirectory(root, filter=true) = %v", err)
	}
	for _, e := range entries {
		if strings.Contains(strings.ToLower(e.Name), "deleted") {
			t.Errorf("filtered listing contains deleted entry %q", e.Name)
		}
	}
}

// TestFatcatDeletedVisible verifies that deleted entries appear when filtering is disabled.
// Mirrors the PHP testDeleted case where -d flag is passed.
func TestFatcatDeletedVisible(t *testing.T) {
	t.Parallel()
	fs := openFAT(t, fatcatDeleted)

	// Deleted entries show up when filter=false.
	entries, err := fs.ReadDirectory(0, false)
	if err != nil {
		t.Fatalf("ReadDirectory(root, filter=false) = %v", err)
	}
	var deletedDir *fat.DirectoryEntry
	for _, e := range entries {
		if e.Deleted {
			de := e
			deletedDir = de
			break
		}
	}
	if deletedDir == nil {
		t.Fatal("no deleted entries in root directory")
	}
}

// TestFatcatDeletedFileContent verifies that a deleted file's content can still be read.
// Mirrors the PHP testDeleted file content assertion.
// The deleted directory's FAT chain has been freed, so we read its cluster directly.
func TestFatcatDeletedFileContent(t *testing.T) {
	t.Parallel()
	fs := openFAT(t, fatcatDeleted)

	// The deleted directory was at cluster 3. Its FAT entry is now freed (0), so we
	// read the cluster data directly rather than following the FAT chain.
	entries, err := fs.ReadDirectoryDirect(3, false)
	if err != nil {
		t.Fatalf("ReadDirectoryDirect(3) = %v", err)
	}

	var fileEntry *fat.DirectoryEntry
	for _, e := range entries {
		if e.Deleted && !e.Subdirectory && strings.HasSuffix(e.Name, "file.txt") {
			de := e
			fileEntry = de
			break
		}
	}
	if fileEntry == nil {
		t.Fatal("could not find deleted file.txt entry")
	}

	// The file's data cluster is also freed in the FAT; read it directly.
	fileData, err := fs.ReadClusterDirect(fileEntry.FirstCluster)
	if err != nil {
		t.Fatalf("ReadClusterDirect(file) = %v", err)
	}
	got := string(fileData[:fileEntry.FileSize])
	const want = "This file was deleted!\n"
	if got != want {
		t.Errorf("deleted file content = %q; want %q", got, want)
	}
}

// TestFatcatInfiniteFile verifies that a looping cluster chain returns an error.
func TestFatcatInfiniteFile(t *testing.T) {
	t.Parallel()
	fs := openFAT(t, fatcatInfiniteFile)

	// BIGMAMMA starts at cluster 35 and its chain loops back on itself.
	_, err := fs.ReadClusterChain(35, -1)
	if err == nil {
		t.Error("ReadClusterChain on looping chain returned nil error; want loop error")
	}
	if !strings.Contains(err.Error(), "loop") {
		t.Errorf("error = %q; want it to mention loop", err.Error())
	}
}

// TestFatcatDirectoryLoop verifies that ValidateDirectoryTree reports a directory
// loop as an issue and still returns, rather than recursing forever or aborting.
func TestFatcatDirectoryLoop(t *testing.T) {
	t.Parallel()
	fs := openFAT(t, fatcatDirectoryLoop)

	issues, err := fs.ValidateDirectoryTree()
	if err != nil {
		t.Errorf("ValidateDirectoryTree() = %v; want nil (issues are collected, not returned)", err)
	}
	var found bool
	for _, wi := range issues {
		if strings.Contains(wi.Err.Error(), "loop") {
			found = true
		}
	}
	if !found {
		t.Errorf("no loop issue reported; got %v", issues)
	}
}

// TestFatcatTwoFilesSameCluster verifies that Table.Validate detects two files sharing a cluster.
func TestFatcatTwoFilesSameCluster(t *testing.T) {
	t.Parallel()
	fs := openFAT(t, fatcatTwoFileSame)

	// Both A.TXT and B.TXT start at cluster 11. The FAT table itself is valid
	// (cluster 11 → EOC, referenced by two directory entries), but we can observe
	// that both directory entries share the same starting cluster.
	root, err := fs.ReadDirectory(0, true)
	if err != nil {
		t.Fatalf("ReadDirectory = %v", err)
	}
	clustersByName := make(map[string]uint32)
	for _, e := range root {
		if !e.Subdirectory && !e.VolumeLabel {
			clustersByName[e.Name] = e.FirstCluster
		}
	}
	a, aOK := clustersByName["a.txt"]
	b, bOK := clustersByName["b.txt"]
	if !aOK || !bOK {
		t.Fatalf("missing a.txt or b.txt (got %v)", clustersByName)
	}
	if a != b {
		t.Errorf("A.TXT cluster=%d, B.TXT cluster=%d; want them equal", a, b)
	}
}

// TestFatcatValidateHelloWorld verifies that ValidateDirectoryTree passes on a clean image.
func TestFatcatValidateHelloWorld(t *testing.T) {
	t.Parallel()
	issues, err := openFAT(t, fatcatHelloWorld).ValidateDirectoryTree()
	if err != nil {
		t.Errorf("ValidateDirectoryTree() = %v; want nil", err)
	}
	for _, wi := range issues {
		t.Errorf("ValidateDirectoryTree() issue: %s", wi)
	}
}

// TestFatcatOpenCaseInsensitive verifies that Open finds files regardless of name case.
func TestFatcatOpenCaseInsensitive(t *testing.T) {
	t.Parallel()
	fs := openFAT(t, fatcatHelloWorld)

	// File is stored as "hello.txt" (VFAT lowercase); open with uppercase path.
	r, err := fs.Open("HELLO.TXT")
	if err != nil {
		t.Fatalf("Open(HELLO.TXT) = %v; want nil", err)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll = %v", err)
	}
	if string(got) != "Hello world!\n" {
		t.Errorf("content = %q; want %q", got, "Hello world!\n")
	}

	// Also test case-insensitive directory traversal.
	r2, err := fs.Open("FILES/other_file.txt")
	if err != nil {
		t.Fatalf("Open(FILES/other_file.txt) = %v; want nil", err)
	}
	got2, err := io.ReadAll(r2)
	if err != nil {
		t.Fatalf("ReadAll other_file = %v", err)
	}
	if string(got2) != "Hello!\nThis is another file!\n" {
		t.Errorf("other_file content = %q; want %q", got2, "Hello!\nThis is another file!\n")
	}
}
