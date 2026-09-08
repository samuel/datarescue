package qic

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
)

// maxCatalogBytes bounds the catalog before a declared size is used to
// allocate anything. A record runs about 130 bytes, so this is room for some
// 300,000 files.
const maxCatalogBytes = 64 << 20

// byteRange is a span of the catalog no segment delivered.
type byteRange struct {
	Offset, Length int64
}

// File is one entry from the catalog, or from the data region when the catalog
// could not be read.
type File struct {
	Name       string
	ShortName  string
	Path       string // as the archive records it, drive letter and backslashes
	Dir        bool
	Empty      bool // a directory the backup recorded as holding nothing
	Size       int64
	Attributes uint8
	Flags      uint8
	Depth      int
	Created    time.Time
	Accessed   time.Time
	Modified   time.Time

	components []string // the path split up, for rebuilding and checking
	recOff     int64    // uncompressed offset of the record's signature
	dataOff    int64    // uncompressed offset of the first data byte, -1 if none
	// idx is the entry's position in the archive's own list. Files hands out a
	// slice a caller can copy from, and a drift found at one record moves every
	// file after it, so a read has to be resolved back to the archive's entry
	// rather than served from a copy holding the position it had before.
	idx     int
	checked anchorState
	intact  bool
}

// anchorState tracks whether the accumulated offset has been checked against
// the record the data region repeats there.
type anchorState uint8

const (
	// anchorUnchecked is a position nothing has been tested against yet.
	anchorUnchecked anchorState = iota
	// anchorOK is a position confirmed by the record the data region repeats
	// there.
	anchorOK
	// anchorUnresolved is a position where the record belongs to a different
	// file and the sweep for the file's own record never ran, because the
	// search budget ran out. The position is not verified, so the file is
	// refused.
	anchorUnresolved
	// anchorLost is a position where the record belongs to a different file
	// and a sweep of the region found the file's own record nowhere.
	anchorLost
)

// catalogRegion is the catalog with its segment headers taken out.
type catalogRegion struct {
	data  []byte
	holes []byteRange
}

// loadCatalogRegion reads the catalog whole, stripping the segment header off
// each of its segments.
//
// Reading it whole means a record straddling a segment boundary needs no
// special handling. The reference implementation reads through the headers
// instead and needs a dedicated path for it, plus another that fetches a
// record's own length word one byte at a time in case the header lands inside
// it; the first of those also decrements an unsigned counter it has just set
// to zero.
func loadCatalogRegion(r io.ReaderAt, x *segIndex, dirSize int64, diag diagFunc) (*catalogRegion, error) {
	held := x.catalogLen
	if held <= 0 {
		return nil, errors.New("qic: the file holds no catalog segment")
	}
	if held > maxCatalogBytes {
		return nil, fmt.Errorf("qic: a catalog of %d bytes is past what this reads", held)
	}

	cr := &catalogRegion{}
	for i := x.dataSegs; i < len(x.segs); i++ {
		seg := x.segs[i]
		if seg.Payload <= 0 {
			continue
		}
		off := seg.FileOffset
		if x.compressed {
			off += segHeadSize
		}
		buf := make([]byte, seg.Payload)
		if _, err := readFull(r, buf, off); err != nil {
			diag(DiagCatalogUnreadable,
				"the catalog segment %d at 0x%x could not be read, losing %d bytes of directory: %v",
				i, off, seg.Payload, err)
			cr.holes = append(cr.holes, byteRange{Offset: int64(len(cr.data)), Length: seg.Payload})
			cr.data = append(cr.data, make([]byte, seg.Payload)...)
			continue
		}
		cr.data = append(cr.data, buf...)
	}

	// The last catalog segment is padded out to the grid, so the directory
	// ends where the volume table says it does rather than where the segments
	// do. A declared size longer than what came back is damage already
	// reported by the segment index.
	if dirSize > 0 && dirSize < int64(len(cr.data)) {
		cr.data = cr.data[:dirSize]
	}
	if len(cr.data) == 0 {
		return nil, errors.New("qic: no catalog bytes were recovered")
	}
	return cr, nil
}

// lost reports whether a span of the catalog fell in a segment that would not
// read, so a record parsed out of zeroed filler is not mistaken for damage of
// its own.
func (cr *catalogRegion) lost(off, n int64) bool {
	for _, h := range cr.holes {
		if off < h.Offset+h.Length && h.Offset < off+n {
			return true
		}
	}
	return false
}

// decodeCatalog parses the catalog into files, rebuilds the tree and works out
// where each file's data sits.
func decodeCatalog(cr *catalogRegion, v Variant, drive string, diag diagFunc) ([]File, string, int64) {
	recs := parseRecords(cr, diag)
	if len(recs) == 0 {
		return nil, drive, 0
	}
	if drive == "" {
		// Windows 95 puts the drive nowhere in the volume table and names the
		// root record after the source volume instead, as "label(C:)".
		drive = driveFromRoot(recs[0].name)
	}
	paths := buildTree(recs, diag)
	files := make([]File, 0, len(recs))
	for i, r := range recs {
		f := File{
			Name:       r.name,
			ShortName:  r.short,
			Dir:        r.isDir(),
			Empty:      r.isEmpty(),
			Size:       r.fileLen,
			Attributes: r.attrib,
			Flags:      r.flag,
			Created:    r.created,
			Accessed:   r.accessed,
			Modified:   r.modified,
			components: paths[i],
			Depth:      len(paths[i]) + 1,
			dataOff:    -1,
		}
		// The root is the volume, not a directory inside it. Windows 95 names
		// it after the source drive and Windows 98 leaves it blank.
		if i == 0 {
			f.Depth = 0
			f.Path = drive
			if f.Path == "" {
				f.Path = r.name
			}
		} else {
			f.Path = joinPath(drive, paths[i], r.name)
		}
		files = append(files, f)
	}
	return files, drive, accumulate(files, recs, v)
}

// driveFromRoot picks the drive letter out of the root record's name.
func driveFromRoot(name string) string {
	i := strings.LastIndex(name, "(")
	if i < 0 {
		return ""
	}
	d := strings.TrimSuffix(name[i+1:], ")")
	if len(d) != 2 || d[1] != ':' {
		return ""
	}
	return strings.ToUpper(d)
}

// parseRecords walks the catalog, resynchronizing past a record whose fields
// disagree rather than giving up on everything after it.
func parseRecords(cr *catalogRegion, diag diagFunc) []record {
	var (
		recs []record
		off  int
	)
	for off+fix1Size <= len(cr.data) {
		r, err := parseRecord(cr.data[off:], false)
		if err != nil {
			next, ok := resync(cr.data, off)
			if !ok {
				if !cr.lost(int64(off), int64(len(cr.data)-off)) {
					diag(DiagRecordDamaged,
						"the catalog record at byte %d could not be decoded and no later record was found, losing the remaining %d bytes",
						off, len(cr.data)-off)
				}
				break
			}
			diag(DiagRecordResync,
				"the catalog record at byte %d could not be decoded; %d bytes were skipped to the next sound record",
				off, next-off)
			off = next
			continue
		}
		recs = append(recs, r)
		off += r.size
		if r.dirEnd() {
			if off < len(cr.data) && !allZero(cr.data[off:]) {
				diag(DiagTreeBroken,
					"the catalog marks its end at byte %d with %d bytes still to come",
					off, len(cr.data)-off)
			}
			break
		}
		if len(recs) > maxCatalogBytes/fix1Size {
			diag(DiagTreeBroken, "the catalog holds more records than it has room for")
			break
		}
	}
	return recs
}

// resync scans for the next position where a record and the one after it both
// check out. Confirming two in a row keeps a chance run of bytes from being
// taken for a record; it is the same test the HFS reader uses when it sweeps
// for catalog leaves. It always advances, since failing to would spin on the
// same window forever.
func resync(data []byte, from int) (int, bool) {
	for off := from + 1; off+fix1Size <= len(data); off++ {
		size, ok := recordSize(data[off:])
		if !ok {
			continue
		}
		if next := off + size; next+fix1Size <= len(data) {
			if _, ok := recordSize(data[next:]); !ok {
				continue
			}
		}
		return off, true
	}
	return 0, false
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// buildTree assigns each record the path of the directory holding it.
//
// The catalog is depth first: a directory's children are listed together and
// the list ends at the record carrying DIRLAST, then the child lists of those
// children follow in order. An empty directory contributes no list. Rebuilding
// it breadth first instead misplaces most of the archive, and the two are told
// apart by the path length every record carries: it is the encoded length of
// the parent's path, which makes it a check on the whole reconstruction that
// needs nothing from the data region.
func buildTree(recs []record, diag diagFunc) [][]string {
	paths := make([][]string, len(recs))
	type pending struct {
		index int
		path  []string
	}
	stack := []pending{{index: 0}}
	i := 1
	for len(stack) > 0 && i < len(recs) {
		dir := stack[0]
		stack = stack[1:]

		var children []int
		for i < len(recs) {
			paths[i] = dir.path
			children = append(children, i)
			last := recs[i].dirLast()
			i++
			if last {
				break
			}
		}

		var subs []pending
		for _, c := range children {
			if !recs[c].isDir() || recs[c].isEmpty() {
				continue
			}
			if len(dir.path) >= maxDepth {
				diag(DiagTreeBroken,
					"the directory %q sits deeper than %d levels, so its contents are left unplaced",
					recs[c].name, maxDepth)
				continue
			}
			subs = append(subs, pending{index: c, path: append(append([]string{}, dir.path...), recs[c].name)})
		}
		stack = append(subs, stack...)
	}

	if i < len(recs) {
		diag(DiagTreeBroken,
			"%d catalog records were left over after the tree was rebuilt, so they are placed at the root",
			len(recs)-i)
	}
	checkPaths(recs, paths, diag)
	return paths
}

// checkPaths compares each record's declared parent path length against the
// rebuilt tree. Where they disagree the record is placed against the longest
// ancestor of its assumed parent whose length does match, so a lost DIRLAST
// costs one child list rather than every directory below it.
func checkPaths(recs []record, paths [][]string, diag diagFunc) {
	var mismatched int
	for i := 1; i < len(recs); i++ {
		if pathLen(paths[i]) == recs[i].pathLen {
			continue
		}
		var fixed bool
		for n := range slices.Backward(paths[i]) {
			if pathLen(paths[i][:n]) == recs[i].pathLen {
				paths[i] = paths[i][:n]
				fixed = true
				break
			}
		}
		mismatched++
		if mismatched <= 1 {
			diag(DiagPathMismatch,
				"%q records a parent path of %d bytes where the rebuilt tree gives %d; it is placed %s",
				recs[i].name, recs[i].pathLen, pathLen(paths[i]),
				map[bool]string{true: "against the nearest matching directory", false: "where the tree put it"}[fixed])
		}
	}
	if mismatched > 1 {
		diag(DiagPathMismatch, "%d catalog records disagree with the rebuilt tree about their parent path", mismatched)
	}
}

// accumulate works out where each file's data sits. The catalog holds no
// pointer to it, so every offset is the sum of the records before it.
func accumulate(files []File, recs []record, v Variant) int64 {
	var pos int64
	for i := range files {
		files[i].recOff = pos
		dataStart, next := dataRecordSize(recs[i], v)
		if dataStart >= 0 {
			files[i].dataOff = pos + dataStart
		}
		pos += next
	}
	// Where the offsets run out, which a sound archive puts exactly at the size
	// the volume table records.
	return pos
}
