package iso

import (
	"encoding/binary"
	"fmt"
	"io/fs"
	"strings"
	"time"
)

// rockRidge is what a directory record's System Use Area adds to it.
type rockRidge struct {
	name    string
	symlink string

	hasPX    bool
	mode     uint32 // POSIX st_mode
	uid, gid uint32

	// modify, access, change and create are the TF times; each is zero when
	// TF does not record it.
	modify, access, change, create time.Time

	// childLink is the LBA a CL entry gives: this record is a placeholder for
	// a directory mkisofs relocated to keep the tree within eight levels, and
	// the real directory is there.
	childLink    int64
	hasChildLink bool
	// relocated is set by an RE entry: this directory is the relocated copy,
	// stored somewhere else (usually rr_moved), and is reached through its
	// placeholder instead.
	relocated bool

	// damage describes entries that were present but could not be decoded.
	damage []string
}

// apply copies the Rock Ridge fields onto e. TF's modify time replaces the
// record date, which is the time the record was written.
func (rr *rockRidge) apply(e *DirEntry) {
	e.Symlink = rr.symlink
	if rr.hasPX {
		e.HasPOSIX = true
		e.Mode = posixMode(rr.mode)
		e.UID, e.GID = rr.uid, rr.gid
	}
	if !rr.modify.IsZero() {
		e.ModTime = rr.modify
	}
	e.AccessTime, e.ChangeTime, e.CreateTime = rr.access, rr.change, rr.create
}

// parseRockRidge decodes a directory record's Rock Ridge entries.
func (w *walker) parseRockRidge(rec []byte, nameLen int) rockRidge {
	// System Use Area starts after the file identifier, padded to even offset.
	suaStart := 33 + nameLen
	if suaStart%2 != 0 {
		suaStart++
	}
	suaStart += w.r.rrSkip
	if suaStart >= len(rec) {
		return rockRidge{}
	}
	var st suaState
	w.parseSUA(rec[suaStart:], &st)

	rr := st.rr
	if len(st.nmParts) > 0 {
		rr.name = strings.Join(st.nmParts, "")
	}
	if len(st.slParts) > 0 {
		rr.symlink = strings.Join(st.slParts, "")
	}
	return rr
}

// suaState holds what one System Use Area scan accumulates, together with the
// guards its continuation chain needs. A CE entry gives an LBA and an offset
// read off the disc, so it can point back at the area that contains it.
type suaState struct {
	nmParts []string
	nmDone  bool
	slParts []string
	slDone  bool
	rr      rockRidge // PX, TF and damage; the first PX and TF are used
	pxSeen  bool
	tfSeen  bool
	// depth is how many CE links deep the scan is, and seen the (lba, offset)
	// pairs it has already followed.
	depth int
	seen  map[[2]int64]bool
}

// maxCEDepth bounds the continuation chain. Real discs use one continuation and
// occasionally two; without a bound a CE pointing back at its own area recurses
// until the stack runs out.
const maxCEDepth = 8

// parseSUA parses System Use Area entries, following CE continuations.
//
// Only the first CE in an area is followed. An area has one continuation, and
// following every CE a damaged area holds fans out: each points at a new
// (lba, offset) the visited set has not seen, so eight levels of a hundred CEs
// each is far more reads than any image holds.
func (w *walker) parseSUA(sua []byte, st *suaState) {
	var ceFollowed bool
	for i := 0; i+4 <= len(sua); {
		if sua[i] == 0x00 {
			break
		}
		sig := string(sua[i : i+2])
		entLen := int(sua[i+2])
		if entLen < 4 || i+entLen > len(sua) {
			break
		}
		data := sua[i+4 : i+entLen]

		switch sig {
		case "NM":
			if !st.nmDone && len(data) >= 2 {
				flags := data[0]
				st.nmParts = append(st.nmParts, string(data[1:]))
				if flags&0x01 == 0 {
					st.nmDone = true
				}
			}
		case "SL":
			if !st.slDone && len(data) >= 2 {
				flags := data[0]
				st.slParts = append(st.slParts, parseSLComponents(data[1:]))
				if flags&0x01 == 0 {
					st.slDone = true
				}
			}
		case "PX":
			if !st.pxSeen {
				st.pxSeen = true
				parsePX(data, &st.rr)
			}
		case "TF":
			if !st.tfSeen {
				st.tfSeen = true
				parseTF(data, &st.rr)
			}
		case "CL":
			if len(data) >= 8 {
				st.rr.childLink = int64(binary.LittleEndian.Uint32(data[0:4]))
				st.rr.hasChildLink = true
			} else {
				st.rr.damage = append(st.rr.damage, "Rock Ridge CL entry is too short to hold an LBA")
			}
		case "RE":
			st.rr.relocated = true
		// PL gives a relocated directory's real parent, for its ".." entry.
		// The walk reports no ".." entries and reaches a relocated directory
		// through its placeholder, so its parent is already the right one.
		case "CE":
			if !ceFollowed && len(data) >= 12 {
				ceFollowed = true
				w.parseCE(
					int64(binary.LittleEndian.Uint32(data[0:4])),
					int64(binary.LittleEndian.Uint32(data[4:8])),
					int64(binary.LittleEndian.Uint32(data[8:12])),
					st)
			}
		}
		i += entLen
	}
}

// parseCE follows one Rock Ridge continuation. The visited set stops a cycle,
// such as an area pointing at itself or two pointing at each other. The depth
// cap stops a deep chain that never revisits a position.
func (w *walker) parseCE(lba, off, length int64, st *suaState) {
	key := [2]int64{lba, off}
	switch {
	case st.depth >= maxCEDepth:
		w.issue("continuation chain at LBA %d is deeper than %d links, so a name or symlink target is truncated",
			lba, maxCEDepth)
		return
	case st.seen[key]:
		w.issue("continuation area at LBA %d offset %d links back on itself, so a name or symlink target is truncated",
			lba, off)
		return
	}
	if st.seen == nil {
		st.seen = make(map[[2]int64]bool, maxCEDepth)
	}
	st.seen[key] = true

	sec, err := w.r.ReadSector(lba)
	switch {
	case err != nil:
		// The continuation holds the rest of a name or symlink target, so
		// losing it truncates the value rather than dropping it. That is a wrong
		// answer, so it must be reported.
		w.issue("continuation area at LBA %d unreadable, a name or symlink target is truncated: %s", lba, err)
	case off+length <= int64(len(sec)):
		st.depth++
		w.parseSUA(sec[off:off+length], st)
		st.depth--
	default:
		w.issue("continuation area at LBA %d runs past the sector, a name or symlink target is truncated", lba)
	}
}

// parsePX decodes a PX entry: st_mode, st_nlink, st_uid and st_gid, each a
// 32-bit value stored in both byte orders, then st_ino in RRIP 1.12.
func parsePX(data []byte, rr *rockRidge) {
	if len(data) < 32 {
		rr.damage = append(rr.damage, fmt.Sprintf("Rock Ridge PX entry is %d bytes, too short to hold a mode, uid and gid", len(data)))
		return
	}
	rr.hasPX = true
	rr.mode = binary.LittleEndian.Uint32(data[0:4])
	rr.uid = binary.LittleEndian.Uint32(data[16:20])
	rr.gid = binary.LittleEndian.Uint32(data[24:28])
}

// TF flag bits. The timestamps that follow appear in this bit order, one for
// each bit set, and tfLongForm selects the 17-byte format over the 7-byte one.
const (
	tfCreation   = 0x01
	tfModify     = 0x02
	tfAccess     = 0x04
	tfAttributes = 0x08
	tfBackup     = 0x10
	tfExpiration = 0x20
	tfEffective  = 0x40
	tfLongForm   = 0x80
)

// parseTF decodes a TF entry's creation, modify, access and attribute change
// times. A damaged timestamp is recorded and left zero; the others are still
// used.
func parseTF(data []byte, rr *rockRidge) {
	if len(data) < 1 {
		rr.damage = append(rr.damage, "Rock Ridge TF entry is empty")
		return
	}
	flags := data[0]
	size, parse := recordTimeSize, parseRecordTime
	if flags&tfLongForm != 0 {
		size, parse = volumeTimeSize, parseVolumeTime
	}
	stamps := data[1:]
	for _, f := range []struct {
		bit  byte
		name string
		dst  *time.Time
	}{
		{tfCreation, "creation", &rr.create},
		{tfModify, "modify", &rr.modify},
		{tfAccess, "access", &rr.access},
		{tfAttributes, "attribute change", &rr.change},
		{tfBackup, "backup", nil},
		{tfExpiration, "expiration", nil},
		{tfEffective, "effective", nil},
	} {
		if flags&f.bit == 0 {
			continue
		}
		if len(stamps) < size {
			rr.damage = append(rr.damage, fmt.Sprintf("Rock Ridge TF entry ends before its %s time", f.name))
			return
		}
		if f.dst != nil {
			t, err := parse(stamps[:size])
			if err != nil {
				rr.damage = append(rr.damage, fmt.Sprintf("Rock Ridge TF %s time is damaged: %s", f.name, err))
			} else {
				*f.dst = t
			}
		}
		stamps = stamps[size:]
	}
}

// POSIX st_mode file type and special bits.
const (
	sIFMT   = 0o170000
	sIFSOCK = 0o140000
	sIFLNK  = 0o120000
	sIFBLK  = 0o060000
	sIFDIR  = 0o040000
	sIFCHR  = 0o020000
	sIFIFO  = 0o010000
	sISUID  = 0o4000
	sISGID  = 0o2000
	sISVTX  = 0o1000
)

// posixMode converts a POSIX st_mode to an fs.FileMode.
func posixMode(m uint32) fs.FileMode {
	mode := fs.FileMode(m & 0o777)
	switch m & sIFMT {
	case sIFDIR:
		mode |= fs.ModeDir
	case sIFLNK:
		mode |= fs.ModeSymlink
	case sIFCHR:
		mode |= fs.ModeDevice | fs.ModeCharDevice
	case sIFBLK:
		mode |= fs.ModeDevice
	case sIFIFO:
		mode |= fs.ModeNamedPipe
	case sIFSOCK:
		mode |= fs.ModeSocket
	}
	if m&sISUID != 0 {
		mode |= fs.ModeSetuid
	}
	if m&sISGID != 0 {
		mode |= fs.ModeSetgid
	}
	if m&sISVTX != 0 {
		mode |= fs.ModeSticky
	}
	return mode
}

// entryMode settles an entry's mode. The directory flag and an SL entry decide
// whether it is a directory or a symlink, since those decide how it is walked
// and read; PX supplies the permissions and any other file type.
func entryMode(e DirEntry) fs.FileMode {
	if !e.HasPOSIX {
		switch {
		case e.Symlink != "":
			return fs.ModeSymlink | 0o444
		case e.IsDir:
			return fs.ModeDir | 0o555
		}
		return 0o444
	}
	mode := e.Mode &^ (fs.ModeDir | fs.ModeSymlink)
	switch {
	case e.Symlink != "":
		mode = mode&^fs.ModeType | fs.ModeSymlink
	case e.IsDir:
		mode = mode&^fs.ModeType | fs.ModeDir
	}
	return mode
}

// parseSLComponents reconstructs a symlink target from Rock Ridge SL component records.
func parseSLComponents(data []byte) string {
	var parts []string
	for i := 0; i+2 <= len(data); {
		compFlags := data[i]
		compLen := int(data[i+1])
		i += 2
		if i+compLen > len(data) {
			break
		}
		switch {
		case compFlags&0x08 != 0: // root
			parts = append(parts, "/")
		case compFlags&0x04 != 0: // parent
			parts = append(parts, "..")
		case compFlags&0x02 != 0: // current
			parts = append(parts, ".")
		default:
			parts = append(parts, string(data[i:i+compLen]))
		}
		i += compLen
	}
	return strings.Join(parts, "/")
}

// findSP looks for an SP entry in the root's dot record, returning the
// number of bytes to skip before the System Use Area of every other record.
func findSP(dotRec []byte) (bool, int) {
	nameLen := int(dotRec[32])
	suaStart := 33 + nameLen
	if suaStart%2 != 0 {
		suaStart++
	}
	if suaStart+7 > len(dotRec) {
		return false, 0
	}
	sua := dotRec[suaStart:]
	for i := 0; i+7 <= len(sua); {
		sig := string(sua[i : i+2])
		entLen := int(sua[i+2])
		if entLen < 4 {
			break
		}
		if sig == "SP" && entLen == 7 && sua[i+4] == 0xBE && sua[i+5] == 0xEF {
			return true, int(sua[i+6])
		}
		i += entLen
	}
	return false, 0
}
