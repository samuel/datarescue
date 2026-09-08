package hfs

import (
	"bytes"
	"cmp"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
)

const logicalBlockSize = 512

var Signature = [2]byte{0x42, 0x44}

// Volume is an open HFS volume. It is not safe for concurrent use: the extents
// overflow file and the catalog fork are both parsed on first need and cached
// on the Volume, so two goroutines reading files at once race on those fields.
// Use one goroutine per Volume, or guard it with a lock.
type Volume struct {
	// r is the image, and base the volume's byte offset within it. Every read
	// goes through readAt, which is the one place the two are added, so no
	// caller has to remember the offset and none can forget it.
	r    io.ReaderAt
	base int64
	// size is the volume's length in bytes — the partition's, or the image's
	// for a bare volume. It is 0 when the caller could not say, which gives up
	// the alternate master directory block: that block sits two logical blocks
	// from the end of the volume and nothing inside the volume says where that
	// is.
	size int64
	MDB  MDB

	// diags are the problems noticed so far, drained by Diagnostics.
	diags []Diagnostic
	// overflowPartial records that the extents overflow file did not parse in
	// full, so a fork that comes back short may be missing a record rather than
	// missing its data.
	overflowPartial bool

	// overflow caches the parsed extents overflow file; every fork read
	// consults it, and a volume without one is normal.
	overflow       map[extentKey]ExtentsRecord
	overflowErr    error
	overflowLoaded bool

	// catalog caches the catalog file's extent list, which every node read
	// needs and which resolving costs a walk of the overflow map.
	catalog *fork
}

// ErrNotHFS reports that the bytes at the offset are not an HFS volume, as
// opposed to an I/O failure reaching them. A caller probing an image has to
// tell those apart: on damaged media "this is not HFS" and "the master
// directory block was never recovered" are different answers, and reporting
// the second as the first sends an operator looking for the wrong problem.
var ErrNotHFS = errors.New("hfs: no HFS master directory block found")

// Open reads and validates the master directory block of the volume at byte
// offset off within r, returning a Volume ready for traversal.
//
// size is the volume's length in bytes — the partition's, or the image's for a
// bare volume. Pass 0 when it is not known, which gives up the alternate master
// directory block: that block sits two logical blocks from the end of the
// volume, and deriving its position from the volume's own geometry instead
// lands 29 blocks short on a real 100 MB Zip disk.
//
// Validation goes beyond the signature: every byte offset in the volume is
// computed from the MDB's geometry, so a zero or absurd allocation block size
// has to be refused here rather than surfacing later as an opaque read failure
// at a nonsensical offset.
func Open(r io.ReaderAt, off, size int64) (*Volume, error) {
	v := &Volume{r: r, base: off, size: size}

	mdb, primaryErr := v.readMDB(mdbOffset)
	if primaryErr == nil {
		if err := v.checkAlternate(mdb); err != nil {
			return nil, err
		}
		v.MDB = *mdb
		return v.finish()
	}

	// The spare copy is the HFS analogue of ext2's backup superblock. Without a
	// size there is nothing to measure back from, so there is nothing to try.
	if size <= 0 {
		return nil, primaryErr
	}
	alt, altErr := v.readMDB(altMDBOffset(size))
	if altErr != nil {
		// The volume genuinely does not open, and the primary failure is the
		// one worth reporting: it separates "this is not HFS" from "the master
		// directory block was never recovered".
		return nil, primaryErr
	}
	v.diag(DiagAlternateMDB, 0, "the master directory block would not read (%s); using the spare copy at byte %d",
		primaryErr, altMDBOffset(size))
	v.MDB = *alt
	return v.finish()
}

// mdbOffset is where the master directory block sits: logical block 2.
const mdbOffset = 2 * logicalBlockSize

// altMDBOffset is where the volume's spare master directory block sits: two
// logical blocks from the end of the volume.
func altMDBOffset(size int64) int64 { return size - 2*logicalBlockSize }

// readMDB reads and validates one copy of the master directory block.
func (v *Volume) readMDB(off int64) (*MDB, error) {
	buf := make([]byte, logicalBlockSize)
	if _, err := v.readAt(buf, off); err != nil {
		return nil, fmt.Errorf("hfs: read MDB at %d: %w", off, err)
	}
	var mdb MDB
	if err := binary.Read(bytes.NewReader(buf), binary.BigEndian, &mdb); err != nil {
		return nil, fmt.Errorf("hfs: read MDB at %d: %w", off, err)
	}
	if mdb.Signature != Signature {
		return nil, fmt.Errorf("%w: signature %q at byte %d, expected %q", ErrNotHFS, mdb.Signature, off, Signature)
	}
	if err := validateMDB(&mdb); err != nil {
		return nil, err
	}
	return &mdb, nil
}

// checkAlternate cross-checks the spare copy against the one in use. It costs
// one read and catches a volume resized under a stale copy.
func (v *Volume) checkAlternate(mdb *MDB) error {
	if v.size <= 0 {
		return nil
	}
	alt, err := v.readMDB(altMDBOffset(v.size))
	if err != nil {
		v.diag(DiagMDBMismatch, 0, "the spare master directory block could not be read (%s)", err)
		return nil
	}
	if alt.AllocationBlockSize != mdb.AllocationBlockSize ||
		alt.AllocationBlocks != mdb.AllocationBlocks ||
		alt.AllocationStartBlock != mdb.AllocationStartBlock {
		v.diag(DiagMDBMismatch, 0,
			"the two master directory blocks disagree about the geometry: %d blocks of %d starting at %d, against %d of %d starting at %d",
			mdb.AllocationBlocks, mdb.AllocationBlockSize, mdb.AllocationStartBlock,
			alt.AllocationBlocks, alt.AllocationBlockSize, alt.AllocationStartBlock)
	}
	return nil
}

// finish completes an opened volume, refusing the one shape that would
// otherwise open as an almost-empty disk.
func (v *Volume) finish() (*Volume, error) {
	if err := v.boundAllocationArea(); err != nil {
		return nil, err
	}
	if v.MDB.EmbeddedVolumeSignature == EmbeddedSignature {
		// The wrapper's own catalog holds one or two placeholder items, so
		// opening it succeeds and lists almost nothing. "This disk is empty" is
		// the most misleading answer a recovery tool can give, so this fails
		// and reports where the real volume is.
		off, size := v.embedded()
		return nil, &EmbeddedHFSPlusError{Offset: off, Size: size}
	}
	return v, nil
}

// boundAllocationArea clamps the declared allocation-block count to what the
// volume or its partition actually holds.
//
// Fork reads are addressed by allocation-block number, and the declared count
// is the only bound on those numbers: a volume declaring more blocks than it
// holds would read a fork's bytes past its own end, from whatever follows it.
// The count also bounds the extents overflow walk, so a count that overshoots
// lets a fully parsed overflow list claim blocks that cannot exist. Clamping
// keeps the volume readable and reports what was lost rather than failing the
// whole open.
func (v *Volume) boundAllocationArea() error {
	if v.size <= 0 {
		return nil
	}
	start := int64(v.MDB.AllocationStartBlock) * logicalBlockSize
	end := start + int64(v.MDB.AllocationBlocks)*int64(v.MDB.AllocationBlockSize)
	if end <= v.size {
		return nil
	}
	declared := v.MDB.AllocationBlocks
	fit := (v.size - start) / int64(v.MDB.AllocationBlockSize)
	if fit <= 0 {
		return fmt.Errorf("hfs: the allocation area starts at byte %d, past the %d bytes the volume holds", start, v.size)
	}
	//nolint:gosec // fit is below the declared count, which is itself a uint16
	v.MDB.AllocationBlocks = uint16(fit)
	v.diag(DiagAllocationAreaTruncated, 0,
		"the volume declares %d allocation blocks ending %d bytes past its %d-byte length; reading only the %d that fit",
		declared, end-v.size, v.size, fit)
	return nil
}

// maxAllocationBlockSize is the largest allocation block HFS can address: the
// block count is a uint16 and the format caps a volume at 2 GiB, which puts the
// block size at 32 KiB even on the largest one.
const maxAllocationBlockSize = 64 << 10

// validateMDB checks the fields the volume's own arithmetic rests on.
func validateMDB(m *MDB) error {
	switch {
	case m.AllocationBlockSize == 0 || m.AllocationBlockSize%logicalBlockSize != 0:
		return fmt.Errorf("hfs: allocation block size %d is not a multiple of %d",
			m.AllocationBlockSize, logicalBlockSize)
	case m.AllocationBlockSize > maxAllocationBlockSize:
		return fmt.Errorf("hfs: allocation block size %d is larger than the %d the format allows",
			m.AllocationBlockSize, maxAllocationBlockSize)
	case m.AllocationBlocks == 0:
		return errors.New("hfs: the volume declares no allocation blocks")
	case int64(m.AllocationStartBlock)*logicalBlockSize >= int64(m.AllocationBlocks)*int64(m.AllocationBlockSize):
		return fmt.Errorf("hfs: the allocation blocks start at byte %d, past the end of the %d bytes they cover",
			int64(m.AllocationStartBlock)*logicalBlockSize, int64(m.AllocationBlocks)*int64(m.AllocationBlockSize))
	}
	return nil
}

// Label returns the volume name, decoded from Mac OS Roman like every other
// name on the volume. The on-disk length byte is untrusted and is clamped to
// the size of the field.
func (m *MDB) Label() string {
	return decodeMacRoman(m.VolumeLabel[:min(int(m.VolumeLabelSize), len(m.VolumeLabel))])
}

// readAt reads at an offset within the volume, adding its base. It is for the
// structures whose positions the format states relative to the volume — the
// master directory block and its spare.
func (v *Volume) readAt(p []byte, off int64) (int, error) {
	return v.r.ReadAt(p, v.base+off)
}

// readAtN reads at an offset within the image and reports how much came back
// before a failure. A gap-filling retry needs that count to know what to keep
// and where to retry from. It has ddrescue.ReadFunc's shape.
//
// The offset is absolute and the base is not added, because every caller comes
// from AllocBlockOffset, which has already added it. Adding it twice reads a
// partitioned volume at twice its own offset, which is invisible on a bare
// volume at 0 and wrong on every disk that has a partition map.
func (v *Volume) readAtN(off int64, buf []byte) (int, error) {
	return v.r.ReadAt(buf, off)
}

// AllocBlockOffset is the byte offset of an allocation block within the image
// the volume was opened over — the volume's own base is already in it, so the
// result can be handed straight to a rescue map.
//
// The block number is a uint16 and the block size is capped at 64 KiB, so the
// product cannot come near overflowing int64 and needs no guard.
func (v *Volume) AllocBlockOffset(n uint16) int64 {
	return v.base + v.allocBlockByteOffset(uint32(n))
}

func (v *Volume) allocBlockByteOffset(n uint32) int64 {
	return int64(v.MDB.AllocationStartBlock)*logicalBlockSize + int64(n)*int64(v.MDB.AllocationBlockSize)
}

// catalogFork returns the catalog file's extents, including any held in the
// extents overflow file. A large volume's catalog outgrows the three extents in
// the MDB, and reading only those silently loses every file past them.
//
// It is resolved once and cached, since every node read goes through here and
// resolving it walks the overflow map.
func (v *Volume) catalogFork() *fork {
	if v.catalog != nil {
		return v.catalog
	}
	ref := ForkRef{
		CNID: CatalogFileID, Type: ForkTypeData,
		Extents: v.MDB.CatalogFileExtentsRecord, Size: int64(v.MDB.CatalogSize),
	}
	// openFork classifies what the extents do not cover, so a catalog that
	// depends on the extents overflow file and comes back short is marked
	// incomplete rather than taken at its recorded length: a B-tree the walk
	// believes complete may still be missing every node past a part of its
	// chain the overflow file could not supply.
	f, err := v.openFork(ref)
	if err != nil {
		f = &fork{v: v, ref: ref, extents: v.MDB.CatalogFileExtentsRecord[:], size: ref.Size}
	}
	v.catalog = f
	return v.catalog
}

// readCatalogNode reads one B-tree node from the catalog file by node number.
func (v *Volume) readCatalogNode(nodeNum uint32, nodeSize uint16) ([]byte, error) {
	buf := make([]byte, nodeSize)
	n, err := v.catalogFork().readAt(buf, int64(nodeNum)*int64(nodeSize))
	if err != nil {
		return nil, err
	}
	if n < int(nodeSize) {
		return nil, io.ErrUnexpectedEOF
	}
	return buf, nil
}

// RawFile is one file entry returned from the catalog walk.
type RawFile struct {
	ParentID    uint32
	Name        string
	CNID        uint32
	DataExtents ExtentsRecord
	DataSize    uint32 // logical EOF of data fork
	// RsrcExtents and RsrcSize are the resource fork, which on a classic Mac
	// volume is where much of the content actually is.
	RsrcExtents               ExtentsRecord
	RsrcSize                  uint32
	Created, Modified, Backup MacTime
	// FInfo and FXInfo are the record's Finder information exactly as stored.
	FInfo, FXInfo [16]byte
}

// RawDir is one directory entry.
type RawDir struct {
	ParentID uint32
	Name     string
	CNID     uint32
	// Valence is how many files and folders the directory holds.
	Valence                   uint16
	Created, Modified, Backup MacTime
	FInfo, FXInfo             [16]byte
}

// RawThread is one thread record: the catalog's own map from a node ID back to
// its parent and name. Threads are stored separately from folder records, so a
// folder whose record is damaged can still be placed by its thread.
type RawThread struct {
	// CNID is the node the thread describes, taken from the record's key.
	CNID     uint32
	ParentID uint32
	Name     string
}

// CatalogRecords holds the records decoded from one catalog leaf node.
type CatalogRecords struct {
	Dirs    []RawDir
	Files   []RawFile
	Threads []RawThread
}

func (c *CatalogRecords) empty() bool {
	return len(c.Dirs) == 0 && len(c.Files) == 0 && len(c.Threads) == 0
}

// catalogNodeCount is how many nodes the catalog file can hold, which is what
// bounds a sweep of it. declared is the header's own count and wins when it is
// smaller: a tree that says it uses fewer nodes than the file has room for is
// the ordinary case.
func (v *Volume) catalogNodeCount(nodeSize uint16, declared uint32) uint32 {
	if nodeSize == 0 {
		return 0
	}
	room := int64(v.MDB.CatalogSize) / int64(nodeSize)
	if room < int64(declared) {
		//nolint:gosec // room is below declared, itself a uint32
		return uint32(room)
	}
	return declared
}

// maxNodeSize is the largest B-tree node HFS defines.
const maxNodeSize = 32768

// validNodeSize reports whether n is a legal B-tree node size: a power of two
// of at least 512 bytes.
func validNodeSize(n uint16) bool {
	return n >= 512 && n <= maxNodeSize && n&(n-1) == 0
}

// CatalogIssue records a catalog node or record that could not be decoded. The
// records recovered from elsewhere in the catalog are still delivered.
type CatalogIssue struct {
	// Node is the catalog node it concerns, 0 when it is not about one.
	Node uint32
	// CNID and Path identify the entry when the issue concerns one: a parent
	// chain that does not reach the root is reported against the file it
	// could not place, not against "node 0", which is unrelated to it.
	CNID uint32
	Path string
	Err  error
}

func (c CatalogIssue) String() string {
	switch {
	case c.Path != "":
		return fmt.Sprintf("%s: %s", c.Path, c.Err)
	case c.CNID != 0:
		return fmt.Sprintf("file %d: %s", c.CNID, c.Err)
	case c.Node != 0:
		return fmt.Sprintf("node %d: %s", c.Node, c.Err)
	}
	return c.Err.Error()
}

// WalkCatalog calls fn once per leaf node with that node's file and directory
// records. Thread records are consumed to resolve parent IDs and are not passed
// to fn.
//
// Leaf nodes are normally reached by following the chain from FirstLeafNode,
// but that chain is a linked list: one unreadable node would truncate every
// node after it. So a malformed record is skipped rather than aborting its
// node, and if anything went wrong the remaining nodes up to TotalNodeCount are
// scanned directly for leaves.
func (v *Volume) WalkCatalog(fn func(CatalogRecords) error) ([]CatalogIssue, error) {
	// Read node 0 at the minimum node size to learn the real one.
	nodeSize := uint16(512)
	node0, err := v.readCatalogNode(0, nodeSize)
	if err != nil {
		return nil, err
	}

	var btHead BTreeHeader
	// The header record follows the 12-byte descriptor and 2 reserved bytes.
	if err := binary.Read(bytes.NewReader(node0[14:]), binary.BigEndian, &btHead); err != nil {
		return nil, fmt.Errorf("hfs: read catalog header: %w", err)
	}

	// NodeSize is a disk field. Zero makes readCatalogNode return an empty
	// buffer and node0[14:] panic; a non-power-of-two or oversized value would
	// misplace every record offset in the tree.
	if !validNodeSize(btHead.NodeSize) {
		return nil, fmt.Errorf("hfs: catalog header declares invalid node size %d", btHead.NodeSize)
	}
	if btHead.NodeSize != nodeSize {
		nodeSize = btHead.NodeSize
		node0, err = v.readCatalogNode(0, nodeSize)
		if err != nil {
			return nil, err
		}
		if err := binary.Read(bytes.NewReader(node0[14:]), binary.BigEndian, &btHead); err != nil {
			return nil, fmt.Errorf("hfs: read catalog header: %w", err)
		}
	}

	w := &catalogWalker{v: v, fn: fn, nodeSize: nodeSize, visited: make(map[uint32]struct{})}

	// Follow the leaf chain.
	chainOK := true
	for currentNode := btHead.FirstLeafNode; currentNode != 0; {
		if _, seen := w.visited[currentNode]; seen {
			w.issue(currentNode, errors.New("hfs: leaf chain loops"))
			chainOK = false
			break
		}
		next, err := w.node(currentNode)
		if err != nil {
			w.issue(currentNode, err)
			chainOK = false
			break
		}
		if w.stopErr != nil {
			return w.issues, w.stopErr
		}
		currentNode = next
	}

	// Something broke the chain: sweep the rest of the tree for leaf nodes so
	// the records past the break are still recovered.
	//
	// TotalNodeCount is a 32-bit field off the disc, and each pass of the sweep
	// allocates a node buffer and a visited entry, so the catalog's own size is
	// the bound: the tree cannot hold more nodes than the file has room for.
	if !chainOK {
		for n := uint32(1); n < v.catalogNodeCount(nodeSize, btHead.TotalNodeCount); n++ {
			if _, seen := w.visited[n]; seen {
				continue
			}
			if _, err := w.node(n); err != nil {
				continue // not a readable leaf; nothing to recover here
			}
			if w.stopErr != nil {
				return w.issues, w.stopErr
			}
		}
	}
	return w.issues, nil
}

type catalogWalker struct {
	v        *Volume
	fn       func(CatalogRecords) error
	nodeSize uint16
	visited  map[uint32]struct{}
	issues   []CatalogIssue
	stopErr  error
}

func (w *catalogWalker) issue(node uint32, err error) {
	w.issues = append(w.issues, CatalogIssue{Node: node, Err: err})
}

// node decodes one leaf node and delivers its records, returning the next node
// in the chain. A node that is unreadable or is not a leaf returns an error
// without recording an issue, so the recovery sweep can skip it quietly.
func (w *catalogWalker) node(nodeNum uint32) (uint32, error) {
	w.visited[nodeNum] = struct{}{}
	node, err := w.v.readCatalogNode(nodeNum, w.nodeSize)
	if err != nil {
		return 0, err
	}

	var desc BTreeDescriptor
	if err := binary.Read(bytes.NewReader(node[:14]), binary.BigEndian, &desc); err != nil {
		return 0, fmt.Errorf("hfs: read node %d descriptor: %w", nodeNum, err)
	}
	if desc.NodeType != BTreeNodeTypeLeaf {
		return 0, fmt.Errorf("hfs: expected leaf node at %d, got %s", nodeNum, desc.NodeType)
	}

	recs := w.records(nodeNum, node, desc)
	if !recs.empty() {
		if err := w.fn(recs); err != nil {
			w.stopErr = err
		}
	}
	return desc.NextNode, nil
}

func (w *catalogWalker) records(currentNode uint32, node []byte, desc BTreeDescriptor) CatalogRecords {
	nodeSize := w.nodeSize
	var recs CatalogRecords

	// Record offsets are stored at the end of the node, counting backwards
	// from nodeSize - 2. A malformed record costs only itself: the rest of the
	// node is still decoded.
	for i := range int(desc.RecordCount) {
		offPos := int(nodeSize) - 2*(i+1)
		if offPos < 0 {
			w.issue(currentNode, fmt.Errorf("hfs: record count %d does not fit in node size %d", desc.RecordCount, nodeSize))
			break
		}
		recordOffset := int(binary.BigEndian.Uint16(node[offPos : offPos+2]))
		if recordOffset >= int(nodeSize) {
			w.issue(currentNode, fmt.Errorf("hfs: record %d offset %d exceeds node size %d", i, recordOffset, nodeSize))
			continue
		}

		// HFS catalog keys are variable length and the length byte excludes itself.
		// Widths are compared as int so the sum cannot wrap.
		keyLen := int(node[recordOffset])
		if recordOffset+keyLen+1 > int(nodeSize) {
			w.issue(currentNode, fmt.Errorf("hfs: key at offset %d exceeds node size", recordOffset))
			continue
		}
		key := node[recordOffset : recordOffset+keyLen+1]

		// Pad short keys out to the full 38-byte CatalogKey.
		var catKey CatalogKey
		var keyBuf [38]byte
		copy(keyBuf[:], key)
		if err := binary.Read(bytes.NewReader(keyBuf[:]), binary.BigEndian, &catKey); err != nil {
			w.issue(currentNode, fmt.Errorf("hfs: read catalog key in record %d: %w", i, err))
			continue
		}

		// The data record follows the key at the next even offset.
		dataOffset := recordOffset + keyLen + 1
		if dataOffset%2 != 0 {
			dataOffset++
		}
		if dataOffset >= int(nodeSize) {
			w.issue(currentNode, fmt.Errorf("hfs: record %d data offset %d exceeds node size %d", i, dataOffset, nodeSize))
			continue
		}

		recordType := int16(node[dataOffset])

		// NameLength is a disk byte and can exceed the fixed-size Name field.
		// HFS names are Mac OS Roman, not UTF-8.
		name := decodeMacRoman(catKey.Name[:min(int(catKey.NameLength), len(catKey.Name))])

		switch recordType {
		case CatalogRecordFolder:
			var folder CatalogFolder
			if err := binary.Read(bytes.NewReader(node[dataOffset:]), binary.BigEndian, &folder); err != nil {
				w.issue(currentNode, fmt.Errorf("hfs: read folder record %d: %w", i, err))
				continue
			}
			recs.Dirs = append(recs.Dirs, RawDir{
				ParentID: catKey.ParentID,
				Name:     name,
				CNID:     folder.CNID,
				Valence:  folder.Valence,
				Created:  MacTime(folder.CreateDate),
				Modified: MacTime(folder.ModifyDate),
				Backup:   MacTime(folder.BackupDate),
				FInfo:    folder.FinderInfo,
				FXInfo:   folder.FinderInfoExt,
			})
		case CatalogRecordFile:
			var file CatalogFile
			if err := binary.Read(bytes.NewReader(node[dataOffset:]), binary.BigEndian, &file); err != nil {
				w.issue(currentNode, fmt.Errorf("hfs: read file record %d: %w", i, err))
				continue
			}
			// FInfo is 16 bytes and the record splits it: the type code is
			// its own field and the other twelve follow.
			var fInfo [16]byte
			binary.BigEndian.PutUint32(fInfo[0:4], file.FileType)
			copy(fInfo[4:16], file.FinderInfo[:])
			recs.Files = append(recs.Files, RawFile{
				ParentID:    catKey.ParentID,
				Name:        name,
				CNID:        file.CNID,
				DataExtents: file.DataExtents,
				DataSize:    file.DataForkLogicalSize,
				RsrcExtents: file.RsrcExtents,
				RsrcSize:    file.RsrcForkLogicalSize,
				Created:     MacTime(file.CreateDate),
				Modified:    MacTime(file.ModifyDate),
				Backup:      MacTime(file.BackupDate),
				FInfo:       fInfo,
				FXInfo:      file.FinderInfoExt,
			})
		case CatalogRecordFolderThread, CatalogRecordFileThread:
			// A thread record's key holds the node ID it describes in the
			// ParentID field, and its body holds the real parent and name.
			var thread CatalogThread
			if err := binary.Read(bytes.NewReader(node[dataOffset:]), binary.BigEndian, &thread); err != nil {
				w.issue(currentNode, fmt.Errorf("hfs: read thread record %d: %w", i, err))
				continue
			}
			recs.Threads = append(recs.Threads, RawThread{
				CNID:     catKey.ParentID,
				ParentID: thread.ParentID,
				Name:     decodeMacRoman(thread.Name[:min(int(thread.NameLength), len(thread.Name))]),
			})
		}
	}
	return recs
}

// catalogParent is a node's parent and name, from whichever record supplied it.
type catalogParent struct {
	id   uint32
	name string
}

// maxPathDepth bounds the walk up the parent chain. A corrupt catalog can point
// a folder at itself or at one of its own descendants.
const maxPathDepth = 64

// Walk calls fn once for every file and folder in the volume, with its path
// reconstructed from the parent chain. Entries recovered from the intact parts
// of the catalog are delivered even when some nodes or records were unreadable,
// and what could not be read comes back as issues.
//
// Directories are delivered before their contents, so an extract can create
// them in order.
func (v *Volume) Walk(fn func(Entry) error) ([]CatalogIssue, error) {
	entries, issues, err := v.List()
	if err != nil {
		return issues, err
	}
	for _, e := range entries {
		if err := fn(e); err != nil {
			return issues, err
		}
	}
	return issues, nil
}

// List returns every file and folder in the volume with its path, along with
// any catalog damage encountered.
func (v *Volume) List() ([]Entry, []CatalogIssue, error) {
	// Folder records supply the parent map; threads fill in what they miss, so
	// a folder whose own record is damaged can still place its children.
	parents := make(map[uint32]catalogParent)
	var rawFiles []RawFile
	var rawDirs []RawDir

	issues, err := v.WalkCatalog(func(recs CatalogRecords) error {
		for _, d := range recs.Dirs {
			parents[d.CNID] = catalogParent{id: d.ParentID, name: d.Name}
		}
		for _, t := range recs.Threads {
			if _, ok := parents[t.CNID]; !ok {
				parents[t.CNID] = catalogParent{id: t.ParentID, name: t.Name}
			}
		}
		rawFiles = append(rawFiles, recs.Files...)
		rawDirs = append(rawDirs, recs.Dirs...)
		return nil
	})
	if err != nil {
		return nil, issues, err
	}

	entries := make([]Entry, 0, len(rawDirs)+len(rawFiles))
	for _, d := range rawDirs {
		if d.CNID == RootFolderID {
			// The root folder's own record carries the volume name, and it is
			// the volume rather than something in it. Listing it puts every
			// file one level down under a folder named after the disk.
			continue
		}
		e := Entry{
			Name: d.Name, CNID: d.CNID, ParentID: d.ParentID, IsDir: true,
			Valence: d.Valence, Created: d.Created, Modified: d.Modified, Backup: d.Backup,
		}
		e.setFinder(d.FInfo, d.FXInfo)
		e.Path, issues = v.pathFor(parents, d.ParentID, d.Name, d.CNID, issues)
		entries = append(entries, e)
	}
	for _, f := range rawFiles {
		e := Entry{
			Name: f.Name, CNID: f.CNID, ParentID: f.ParentID,
			Created: f.Created, Modified: f.Modified, Backup: f.Backup,
			DataExtents: f.DataExtents, DataSize: int64(f.DataSize),
			RsrcExtents: f.RsrcExtents, RsrcSize: int64(f.RsrcSize),
		}
		e.setFinder(f.FInfo, f.FXInfo)
		e.Path, issues = v.pathFor(parents, f.ParentID, f.Name, f.CNID, issues)
		entries = append(entries, e)
	}

	// Shallower first, then by path, so a directory is delivered before what is
	// under it and two runs over one volume list it the same way.
	slices.SortFunc(entries, func(a, b Entry) int {
		if n := cmp.Compare(len(a.Path), len(b.Path)); n != 0 {
			return n
		}
		return cmp.Compare(strings.Join(a.Path, "\x00"), strings.Join(b.Path, "\x00"))
	})
	return entries, issues, nil
}

// pathFor builds one entry's path and records a broken parent chain against the
// entry it could not place, rather than against an unrelated catalog node.
func (v *Volume) pathFor(parents map[uint32]catalogParent, parentID uint32, name string,
	cnid uint32, issues []CatalogIssue,
) ([]string, []CatalogIssue) {
	path, err := buildPath(parents, parentID, name)
	if err != nil {
		issues = append(issues, CatalogIssue{
			CNID: cnid,
			Path: strings.Join(path, "/"),
			Err:  fmt.Errorf("hfs: path for %q: %w", name, err),
		})
	}
	return path, issues
}

// buildPath walks up the parent chain from parentID, prepending folder names.
//
// The path is returned as components rather than joined, because "/" is an
// ordinary character in an HFS filename — the classic Mac separator is ":" — so
// a file named "10/20/99 Budget" joined with slashes is indistinguishable from
// three nested folders, and an extract would build a tree out of one file.
//
// Paths are relative to the root, so the root folder's own name is not
// included. A break in the chain is reported rather than silently producing a
// wrong path; the path built so far is returned either way, so the entry is
// still listed.
func buildPath(parents map[uint32]catalogParent, parentID uint32, name string) ([]string, error) {
	path := []string{name}
	id := parentID
	for depth := 0; ; depth++ {
		// The root folder's parent is RootParentID and has no record, so
		// reaching either means the walk is complete.
		if id == RootFolderID || id == RootParentID {
			return path, nil
		}
		if depth >= maxPathDepth {
			return append([]string{"?"}, path...), fmt.Errorf("parent chain exceeds %d levels", maxPathDepth)
		}
		p, ok := parents[id]
		if !ok {
			return append([]string{"?"}, path...),
				fmt.Errorf("no catalog or thread record for parent node %d", id)
		}
		path = append([]string{p.name}, path...)
		id = p.id
	}
}

// Volume attribute flags.
const (
	// KHFSVolumeHardwareLockBit is set when the volume is write-protected by hardware.
	KHFSVolumeHardwareLockBit = 0x00000080
	// KHFSVolumeUnmountedBit is set when the volume was flushed before unmounting;
	// if clear, the volume was not cleanly dismounted.
	KHFSVolumeUnmountedBit = 0x00000100
	// KHFSVolumeSparedBlocksBit is set when the extents overflow file holds bad-block records.
	KHFSVolumeSparedBlocksBit = 0x00000200
	// KHFSVolumeNoCacheRequiredBit is set when this volume's blocks should not be cached.
	KHFSVolumeNoCacheRequiredBit = 0x00000400
	// KHFSBootVolumeInconsistentBit is set while the volume is mounted for writing.
	KHFSBootVolumeInconsistentBit = 0x00000800
	// KHFSCatalogNodeIDsReusedBit is set once the next catalog ID has overflowed
	// and smaller IDs are being reused.
	KHFSCatalogNodeIDsReusedBit = 0x00001000
	// KHFSVolumeJournaledBit is set when the volume has a journal.
	KHFSVolumeJournaledBit    = 0x00002000
	KHFSVolumeInconsistentBit = 0x00004000
	// KHFSVolumeSoftwareLockBit is set when the volume is write-protected by software.
	KHFSVolumeSoftwareLockBit = 0x00008000
	KHFSContentProtectionBit  = 0x40000000
	KHFSUnusedNodeFixBit      = 0x80000000
)

type BTreeNodeType int8

const (
	BTreeNodeTypeLeaf   BTreeNodeType = -1
	BTreeNodeTypeIndex  BTreeNodeType = 0
	BTreeNodeTypeHeader BTreeNodeType = 1
	BTreeNodeTypeMap    BTreeNodeType = 2
)

func (t BTreeNodeType) String() string {
	switch t {
	case BTreeNodeTypeLeaf:
		return "Leaf"
	case BTreeNodeTypeIndex:
		return "Index"
	case BTreeNodeTypeHeader:
		return "Header"
	case BTreeNodeTypeMap:
		return "Map"
	}
	return fmt.Sprintf("BTreeNodeType(%d)", int(t))
}

type BTreeHeader struct {
	TreeDepth       uint16
	RootNode        uint32
	LeafRecordCount uint32
	FirstLeafNode   uint32
	LastLeafNode    uint32
	NodeSize        uint16
	MaxKeyLength    uint16
	TotalNodeCount  uint32
	FreeNodeCount   uint32
	// HFS+
	Reserved1      uint16
	ClumpSize      uint32
	BTreeFileType  byte
	KeyCompareType byte
	Attributes     uint32
	Reserved2      [64]byte
}

type BTreeDescriptor struct {
	// NextNode and PrevNode link the nodes at this level; 0 means no sibling.
	NextNode uint32
	PrevNode uint32
	NodeType BTreeNodeType
	// NodeLevel is the node's depth in the tree; leaves are level 1.
	NodeLevel   int8
	RecordCount uint16
	Reserved    uint16
}

type MDB struct {
	Signature            [2]byte // "BD" (\x42\x44)
	CreatedTimestamp     uint32
	ModifiedTimestamp    uint32
	VolumeAttributeFlags uint16
	RootFileCount        uint16
	// VolumeBitmapBlockNumber is an allocation block number from the start of
	// the volume, normally 3.
	VolumeBitmapBlockNumber uint16
	// Unknown is the start of the next allocation search.
	Unknown uint16
	// AllocationBlocks is capped at 65535 by the format.
	AllocationBlocks uint16
	// AllocationBlockSize is in bytes and is a multiple of 512.
	AllocationBlockSize uint32
	DefaultClumpSize    uint32
	// AllocationStartBlock is drAlBlSt: where the first allocation block sits,
	// counted in 512-byte logical blocks from the start of the volume. It is
	// not an allocation block number, and it has nothing to do with the extents
	// overflow file — that is ExtentsRecord, further down.
	AllocationStartBlock uint16
	// CNID is the next available catalog node identifier.
	CNID uint32
	// UnusedBlocks is the number of unused (allocation) blocks.
	UnusedBlocks    uint16
	VolumeLabelSize byte // at most 27
	VolumeLabel     [27]byte
	// BackupTimestamp is the date and time when the volume was last backed up.
	BackupTimestamp      uint32
	BackupSequenceNumber uint16
	// VolumeWriteCount contains the number of times the volume has been written to.
	VolumeWriteCount uint32
	// ClumpSizeForExtentsFile is the clump size for extents (overflow) file
	ClumpSizeForExtentsFile uint32
	ClumpSizeForCatalogFile uint32
	RootSubDirectoryCount   uint16
	TotalFileCount          uint32
	TotalDirectoryCount     uint32
	FinderInfo              FinderInfo
	// EmbeddedVolumeSignature and EmbeddedVolumeExtentDescriptor locate an
	// embedded HFS+ volume; they occupied drVCSize/drVBMCSize/drCtlCSize in HFS.
	EmbeddedVolumeSignature        uint16
	EmbeddedVolumeExtentDescriptor uint32
	ExtentsFileSize                uint32
	ExtentsRecord                  ExtentsRecord
	CatalogSize                    uint32
	CatalogFileExtentsRecord       ExtentsRecord
}

type FinderInfo struct {
	BootableSystemDirectoryID  uint32
	StartupAppParentID         uint32
	FinderWindowDirectoryID    uint32
	BootableSystemDirectoryID2 uint32
	Reserved                   uint32
	BootableSystemDirectoryID3 uint32
	UniqueVolumeID             uint64
}

type ExtentsRecord [3]Extent

// Extent is one HFS extent: a run of contiguous allocation blocks.
type Extent struct {
	StartBlock uint16
	// BlockCount is the extent length in allocation blocks.
	BlockCount uint16
}
