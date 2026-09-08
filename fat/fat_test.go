package fat_test

import (
	"bytes"
	"strings"
	"testing"

	"samuelks.com/datarescue/fat"
	"samuelks.com/datarescue/internal/testfixture"
)

const (
	fat12Image = "testdata/fat12-empty.img.gz"
	fat16Image = "testdata/fat16-empty.img.gz"
	fat32Image = "testdata/fat32-empty.img.gz"
)

// decompressGZImage returns a fixture image, skipping the test when it is
// absent so every package reports a missing fixture the same way.
func decompressGZImage(t *testing.T, path string) []byte {
	t.Helper()
	return testfixture.Bytes(t, path)
}

func readGZImage(t *testing.T, path string) *bytes.Reader {
	return bytes.NewReader(decompressGZImage(t, path))
}

func openFAT(t *testing.T, path string) *fat.FAT {
	t.Helper()
	fs, err := fat.Open(readGZImage(t, path), 0)
	if err != nil {
		t.Fatal(err)
	}
	return fs
}

func TestFAT12Open(t *testing.T) {
	t.Parallel()
	fs := openFAT(t, fat12Image)
	if fs.Type != fat.TypeFAT12 {
		t.Errorf("Type = %q; want %q", fs.Type, fat.TypeFAT12)
	}
	if len(fs.Tables) != 2 {
		t.Errorf("len(Tables) = %d; want 2", len(fs.Tables))
	}
}

func TestFAT12BPBFields(t *testing.T) {
	t.Parallel()
	fs := openFAT(t, fat12Image)
	bpb := fs.BootSector.BIOSParameterBlock
	if bpb.BytesPerSector != 512 {
		t.Errorf("BytesPerSector = %d; want 512", bpb.BytesPerSector)
	}
	if bpb.SectorsPerCluster != 1 {
		t.Errorf("SectorsPerCluster = %d; want 1", bpb.SectorsPerCluster)
	}
	if bpb.NumberOfFATs != 2 {
		t.Errorf("NumberOfFATs = %d; want 2", bpb.NumberOfFATs)
	}
	if bpb.RootEntries != 224 {
		t.Errorf("RootEntries = %d; want 224", bpb.RootEntries)
	}
	if bpb.SectorsPerFAT != 9 {
		t.Errorf("SectorsPerFAT = %d; want 9", bpb.SectorsPerFAT)
	}
	if fs.TotalSectors != 2880 {
		t.Errorf("TotalSectors = %d; want 2880", fs.TotalSectors)
	}
}

func TestFAT12TableEntries(t *testing.T) {
	t.Parallel()
	fs := openFAT(t, fat12Image)
	for i, table := range fs.Tables {
		// Entry 0: media descriptor 0xFF0 expanded to 0xFFFFFFF0.
		if table.Entries[0] != fat.TableEntryNonStandard0 {
			t.Errorf("table.Entries[%d][0] = 0x%08X; want 0x%08X", i, table.Entries[0], fat.TableEntryNonStandard0)
		}
		// Entry 1: EOC 0xFFF expanded to 0xFFFFFFFF.
		if table.Entries[1] != fat.TableEntryEOCF {
			t.Errorf("table.Entries[%d][1] = 0x%08X; want 0x%08X", i, table.Entries[1], fat.TableEntryEOCF)
		}
		for j := 2; j < 10; j++ {
			if table.Entries[j] != fat.TableEntryFree {
				t.Errorf("table.Entries[%d][%d] = 0x%08X; want 0x00000000 (free)", i, j, table.Entries[j])
			}
		}
	}
}

func TestFAT16Open(t *testing.T) {
	t.Parallel()
	fs := openFAT(t, fat16Image)
	if fs.Type != fat.TypeFAT16 {
		t.Errorf("Type = %q; want %q", fs.Type, fat.TypeFAT16)
	}
	if len(fs.Tables) != 2 {
		t.Errorf("len(Tables) = %d; want 2", len(fs.Tables))
	}
}

func TestFAT16BPBFields(t *testing.T) {
	t.Parallel()
	fs := openFAT(t, fat16Image)
	bpb := fs.BootSector.BIOSParameterBlock
	if bpb.BytesPerSector != 512 {
		t.Errorf("BytesPerSector = %d; want 512", bpb.BytesPerSector)
	}
	if bpb.SectorsPerCluster != 4 {
		t.Errorf("SectorsPerCluster = %d; want 4", bpb.SectorsPerCluster)
	}
	if bpb.NumberOfFATs != 2 {
		t.Errorf("NumberOfFATs = %d; want 2", bpb.NumberOfFATs)
	}
	if bpb.RootEntries != 512 {
		t.Errorf("RootEntries = %d; want 512", bpb.RootEntries)
	}
	if bpb.SectorsPerFAT != 32 {
		t.Errorf("SectorsPerFAT = %d; want 32", bpb.SectorsPerFAT)
	}
	if fs.TotalSectors != 32768 {
		t.Errorf("TotalSectors = %d; want 32768", fs.TotalSectors)
	}
}

func TestFAT16TableEntries(t *testing.T) {
	t.Parallel()
	fs := openFAT(t, fat16Image)
	for i, table := range fs.Tables {
		// Entry 0: media descriptor 0xFFF8 expanded to 0xFFFFFFF8.
		if table.Entries[0] != fat.TableEntryEOC8 {
			t.Errorf("table.Entries[%d][0] = 0x%08X; want 0x%08X", i, table.Entries[0], fat.TableEntryEOC8)
		}
		// Entry 1: EOC 0xFFFF expanded to 0xFFFFFFFF.
		if table.Entries[1] != fat.TableEntryEOCF {
			t.Errorf("table.Entries[%d][1] = 0x%08X; want 0x%08X", i, table.Entries[1], fat.TableEntryEOCF)
		}
		for j := 2; j < 10; j++ {
			if table.Entries[j] != fat.TableEntryFree {
				t.Errorf("table.Entries[%d][%d] = 0x%08X; want 0x00000000 (free)", i, j, table.Entries[j])
			}
		}
	}
}

func TestRootDirectoryEmpty(t *testing.T) {
	t.Parallel()
	for _, img := range []string{fat12Image, fat16Image} {
		t.Run(img, func(t *testing.T) {
			t.Parallel()
			entries, err := openFAT(t, img).ReadDirectory(0, true)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Errorf("got %d root directory entries; want 0", len(entries))
			}
		})
	}
}

func TestOpenFileNotFound(t *testing.T) {
	t.Parallel()
	for _, img := range []string{fat12Image, fat16Image} {
		t.Run(img, func(t *testing.T) {
			t.Parallel()
			_, err := openFAT(t, img).Open("MISSING.TXT")
			if err == nil {
				t.Error("Open(\"MISSING.TXT\") returned nil error; want error for nonexistent file")
			}
		})
	}
}

func TestValidateDirectoryTree(t *testing.T) {
	t.Parallel()
	for _, img := range []string{fat12Image, fat16Image} {
		t.Run(img, func(t *testing.T) {
			t.Parallel()
			issues, err := openFAT(t, img).ValidateDirectoryTree()
			if err != nil {
				t.Errorf("ValidateDirectoryTree() = %v; want nil", err)
			}
			for _, wi := range issues {
				t.Errorf("ValidateDirectoryTree() issue: %s", wi)
			}
		})
	}
}

func TestFAT32Open(t *testing.T) {
	t.Parallel()
	fs, err := fat.Open(readGZImage(t, fat32Image), 0)
	if err != nil {
		t.Fatalf("Open() = %v; want nil", err)
	}
	if fs.Type != fat.TypeFAT32 {
		t.Errorf("Type = %q; want %q", fs.Type, fat.TypeFAT32)
	}
	if len(fs.Tables) != 2 {
		t.Errorf("len(Tables) = %d; want 2", len(fs.Tables))
	}
}

// TestZeroMonthDatesReturnZeroTime verifies that a directory entry whose date fields
// encode month=0 (a corrupted or unset timestamp) yields time.Time{} rather than
// silently wrapping to December of the prior year.
func TestZeroMonthDatesReturnZeroTime(t *testing.T) {
	t.Parallel()
	data := decompressGZImage(t, fat16Image)

	// Open once to discover RootDirectoryOffset without hardcoding layout math.
	fs, err := fat.Open(bytes.NewReader(data), 0)
	if err != nil {
		t.Fatal(err)
	}
	rootOff := fs.RootDirectoryOffset

	// Inject a 32-byte directory entry: Name="TEST    ", Ext="TXT", Attribute=0x20
	// (archive), all date/time fields zero (month bits = 0 → invalid date).
	var entry [32]byte
	copy(entry[0:8], "TEST    ")
	copy(entry[8:11], "TXT")
	entry[11] = 0x20
	for i, b := range entry {
		data[rootOff+int64(i)] = b
	}

	fs2, err := fat.Open(bytes.NewReader(data), 0)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := fs2.ReadDirectory(0, true)
	if err != nil {
		t.Fatalf("ReadDirectory = %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no directory entries found after patching root directory")
	}
	e := entries[0]
	if !e.Created.IsZero() {
		t.Errorf("Created = %v; want zero time for zero-month date", e.Created)
	}
	if !e.LastModified.IsZero() {
		t.Errorf("LastModified = %v; want zero time for zero-month date", e.LastModified)
	}
	if !e.LastAccessed.IsZero() {
		t.Errorf("LastAccessed = %v; want zero time for zero-month date", e.LastAccessed)
	}
}

// bpbNumberOfFATsOffset is the byte offset of the NumberOfFATs field in a FAT boot sector.
// Jump[3] + OEMName[8] + BytesPerSector[2] + SectorsPerCluster[1] + ReservedSectors[2] = 16.
const bpbNumberOfFATsOffset = 16

func TestSingleFATTable(t *testing.T) {
	t.Parallel()
	data := decompressGZImage(t, fat16Image)
	data[bpbNumberOfFATsOffset] = 1 // reduce to a single FAT table

	fs, err := fat.Open(bytes.NewReader(data), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(fs.Tables) != 1 {
		t.Fatalf("len(Tables) = %d; want 1", len(fs.Tables))
	}

	// ReadClusterChain must not panic when only one FAT table is present.
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("ReadClusterChain panicked with a single FAT table: %v", r)
		}
	}()
	_, _ = fs.ReadClusterChain(2, -1)
}

// TestOpenEmptyPath verifies that Open returns an error for empty and root-only paths.
func TestOpenEmptyPath(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"", "/", "///"} {
		fs := openFAT(t, fat16Image)
		_, err := fs.Open(path)
		if err == nil {
			t.Errorf("Open(%q) returned nil error; want error for empty path", path)
		}
	}
}

// TestTableValidateReservedCluster1 verifies that Table.Validate rejects an entry
// pointing to reserved cluster 1.
func TestTableValidateReservedCluster1(t *testing.T) {
	t.Parallel()
	table := fat.Table{Entries: []fat.TableEntry{
		fat.TableEntryEOC8,      // entry 0: media descriptor
		fat.TableEntryEOCF,      // entry 1: EOC
		fat.TableEntryReserved1, // entry 2: invalid — points to reserved cluster 1
	}}
	if err := table.Validate(); err == nil {
		t.Error("Validate() returned nil; want error for entry pointing to cluster 1")
	}
}

// TestTableValidateDirtyVolumeFlags verifies that the reserved value in entry 1
// is not read as a cluster pointer. A FAT16 volume that was not cleanly
// unmounted has the clean-shutdown bit cleared there, leaving 0x7FFF, which
// collides with whichever entry genuinely points at cluster 32767.
func TestTableValidateDirtyVolumeFlags(t *testing.T) {
	t.Parallel()
	entries := make([]fat.TableEntry, 40)
	entries[0] = 0xFFFFFFF8 // media descriptor for a fixed disk
	entries[1] = 0xFFFF7FFF // EOC with the clean-shutdown bit cleared
	entries[2] = 3
	entries[3] = fat.TableEntryEOCF
	table := fat.Table{Entries: entries}
	if err := table.Validate(); err != nil {
		t.Errorf("Validate() = %v; entry 1 holds volume flags, not a cluster pointer", err)
	}
}

// TestTableValidateDuplicateClaim verifies that two entries pointing at one
// cluster are still reported, and that the message identifies both of them.
func TestTableValidateDuplicateClaim(t *testing.T) {
	t.Parallel()
	table := fat.Table{Entries: []fat.TableEntry{
		fat.TableEntryEOC8, // entry 0: media descriptor
		fat.TableEntryEOCF, // entry 1: EOC
		4,                  // entry 2
		4,                  // entry 3: claims cluster 4 a second time
		fat.TableEntryEOCF, // entry 4
	}}
	err := table.Validate()
	if err == nil {
		t.Fatal("Validate() returned nil; want error for cluster 4 claimed twice")
	}
	for _, want := range []string{"cluster 4", "entry 2", "entry 3"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Validate() = %q; want it to mention %q", err, want)
		}
	}
}

// TestWalk verifies that Walk enumerates all files in a FAT32 image.
func TestWalk(t *testing.T) {
	t.Parallel()
	fs := openFAT(t, fatcatHelloWorld)

	seen := make(map[string]bool)
	issues, err := fs.Walk(func(path string, entry *fat.DirectoryEntry) error {
		seen[path] = true
		return nil
	})
	if err != nil {
		t.Fatalf("Walk() = %v", err)
	}
	for _, wi := range issues {
		t.Errorf("Walk() issue: %s", wi)
	}

	for _, want := range []string{"hello.txt", "files", "files/other_file.txt"} {
		if !seen[want] {
			t.Errorf("Walk did not visit %q; visited: %v", want, seen)
		}
	}
}
