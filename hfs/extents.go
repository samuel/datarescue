package hfs

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
)

// extentKeyLength is the on-disk length byte of an extents B-tree key: one byte
// of fork type, four of file ID and two of starting allocation block.
const extentKeyLength = 7

// errNoExtentsOverflow reports a volume with no extents overflow file at all,
// which is normal. It is distinct from an overflow file that would not parse.
// With no overflow file, a short fork's remaining bytes were never recorded
// anywhere, so the missing length can be measured. With an unparsable one, the
// record may exist but could not be read, so it cannot.
var errNoExtentsOverflow = errors.New("hfs: volume has no extents overflow file")

// extentsOverflow parses the extents overflow file into a lookup from
// (fork, file, starting block) to the next three extents. The result is cached,
// including a nil result for a volume that has no overflow file, since every
// fork read consults it.
func (v *Volume) extentsOverflow() (map[extentKey]ExtentsRecord, error) {
	if v.overflowLoaded {
		return v.overflow, v.overflowErr
	}
	v.overflowLoaded = true
	v.overflow, v.overflowErr = v.readExtentsOverflow()
	return v.overflow, v.overflowErr
}

func (v *Volume) readExtentsOverflow() (map[extentKey]ExtentsRecord, error) {
	f := &fork{v: v, extents: v.MDB.ExtentsRecord[:], size: int64(v.MDB.ExtentsFileSize)}
	if f.size == 0 {
		return nil, errNoExtentsOverflow
	}

	head, nodeSize, err := readBTreeHeader(f)
	if err != nil {
		return nil, err
	}

	out := make(map[extentKey]ExtentsRecord)
	visited := make(map[uint32]struct{})
	for node := head.FirstLeafNode; node != 0; {
		if _, seen := visited[node]; seen {
			// The leaf chain loops, so the rest of it was never loaded.
			v.overflowPartial = true
			break
		}
		visited[node] = struct{}{}
		buf := make([]byte, nodeSize)
		if _, err := f.readAt(buf, int64(node)*int64(nodeSize)); err != nil {
			// Stop at the first unreadable node. What was parsed is kept, since
			// a partial extents map is better than none. But records that exist
			// on disk were never loaded, so a fork that comes back short must
			// not be treated as having a hole of measurable length.
			v.overflowPartial = true
			v.diag(DiagOverflowTruncated, 0,
				"the extents overflow file stops at node %d (%s); records past it were not loaded", node, err)
			break
		}
		var desc BTreeDescriptor
		if err := binary.Read(bytes.NewReader(buf[:14]), binary.BigEndian, &desc); err != nil {
			v.overflowPartial = true
			break
		}
		if desc.NodeType != BTreeNodeTypeLeaf {
			v.overflowPartial = true
			break
		}
		parseExtentNode(buf, nodeSize, desc, out)
		node = desc.NextNode
	}
	return out, nil //nolint:nilerr // a truncated walk still yields a usable extents map
}

// readBTreeHeader reads node 0 of a B-tree file and returns its header record
// and validated node size.
func readBTreeHeader(f *fork) (BTreeHeader, uint16, error) {
	nodeSize := uint16(512)
	buf := make([]byte, nodeSize)
	if _, err := f.readAt(buf, 0); err != nil {
		return BTreeHeader{}, 0, fmt.Errorf("hfs: read B-tree header node: %w", err)
	}
	var head BTreeHeader
	if err := binary.Read(bytes.NewReader(buf[14:]), binary.BigEndian, &head); err != nil {
		return BTreeHeader{}, 0, fmt.Errorf("hfs: read B-tree header: %w", err)
	}
	if !validNodeSize(head.NodeSize) {
		return BTreeHeader{}, 0, fmt.Errorf("hfs: B-tree header declares invalid node size %d", head.NodeSize)
	}
	if head.NodeSize != nodeSize {
		nodeSize = head.NodeSize
		buf = make([]byte, nodeSize)
		if _, err := f.readAt(buf, 0); err != nil {
			return BTreeHeader{}, 0, fmt.Errorf("hfs: re-read B-tree header node: %w", err)
		}
		if err := binary.Read(bytes.NewReader(buf[14:]), binary.BigEndian, &head); err != nil {
			return BTreeHeader{}, 0, fmt.Errorf("hfs: re-read B-tree header: %w", err)
		}
	}
	return head, nodeSize, nil
}

// parseExtentNode decodes the records of one extents overflow leaf node into
// out. Malformed records are skipped, as in the catalog walker.
func parseExtentNode(node []byte, nodeSize uint16, desc BTreeDescriptor, out map[extentKey]ExtentsRecord) {
	for i := range int(desc.RecordCount) {
		offPos := int(nodeSize) - 2*(i+1)
		if offPos < 0 {
			return
		}
		recordOffset := int(binary.BigEndian.Uint16(node[offPos : offPos+2]))
		if recordOffset+1+extentKeyLength > int(nodeSize) {
			continue
		}
		if int(node[recordOffset]) != extentKeyLength {
			continue // not an extents key
		}
		key := extentKey{
			forkType:   node[recordOffset+1],
			cnid:       binary.BigEndian.Uint32(node[recordOffset+2 : recordOffset+6]),
			startBlock: uint32(binary.BigEndian.Uint16(node[recordOffset+6 : recordOffset+8])),
		}
		dataOffset := recordOffset + 1 + extentKeyLength
		if dataOffset%2 != 0 {
			dataOffset++
		}
		if dataOffset+12 > int(nodeSize) {
			continue
		}
		var rec ExtentsRecord
		if err := binary.Read(bytes.NewReader(node[dataOffset:dataOffset+12]), binary.BigEndian, &rec); err != nil {
			continue
		}
		out[key] = rec
	}
}
