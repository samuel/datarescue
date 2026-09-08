package test1step

import (
	"encoding/binary"
	"fmt"
	"math"
	"strings"
	"time"
)

// column is one field of a table.
type column struct {
	name   string
	typ    byte
	length int
}

const (
	dbfVersion3     = 0x03
	dbfHeaderSize   = 32
	dbfFieldSize    = 32
	dbfFieldTerm    = 0x0D
	dbfEOF          = 0x1A
	memberEntrySize = 0x130
	containerMagic  = 0xCA7557AC
)

// writeTable renders a dBase III table. Rows are field values in column order,
// already the right width; the control record every 1-Step table starts with
// is prepended here.
func writeTable(cols []column, rows [][]string) []byte {
	recordSize := 1
	for _, c := range cols {
		recordSize += c.length
	}
	all := append([][]string{controlRow(cols, len(rows)+1)}, rows...)

	headerLen := dbfHeaderSize + len(cols)*dbfFieldSize + 1
	header := make([]byte, headerLen)
	header[0] = dbfVersion3
	header[1] = byte((Created.Year() - 1900) & 0xff)
	header[2] = byte(int(Created.Month()) & 0xff)
	header[3] = byte(Created.Day() & 0xff)
	binary.LittleEndian.PutUint32(header[4:], uint32(len(all)&0xffffffff))
	binary.LittleEndian.PutUint16(header[8:], uint16(headerLen&0xffff))
	binary.LittleEndian.PutUint16(header[10:], uint16(recordSize&0xffff))
	offset := 1
	for i, c := range cols {
		d := header[dbfHeaderSize+i*dbfFieldSize:]
		copy(d[:11], c.name)
		d[11] = c.typ
		binary.LittleEndian.PutUint32(d[12:], uint32(offset&0xffffffff))
		// A character field longer than 255 bytes carries the high byte of its
		// length where the decimal count would be.
		d[16] = byte(c.length & 0xff)
		d[17] = byte((c.length >> 8) & 0xff)
		offset += c.length
	}
	header[headerLen-1] = dbfFieldTerm

	out := make([]byte, 0, headerLen+len(all)*recordSize+1)
	out = append(out, header...)
	for _, row := range all {
		rec := make([]byte, recordSize)
		rec[0] = ' '
		at := 1
		for i, c := range cols {
			copy(rec[at:at+c.length], pad(row[i], c.length))
			at += c.length
		}
		out = append(out, rec...)
	}
	return append(out, dbfEOF)
}

// controlRow is the record every table starts with: its second field holds the
// row count including itself, and its other numeric fields are -1.
func controlRow(cols []column, total int) []string {
	row := make([]string, len(cols))
	for i, c := range cols {
		switch {
		case i == 0:
			row[i] = num(0)
		case i == 1:
			row[i] = num(int64(total))
		case c.typ == 'N':
			row[i] = num(-1)
		default:
			row[i] = ""
		}
	}
	return row
}

// num renders a numeric field the way the catalog does, right-justified in the
// 12 bytes every numeric field uses.
func num(v int64) string { return fmt.Sprintf("%12d", v) }

// numLeft renders a numeric field left-justified, which is how the catalog
// stores the size and offset fields.
func numLeft(v int64) string { return fmt.Sprintf("%-12d", v) }

func pad(s string, n int) string {
	if len(s) >= n {
		return s[:n]
	}
	return s + strings.Repeat(" ", n-len(s))
}

func stamp(t time.Time) string {
	if t.IsZero() {
		t = Created
	}
	return t.Format("20060102150405")
}

var (
	diskCols = []column{
		{"SERIAL", 'N', 12}, {"NUMDIRS", 'N', 12}, {"NUMFILES", 'N', 12},
		{"VLSRDW", 'N', 12}, {"USED_HI", 'N', 12}, {"USED_LO", 'N', 12},
		{"FREE_HI", 'N', 12}, {"FREE_LO", 'N', 12}, {"LABEL", 'C', 12},
		{"VLSRDT", 'C', 14}, {"DATETIME", 'C', 14}, {"DRV_LTR", 'C', 2},
		{"HASDSKST", 'N', 12},
	}
	dirCols = []column{
		{"SERIAL", 'N', 12}, {"DISKSER", 'N', 12}, {"DIRSER", 'N', 12},
		{"NAME", 'C', 240},
	}
	fileCols = []column{
		{"SERIAL", 'N', 12}, {"DIRSER", 'N', 12}, {"STATUS", 'N', 12},
		{"DISKSER", 'N', 12}, {"ATTRIB", 'N', 12}, {"SIZE_HI", 'N', 12},
		{"SIZE_LO", 'N', 12}, {"DATETIME", 'C', 14}, {"NAME", 'C', 240},
	}
	chunkCols = []column{
		{"SERIAL", 'N', 12}, {"ORGSER", 'N', 12}, {"SEQUENCE", 'N', 12},
		{"ORGSIZE", 'N', 12}, {"COMPSIZE", 'N', 12}, {"ARCDSKSE", 'N', 12},
		{"CHK_SUM", 'N', 12}, {"COMP_LVL", 'N', 12}, {"OFFS_HI", 'N', 12},
		{"OFFS_LO", 'N', 12}, {"IS_LAST", 'N', 12},
	}
	jobCols = []column{
		{"SERIAL", 'N', 12}, {"JOBNUM", 'N', 12}, {"NUMDISKS", 'N', 12},
		{"TGDRV", 'N', 12}, {"TGDRVT", 'N', 12}, {"HASPSW", 'N', 12},
		{"ISCUST", 'N', 12}, {"ISCOMP", 'N', 12}, {"CUSTDATE", 'C', 14},
		{"PASSWORD", 'C', 32}, {"DESCR", 'C', 256},
	}
	textCols = []column{
		{"SERIAL", 'N', 12}, {"DATATYPE", 'N', 12}, {"DATATEXT", 'C', 240},
	}
	sessionCols = []column{
		{"SERIAL", 'N', 12}, {"SESSFROM", 'N', 12}, {"SESSTO", 'N', 12},
		{"JOBNUM", 'N', 12}, {"DISKNUM", 'N', 12}, {"SESSDATE", 'C', 14},
	}
)

// buildCatalog renders the container: the seven tables, each with the index
// member the original writer put beside it, behind a member directory.
func buildCatalog(opts Options, dirs []dirRow, dirOf []int, chunks []chunk, sessions []session) []byte {
	tables := []struct {
		name string
		data []byte
	}{
		{"IOADISK.DBF", writeTable(diskCols, diskRows())},
		{"IOADIR.DBF", writeTable(dirCols, dirRows(dirs))},
		{"IOAFILE.DBF", writeTable(fileCols, fileRows(opts.Files, dirOf))},
		{"IOAARCF.DBF", writeTable(chunkCols, chunkRows(chunks, opts))},
		{"IOJOB.DBF", writeTable(jobCols, jobRows(opts))},
		{"IOTEXT.DBF", writeTable(textCols, textRows())},
		{"IOSESS.DBF", writeTable(sessionCols, sessionRows(sessions))},
	}

	// Each table is followed by its index, which the reader has no use for but
	// which has to be stepped over correctly.
	type member struct {
		name string
		data []byte
	}
	members := make([]member, 0, len(tables)*2)
	for _, t := range tables {
		members = append(members, member{t.name, t.data})
		index := make([]byte, 512)
		copy(index, "index stub for "+t.name)
		members = append(members, member{strings.TrimSuffix(t.name, ".DBF") + ".CDX", index})
	}

	entriesSize := len(members) * memberEntrySize
	var data []byte
	for _, m := range members {
		data = append(data, m.data...)
	}

	dir := make([]byte, 0x10+entriesSize)
	binary.LittleEndian.PutUint32(dir, containerMagic)
	binary.LittleEndian.PutUint32(dir[4:], uint32(len(members)&0xffffffff))
	binary.LittleEndian.PutUint32(dir[8:], uint32(entriesSize&0xffffffff))
	binary.LittleEndian.PutUint32(dir[12:], uint32(len(data)&0xffffffff))
	for i, m := range members {
		e := dir[0x10+i*memberEntrySize:]
		copy(e[0x0c:], m.name)
		binary.LittleEndian.PutUint64(e[0x10c:], math.Float64bits(oleDays(Created)))
		binary.LittleEndian.PutUint64(e[0x118:], uint64(len(m.data)))
		binary.LittleEndian.PutUint64(e[0x120:], 0x20)
	}
	out := make([]byte, 0, len(dir)+len(data))
	out = append(out, dir...)
	return append(out, data...)
}

func diskRows() [][]string {
	return [][]string{{
		num(1), num(0), num(0), num(0), num(0), numLeft(100 << 20),
		num(0), numLeft(200 << 20), Label, stamp(Created), stamp(Created), Drive, num(1),
	}}
}

func dirRows(dirs []dirRow) [][]string {
	rows := make([][]string, 0, len(dirs))
	for _, d := range dirs {
		rows = append(rows, []string{num(int64(d.serial)), num(1), num(int64(d.parent)), d.name})
	}
	return rows
}

func fileRows(files []File, dirOf []int) [][]string {
	rows := make([][]string, 0, len(files))
	for i, f := range files {
		rows = append(rows, []string{
			num(int64(i + 1)), num(int64(dirOf[i])), num(4), num(1), num(32),
			num(0), numLeft(int64(len(f.Data))), stamp(f.Modified), f.Name,
		})
	}
	return rows
}

func chunkRows(chunks []chunk, opts Options) [][]string {
	rows := make([][]string, 0, len(chunks))
	for _, c := range chunks {
		var sum int64
		if c.level > 0 && !opts.OmitChecksums {
			sum = int64(int32(Checksum(c.orig))) //nolint:gosec // the catalog stores it as a signed decimal
		}
		if c.serial == opts.BadChecksum {
			sum ^= 0x5A5A5A5A
		}
		var last int64
		if c.last {
			last = 1
		}
		comp := int64(len(c.stored)) - opts.ShortCompSize[c.serial]
		orig := int64(len(c.orig)) + opts.LongOrigSize[c.serial]
		rows = append(rows, []string{
			num(int64(c.serial)), num(int64(c.file)), num(int64(c.sequence)),
			num(orig), num(comp), num(0),
			num(sum), num(int64(c.level)), num(0), numLeft(c.offset), num(last),
		})
	}
	return rows
}

func jobRows(opts Options) [][]string {
	var compressed int64
	if opts.Compress {
		compressed = 1
	}
	return [][]string{{
		num(1), num(JobNumber), num(int64(opts.Disks)), num(3), num(100),
		num(0), num(1), num(compressed), stamp(Created), "", Description,
	}}
}

func textRows() [][]string {
	return [][]string{
		{num(1), num(1), Drive + `\DOS`},
		{num(2), num(2), ""},
	}
}

func sessionRows(sessions []session) [][]string {
	rows := make([][]string, 0, len(sessions))
	for i, s := range sessions {
		rows = append(rows, []string{
			num(int64(i + 1)), num(int64(s.firstChunk)), num(int64(s.lastChunk)),
			num(JobNumber), num(int64(s.disk)), stamp(Created),
		})
	}
	return rows
}
