package fat_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"samuelks.com/datarescue/fat"
)

// The fatcat fixtures each capture a specific kind of damage. These tests pin
// the behavior the recovery fixes give on them, so a regression shows up as a
// changed result rather than as a hang or a silent truncation.

// A directory that points back at an ancestor must be reported and the walk
// must terminate. Without the cycle guard this would recurse forever.
func TestRegressionDirectoryLoopTerminates(t *testing.T) {
	t.Parallel()
	fs := openFAT(t, fatcatDirectoryLoop)

	done := make(chan struct{})
	var issues []fat.WalkIssue
	var visited int
	go func() {
		defer close(done)
		issues, _ = fs.Walk(func(string, *fat.DirectoryEntry) error {
			visited++
			return nil
		})
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Walk did not terminate on a looping directory tree")
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
	if visited == 0 {
		t.Error("the walk reported the loop but visited nothing")
	}
}

// A file whose cluster chain points at itself must be reported as a corrupt
// chain, not followed until memory runs out.
func TestRegressionInfiniteFile(t *testing.T) {
	t.Parallel()
	fs := openFAT(t, fatcatInfiniteFile)
	issues, err := fs.CheckDirectoryEntries()
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, is := range issues {
		if is.IssueKind == fat.IssueChainCorrupt && strings.Contains(is.Detail, "loops") {
			found = true
			t.Logf("%s: %s", is.Path, is.Detail)
		}
	}
	if !found {
		t.Errorf("no looping-chain issue reported; got %+v", issues)
	}
}

// The volume declares a 1 TB disk that the image does not contain. Opening must
// succeed on the readable FAT copy rather than failing outright, and the entries
// backed by absent sectors must be marked unreadable rather than read as free.
func TestRegressionFakeBigDisk(t *testing.T) {
	t.Parallel()
	fs := openFAT(t, fatcatFakeBigDisk)
	if len(fs.Tables) == 0 {
		t.Fatal("no FAT copy loaded")
	}
	t.Logf("copies loaded: %v", fs.TableCopies)
	var unreadable int
	for _, tb := range fs.Tables {
		unreadable += tb.UnreadableEntries()
	}
	if unreadable == 0 {
		t.Error("no entries marked unreadable although the image is far shorter than the declared volume")
	}
	t.Logf("%d entries lie in sectors the image does not contain", unreadable)
}

// Two directory entries share one cluster. The FAT itself is consistent — the
// cluster is allocated once and ends the chain — so the allocation summary must
// count it once rather than reporting a phantom orphan or a double-used cluster.
func TestRegressionTwoFilesSameCluster(t *testing.T) {
	t.Parallel()
	fs := openFAT(t, fatcatTwoFileSame)

	// The shared cluster is genuinely allocated, so the table is valid; the
	// duplication lives in the directory entries, not the FAT.
	if err := fs.Tables[0].Validate(); err != nil {
		t.Errorf("Validate() = %v; the FAT is consistent, only the directory entries overlap", err)
	}

	summary, err := fs.AllocationSummary()
	if err != nil {
		t.Fatalf("AllocationSummary = %v", err)
	}
	t.Logf("total=%d free=%d reachable=%d orphaned=%d bad=%d",
		summary.Total, summary.Free, summary.Reachable, summary.Orphaned, summary.Bad)
	if summary.Reachable+summary.Free+summary.Orphaned+summary.Bad != summary.Total {
		t.Errorf("cluster counts do not add up to the total: %+v", summary)
	}

	a, err := fs.Open("a.txt")
	if err != nil {
		t.Fatal(err)
	}
	b, err := fs.Open("b.txt")
	if err != nil {
		t.Fatal(err)
	}
	da, _ := io.ReadAll(a)
	db, _ := io.ReadAll(b)
	if string(da) != string(db) {
		t.Errorf("two entries share cluster %d but read back differently", 11)
	}
}

// One FAT copy has a chain the other does not. The default policy falls back to
// the copy that carries an end-of-chain marker, and the explicit policies pick
// the copy the operator selects.
func TestRegressionFATCopyFallback(t *testing.T) {
	t.Parallel()
	const brokenPath = "fat1_broken/file.txt"
	tests := []struct {
		policy    fat.ChainPolicy
		wantOpen  bool
		wantDiags bool
	}{
		{fat.ChainPolicyStrict, true, true},
		{fat.ChainPolicyCopy0, false, false},
		{fat.ChainPolicyCopy1, true, false},
		{fat.ChainPolicyBestEffort, true, false},
	}
	for _, tc := range tests {
		t.Run(tc.policy.String(), func(t *testing.T) {
			t.Parallel()
			fs := openFAT(t, fatcatRepair)
			fs.ChainPolicy = tc.policy
			_ = fs.Diagnostics() // drop anything from Open

			r, err := fs.Open(brokenPath)
			if tc.wantOpen {
				if err != nil {
					t.Fatalf("Open(%q) = %v; want success under %s", brokenPath, err, tc.policy)
				}
				if _, err := io.ReadAll(r); err != nil {
					t.Errorf("reading %q: %v", brokenPath, err)
				}
			} else if err == nil {
				t.Fatalf("Open(%q) succeeded under %s; copy 0 has no chain for it", brokenPath, tc.policy)
			}
			diags := fs.Diagnostics()
			if tc.wantDiags && len(diags) == 0 {
				t.Errorf("%s resolved the chain with no diagnostic explaining which copy was used", tc.policy)
			}
			for _, d := range diags {
				t.Logf("%s: %s", tc.policy, d)
			}
		})
	}
}

// A FAT32 boot sector with RootClus = 0 has no root directory to walk. Without
// a chain the read would fall into the FAT12/16 fixed-root branch sized by
// RootEntries (0 on FAT32) and the volume would parse as silently empty.
func TestRegressionFAT32RootClusterZeroIsLoud(t *testing.T) {
	t.Parallel()
	data := decompressGZImage(t, fatcatHelloWorld)
	clear(data[0x2C:0x30]) // extended BPB RootClus

	fs, err := fat.Open(bytes.NewReader(data), 0)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, d := range fs.Diagnostics() {
		if strings.Contains(d.Message, "root cluster") {
			found = true
		}
	}
	if !found {
		t.Error("Open recorded no root-cluster-zero diagnostic")
	}

	_, err = fs.ReadDirectory(0, true)
	if err == nil {
		t.Fatal("ReadDirectory on a FAT32 with RootClus = 0 succeeded; want a loud error")
	}
	if !strings.Contains(err.Error(), "root cluster") {
		t.Errorf("ReadDirectory error = %q; want it to name the root cluster", err)
	}
}

// A boot sector declaring 4 GiB over a ~50 MiB image must not let a chain read
// allocate for the whole declared region: the chain is bounded against the
// image's actual end at Open. Both paths that read a chain into one buffer are
// covered — ReadClusterChain and the root directory read.
func TestRegressionChainLongerThanImageIsRejected(t *testing.T) {
	t.Parallel()
	data := decompressGZImage(t, fatcatHelloWorld)
	// FAT32 extended BPB: inflate the declared volume to 4 GiB and each FAT
	// copy to 16 MiB so a 400k-cluster chain resolves inside the table.
	binary.LittleEndian.PutUint32(data[0x20:0x24], uint32(4<<30/512)) // LargeSectors
	binary.LittleEndian.PutUint32(data[0x24:0x28], 32<<10)            // SectorsPerFAT
	seed, err := fat.Open(bytes.NewReader(data), 0)
	if err != nil {
		t.Fatal(err)
	}

	const chainStart, chainLen = 300000, 400000
	const eocValue = uint32(0x0FFFFFFF) // FAT32 end-of-chain marker
	for _, fatOff := range seed.FATTableOffsets {
		for c := uint32(chainStart); c < chainStart+chainLen; c++ {
			binary.LittleEndian.PutUint32(data[fatOff+int64(c)*4:], c+1)
		}
		binary.LittleEndian.PutUint32(data[fatOff+int64(chainStart+chainLen)*4:], eocValue)
	}

	fs, err := fat.Open(bytes.NewReader(data), 0)
	if err != nil {
		t.Fatal(err)
	}
	fs.ChainPolicy = fat.ChainPolicyCopy0
	_, err = fs.ReadClusterChain(chainStart, -1)
	if err == nil || !strings.Contains(err.Error(), "data region") {
		t.Fatalf("ReadClusterChain = %v; want a data-region bound error", err)
	}

	// The root directory's own chain now runs off the same long chain, which
	// exercises the same bound in the directory read.
	for _, fatOff := range seed.FATTableOffsets {
		binary.LittleEndian.PutUint32(data[fatOff+int64(seed.RootCluster)*4:], chainStart)
	}
	fs2, err := fat.Open(bytes.NewReader(data), 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = fs2.ReadDirectory(0, true)
	if err == nil || !strings.Contains(err.Error(), "data region") {
		t.Fatalf("ReadDirectory = %v; want a data-region bound error", err)
	}
}

// A read that runs off the end of a truncated image must return the bytes
// recovered before the failure, with a typed error identifying the cluster,
// rather than discarding the readable prefix.
func TestRegressionPartialChainRead(t *testing.T) {
	t.Parallel()
	full := decompressGZImage(t, fatcatHelloWorld)
	fs, err := fat.Open(bytes.NewReader(full), 0)
	if err != nil {
		t.Fatal(err)
	}
	// Cut the image three clusters into a six-cluster run.
	cut, err := fs.ClusterOffset(5)
	if err != nil {
		t.Fatal(err)
	}
	truncated, err := fat.Open(bytes.NewReader(full[:cut]), 0)
	if err != nil {
		t.Fatal(err)
	}
	r, err := truncated.OpenContiguousChain(2, 6, 0)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	var cre *fat.ClusterReadError
	if !errors.As(err, &cre) {
		t.Fatalf("err = %v; want a *fat.ClusterReadError", err)
	}
	want := 3 * truncated.ClusterSize
	t.Logf("recovered %d bytes before cluster %d failed", len(got), cre.Cluster)
	if len(got) != want {
		t.Errorf("recovered %d bytes; want the %d readable bytes before the cut", len(got), want)
	}
}
