package hfs

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"samuelks.com/datarescue/diskimage"
	"samuelks.com/datarescue/internal/testfixture"
	"samuelks.com/datarescue/mbr"
)

// The on-disk cdrThdRec is 46 bytes: type and reserved (2), two reserved longs
// (8), the parent ID (4) and a Str31 (32). binary.Read decodes positionally and
// reports nothing wrong, so a struct four bytes short reads ParentID from the
// reserved tail and NameLength from the high byte of the real parent ID, with
// no error, on every thread record of every volume.
func TestCatalogRecordSizes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		rec  any
		want int
	}{
		{"CatalogKey", CatalogKey{}, 38},
		{"CatalogFolder", CatalogFolder{}, 70},
		{"CatalogFile", CatalogFile{}, 102},
		{"CatalogThread", CatalogThread{}, 46},
	} {
		if got := binary.Size(tc.rec); got != tc.want {
			t.Errorf("%s is %d bytes on the wire; the on-disk record is %d", tc.name, got, tc.want)
		}
	}
}

// catalogMaps walks the fixture and returns the parent map as the folder
// records give it and as the thread records give it, separately.
func catalogMaps(t *testing.T, v *Volume) (folders, threads map[uint32]catalogParent) {
	t.Helper()
	folders = make(map[uint32]catalogParent)
	threads = make(map[uint32]catalogParent)
	issues, err := v.WalkCatalog(func(recs CatalogRecords) error {
		for _, d := range recs.Dirs {
			folders[d.CNID] = catalogParent{id: d.ParentID, name: d.Name}
		}
		for _, th := range recs.Threads {
			threads[th.CNID] = catalogParent{id: th.ParentID, name: th.Name}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, ci := range issues {
		t.Errorf("catalog issue on the sound fixture: %s", ci)
	}
	return folders, threads
}

// A folder whose own record is damaged is still placed through its thread
// record, which is why there are two sources. It only works if threads decode
// correctly: with a struct four bytes short, every one comes out as {0, ""}
// and fills the parent map with nothing.
func TestThreadRecordsPlaceAFolderWithoutItsRecord(t *testing.T) {
	t.Parallel()
	v, err := openFixture(t)
	if err != nil {
		t.Fatal(err)
	}
	folders, threads := catalogMaps(t, v)
	if len(threads) == 0 {
		t.Skip("the fixture carries no thread records")
	}

	// Every thread must agree with the folder record for the same node.
	var checked int
	for cnid, th := range threads {
		f, ok := folders[cnid]
		if !ok {
			continue
		}
		checked++
		if th != f {
			t.Errorf("node %d: the thread says %+v and the folder record says %+v", cnid, th, f)
		}
	}
	if checked == 0 {
		t.Fatal("no node has both a folder record and a thread; the comparison proves nothing")
	}

	// And the threads alone build the same path, which is what happens when
	// the folder record is the damaged one.
	for cnid, f := range folders {
		if cnid == RootFolderID {
			continue
		}
		want, err := buildPath(folders, cnid, "file")
		if err != nil {
			continue
		}
		got, err := buildPath(threads, cnid, "file")
		if err != nil {
			t.Errorf("path for a file under node %d from threads alone: %v", cnid, err)
			continue
		}
		if !slices.Equal(got, want) {
			t.Errorf("node %d (%q): threads give %v, folder records give %v", cnid, f.name, got, want)
		}
	}
}

// patchMDB rewrites the fixture's master directory block through fn. Both
// copies are rewritten, since Open falls back to the spare and a change made to
// only one would be undone by that fallback rather than tested.
func patchMDB(t *testing.T, fn func(*MDB)) []byte {
	t.Helper()
	data := testfixture.Bytes(t, testImagePath)
	patchMDBAt(t, data, mdbOffset, fn)
	patchMDBAt(t, data, altMDBOffset(int64(len(data))), fn)
	return data
}

// patchMDBOnly rewrites one copy, for the tests about the fallback itself.
func patchMDBOnly(t *testing.T, off int64, fn func(*MDB)) []byte {
	t.Helper()
	data := testfixture.Bytes(t, testImagePath)
	patchMDBAt(t, data, off, fn)
	return data
}

func patchMDBAt(t *testing.T, data []byte, off int64, fn func(*MDB)) {
	t.Helper()
	var mdb MDB
	if err := binary.Read(bytes.NewReader(data[off:]), binary.BigEndian, &mdb); err != nil {
		t.Fatal(err)
	}
	fn(&mdb)
	var buf bytes.Buffer
	if err := binary.Write(&buf, binary.BigEndian, &mdb); err != nil {
		t.Fatal(err)
	}
	copy(data[off:], buf.Bytes())
}

// The spare master directory block sits two logical blocks from the end of the
// volume, which is why Open needs the volume's length: deriving the position
// from the volume's own geometry lands short on a real disk.
//
// This is the HFS analogue of ext2's backup superblock, and a volume whose
// primary copy was lost is exactly what it exists for.
func TestOpenFallsBackToTheSpareMDB(t *testing.T) {
	t.Parallel()
	data := patchMDBOnly(t, mdbOffset, func(m *MDB) { m.Signature = [2]byte{0, 0} })
	v, err := openBytes(data)
	if err != nil {
		t.Fatalf("Open did not fall back to the spare copy: %v", err)
	}
	if v.MDB.Label() != "disk" {
		t.Errorf("volume label = %q; want the one the spare copy carries", v.MDB.Label())
	}
	var reported bool
	for _, d := range v.Diagnostics() {
		if d.Kind == DiagAlternateMDB {
			reported = true
		}
	}
	if !reported {
		t.Error("falling back to the spare copy was not reported")
	}
	// The volume is fully usable from the spare copy.
	files, issues, err := v.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, ci := range issues {
		t.Errorf("catalog issue after falling back: %s", ci)
	}
	if len(files) == 0 {
		t.Error("the volume listed nothing after falling back")
	}
}

// Without a size there is nothing to measure the spare copy back from, so there
// is nothing to fall back to.
func TestOpenWithoutASizeCannotReachTheSpare(t *testing.T) {
	t.Parallel()
	data := patchMDBOnly(t, mdbOffset, func(m *MDB) { m.Signature = [2]byte{0, 0} })
	if _, err := Open(bytes.NewReader(data), 0, 0); err == nil {
		t.Error("Open succeeded with no size and a broken primary copy")
	}
}

// Every byte offset in the volume is computed from the MDB's geometry, so a
// zero or absurd value has to be refused at Open rather than surfacing later as
// a read failure at a nonsensical offset — or, for the block size, as a
// division by zero.
func TestOpenValidatesTheMDB(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		fn   func(*MDB)
	}{
		{"zero allocation block size", func(m *MDB) { m.AllocationBlockSize = 0 }},
		{"block size not a multiple of 512", func(m *MDB) { m.AllocationBlockSize = 513 }},
		{"absurd block size", func(m *MDB) { m.AllocationBlockSize = 1 << 30 }},
		{"no allocation blocks", func(m *MDB) { m.AllocationBlocks = 0 }},
		{"blocks start past their own end", func(m *MDB) { m.AllocationStartBlock = 0xFFFF }},
		{"not an HFS volume", func(m *MDB) { m.Signature = [2]byte{'X', 'Y'} }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := openBytes(patchMDB(t, tc.fn)); err == nil {
				t.Error("Open accepted it")
			}
		})
	}

	// The untouched fixture still opens.
	if _, err := openBytes(patchMDB(t, func(*MDB) {})); err != nil {
		t.Errorf("re-encoding the MDB unchanged broke Open: %v", err)
	}
}

// The catalog's extent list is resolved once. Every node read goes through
// catalogFork, and without caching each one would re-walk the extents overflow
// map to rebuild the same list.
func TestCatalogForkIsCached(t *testing.T) {
	t.Parallel()
	v, err := openFixture(t)
	if err != nil {
		t.Fatal(err)
	}
	if a, b := v.catalogFork(), v.catalogFork(); a != b {
		t.Error("catalogFork rebuilt the fork; it is resolved once and cached")
	}
}

// The recovery sweep runs when the leaf chain breaks, and it allocates a node
// buffer and a visited entry per pass. The 32-bit node count off the disc is
// not a safe bound, so the catalog's own size bounds it: the tree cannot hold
// more nodes than the file has room for.
func TestRecoverySweepBoundedByTheCatalogSize(t *testing.T) {
	t.Parallel()
	data := testfixture.Bytes(t, testImagePath)
	var mdb MDB
	if err := binary.Read(bytes.NewReader(data[2*logicalBlockSize:]), binary.BigEndian, &mdb); err != nil {
		t.Fatal(err)
	}
	// The catalog's first byte, and node 0's B-tree header record inside it.
	catStart := int64(mdb.AllocationStartBlock)*logicalBlockSize +
		int64(mdb.CatalogFileExtentsRecord[0].StartBlock)*int64(mdb.AllocationBlockSize)
	head := data[catStart+14:]
	// Break the leaf chain so the sweep runs, and declare four billion nodes.
	binary.BigEndian.PutUint32(head[10:14], 0xFFFF) // FirstLeafNode
	binary.BigEndian.PutUint32(head[22:26], 0xFFFFFFFF)

	v, err := Open(bytes.NewReader(data), 0, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}

	type result struct {
		files  int
		issues []CatalogIssue
	}
	done := make(chan result, 1)
	go func() {
		var files int
		issues, err := v.WalkCatalog(func(recs CatalogRecords) error {
			files += len(recs.Files)
			return nil
		})
		if err != nil {
			t.Error(err)
		}
		done <- result{files, issues}
	}()
	select {
	case got := <-done:
		if got.files == 0 {
			t.Error("the sweep recovered no files past the broken chain")
		}
		if len(got.issues) == 0 {
			t.Error("the broken leaf chain was not reported")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("WalkCatalog did not finish; the sweep is bounded by the declared node count, not the catalog's size")
	}
}

// Fork reads are addressed by allocation-block number, and the declared block
// count is the only bound on those numbers: a volume claiming more blocks than
// it holds would read a fork's bytes past its own end, from whatever follows
// it. Open clamps the count to what the volume holds and reports it, rather
// than failing the whole volume.
func TestAllocationAreaIsClampedToTheVolume(t *testing.T) {
	t.Parallel()
	data := patchMDB(t, func(m *MDB) { m.AllocationBlocks = 17000 })
	v, err := openBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	want := (v.size - int64(v.MDB.AllocationStartBlock)*logicalBlockSize) / int64(v.MDB.AllocationBlockSize)
	if int64(v.MDB.AllocationBlocks) != want {
		t.Errorf("AllocationBlocks = %d; want %d, the declared 17000 clamped to the volume's %d bytes",
			v.MDB.AllocationBlocks, want, v.size)
	}
	var reported bool
	for _, d := range v.Diagnostics() {
		if d.Kind == DiagAllocationAreaTruncated {
			reported = true
		}
	}
	if !reported {
		t.Error("clamping the allocation area was not reported")
	}
	// The volume is still usable: the files inside the clamped area are read.
	files, issues, err := v.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, ci := range issues {
		t.Errorf("catalog issue after clamping: %s", ci)
	}
	if len(files) == 0 {
		t.Error("the volume listed nothing after clamping")
	}
	e := find(t, v, "TwinPeaksTrivia")
	if got := readAll(t, v, e); len(got) != int(e.DataSize) {
		t.Errorf("read %d bytes of TwinPeaksTrivia; want %d", len(got), e.DataSize)
	}
}

// An allocation area that starts at or past the volume's own end cannot be
// clamped down to anything, so such a volume is refused.
func TestAllocationAreaPastTheVolumeIsRefused(t *testing.T) {
	t.Parallel()
	data := patchMDB(t, func(m *MDB) {
		m.AllocationStartBlock = 16384 // starts at the volume's own byte 8388608
		m.AllocationBlocks = 16385     // still passes validateMDB, since it outnumbers the start block
	})
	if _, err := openBytes(data); err == nil {
		t.Error("Open accepted an allocation area starting at the volume's end")
	}
}

// The catalog fork must not be taken at its recorded length when the extents
// overflow file did not parse in full: a continuation record that exists on
// disk but was never loaded leaves the fork short for an unknown reason, and
// treating the gap as a measurable hole is the guess the fill rule forbids.
func TestCatalogForkNotCompleteWhenOverflowIsTruncated(t *testing.T) {
	t.Parallel()
	data := patchMDB(t, func(m *MDB) { m.CatalogSize = 1 << 20 })
	var mdb MDB
	if err := binary.Read(bytes.NewReader(data[mdbOffset:]), binary.BigEndian, &mdb); err != nil {
		t.Fatal(err)
	}
	// Node 0 of the extents overflow B-tree: allocation block 0. Its B-tree
	// header carries the node size 18 bytes into the header record, which
	// follows the 14-byte node descriptor. A node size of 0 makes the whole
	// file unparseable.
	overflowStart := int64(mdb.AllocationStartBlock)*logicalBlockSize +
		int64(mdb.ExtentsRecord[0].StartBlock)*int64(mdb.AllocationBlockSize)
	binary.BigEndian.PutUint16(data[overflowStart+14+18:overflowStart+14+18+2], 0)

	v, err := openBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	if got := v.catalogFork().tail; got != forkListUnknown {
		t.Errorf("catalog fork tail = %v; want forkListUnknown when the overflow file is truncated", got)
	}
	var reported bool
	for _, d := range v.Diagnostics() {
		if d.Kind == DiagOverflowTruncated {
			reported = true
		}
	}
	if !reported {
		t.Error("the catalog fork's truncated overflow was not reported")
	}
}

// A volume inside an MBR partition is bounded by the partition, not by the
// image it was dumped into. The spare master directory block sits two logical
// blocks from the end of the *volume*, and nothing inside it says where that
// is. Given the whole image's length, a volume in an image with trailing bytes
// past the partition would look for the spare past its real end, losing the
// fallback that the volume below, whose primary is gone, depends on.
func TestOpenInAnMBRPartitionFindsTheSpareMDB(t *testing.T) {
	t.Parallel()
	vol := testfixture.Bytes(t, testImagePath)

	const partLBA = 2
	partStart := int64(partLBA) * mbr.SectorSize
	partSize := int64(len(vol))
	imageSize := partStart + partSize + 4096 // trailing bytes past the partition
	disk := make([]byte, imageSize)
	disk[446+4] = 0xAF // Apple HFS
	binary.LittleEndian.PutUint32(disk[446+8:], partLBA)
	binary.LittleEndian.PutUint32(disk[446+12:], uint32(partSize/mbr.SectorSize))
	disk[510], disk[511] = 0x55, 0xAA
	copy(disk[partStart:], vol)
	// Lose the primary master directory block; only the spare copy can still
	// open the volume.
	clear(disk[partStart+mdbOffset : partStart+mdbOffset+8])

	path := filepath.Join(t.TempDir(), "mbr.img")
	if err := os.WriteFile(path, disk, 0o600); err != nil {
		t.Fatal(err)
	}

	im, err := diskimage.Open(path, diskimage.Options{Offset: -1, Partition: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer im.Close() //nolint:errcheck // test cleanup
	if want := partStart + partSize; im.Size != want {
		t.Fatalf("Size = %d; want %d, the partition's end and not the image's %d", im.Size, want, imageSize)
	}
	if im.Offset != partStart {
		t.Fatalf("Offset = %d; want %d", im.Offset, partStart)
	}

	ra, err := im.ReaderAt()
	if err != nil {
		t.Fatal(err)
	}
	v, err := Open(ra, 0, im.Size-im.Offset)
	if err != nil {
		t.Fatalf("Open of the primary-less partition: %v", err)
	}
	if v.MDB.Label() != "disk" {
		t.Errorf("volume label = %q; want the spare copy's", v.MDB.Label())
	}
	var reported bool
	for _, d := range v.Diagnostics() {
		if d.Kind == DiagAlternateMDB {
			reported = true
		}
	}
	if !reported {
		t.Error("opening from the spare copy was not reported")
	}
	files, issues, err := v.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, ci := range issues {
		t.Errorf("catalog issue: %s", ci)
	}
	var found bool
	for _, f := range files {
		if strings.Join(f.Path, "/") == "TwinPeaksTrivia" {
			found = true
		}
	}
	if !found {
		t.Error("the volume in the MBR partition listed no TwinPeaksTrivia")
	}
}
