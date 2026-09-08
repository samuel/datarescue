package fat_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"testing"

	"samuelks.com/datarescue/fat"
	"samuelks.com/datarescue/internal/testfat"
)

// subDir is the multi-cluster subdirectory these tests damage. Its own "." and
// ".." entries plus one per file at 32 bytes each have to overflow a cluster,
// or a bad sector in the middle of it would not be in the middle of anything.
const subDir = "SUB"

func multiClusterDir() (testfat.Options, []string) {
	var files []testfat.File
	var names []string
	for i := range testfat.EntriesPerCluster + 8 {
		f := testfat.File{Name: fmt.Sprintf("F%03d.BIN", i), Size: 1}
		files = append(files, f)
		names = append(names, f.Name)
	}
	return testfat.Options{Dirs: []testfat.Dir{{Name: subDir, Files: files}}}, names
}

// A directory is read sector by sector, so one bad sector loses only the
// sixteen entries it held. Reading the chain a cluster at a time with one
// io.ReadFull would lose everything after the failure (the rest of that cluster
// and every cluster after it, however well they read) and with it every file
// and subdirectory listed there.
func TestDirectorySurvivesOneBadSector(t *testing.T) {
	t.Parallel()
	opts, names := multiClusterDir()
	im := testfat.Build(opts)
	d := im.FindDir(subDir)
	if d.Clusters < 2 {
		t.Fatalf("%s spans %d cluster(s); this test needs it to span more than one", subDir, d.Clusters)
	}

	// The second sector of the directory's first cluster: entries 16 to 31.
	const lostFrom, lostTo = testfat.SectorSize, 2 * testfat.SectorSize
	fs, err := fat.Open(&holedReader{data: im.Data, badOff: d.Offset + lostFrom, badSize: testfat.SectorSize}, 0)
	if err != nil {
		t.Fatalf("opening the damaged image: %v", err)
	}

	entries, err := fs.ReadDirectory(uint32(d.Cluster), true)
	if err == nil {
		t.Fatal("ReadDirectory reported no problem for a directory with a bad sector in it")
	}
	var dre *fat.DirReadError
	if !errors.As(err, &dre) {
		t.Fatalf("error = %v; want a *fat.DirReadError", err)
	}
	if len(dre.Lost) != 1 || dre.Lost[0].Start != lostFrom || dre.Lost[0].End != lostTo {
		t.Errorf("DirReadError.Lost = %v; want the one sector at %d", dre.Lost, lostFrom)
	}

	got := make(map[string]bool, len(entries))
	for _, e := range entries {
		got[e.Name] = true
	}
	// Entry 0 is "." and entry 1 is "..", so file i sits at entry i+2.
	var recovered int
	for i, name := range names {
		off := (i + 2) * testfat.EntrySize
		lost := off >= lostFrom && off < lostTo
		switch {
		case lost && got[name]:
			t.Errorf("%s is in the unreadable sector but came back", name)
		case !lost && !got[name]:
			t.Errorf("%s is in a sector that read but is missing", name)
		case !lost:
			recovered++
		}
	}
	if recovered == 0 {
		t.Fatal("nothing was recovered")
	}
	// This is why the directory is read by sector: the entries past the hole,
	// in the second cluster, are still there.
	if !got[names[len(names)-1]] {
		t.Error("the last entry, in the directory's second cluster, was lost to a bad sector in the first")
	}
}

// The walk carries on past a damaged directory and still descends into what was
// recovered from it.
func TestWalkPastADamagedDirectory(t *testing.T) {
	t.Parallel()
	opts, names := multiClusterDir()
	im := testfat.Build(opts)
	d := im.FindDir(subDir)
	fs, err := fat.Open(&holedReader{data: im.Data, badOff: d.Offset + testfat.SectorSize, badSize: testfat.SectorSize}, 0)
	if err != nil {
		t.Fatalf("opening the damaged image: %v", err)
	}

	var seen int
	issues, err := fs.Walk(func(string, *fat.DirectoryEntry) error {
		seen++
		return nil
	})
	if err != nil {
		t.Fatalf("Walk = %v; a damaged directory must not stop it", err)
	}
	if len(issues) == 0 {
		t.Error("Walk recorded no issue for the directory with a bad sector in it")
	}
	// The directory itself plus every file outside the lost sector.
	if seen <= len(names)/2 {
		t.Errorf("Walk visited %d entries of %d; the recovered part of the directory was thrown away", seen, len(names))
	}
}

// A zero-length file has no data and no chain to resolve. A sound volume leaves
// its entry's first cluster at 0, which is not a cluster at all, and a stale
// pointer left there must not have that cluster's contents streamed as the
// file's.
func TestEmptyFileOpensEmpty(t *testing.T) {
	t.Parallel()
	const empty = "EMPTY.TXT"
	im := testfat.Build(testfat.Options{Files: []testfat.File{
		{Name: empty, Size: 0},
		{Name: bigFile, Size: bigSize},
	}})
	if p := im.Find(empty); p.Cluster != 0 || p.Clusters != 0 {
		t.Fatalf("the empty file was given cluster %d; a real volume allocates it none", p.Cluster)
	}
	fs, err := fat.Open(bytes.NewReader(im.Data), 0)
	if err != nil {
		t.Fatal(err)
	}

	r, err := fs.Open(empty)
	if err != nil {
		t.Fatalf("opening a zero-length file by name: %v", err)
	}
	b, err := io.ReadAll(r)
	if err != nil {
		t.Errorf("reading a zero-length file: %v", err)
	}
	if len(b) != 0 {
		t.Errorf("a zero-length file delivered %d bytes", len(b))
	}

	// The second order of the same bug: a size-0 entry that kept a first
	// cluster from before it was truncated.
	p := im.Find(bigFile)
	stale, err := fs.OpenClusterChain(uint32(p.Cluster), 0)
	if err != nil {
		t.Fatalf("opening a size-0 entry with a stale first cluster: %v", err)
	}
	b, err = io.ReadAll(stale)
	if err != nil {
		t.Errorf("reading it: %v", err)
	}
	if len(b) != 0 {
		t.Errorf("a size-0 entry with a stale first cluster streamed %d bytes of another file's data", len(b))
	}
}

// A path the volume does not hold is a different finding from one that would
// not read, and the sentinel is how a caller tells them apart without matching
// on message text.
func TestOpenNotFoundIsTyped(t *testing.T) {
	t.Parallel()
	opts, _ := multiClusterDir()
	im := testfat.Build(opts)
	fs, err := fat.Open(bytes.NewReader(im.Data), 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"NOPE.TXT", subDir, subDir + "/NOPE.TXT", "F000.BIN/X"} {
		if _, err := fs.Open(path); !errors.Is(err, fat.ErrNotFound) {
			t.Errorf("Open(%q) = %v; want fat.ErrNotFound", path, err)
		}
	}
}

// A cluster number past the end of the table is not a cluster of this volume,
// so it must not be counted as one of the chain's. Otherwise every size derived
// from the chain (an orphan's, a file's) comes out one cluster too large.
func TestClusterChainDropsAClusterPastTheTable(t *testing.T) {
	t.Parallel()
	// Cluster 2 chains to 3, which points at 5 — past this four-entry table.
	tbl := fat.Table{Entries: []fat.TableEntry{0x0FFFFFF8, 0x0FFFFFFF, 3, 5}}
	got, final, err := tbl.Cluster(2)
	if err == nil {
		t.Fatal("Cluster reported no problem for a chain running past the table")
	}
	want := []uint32{2, 3}
	if len(got) != len(want) {
		t.Fatalf("Cluster = %v; want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("cluster %d = %d; want %d", i, got[i], want[i])
		}
	}
	if final != 5 {
		t.Errorf("final entry = %d; want the out-of-range value 5", final)
	}
}

// The FAT is a whole number of sectors, so it has entries past the last real
// cluster. Counting those reports free clusters the volume does not have.
func TestAllocationSummaryCountsDataClusters(t *testing.T) {
	t.Parallel()
	im := testfat.Build(testfat.Options{Files: []testfat.File{{Name: bigFile, Size: bigSize}}})
	fs, err := fat.Open(bytes.NewReader(im.Data), 0)
	if err != nil {
		t.Fatal(err)
	}
	if int64(fs.Tables[0].Len()) <= fs.DataClusters()+2 {
		t.Skip("this geometry's FAT has no entries past the last cluster, so there is nothing to miscount")
	}
	s, err := fs.AllocationSummary()
	if err != nil {
		t.Fatal(err)
	}
	if int64(s.Total) != fs.DataClusters() {
		t.Errorf("Total = %d; want the %d clusters the data region holds, not the %d FAT entries",
			s.Total, fs.DataClusters(), fs.Tables[0].Len())
	}
	if s.Free+s.Bad+s.Reachable+s.Orphaned != s.Total {
		t.Errorf("the summary does not add up: %+v", s)
	}
}

// merged is declared under the mutex; -race is what checks it stays there.
func TestMergedTableConcurrent(t *testing.T) {
	t.Parallel()
	im := testfat.Build(testfat.Options{Files: []testfat.File{{Name: bigFile, Size: bigSize}}})
	fs, err := fat.Open(bytes.NewReader(im.Data), 0)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 8)
	for range 8 {
		go func() {
			_, err := fs.MergedTable()
			done <- err
		}()
	}
	for range 8 {
		if err := <-done; err != nil {
			t.Error(err)
		}
	}
}

// countingReader counts the reads it serves, so a test can assert that a chain
// is read once rather than once per pass of a fixpoint loop. On damaged media
// the drive's retry budget is the scarce resource.
type countingReader struct {
	data  []byte
	off   int64
	reads int
}

func (r *countingReader) Read(p []byte) (int, error) {
	if r.off >= int64(len(r.data)) {
		return 0, io.EOF
	}
	r.reads++
	n := copy(p, r.data[r.off:])
	r.off += int64(n)
	return n, nil
}

func (r *countingReader) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		r.off = off
	case io.SeekCurrent:
		r.off += off
	default:
		r.off = int64(len(r.data)) + off
	}
	return r.off, nil
}

// Every orphan chain is read at most once. Otherwise the fixpoint loop re-reads
// and re-parses each chain that has not turned out to be a directory on every
// pass, and re-appends the parse's diagnostics each time.
func TestFindOrphansReadsEachChainOnce(t *testing.T) {
	t.Parallel()
	files := []testfat.File{
		{Name: "A.BIN", Size: 3 * testfat.ClusterSize},
		{Name: "B.BIN", Size: 2 * testfat.ClusterSize},
		{Name: "C.BIN", Size: testfat.ClusterSize},
	}
	im := testfat.Build(testfat.Options{Files: files})
	// Erase the root directory's entries. The chains stay allocated in the FAT
	// and no directory entry points to them, which is what an orphan is.
	fs, err := fat.Open(bytes.NewReader(im.Data), 0)
	if err != nil {
		t.Fatal(err)
	}
	root := fs.RootDirectoryOffset
	clear(im.Data[root : root+int64(len(files))*testfat.EntrySize])

	cr := &countingReader{data: im.Data}
	fs, err = fat.Open(cr, 0)
	if err != nil {
		t.Fatal(err)
	}
	before := cr.reads
	orphans, err := fs.FindOrphans()
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != len(files) {
		t.Fatalf("found %d orphan chains; want %d", len(orphans), len(files))
	}
	var clusters int
	for _, o := range orphans {
		clusters += len(o.Clusters)
	}
	// One read per cluster of each chain, plus the root directory's sectors.
	if reads := cr.reads - before; reads > clusters+testfat.EntriesPerCluster {
		t.Errorf("FindOrphans served %d reads for %d orphan clusters; the chains are being re-read",
			reads, clusters)
	}
}

// A diagnostic belongs to the directory whose read produced it. Diagnostics()
// clears the list, so one left pending surfaces under whichever file is read
// next — a directory's problems reported against its first child.
func TestWalkAttributesDirectoryDiagnostics(t *testing.T) {
	t.Parallel()
	opts, _ := multiClusterDir()
	im := testfat.Build(opts)
	sound, err := fat.Open(bytes.NewReader(im.Data), 0)
	if err != nil {
		t.Fatal(err)
	}
	// The first sector of FAT copy 1, which holds the entries for the
	// subdirectory's own chain. Copy 0 still has them, so resolving the chain
	// falls back to it and says so — which is the diagnostic to attribute.
	bad := sound.FATTableOffsets[1]
	fs, err := fat.Open(&holedReader{data: im.Data, badOff: bad, badSize: testfat.SectorSize}, 0)
	if err != nil {
		t.Fatalf("opening the damaged image: %v", err)
	}
	if len(fs.Diagnostics()) == 0 {
		t.Fatal("opening an image with a damaged FAT copy reported nothing")
	}

	issues, err := fs.Walk(func(string, *fat.DirectoryEntry) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	var diags int
	for _, wi := range issues {
		if !wi.Diag {
			t.Errorf("unexpected walk failure: %s", wi)
			continue
		}
		diags++
		if wi.Path != subDir {
			t.Errorf("the diagnostic is attributed to %q; want the directory %q that produced it", wi.Path, subDir)
		}
	}
	if diags == 0 {
		t.Error("the walk reported no diagnostic; the fallback to FAT copy 0 went unattributed")
	}
	if left := fs.Diagnostics(); len(left) != 0 {
		t.Errorf("%d diagnostic(s) left pending after the walk, to surface under the next read: %v", len(left), left)
	}
}

// The FAT is a whole number of sectors, so it has entries past the last real
// cluster. A non-free entry in that slack is not an orphan chain: there is no
// data region under it to read, and listing one reports a recovery of bytes the
// volume does not have. AllocationSummary and FindOrphans are bounded the same
// way, so the count verify prints and the chains orphans lists agree.
func TestFindOrphansStopsAtTheLastDataCluster(t *testing.T) {
	t.Parallel()
	im := testfat.Build(testfat.Options{Files: []testfat.File{{Name: bigFile, Size: bigSize}}})
	sound, err := fat.Open(bytes.NewReader(im.Data), 0)
	if err != nil {
		t.Fatal(err)
	}
	last := sound.DataClusters() + 1
	phantom := last + 4
	if phantom >= int64(sound.Tables[0].Len()) {
		t.Skip("this geometry's FAT has no slack past the last cluster")
	}
	// Mark the phantom entry allocated and ending a chain, in both copies, so
	// nothing points at it and it reads as the head of a one-cluster chain.
	for _, off := range sound.FATTableOffsets {
		binary.LittleEndian.PutUint16(im.Data[off+phantom*2:], 0xFFFF)
	}

	fs, err := fat.Open(bytes.NewReader(im.Data), 0)
	if err != nil {
		t.Fatal(err)
	}
	orphans, err := fs.FindOrphans()
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range orphans {
		for _, c := range o.Clusters {
			if int64(c) > last {
				t.Errorf("orphan chain from %d holds cluster %d, past the last of %d — the FAT's own slack",
					o.StartCluster, c, last)
			}
		}
	}
}
