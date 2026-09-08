package onestep

import (
	"fmt"
	"strings"
	"time"

	"samuelks.com/datarescue/internal/dbf"
)

// Compression levels recorded per chunk. A chunk is stored rather than
// deflated when compressing it did not make it smaller, which is decided per
// chunk and not per file, so one file can hold both kinds. An empty file is
// recorded as a single chunk at LevelEmpty, which holds no bytes at all.
const (
	LevelStored  = -1
	LevelEmpty   = 0
	LevelDeflate = 4
)

// ChunkSize is the amount of original data one chunk holds at most. The writer
// filled a buffer this size and emitted a chunk each time it flushed, so a
// file's chunk count is its size divided by this, rounded up.
const ChunkSize = 65535

// DiskInfo describes one source drive the backup read from.
type DiskInfo struct {
	Serial int
	// Label is the drive's volume name and Letter its drive letter with the
	// colon, as in "C:". Paths are built from Letter; Label is informational.
	Label    string
	Letter   string
	Used     int64
	Free     int64
	Modified time.Time
}

// Job describes the backup as a whole. There is one job record per catalog.
type Job struct {
	Number      int
	Disks       int
	Compressed  bool
	HasPassword bool
	Created     time.Time
	Description string
}

// Session records which chunks were written to one media disk.
type Session struct {
	Disk int
	// FirstChunk and LastChunk are inclusive chunk serials. The reference
	// documentation calls them a buffer read count of unknown meaning; there
	// is one count per chunk, so the range identifies the chunks on this disk.
	FirstChunk int
	LastChunk  int
	Date       time.Time
}

// Selection is one entry from the set of paths the operator chose to back up.
// It does not enumerate subdirectories, so it describes the operator's intent
// rather than the contents; use Files for that.
type Selection struct {
	Path   string
	IsFile bool
}

// Chunk is one stored run of a file's data.
type Chunk struct {
	Serial int
	// File is the serial of the file this chunk belongs to.
	File     int
	Sequence int
	OrigSize int64
	CompSize int64
	// Disk is the media disk of the set holding this chunk's bytes, counted
	// from 1, or 0 when it could not be worked out.
	Disk int
	// Offset is where the chunk starts within its own disk's file. Offsets are
	// per-disk and restart at HeaderSize on each one, so a chunk cannot be
	// located without knowing which disk it is on.
	Offset int64
	// Checksum is the one's complement of the CRC-32 of the chunk's original
	// bytes. It is 0 when none was recorded, which is the case for a stored
	// chunk and for every chunk of a backup whose writer did not record them.
	Checksum uint32
	Level    int
	// Last marks the final chunk of its file.
	Last bool

	// diskIdx indexes Archive.disks, or is -1 when no supplied disk holds this
	// chunk's bytes.
	diskIdx int
}

// Stored reports whether the chunk holds its data uncompressed.
func (c Chunk) Stored() bool { return c.Level == LevelStored }

// HasChecksum reports whether the catalog recorded a checksum for the chunk.
// Not every writer did: a backup can carry 0 in the field throughout, and a
// stored chunk never has one, so 0 means absent rather than a value to check
// against.
func (c Chunk) HasChecksum() bool { return c.Checksum != 0 }

// Empty reports whether the chunk holds nothing, which is how a zero-length
// file is recorded. It has no stored bytes and no checksum, so it comes back
// whether or not its disk was supplied.
func (c Chunk) Empty() bool { return c.Level == LevelEmpty }

// File is one backed-up file.
type File struct {
	Serial int
	// Name is the name as stored and Path the full path rebuilt from the
	// directory table, in the DOS form the backup recorded, as in
	// `C:\DOS\ATTRIB.EXE`.
	Name       string
	Path       string
	Size       int64
	Modified   time.Time
	Attributes uint32
	Status     int
	Chunks     []Chunk

	// dirSer is the directory table row the file sits in, kept so Path can be
	// built after the whole directory table is read.
	dirSer int
}

// Recoverable reports whether every one of the file's chunks is on a disk that
// was supplied.
func (f *File) Recoverable() bool {
	if len(f.Chunks) == 0 {
		// A file with no chunks has nothing to read, which is correct only
		// when the file is empty.
		return f.Size == 0
	}
	for _, c := range f.Chunks {
		if c.diskIdx < 0 && !c.Empty() {
			return false
		}
	}
	return true
}

// tableID identifies the seven tables the catalog holds.
type tableID int

const (
	tblDisk tableID = iota
	tblDir
	tblFile
	tblChunk
	tblJob
	tblText
	tblSession
	numTables
)

// tableSpecs pairs each table with its container member name and its complete
// field list. The member name identifies a table found through the directory;
// the field list identifies one found by scanning, where there is no name.
var tableSpecs = [numTables]struct {
	member string
	fields []string
}{
	tblDisk: {"IOADISK", []string{
		"SERIAL", "NUMDIRS", "NUMFILES", "VLSRDW", "USED_HI", "USED_LO",
		"FREE_HI", "FREE_LO", "LABEL", "VLSRDT", "DATETIME", "DRV_LTR", "HASDSKST",
	}},
	tblDir: {"IOADIR", []string{"SERIAL", "DISKSER", "DIRSER", "NAME"}},
	tblFile: {"IOAFILE", []string{
		"SERIAL", "DIRSER", "STATUS", "DISKSER", "ATTRIB", "SIZE_HI", "SIZE_LO",
		"DATETIME", "NAME",
	}},
	tblChunk: {"IOAARCF", []string{
		"SERIAL", "ORGSER", "SEQUENCE", "ORGSIZE", "COMPSIZE", "ARCDSKSE",
		"CHK_SUM", "COMP_LVL", "OFFS_HI", "OFFS_LO", "IS_LAST",
	}},
	tblJob: {"IOJOB", []string{
		"SERIAL", "JOBNUM", "NUMDISKS", "TGDRV", "TGDRVT", "HASPSW", "ISCUST",
		"ISCOMP", "CUSTDATE", "PASSWORD", "DESCR",
	}},
	tblText: {"IOTEXT", []string{"SERIAL", "DATATYPE", "DATATEXT"}},
	tblSession: {"IOSESS", []string{
		"SERIAL", "SESSFROM", "SESSTO", "JOBNUM", "DISKNUM", "SESSDATE",
	}},
}

func (id tableID) String() string { return tableSpecs[id].member }

// memberTableID returns the table a container member holds, if any. The .CDX
// members are FoxPro indexes over the same rows and carry nothing the restore
// needs.
func memberTableID(name string) (tableID, bool) {
	name = strings.ToUpper(name)
	if !strings.HasSuffix(name, ".DBF") {
		return 0, false
	}
	stem := strings.TrimSuffix(name, ".DBF")
	for id := range numTables {
		if stem == tableSpecs[id].member {
			return id, true
		}
	}
	return 0, false
}

// identifyTable matches a table against the specs by its field names, for the
// scan fallback where the member name is not available. The match must be
// exact: IOADIR and IOAFILE share three field names, so a subset test would
// confuse them.
func identifyTable(t *dbf.Table) (tableID, bool) {
	for id := range numTables {
		want := tableSpecs[id].fields
		if len(t.Fields) != len(want) {
			continue
		}
		if t.HasFields(want...) {
			return id, true
		}
	}
	return 0, false
}

// catalog is the decoded contents of the seven tables.
type catalog struct {
	disks      []DiskInfo
	dirs       []dirRow
	files      []File
	chunks     []Chunk
	job        Job
	selections []Selection
	sessions   []Session
}

type dirRow struct {
	serial   int
	diskSer  int
	parent   int
	name     string
	nameSeen bool // false when the name field was blank, which marks a root
}

// maxRowDiags bounds how many per-row problems one table reports. A table
// over a bad region fails one row per unrecovered sector, and thousands of
// identical lines would crowd out every other finding; a tally after them
// gives the total.
const maxRowDiags = 10

// rows walks a table's data records, skipping the control record at index 0
// and any record dBase has flagged deleted. A record that cannot be read is
// reported and the walk continues, so a bad sector costs its own rows only.
func rows(t *dbf.Table, id tableID, diag func(DiagnosticKind, string, ...any), fn func(dbf.Record) error) {
	if declared, err := controlCount(t); err == nil && declared != t.Len() {
		diag(DiagTableDamaged, "%s: control record says %d rows, table holds %d", id, declared, t.Len())
	}
	if t.DeclaredLen() != t.Len() {
		diag(DiagTableDamaged, "%s: header says %d rows, only %d fit in the member", id, t.DeclaredLen(), t.Len())
	}
	var bad int
	report := func(format string, args ...any) {
		bad++
		if bad <= maxRowDiags {
			diag(DiagTableDamaged, format, args...)
		}
	}
	for i := 1; i < t.Len(); i++ {
		r, err := t.Record(i)
		if err != nil {
			report("%s: row %d unreadable: %s", id, i, err)
			continue
		}
		if r.Deleted() {
			continue
		}
		if err := fn(r); err != nil {
			report("%s: row %d: %s", id, i, err)
		}
	}
	if bad > maxRowDiags {
		diag(DiagTableDamaged, "%s: and %d more bad rows", id, bad-maxRowDiags)
	}
}

// controlCount reads the row count out of a table's control record, which is
// its second field. The rest of that record's numeric fields are -1.
func controlCount(t *dbf.Table) (int, error) {
	if len(t.Fields) < 2 {
		return 0, fmt.Errorf("onestep: table has %d fields", len(t.Fields))
	}
	r, err := t.Record(0)
	if err != nil {
		return 0, err
	}
	n, err := r.Int(t.Fields[1].Name)
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

// readCatalog decodes the tables listed in tables, which maps each table to its
// location. Missing tables are reported; only the file and chunk tables are
// fatal, since without them there is nothing to restore.
func readCatalog(tables map[tableID]*dbf.Table, diag func(DiagnosticKind, string, ...any)) (*catalog, error) {
	c := &catalog{}
	for id := range numTables {
		t, ok := tables[id]
		if !ok {
			diag(DiagTableMissing, "%s is missing from the catalog", id)
			continue
		}
		switch id {
		case tblDisk:
			c.readDisks(t, diag)
		case tblDir:
			c.readDirs(t, diag)
		case tblFile:
			c.readFiles(t, diag)
		case tblChunk:
			c.readChunks(t, diag)
		case tblJob:
			c.readJob(t, diag)
		case tblText:
			c.readSelections(t, diag)
		case tblSession:
			c.readSessions(t, diag)
		}
	}
	if _, ok := tables[tblFile]; !ok {
		return nil, fmt.Errorf("onestep: catalog has no %s table", tblFile)
	}
	if _, ok := tables[tblChunk]; !ok {
		return nil, fmt.Errorf("onestep: catalog has no %s table", tblChunk)
	}
	return c, nil
}

func (c *catalog) readDisks(t *dbf.Table, diag func(DiagnosticKind, string, ...any)) {
	rows(t, tblDisk, diag, func(r dbf.Record) error {
		serial, err := r.Int("SERIAL")
		if err != nil {
			return err
		}
		label, _ := r.String("LABEL")
		letter, _ := r.String("DRV_LTR")
		stamp, _ := r.String("DATETIME")
		c.disks = append(c.disks, DiskInfo{
			Serial:   int(serial),
			Label:    label,
			Letter:   letter,
			Used:     pair64(r, "USED_HI", "USED_LO"),
			Free:     pair64(r, "FREE_HI", "FREE_LO"),
			Modified: parseStamp(stamp),
		})
		return nil
	})
}

func (c *catalog) readDirs(t *dbf.Table, diag func(DiagnosticKind, string, ...any)) {
	rows(t, tblDir, diag, func(r dbf.Record) error {
		serial, err := r.Int("SERIAL")
		if err != nil {
			return err
		}
		diskSer, _ := r.Int("DISKSER")
		parent, _ := r.Int("DIRSER")
		name, _ := r.String("NAME")
		c.dirs = append(c.dirs, dirRow{
			serial:   int(serial),
			diskSer:  int(diskSer),
			parent:   int(parent),
			name:     name,
			nameSeen: name != "",
		})
		return nil
	})
}

func (c *catalog) readFiles(t *dbf.Table, diag func(DiagnosticKind, string, ...any)) {
	rows(t, tblFile, diag, func(r dbf.Record) error {
		serial, err := r.Int("SERIAL")
		if err != nil {
			return err
		}
		name, _ := r.String("NAME")
		stamp, _ := r.String("DATETIME")
		dirSer, _ := r.Int("DIRSER")
		attrib, _ := r.Int("ATTRIB")
		status, _ := r.Int("STATUS")
		c.files = append(c.files, File{
			Serial:     int(serial),
			Name:       name,
			Size:       pair64(r, "SIZE_HI", "SIZE_LO"),
			Modified:   parseStamp(stamp),
			Attributes: uint32(attrib), //nolint:gosec // a DOS attribute word is 16 bits
			Status:     int(status),
			dirSer:     int(dirSer),
		})
		return nil
	})
}

func (c *catalog) readChunks(t *dbf.Table, diag func(DiagnosticKind, string, ...any)) {
	rows(t, tblChunk, diag, func(r dbf.Record) error {
		serial, err := r.Int("SERIAL")
		if err != nil {
			return err
		}
		file, _ := r.Int("ORGSER")
		seq, _ := r.Int("SEQUENCE")
		orig, _ := r.Int("ORGSIZE")
		comp, _ := r.Int("COMPSIZE")
		sum, _ := r.Int("CHK_SUM")
		level, _ := r.Int("COMP_LVL")
		last, _ := r.Int("IS_LAST")
		c.chunks = append(c.chunks, Chunk{
			Serial:   int(serial),
			File:     int(file),
			Sequence: int(seq),
			OrigSize: orig,
			CompSize: comp,
			Offset:   pair64(r, "OFFS_HI", "OFFS_LO"),
			Checksum: uint32(int32(sum)), //nolint:gosec // stored as a signed decimal; the bits are the checksum
			Level:    int(level),
			Last:     last != 0,
			diskIdx:  -1,
		})
		return nil
	})
}

func (c *catalog) readJob(t *dbf.Table, diag func(DiagnosticKind, string, ...any)) {
	var found bool
	rows(t, tblJob, diag, func(r dbf.Record) error {
		if found {
			diag(DiagTableDamaged, "%s holds more than one job record; using the first", tblJob)
			return nil
		}
		found = true
		number, _ := r.Int("JOBNUM")
		disks, _ := r.Int("NUMDISKS")
		compressed, _ := r.Int("ISCOMP")
		password, _ := r.Int("HASPSW")
		stamp, _ := r.String("CUSTDATE")
		descr, _ := r.String("DESCR")
		c.job = Job{
			Number:      int(number),
			Disks:       int(disks),
			Compressed:  compressed != 0,
			HasPassword: password != 0,
			Created:     parseStamp(stamp),
			Description: descr,
		}
		return nil
	})
	if !found {
		diag(DiagTableMissing, "%s holds no job record", tblJob)
	}
}

func (c *catalog) readSelections(t *dbf.Table, diag func(DiagnosticKind, string, ...any)) {
	rows(t, tblText, diag, func(r dbf.Record) error {
		kind, _ := r.Int("DATATYPE")
		text, _ := r.String("DATATEXT")
		if text == "" {
			return nil
		}
		c.selections = append(c.selections, Selection{Path: text, IsFile: kind == 2})
		return nil
	})
}

func (c *catalog) readSessions(t *dbf.Table, diag func(DiagnosticKind, string, ...any)) {
	rows(t, tblSession, diag, func(r dbf.Record) error {
		disk, _ := r.Int("DISKNUM")
		from, _ := r.Int("SESSFROM")
		to, _ := r.Int("SESSTO")
		stamp, _ := r.String("SESSDATE")
		c.sessions = append(c.sessions, Session{
			Disk:       int(disk),
			FirstChunk: int(from),
			LastChunk:  int(to),
			Date:       parseStamp(stamp),
		})
		return nil
	})
}

// pair64 joins a value split across a high and a low field. Every such pair in
// the samples has a zero high half, but the fields exist because the format
// allows a file larger than 4 GiB.
func pair64(r dbf.Record, hi, lo string) int64 {
	h, _ := r.Int(hi)
	l, _ := r.Int(lo)
	if h == 0 {
		return l
	}
	return h<<32 | l&0xffffffff
}

// parseStamp decodes the catalog's 14-character timestamps. They carry no zone,
// having been written by a machine in its own local time, so they are read as
// local.
func parseStamp(s string) time.Time {
	s = strings.TrimSpace(s)
	if len(s) < 14 {
		return time.Time{}
	}
	t, err := time.ParseInLocation("20060102150405", s[:14], time.Local)
	if err != nil {
		return time.Time{}
	}
	return t
}
