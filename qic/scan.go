package qic

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// scanWindow is how much of the data region is examined at a time. The overlap
// keeps a record header from being missed because it straddles two windows.
const (
	scanWindow  = 1 << 16
	scanOverlap = 4 + fix1Size
)

// verifyAnchor checks that the record the data region repeats at the file's
// accumulated offset really is that file's. Only this can catch a drift in the
// accumulation: the catalog holds no pointer to a file's data, so a single bad
// record length silently moves every file after it.
//
// It runs when a file is opened, where the record shares a segment with the
// file's first bytes and so costs nothing extra, and at Open for the first
// file, where a wrong variant or root record would show immediately.
func (a *Archive) verifyAnchor(f *File) {
	if f.checked != anchorUnchecked || f.dataOff < 0 {
		return
	}
	if a.scanned {
		f.checked = anchorOK // the offsets came from the records themselves
		return
	}

	switch a.anchorAt(f, f.recOff) {
	case anchorSame:
		f.checked = anchorOK
		return
	case anchorUnread:
		// The record could not be read, which says nothing about where the
		// file is: the catalog's arithmetic still puts it here, and the
		// segments past the damage are intact. Sweeping for a record that lies
		// inside the hole finds nothing and condemns the whole file, when the
		// damage only affects the part of it over the bad segment.
		f.checked = anchorOK
		a.diags.add(DiagDataOffsetUnchecked,
			"%q could not be checked against the data region, the record at %d never having been recovered, so the position the catalog gives it stands",
			f.Path, f.recOff)
		return
	}
	if a.sweeps >= maxAnchorSweeps {
		// A sweep runs to the end of the data region. One rebase moves every
		// file after the record that drifted, so a sound archive needs one; an
		// archive whose every record disagrees would sweep the whole region
		// once per file. The record here reads and belongs to a different
		// file, so with the budget spent the position is marked unresolved
		// rather than verified, and the remaining files stay where the catalog
		// puts them.
		f.checked = anchorUnresolved
		a.diags.add(DiagDataOffsetDrift,
			"%q is not where the catalog puts it either, and the data region has been swept %d times already, so its position is left unresolved and none of its bytes are reported",
			f.Path, a.sweeps)
		return
	}
	a.sweeps++
	a.diags.add(DiagDataOffsetDrift,
		"%q was expected at %d but the data region holds a different record there, so the region is searched for it",
		f.Path, f.recOff)

	from := f.recOff
	if from > scanWindow {
		from -= scanWindow
	} else {
		from = 0
	}
	off, err := a.findAnchor(f, from)
	if err != nil {
		f.checked = anchorLost
		a.diags.add(DiagDataOffsetLost,
			"%q could not be found in the data region, so none of its bytes are reported: %v", f.Path, err)
		return
	}

	shift := off - f.recOff
	a.rebase(f, shift)
	a.diags.add(DiagDataOffsetDrift,
		"%q was found %d bytes from where the catalog put it; every file after it is moved by the same amount",
		f.Path, shift)
}

// anchorResult is what the record at an offset shows about the file.
type anchorResult uint8

const (
	// anchorSame means the record there is this file's.
	anchorSame anchorResult = iota
	// anchorOther means the bytes there read, and are not this file's record.
	anchorOther
	// anchorUnread means the record's bytes were never recovered, which is not
	// evidence that the file is somewhere else.
	anchorUnread
)

// anchorAt reads the record at off and reports what it is. A read that failed
// only settles the question when the bytes that did come back are the file's
// record anyway: otherwise the answer is that nothing was read, since a record
// lying under a bad segment is neither present nor absent.
func (a *Archive) anchorAt(f *File, off int64) anchorResult {
	buf := make([]byte, 4+fix1Size+encodedLen(f.Name)+fix2Size+encodedLen(f.ShortName)+pathLen(f.components))
	_, err := a.data.ReadAt(buf, off)
	if isDataAnchor(buf) {
		if r, perr := parseRecord(buf[4:], true); perr == nil &&
			r.name == f.Name && samePath(r.path, f.components) {
			return anchorSame
		}
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return anchorUnread
	}
	return anchorOther
}

func samePath(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// findAnchor sweeps forward for the record belonging to the file.
func (a *Archive) findAnchor(f *File, from int64) (int64, error) {
	end := a.data.Size()
	buf := make([]byte, scanWindow)
	for off := from; off < end; off += scanWindow - scanOverlap {
		n, err := a.data.ReadAt(buf, off)
		if n == 0 {
			if err == nil || errors.Is(err, io.EOF) {
				break
			}
			// A segment that will not read cannot hide the record from the
			// segments after it, so the sweep steps over it.
			continue
		}
		window := buf[:n]
		for i := 0; i+scanOverlap <= len(window); i++ {
			if binary.LittleEndian.Uint32(window[i:]) != datSig || !isDataAnchor(window[i:]) {
				continue
			}
			if a.anchorAt(f, off+int64(i)) == anchorSame {
				return off + int64(i), nil
			}
		}
	}
	return 0, fmt.Errorf("no record naming %q was found", f.Name)
}

// rebase moves the file and every file after it by the same amount, since a
// drift found at one record displaces all the ones the accumulation reached
// through it.
func (a *Archive) rebase(f *File, shift int64) {
	from := f.idx
	if from < 0 || from >= len(a.files) {
		return
	}
	for i := from; i < len(a.files); i++ {
		a.files[i].recOff += shift
		if a.files[i].dataOff >= 0 {
			a.files[i].dataOff += shift
		}
		if i > from {
			a.files[i].checked = anchorUnchecked
		}
	}
	a.markIntact()
}

// scanData recovers the file list from the data region alone, for a backup
// whose catalog will not read.
//
// Every record is repeated inline ahead of the bytes it describes, complete
// with the path of the directory holding it, so no tree has to be rebuilt and
// no offset accumulated. What the data region does not carry is a file's
// length -- the field holding it is all ones here -- so a size is the distance
// to the next record less the trailer bytes between them. The reference
// implementation works that subtraction out and then never applies it, which
// is why its own scan returns every Windows 95 file twelve bytes too long.
func (a *Archive) scanData() ([]File, error) {
	end := a.data.Size()
	if end <= 0 {
		return nil, errors.New("the data region holds no bytes")
	}

	type anchor struct {
		off int64
		rec record
	}
	var found []anchor
	buf := make([]byte, scanWindow)
	// taken is where the last record accepted ends. Windows overlap so that a
	// record on a boundary is not missed, which means it is also seen twice,
	// and a second copy of one record puts a phantom entry of no length between
	// the two.
	var taken int64
	for off := int64(0); off < end; off += scanWindow - scanOverlap {
		n, err := a.data.ReadAt(buf, off)
		if n == 0 {
			if err == nil || errors.Is(err, io.EOF) {
				break
			}
			continue
		}
		window := buf[:n]
		for i := 0; i+scanOverlap <= len(window); i++ {
			at := off + int64(i)
			if at < taken {
				continue
			}
			if binary.LittleEndian.Uint32(window[i:]) != datSig || !isDataAnchor(window[i:]) {
				continue
			}
			// The record may run past the window, so it is read again at its
			// own offset where the path is sure to be complete.
			rec, ok := a.readDataRecord(at)
			if !ok {
				continue
			}
			found = append(found, anchor{off: at, rec: rec})
			taken = at + 4 + int64(rec.size)
			i += rec.size + 3
		}
	}
	if len(found) == 0 {
		return nil, errors.New("no record signature was found in the data region")
	}

	if a.driveLetter == "" {
		// The root record's name gives the source volume as "label(C:)", the
		// only place a Windows 95 archive records the drive.
		a.driveLetter = driveFromRoot(found[0].rec.name)
	}

	files := make([]File, 0, len(found))
	for i, f := range found {
		next := end
		if i+1 < len(found) {
			next = found[i+1].off
		}
		file := File{
			Name:       f.rec.name,
			ShortName:  f.rec.short,
			Dir:        f.rec.isDir(),
			Empty:      f.rec.isEmpty(),
			Attributes: f.rec.attrib,
			Flags:      f.rec.flag,
			Created:    f.rec.created,
			Accessed:   f.rec.accessed,
			Modified:   f.rec.modified,
			components: f.rec.path,
			Depth:      len(f.rec.path) + 1,
			Path:       joinPath(a.driveLetter, f.rec.path, f.rec.name),
			recOff:     f.off,
			dataOff:    -1,
			checked:    anchorOK,
		}
		if i == 0 && f.rec.flag&flagRoot != 0 {
			file.Path = a.driveLetter
			file.Depth = 0
		}
		if !file.Dir {
			start, _ := dataRecordSize(f.rec, a.lay.variant)
			file.dataOff = f.off + start
			file.Size = max(next-file.dataOff-trailerSize*a.lay.variant.fileTrailers(), 0)
		}
		files = append(files, file)
	}
	a.diags.add(DiagDataScan, "%d records were recovered by scanning the data region", len(files))
	return files, nil
}

// readDataRecord reads one whole record, path included, at its own offset.
func (a *Archive) readDataRecord(off int64) (record, bool) {
	head := make([]byte, 4+fix1Size)
	if _, err := a.data.ReadAt(head, off); err != nil && !errors.Is(err, io.EOF) {
		return record{}, false
	}
	if !isDataAnchor(head) {
		return record{}, false
	}
	size, ok := plausibleRecord(head[4:])
	if !ok {
		return record{}, false
	}
	pathBytes := int(binary.LittleEndian.Uint16(head[4+offPathLen:]))
	buf := make([]byte, 4+size+pathBytes)
	if _, err := a.data.ReadAt(buf, off); err != nil && !errors.Is(err, io.EOF) {
		return record{}, false
	}
	rec, err := parseRecord(buf[4:], true)
	if err != nil {
		return record{}, false
	}
	return rec, true
}
