// Package test1step builds deterministic Iomega 1-Step Backup archives for the
// tests to read.
//
// It is a normal package rather than a test helper because both the onestep
// tests and the 1steptool tests need the same archive, and a real one cannot
// be committed: the archives that drove this work carry Microsoft system files.
// Building one also means the damage cases can be produced by mutating known
// bytes instead of hoping a fixture happens to contain them.
package test1step

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Constants the tests assert against.
const (
	HeaderSize = 0x200
	ChunkSize  = 65535
	Drive      = "C:"
	Label      = "TESTVOL"
	JobNumber  = 7
	// Description is the text in the file header and the job record.
	Description = "synthetic 1-Step backup"
)

// Created is the set's timestamp, stored in every disk header.
var Created = time.Date(2001, time.February, 3, 4, 5, 6, 0, time.UTC)

// File is one file to put in the archive. Dir is the directory below the drive
// root, empty for the root itself, with backslashes between components.
type File struct {
	Dir      string
	Name     string
	Data     []byte
	Modified time.Time
}

// Path returns the file's full path as the catalog records it.
func (f File) Path() string {
	if f.Dir == "" {
		return Drive + `\` + f.Name
	}
	return Drive + `\` + f.Dir + `\` + f.Name
}

// Options controls what Build produces.
type Options struct {
	// Compress deflates each chunk, storing it verbatim when deflating did not
	// make it smaller, which is what the original writer did.
	Compress bool
	// Disks is how many media disks to spread the data over, at least 1, and
	// at most one per chunk. A disk holds whole chunks and the split falls
	// inside a file, so a file's data has to be read across the boundary.
	Disks int
	// Files defaults to DefaultFiles.
	Files []File
	// BadChecksum, when non-zero, records the wrong checksum for the chunk
	// with that serial, so a reader's verification has something to catch.
	BadChecksum int
	// OmitChecksums records 0 in every CHK_SUM field, which is what the
	// writers that recorded none at all produced.
	OmitChecksums bool
	// Truncate cuts the given number of bytes off the end of a disk's image,
	// keyed by disk number, modelling one that was only partly recovered.
	Truncate map[int]int64
	// ShortCompSize records a COMP_SIZE that many bytes short of the chunk's
	// real stored length, keyed by chunk serial. The bytes themselves are left
	// where they are, modelling a corrupt catalog digit that makes a chunk's
	// stream end cleanly before its recorded original size.
	ShortCompSize map[int]int64
	// LongOrigSize records an ORG_SIZE that many bytes longer than the chunk
	// really holds, keyed by chunk serial: the catalog then claims more for the
	// chunk than the file table gives the whole file.
	LongOrigSize map[int]int64
}

// DefaultFiles returns the file set Build uses when none is given. It covers a
// file in the drive root, one nested two directories deep, one empty, one that
// does not compress so a compressed archive still holds a stored chunk, and
// one large enough that it straddles the boundary however the set is split.
func DefaultFiles() []File {
	return []File{
		{Dir: "", Name: "ROOT.TXT", Data: text("root file ", 200)},
		{Dir: "DOS", Name: "EMPTY.DAT", Data: nil},
		{Dir: "DOS", Name: "ATTRIB.EXE", Data: text("attrib ", 4000)},
		{Dir: "DOS", Name: "BIG.BIN", Data: text("big ", 3*ChunkSize+1234)},
		{Dir: `DOS\SUB`, Name: "NESTED.TXT", Data: text("nested ", 900)},
		{Dir: "DOS", Name: "RANDOM.BIN", Data: incompressible(3000)},
	}
}

func text(seed string, n int) []byte {
	b := make([]byte, 0, n)
	for len(b) < n {
		b = append(b, seed...)
	}
	return b[:n]
}

// incompressible returns bytes deflate cannot shrink, so the writer stores the
// chunk instead.
func incompressible(n int) []byte {
	b := make([]byte, n)
	// A xorshift sequence: deterministic, and dense enough that deflate gives
	// up on it.
	x := uint32(0x9E3779B9)
	for i := range b {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		b[i] = byte(x & 0xff)
	}
	return b
}

// chunk is one stored run of a file's data.
type chunk struct {
	serial   int
	file     int
	sequence int
	orig     []byte
	stored   []byte
	level    int
	last     bool
	disk     int
	offset   int64 // within its own disk's file
}

// Build returns one byte slice per disk of the set, in disk order. The catalog
// goes on the last one.
func Build(opts Options) [][]byte {
	if opts.Files == nil {
		opts.Files = DefaultFiles()
	}
	dirs, dirOf := buildDirs(opts.Files)
	chunks := buildChunks(opts.Files, opts.Compress)
	opts.Disks = min(max(opts.Disks, 1), len(chunks))

	layout(chunks, opts.Disks)
	sessions := buildSessions(chunks, opts.Disks)
	catalog := buildCatalog(opts, dirs, dirOf, chunks, sessions)

	out := make([][]byte, opts.Disks)
	for d := 1; d <= opts.Disks; d++ {
		var part []byte
		for _, c := range chunks {
			if c.disk == d {
				part = append(part, c.stored...)
			}
		}
		last := d == opts.Disks
		var catOffset int64
		if last {
			catOffset = HeaderSize + int64(len(part))
		}
		image := header(d, catOffset, opts.Files)
		image = append(image, part...)
		if last {
			image = append(image, catalog...)
		}
		if cut := opts.Truncate[d]; cut > 0 {
			image = image[:max(int64(len(image))-cut, 0)]
		}
		out[d-1] = image
	}
	return out
}

// layout places the chunks on the disks. A chunk never spans a disk: each
// disk's data runs from the end of its own header to the last chunk that was
// written to it, and the offsets restart at HeaderSize on the next one. The
// boundaries are put inside a file's run of chunks, so reading that file has
// to cross from one disk to the next.
func layout(chunks []chunk, disks int) {
	n := len(chunks)
	disk := 1
	off := int64(HeaderSize)
	for i := range chunks {
		// Round the boundary up so a set of two takes the larger half first,
		// which lands it inside the file with the most chunks.
		if disk < disks && i >= (disk*n+disks-1)/disks {
			disk++
			off = HeaderSize
		}
		chunks[i].disk = disk
		chunks[i].offset = off
		off += int64(len(chunks[i].stored))
	}
}

func header(disk int, catOffset int64, files []File) []byte {
	b := make([]byte, HeaderSize)
	binary.LittleEndian.PutUint32(b, 0xABCDABCD)
	binary.LittleEndian.PutUint32(b[4:], HeaderSize)
	binary.LittleEndian.PutUint32(b[8:], 0x00010002)
	binary.LittleEndian.PutUint64(b[0x0c:], math.Float64bits(oleDays(Created)))
	binary.LittleEndian.PutUint16(b[0x18:], JobNumber)
	binary.LittleEndian.PutUint16(b[0x1a:], uint16(disk&0xffff))
	binary.LittleEndian.PutUint32(b[0x1c:], uint32(catOffset&0xffffffff))
	copy(b[0x34:], Description)
	return b
}

func oleDays(t time.Time) float64 {
	epoch := time.Date(1899, time.December, 30, 0, 0, 0, 0, time.UTC)
	return t.Sub(epoch).Seconds() / (24 * 60 * 60)
}

// dirRow is one row of the directory table. Serial 1 is always the drive root,
// whose name is blank.
type dirRow struct {
	serial int
	parent int
	name   string
}

// buildDirs returns the directory rows and, for each file, the row it sits in.
func buildDirs(files []File) ([]dirRow, []int) {
	rows := []dirRow{{serial: 1, parent: 0, name: ""}}
	serialOf := map[string]int{"": 1}
	dirOf := make([]int, len(files))
	for i, f := range files {
		parent := 1
		var path string
		for part := range strings.SplitSeq(f.Dir, `\`) {
			if part == "" {
				continue
			}
			if path == "" {
				path = part
			} else {
				path += `\` + part
			}
			if s, ok := serialOf[path]; ok {
				parent = s
				continue
			}
			serial := len(rows) + 1
			rows = append(rows, dirRow{serial: serial, parent: parent, name: part})
			serialOf[path] = serial
			parent = serial
		}
		dirOf[i] = parent
	}
	return rows, dirOf
}

func buildChunks(files []File, compress bool) []chunk {
	var out []chunk
	for i, f := range files {
		pieces := split(f.Data)
		for j, piece := range pieces {
			c := chunk{
				serial:   len(out) + 1,
				file:     i + 1,
				sequence: j + 1,
				orig:     piece,
				stored:   piece,
				level:    -1,
				last:     j == len(pieces)-1,
			}
			// An empty file is recorded as one chunk at level 0 holding
			// nothing, rather than as a stored chunk of no bytes.
			if len(piece) == 0 {
				c.level = 0
			}
			if compress && len(piece) > 0 {
				if deflated := deflate(piece); len(deflated) < len(piece) {
					c.stored = deflated
					c.level = 4
				}
			}
			out = append(out, c)
		}
	}
	return out
}

func split(data []byte) [][]byte {
	var out [][]byte
	for len(data) > ChunkSize {
		out = append(out, data[:ChunkSize])
		data = data[ChunkSize:]
	}
	return append(out, data)
}

// deflate produces the raw deflate stream the format stores: no zlib or gzip
// wrapper and no trailing checksum.
func deflate(data []byte) []byte {
	var buf bytes.Buffer
	w, err := flate.NewWriter(&buf, flate.DefaultCompression)
	if err != nil {
		panic(err)
	}
	if _, err := w.Write(data); err != nil {
		panic(err)
	}
	if err := w.Close(); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// Checksum returns the value the catalog records for a chunk: the one's
// complement of the CRC-32 of its original bytes.
func Checksum(data []byte) uint32 { return ^crc32.ChecksumIEEE(data) }

type session struct {
	disk       int
	firstChunk int
	lastChunk  int
}

// buildSessions records the range of chunk serials written to each disk, which
// is the catalog's only map from a chunk to the disk holding it.
func buildSessions(chunks []chunk, disks int) []session {
	out := make([]session, 0, disks)
	for d := 1; d <= disks; d++ {
		s := session{disk: d}
		for _, c := range chunks {
			if c.disk != d {
				continue
			}
			if s.firstChunk == 0 {
				s.firstChunk = c.serial
			}
			s.lastChunk = c.serial
		}
		if s.firstChunk == 0 {
			continue
		}
		out = append(out, s)
	}
	return out
}

// WriteSet writes each disk of a set into dir, using names in the form the
// program itself produced, and returns the paths in disk order.
func WriteSet(dir string, opts Options) ([]string, error) {
	disks := Build(opts)
	paths := make([]string, 0, len(disks))
	for i, data := range disks {
		name := fmt.Sprintf("Backup Job %d, Disk %d, %s.1-Step",
			JobNumber, i+1, Created.Format("06-01-02 15.04.05"))
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			return nil, err
		}
		paths = append(paths, path)
	}
	return paths, nil
}
