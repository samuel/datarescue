// Package testqic builds the synthetic Windows 9x MS Backup archives the qic
// tests read.
//
// It is a normal package rather than a test helper because both qic and
// cmd/qictool need the same archives, and a real one cannot be committed:
// they are somebody's backup, and the format's own damage cases are easier to
// produce by mutating bytes whose meaning is known than by hoping a fixture
// carries them.
//
// It has to model the real layout exactly, since a builder that invents a
// plausible layout of its own will agree with a reader that shares the same
// misunderstanding. In particular the segment stride is the grid and never the
// recorded payload length, a segment header counts the bytes preceding it
// rather than its own, the catalog is stored where the data is compressed, and
// the records run depth first with an empty directory contributing no child
// list at all.
package testqic

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf16"
)

// Layout constants, matching the format.
const (
	SegmentSize = 29696
	VTBLSize    = 128

	segHeadSize = 10
	segPayload  = SegmentSize - segHeadSize
	rawSegFlag  = 0x8000

	fix1Size = 0x47
	fix2Size = 0x17

	datSig  = 0x33CC33CC
	edatSig = 0x66996699
	undef   = 0xFFFFFFFF

	dateFudge = 63072000
)

// Record flags.
const (
	flagSubdir   = 0x01
	flagEmptyDir = 0x02
	flagDirLast  = 0x08
	flagDirEnd   = 0x30
	flagRoot     = 0x40
)

// Fixed properties of every archive this builds.
const (
	Label     = "synthetic qic backup"
	Drive     = "C:"
	VolumeTag = "TESTVOL"
	RootName  = VolumeTag + "(" + Drive + ")"
)

// Created is the volume's timestamp.
var Created = time.Date(1997, time.March, 12, 9, 30, 0, 0, time.UTC)

// Variant is which dialect to write.
type Variant uint8

const (
	Win95 Variant = iota // one volume table record and nothing else
	Win98                // a volume table record followed by an MDID
)

// File is one entry to put in the archive. Dir is the directory holding it,
// with backslashes and no drive letter; intermediate directories are created
// as needed.
type File struct {
	Dir      string
	Name     string
	Short    string
	Data     []byte
	Modified time.Time
	IsDir    bool // an explicit directory, which Empty then marks as childless
	Empty    bool
}

// Path is the file's path inside the archive, as the reader reports it.
func (f File) Path() string {
	if f.Dir == "" {
		return Drive + `\` + f.Name
	}
	return Drive + `\` + f.Dir + `\` + f.Name
}

// Options describes an archive to build. The fields past Files each introduce
// one kind of damage, so a test can choose the damage it exercises.
type Options struct {
	Compress bool
	Variant  Variant
	Files    []File

	// BadSegment corrupts the payload of one data segment, 0-based, so its
	// stream decodes to fewer bytes than its offsets account for.
	BadSegment int
	// LostSegmentHeader zeroes one data segment's header.
	LostSegmentHeader int
	// BadCumSz sets one data segment's recorded offset behind its neighbor.
	BadCumSz int
	// Terminator writes a segment declaring no payload after the data, rather
	// than letting the directory size mark the end.
	Terminator bool
	// TruncateSegments cuts whole segments off the end of the file.
	TruncateSegments int
	// BadRecLen corrupts one catalog record's declared length, 0-based in
	// catalog order.
	BadRecLen int
	// BadPathLen corrupts one catalog record's declared parent path length.
	BadPathLen int
	// DropCatalog fills the catalog region with zeros.
	DropCatalog bool
	// WrongDataSize records a data region size the records disagree with.
	WrongDataSize int64
}

// DefaultFiles is a set covering the cases that have caught mistakes: a file
// in the drive root, one nested three deep, a zero-length file, an empty
// directory, a file spanning several segments, and one that will not compress
// so a compressed archive still holds a segment near its raw size.
func DefaultFiles() []File {
	return []File{
		{Name: "README.TXT", Data: []byte("recovered from a synthetic archive\r\n")},
		{Name: "EMPTY.DAT", Data: nil},
		{Dir: "DOS", Name: "ATTRIB.EXE", Data: repeat("MZ\x90\x00", 900)},
		{Dir: "DOS", Name: "COMMAND.COM", Data: repeat("command interpreter ", 300)},
		{Dir: `SRC\LIB\NET`, Name: "socket.c", Data: repeat("int main(void) { return 0; }\n", 500)},
		{Dir: "SRC", Name: "BIG.BIN", Data: repeat("0123456789abcdef", 6000)},
		{Dir: "SRC", Name: "NOISE.BIN", Data: noise(80000)},
		{Dir: "TEMP", Name: "SCRATCH", IsDir: true, Empty: true},
	}
}

func repeat(s string, n int) []byte { return []byte(strings.Repeat(s, n)) }

// noise is data no compressor can shrink, so a segment holding it is written
// at nearly its full payload and the reader meets a full segment rather than
// only ones the match encoder emptied.
func noise(n int) []byte {
	b := make([]byte, n)
	// A xorshift sequence: deterministic, and dense enough that a match
	// encoder finds nothing in it.
	x := uint32(0x9E3779B9)
	for i := range b {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		b[i] = byte(x & 0xff)
	}
	return b
}

// node is one entry of the tree being built.
type node struct {
	name     string
	short    string
	file     *File
	dir      bool
	empty    bool
	children []*node
	path     []string
}

// Build assembles one archive.
func Build(opts Options) []byte {
	if len(opts.Files) == 0 {
		opts.Files = DefaultFiles()
	}
	root := buildTree(opts.Files)
	order := emitOrder(root)

	data := buildData(order, opts)
	catalog := buildCatalog(order, opts)

	dataSegs := segmentize(data, opts, len(catalog))
	catSegs := storedSegments(catalog, int64(len(data)), opts.Compress)

	dataSize := int64(len(data))
	if opts.WrongDataSize != 0 {
		dataSize = opts.WrongDataSize
	}

	out := header(opts, dataSize, int64(len(catalog)), (len(dataSegs)+len(catSegs))/SegmentSize)
	out = append(out, dataSegs...)
	out = append(out, catSegs...)

	if opts.TruncateSegments > 0 {
		cut := int64(opts.TruncateSegments) * SegmentSize
		if cut < int64(len(out)) {
			out = out[:int64(len(out))-cut]
		}
	}
	return out
}

// Write puts an archive in dir and returns its path.
func Write(dir string, opts Options) (string, error) {
	path := filepath.Join(dir, "backup.qic")
	if err := os.WriteFile(path, Build(opts), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// buildTree turns the flat file list into the directory tree the catalog
// describes, creating intermediate directories as paths refer to them.
func buildTree(files []File) *node {
	root := &node{name: RootName, dir: true}
	dirs := map[string]*node{"": root}

	find := func(path string) *node {
		if n, ok := dirs[path]; ok {
			return n
		}
		var cur = root
		var built string
		for part := range strings.SplitSeq(path, `\`) {
			if built == "" {
				built = part
			} else {
				built += `\` + part
			}
			next, ok := dirs[built]
			if !ok {
				next = &node{name: part, short: strings.ToUpper(part), dir: true, path: append(append([]string{}, cur.path...), part)}
				dirs[built] = next
				cur.children = append(cur.children, next)
			}
			cur = next
		}
		return cur
	}

	for i := range files {
		f := &files[i]
		parent := find(f.Dir)
		short := f.Short
		if short == "" {
			short = strings.ToUpper(f.Name)
		}
		n := &node{
			name:  f.Name,
			short: short,
			file:  f,
			dir:   f.IsDir,
			empty: f.Empty,
			path:  append(append([]string{}, parent.path...), f.Name),
		}
		if f.IsDir {
			short := strings.ToUpper(f.Name)
			n.short = short
		}
		parent.children = append(parent.children, n)
	}
	return root
}

// emitOrder walks the tree the way the catalog records it: a directory's whole
// child list at once, then the child lists of those children in order, depth
// first. An empty directory contributes no list.
func emitOrder(root *node) []*node {
	order := []*node{root}
	stack := []*node{root}
	for len(stack) > 0 {
		dir := stack[0]
		stack = stack[1:]
		if len(dir.children) == 0 {
			continue
		}
		order = append(order, dir.children...)

		var subs []*node
		for _, c := range dir.children {
			if c.dir && !c.empty && len(c.children) > 0 {
				subs = append(subs, c)
			}
		}
		stack = append(subs, stack...)
	}
	return order
}

// flagFor is the record's flag byte. last marks the end of the child list it
// belongs to, and end the last record of the catalog.
func flagFor(n *node, root, last, end bool) uint8 {
	var flag uint8
	if n.dir {
		flag |= flagSubdir
		if n.empty || len(n.children) == 0 {
			flag |= flagEmptyDir
		}
	}
	if root {
		flag |= flagRoot | flagDirLast | flagSubdir
		flag &^= flagEmptyDir
	}
	if last {
		flag |= flagDirLast
	}
	if end {
		flag |= flagDirEnd
	}
	return flag
}

// childListEnds marks which records close the child list they sit in, which
// the reader rebuilds the tree from.
func childListEnds(order []*node) map[int]bool {
	ends := map[int]bool{}
	byNode := map[*node]int{}
	for i, n := range order {
		byNode[n] = i
	}
	for _, n := range order {
		if len(n.children) == 0 {
			continue
		}
		last := n.children[len(n.children)-1]
		ends[byNode[last]] = true
	}
	return ends
}

// fixed1 writes the first fixed part of a record. inData decides whether the
// size field carries the file's length, which only the catalog records, and
// whether the index words are the unused pattern the data region holds.
func fixed1(n *node, flag uint8, pathBytes, nameLen, shortLen int, inData bool) []byte {
	b := make([]byte, fix1Size)
	size := fix1Size + nameLen + fix2Size + shortLen
	binary.LittleEndian.PutUint16(b[0x00:], uint16((size-2)&0xffff))
	if inData {
		binary.LittleEndian.PutUint32(b[0x02:], undef)
		binary.LittleEndian.PutUint32(b[0x06:], undef)
	}
	binary.LittleEndian.PutUint16(b[0x0A:], uint16(pathBytes&0xffff))
	binary.LittleEndian.PutUint16(b[0x0C:], 0x000A)
	b[0x0E] = flag
	binary.LittleEndian.PutUint16(b[0x0F:], 0x0007)

	length := uint32(undef)
	if !inData {
		length = 0
		if n.file != nil && !n.dir {
			length = uint32(len(n.file.Data) & 0xffffffff)
		}
	}
	binary.LittleEndian.PutUint32(b[0x11:], length)

	b[0x29] = 0x20 // archive
	if n.dir {
		b[0x29] = 0x10
	}

	mod := Created
	if n.file != nil && !n.file.Modified.IsZero() {
		mod = n.file.Modified
	}
	secs := uint32(mod.Unix() & 0xffffffff)
	binary.LittleEndian.PutUint32(b[0x2D:], secs)
	binary.LittleEndian.PutUint32(b[0x31:], undef)
	binary.LittleEndian.PutUint32(b[0x35:], secs)
	binary.LittleEndian.PutUint32(b[0x39:], undef)
	binary.LittleEndian.PutUint32(b[0x3D:], secs)
	binary.LittleEndian.PutUint32(b[0x41:], undef)
	binary.LittleEndian.PutUint16(b[0x45:], uint16(nameLen&0xffff))
	return b
}

func fixed2(shortLen int) []byte {
	b := make([]byte, fix2Size)
	binary.LittleEndian.PutUint16(b[0x15:], uint16(shortLen&0xffff))
	return b
}

// encodeUTF16 writes a name the way the format stores it, with no terminator.
func encodeUTF16(s string) []byte {
	units := utf16.Encode([]rune(s))
	b := make([]byte, 0, len(units)*2)
	for _, u := range units {
		b = append(b, byte(u&0xff), byte(u>>8&0xff))
	}
	return b
}

// encodePath writes a parent path: a separator unit ahead of the first
// component and two between each pair after it.
func encodePath(parts []string) []byte {
	if len(parts) == 0 {
		return nil
	}
	var b []byte
	for i, p := range parts {
		b = append(b, 0, 0)
		if i > 0 {
			b = append(b, 0, 0)
		}
		b = append(b, encodeUTF16(p)...)
	}
	return b
}

// parentOf is the path of the directory holding the node, which is its own
// path less its name.
func parentOf(n *node) []string {
	if len(n.path) == 0 {
		return nil
	}
	return n.path[:len(n.path)-1]
}

// buildData writes the data region: each record repeated inline, with the
// file's bytes after it, bracketed by the signatures the format puts there.
func buildData(order []*node, opts Options) []byte {
	ends := childListEnds(order)
	var out []byte
	for i, n := range order {
		parent := encodePath(parentOf(n))
		name := encodeUTF16(n.name)
		short := encodeUTF16(n.short)
		flag := flagFor(n, i == 0, ends[i], i == len(order)-1)

		out = binary.LittleEndian.AppendUint32(out, datSig)
		out = append(out, fixed1(n, flag, len(parent), len(name), len(short), true)...)
		out = append(out, name...)
		out = append(out, fixed2(len(short))...)
		out = append(out, short...)
		out = append(out, parent...)

		if n.dir {
			for range dirTrailers(opts.Variant) {
				out = trailer(out)
			}
			continue
		}
		out = trailer(out)
		if n.file != nil {
			out = append(out, n.file.Data...)
		}
		for range fileTrailers(opts.Variant) {
			out = trailer(out)
		}
	}
	return out
}

func trailer(out []byte) []byte {
	out = binary.LittleEndian.AppendUint32(out, edatSig)
	return binary.LittleEndian.AppendUint16(out, 0x0007)
}

func dirTrailers(v Variant) int {
	if v == Win98 {
		return 1
	}
	return 3
}

func fileTrailers(v Variant) int {
	if v == Win98 {
		return 0
	}
	return 2
}

// buildCatalog writes the directory: the same records without the signatures,
// the path bytes or the data, and with the file sizes the data region omits.
func buildCatalog(order []*node, opts Options) []byte {
	ends := childListEnds(order)
	var out []byte
	for i, n := range order {
		parent := encodePath(parentOf(n))
		name := encodeUTF16(n.name)
		short := encodeUTF16(n.short)
		flag := flagFor(n, i == 0, ends[i], i == len(order)-1)

		rec := fixed1(n, flag, len(parent), len(name), len(short), false)
		if i == opts.BadRecLen && opts.BadRecLen > 0 {
			binary.LittleEndian.PutUint16(rec[0x00:], 0x1234)
		}
		if i == opts.BadPathLen && opts.BadPathLen > 0 {
			binary.LittleEndian.PutUint16(rec[0x0A:], uint16((len(parent)+8)&0xffff))
		}
		out = append(out, rec...)
		out = append(out, name...)
		out = append(out, fixed2(len(short))...)
		out = append(out, short...)
	}
	if opts.DropCatalog {
		for i := range out {
			out[i] = 0
		}
	}
	return out
}

// segmentize cuts the data region into segments. A compressed archive fills
// each one and records the bytes preceding it; an uncompressed archive has no
// segment headers at all.
func segmentize(data []byte, opts Options, catalogLen int) []byte {
	if !opts.Compress {
		out := make([]byte, 0, len(data)+SegmentSize)
		out = append(out, data...)
		if pad := len(out) % SegmentSize; pad != 0 {
			out = append(out, make([]byte, SegmentSize-pad)...)
		}
		return out
	}

	var out []byte
	var cum int64
	var index int
	for pos := 0; pos < len(data); index++ {
		payload, used := Compress(data[pos:], segPayload)
		if used == 0 {
			// Nothing fit, which a sound archive never produces; stop rather
			// than loop.
			break
		}
		out = appendSegment(out, cum, payload, false, opts, index)
		cum += int64(used)
		pos += used
	}
	if opts.Terminator {
		out = appendSegment(out, cum, nil, false, opts, index)
	}
	return out
}

// storedSegments writes the catalog. It is never compressed, yet in a
// compressed archive it still carries a segment header on every segment, and
// the count in it continues past the end of the data region.
func storedSegments(catalog []byte, dataLen int64, compress bool) []byte {
	if !compress {
		out := append([]byte{}, catalog...)
		if pad := len(out) % SegmentSize; pad != 0 {
			out = append(out, make([]byte, SegmentSize-pad)...)
		}
		return out
	}
	var out []byte
	cum := dataLen
	for pos := 0; pos < len(catalog); pos += segPayload {
		end := min(pos+segPayload, len(catalog))
		chunk := catalog[pos:end]
		out = appendSegment(out, cum, chunk, true, Options{}, -1)
		cum += int64(len(chunk))
	}
	return out
}

// appendSegment writes one segment: the ten-byte header, the payload, and zero
// padding out to the grid, which is the stride whatever the payload length is.
func appendSegment(out []byte, cum int64, payload []byte, raw bool, opts Options, index int) []byte {
	head := make([]byte, segHeadSize)
	binary.LittleEndian.PutUint32(head[0:], uint32(cum&0xffffffff))
	size := uint16(len(payload) & 0xffff)
	if raw {
		size |= rawSegFlag
	}
	binary.LittleEndian.PutUint16(head[8:], size)

	if index >= 0 && index == opts.BadCumSz && opts.BadCumSz > 0 {
		binary.LittleEndian.PutUint32(head[0:], uint32(max(cum-SegmentSize, 0)&0xffffffff))
	}
	if index >= 0 && index == opts.LostSegmentHeader && opts.LostSegmentHeader > 0 {
		head = make([]byte, segHeadSize)
	}

	seg := make([]byte, SegmentSize)
	copy(seg, head)
	copy(seg[segHeadSize:], payload)
	if index >= 0 && index == opts.BadSegment && opts.BadSegment > 0 && len(payload) > 64 {
		// Flip bits part way in, which desynchronizes the bit stream from
		// there on the way a bad sector does.
		for i := segHeadSize + 32; i < segHeadSize+48; i++ {
			seg[i] ^= 0xFF
		}
	}
	return append(out, seg...)
}

// header writes the volume table, and the MDID record a Windows 98 or ME
// archive follows it with.
func header(opts Options, dataSize, dirSize int64, segments int) []byte {
	b := make([]byte, 0, 2*VTBLSize)
	b = append(b, vtbl(opts, dataSize, dirSize, segments)...)
	if opts.Variant == Win98 {
		b = append(b, mdid()...)
	}
	return b
}

func vtbl(opts Options, dataSize, dirSize int64, segments int) []byte {
	b := make([]byte, VTBLSize)
	copy(b[0:4], "VTBL")
	// The field the format calls a segment count is two 16-bit QFA block
	// numbers, the first and last of the volume.
	binary.LittleEndian.PutUint16(b[4:], 3)
	binary.LittleEndian.PutUint16(b[6:], uint16((3+segments-1)&0xffff))
	copy(b[8:52], fmt.Sprintf("%-44s", Label))
	binary.LittleEndian.PutUint32(b[52:], uint32((Created.Unix()+dateFudge)&0xffffffff))
	b[56] = 0x24 // written without verification, directory follows data
	b[57] = 1
	binary.LittleEndian.PutUint16(b[58:], 113)
	binary.LittleEndian.PutUint16(b[60:], 6)
	copy(b[62:76], "CMS")
	binary.LittleEndian.PutUint32(b[76:], 256)
	binary.LittleEndian.PutUint32(b[92:], uint32(dirSize&0xffffffff))
	binary.LittleEndian.PutUint32(b[96:], uint32(dataSize&0xffffffff))
	b[122] = 0xC0
	if opts.Compress {
		b[124] = 0x81 // compressed, QIC-122
	}
	b[125] = 7 // Windows 95

	if opts.Variant == Win98 {
		binary.LittleEndian.PutUint16(b[58:], 0x5341)
		binary.LittleEndian.PutUint16(b[60:], 0x49)
		copy(b[106:122], VolumeTag)
		b[122] = 3 // drive C
	}
	return b
}

func mdid() []byte {
	b := make([]byte, VTBLSize)
	copy(b[0:4], "MDID")
	pos := 4
	for _, kv := range []string{
		"MediumID4573813190085068774",
		"VR0100",
		"CSB6D7",
		"FM2",
		"UL" + VolumeTag,
		fmt.Sprintf("DT%08X", uint32(Created.Unix()&0xffffffff)),
	} {
		copy(b[pos:], kv)
		pos += len(kv)
		b[pos] = 0xB0
		pos++
	}
	return b
}

func (v Variant) String() string {
	if v == Win98 {
		return "win98"
	}
	return "win95"
}
