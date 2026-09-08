// Package testfat builds the small synthetic FAT16 images the tests read. It
// is a normal package rather than a test helper because both the fat package
// and the fattool tests need the same image, and because a test that pokes a
// hole in an image has to know where the hole lands: Build returns each file's
// byte range, so a ddrescue map can be written against it without asking the
// code under test where anything is.
package testfat

import (
	"encoding/binary"
	"fmt"
	"time"
)

// Geometry of every image built here. The cluster is four sectors, so a hole
// smaller than a cluster exercises the per-sector recovery that a one-sector
// cluster would hide.
const (
	SectorSize   = 512
	ClusterSize  = 4 * SectorSize
	EntrySize    = 32 // one directory entry
	rootEntries  = 512
	reserved     = 1
	numFATs      = 2
	totalSectors = 20480 // 10 MiB, which is enough clusters to be FAT16
	mediaType    = 0xF8
)

// EntriesPerCluster is how many directory entries one cluster holds, which a
// test needs to make a directory span more than one cluster.
const EntriesPerCluster = ClusterSize / EntrySize

// File is one file to place in a directory. Contents are generated from the
// name, so a test can regenerate them without holding a copy.
type File struct {
	Name string // 8.3, upper case, as it is stored
	Size int
}

// Dir is a subdirectory of the root, holding files of its own. Its entries live
// in a cluster chain rather than in the fixed root region, so a test can lose
// one sector in the middle of a multi-cluster directory.
type Dir struct {
	Name  string // 8.3, upper case
	Files []File
}

// Options describes the image to build.
type Options struct {
	// Files go in the root directory.
	Files []File
	// Dirs are subdirectories of the root.
	Dirs []Dir
}

// Placed is where a file's data landed in the image.
type Placed struct {
	File
	// Path is the file's path in the volume: its own name, or "DIR/NAME.EXT"
	// for a file inside a subdirectory.
	Path string
	// Offset is the byte offset of the file's first cluster in the image, and
	// Clusters how many follow it. The clusters are contiguous: files are laid
	// out in order from cluster 2 with no gaps. An empty file has no cluster at
	// all, so both are zero, as is Cluster.
	Offset   int64
	Clusters int
	Cluster  uint16 // first cluster number
}

// PlacedDir is where a subdirectory's entries landed.
type PlacedDir struct {
	Name string
	// Offset is the byte offset of the directory's first cluster, and Clusters
	// how many its entries span.
	Offset   int64
	Clusters int
	Cluster  uint16
}

// Image is a built image and the layout the caller needs to damage it.
type Image struct {
	Data  []byte
	Files []Placed
	Dirs  []PlacedDir
}

// Find returns the placement of a file by its path in the volume.
func (im *Image) Find(path string) Placed {
	for _, p := range im.Files {
		if p.Path == path {
			return p
		}
	}
	panic("testfat: no file at " + path)
}

// FindDir returns the placement of a subdirectory by name.
func (im *Image) FindDir(name string) PlacedDir {
	for _, d := range im.Dirs {
		if d.Name == name {
			return d
		}
	}
	panic("testfat: no directory named " + name)
}

// modTime is the timestamp every entry records, so a listing is reproducible.
var modTime = time.Date(1997, 8, 29, 2, 14, 0, 0, time.UTC)

// Content is what a file of the given name and size holds. No byte is zero, so
// a zero-filled hole in a recovered copy cannot be mistaken for data.
func Content(name string, size int) []byte {
	var h byte
	for i := range len(name) {
		h += name[i]
	}
	b := make([]byte, size)
	for i := range b {
		b[i] = 1 + byte((i+int(h))%255)
	}
	return b
}

// Build lays out an image holding the given files and subdirectories.
func Build(o Options) *Image {
	rootSectors := (rootEntries*EntrySize + SectorSize - 1) / SectorSize
	spc := ClusterSize / SectorSize

	// Size the FAT to the clusters it ends up describing, which depends on the
	// size of the FAT itself.
	spf := 1
	for {
		clusters := (totalSectors - reserved - numFATs*spf - rootSectors) / spc
		need := ((clusters+2)*2 + SectorSize - 1) / SectorSize
		if need <= spf {
			break
		}
		spf = need
	}
	dataClusters := (totalSectors - reserved - numFATs*spf - rootSectors) / spc
	if dataClusters < 4085 {
		panic("testfat: geometry yields a FAT12 volume")
	}

	b := &builder{
		img:     make([]byte, totalSectors*SectorSize),
		entries: make([]uint16, dataClusters+2),
		next:    2,
		out:     &Image{},
	}
	fatOffset := reserved * SectorSize
	rootOffset := fatOffset + numFATs*spf*SectorSize
	b.dataOffset = rootOffset + rootSectors*SectorSize

	writeBootSector(b.img, spf)
	b.entries[0] = 0xFF00 | mediaType
	b.entries[1] = 0xFFFF

	// The root directory's entries go in the fixed region; a subdirectory's own
	// entry goes there too, ahead of the files it holds.
	root := b.img[rootOffset:]
	var slot int
	for _, f := range o.Files {
		b.placeFile(root[slot*EntrySize:], f, f.Name)
		slot++
	}
	for _, d := range o.Dirs {
		b.placeDir(root[slot*EntrySize:], d)
		slot++
	}
	if slot > rootEntries {
		panic("testfat: more root entries than the root directory holds")
	}

	for c := range numFATs {
		base := fatOffset + c*spf*SectorSize
		for i, e := range b.entries {
			binary.LittleEndian.PutUint16(b.img[base+i*2:], e)
		}
	}
	b.out.Data = b.img
	return b.out
}

// builder carries the allocation state while an image is laid out.
type builder struct {
	img        []byte
	entries    []uint16
	dataOffset int
	next       uint16 // the next unallocated cluster
	out        *Image
}

// alloc reserves n clusters as one chain and returns the first cluster number
// and its byte offset. Chains are laid out in order from cluster 2 with no
// gaps, so a caller can compute where anything landed.
func (b *builder) alloc(n int) (uint16, int64) {
	first := b.next
	if int(first)+n > len(b.entries) {
		panic("testfat: the contents do not fit in the volume")
	}
	for c := range n {
		e := first + uint16(c) //nolint:gosec // bounded by the cluster count above
		if c == n-1 {
			b.entries[e] = 0xFFFF
		} else {
			b.entries[e] = e + 1
		}
	}
	b.next = first + uint16(n) //nolint:gosec // bounded by the cluster count above
	return first, int64(b.dataOffset) + int64(first-2)*ClusterSize
}

// placeFile allocates a file's clusters, writes its contents and fills in the
// directory entry at dst.
func (b *builder) placeFile(dst []byte, f File, path string) {
	p := Placed{File: f, Path: path}
	// An empty file is allocated no cluster and its entry's pointer stays 0,
	// which is what a real volume writes. Giving it a cluster would produce a
	// layout no real volume has, and a reader that wrongly streams that cluster
	// would look correct against it.
	if f.Size > 0 {
		p.Clusters = (f.Size + ClusterSize - 1) / ClusterSize
		p.Cluster, p.Offset = b.alloc(p.Clusters)
		copy(b.img[p.Offset:], Content(f.Name, f.Size))
	}
	name, ext := split83(f.Name)
	writeEntry(dst, name, ext, attrArchive, p.Cluster, uint32(f.Size)) //nolint:gosec // test sizes are small
	b.out.Files = append(b.out.Files, p)
}

// placeDir allocates a subdirectory's clusters, writes its "." and ".." entries
// and then its files, and fills in the root entry at dst.
func (b *builder) placeDir(dst []byte, d Dir) {
	// Two entries of its own before the files it holds.
	clusters := (len(d.Files)+2)*EntrySize + ClusterSize - 1
	clusters /= ClusterSize
	first, off := b.alloc(clusters)

	data := b.img[off : off+int64(clusters)*ClusterSize]
	writeEntry(data, ".       ", "   ", attrSubdirectory, first, 0)
	writeEntry(data[EntrySize:], "..      ", "   ", attrSubdirectory, 0, 0)
	for i, f := range d.Files {
		b.placeFile(data[(i+2)*EntrySize:], f, d.Name+"/"+f.Name)
	}

	name, ext := split83(d.Name)
	writeEntry(dst, name, ext, attrSubdirectory, first, 0)
	b.out.Dirs = append(b.out.Dirs, PlacedDir{Name: d.Name, Offset: off, Clusters: clusters, Cluster: first})
}

// Directory entry attribute bytes.
const (
	attrSubdirectory = 0x10
	attrArchive      = 0x20
)

func writeBootSector(img []byte, spf int) {
	copy(img, []byte{0xEB, 0x3C, 0x90})
	copy(img[3:], "datarscu")
	binary.LittleEndian.PutUint16(img[0x0B:], SectorSize)
	img[0x0D] = ClusterSize / SectorSize
	binary.LittleEndian.PutUint16(img[0x0E:], reserved)
	img[0x10] = numFATs
	binary.LittleEndian.PutUint16(img[0x11:], rootEntries)
	binary.LittleEndian.PutUint16(img[0x13:], totalSectors)
	img[0x15] = mediaType
	binary.LittleEndian.PutUint16(img[0x16:], uint16(spf)) //nolint:gosec // spf is sized from the volume above
	binary.LittleEndian.PutUint16(img[0x18:], 32)          // sectors per track
	binary.LittleEndian.PutUint16(img[0x1A:], 2)           // heads
	img[0x24] = 0x80
	img[0x26] = 0x29
	copy(img[0x27:], []byte{0x01, 0x02, 0x03, 0x04})
	copy(img[0x2B:], "DATARESCUE ")
	copy(img[0x36:], "FAT16   ")
	img[510], img[511] = 0x55, 0xAA
}

// writeEntry writes one 8.3 directory entry, with both name fields already
// padded to their fixed widths.
func writeEntry(dst []byte, name, ext string, attr byte, cluster uint16, size uint32) {
	copy(dst[0:8], name)
	copy(dst[8:11], ext)
	dst[0x0B] = attr
	binary.LittleEndian.PutUint16(dst[0x16:], fatTime(modTime))
	binary.LittleEndian.PutUint16(dst[0x18:], fatDate(modTime))
	binary.LittleEndian.PutUint16(dst[0x1A:], cluster)
	binary.LittleEndian.PutUint32(dst[0x1C:], size)
}

// split83 pads a NAME.EXT into the fixed 8 and 3 byte fields.
func split83(s string) (name, ext string) {
	name, ext = s, ""
	for i := range len(s) {
		if s[i] == '.' {
			name, ext = s[:i], s[i+1:]
			break
		}
	}
	if len(name) == 0 || len(name) > 8 || len(ext) > 3 {
		panic(fmt.Sprintf("testfat: %q is not an 8.3 name", s))
	}
	return name + "        "[:8-len(name)], ext + "   "[:3-len(ext)]
}

func fatTime(t time.Time) uint16 {
	return uint16(t.Hour()<<11 | t.Minute()<<5 | t.Second()/2) //nolint:gosec // the fields are in range by construction
}

func fatDate(t time.Time) uint16 {
	return uint16((t.Year()-1980)<<9 | int(t.Month())<<5 | t.Day()) //nolint:gosec // the fields are in range by construction
}
