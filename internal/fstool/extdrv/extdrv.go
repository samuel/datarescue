// Package extdrv reads ext2 and ext3 volumes for the filesystem tool.
package extdrv

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"samuelks.com/datarescue/diskimage"
	"samuelks.com/datarescue/extfs"
	"samuelks.com/datarescue/internal/fstool"
	"samuelks.com/datarescue/internal/humanize"
	"samuelks.com/datarescue/mbr"
)

// Driver reads ext2/ext3.
type Driver struct{}

// New returns the ext driver.
func New() *Driver { return &Driver{} }

func (*Driver) Name() string    { return "ext" }
func (*Driver) Summary() string { return "ext2/ext3" }

// Flags registers nothing: everything ext2 recovery needs is already a shared
// flag. The method exists because the Driver interface requires it.
func (*Driver) Flags(*fstool.FlagSet) {}

// Probe reports whether an ext2 superblock sits at off.
//
// The three answers are kept apart. No superblock means "some other format". A
// failed read means the bytes needed to decide were never recovered, which is
// the case the rescue map exists for and must not be reported as the first.
func (*Driver) Probe(r io.ReadSeeker, off int64) fstool.Verdict {
	fs, err := extfs.Open(r, off)
	switch {
	case err == nil:
		return fstool.Recognized(fs)
	case errors.Is(err, extfs.ErrNotExt2):
		return fstool.Rejected()
	}
	return fstool.Unrecovered(err)
}

// PartitionMatches narrows auto-detection to the types an ext2 volume is found
// under. Anything not obviously FAT is worth probing: the type byte is only a
// hint the formatter wrote, and the probe decides.
func (*Driver) PartitionMatches(t byte) bool {
	return t == 0x83 || t == 0x8E || !mbr.IsFAT(t)
}

// Open opens the volume. hint is the *extfs.FS the probe already built, so the
// superblock and descriptor table are not read a second time.
func (*Driver) Open(im *diskimage.Image, hint any) (fstool.Volume, error) {
	if fs, ok := hint.(*extfs.FS); ok && fs != nil {
		return &volume{fs: fs, im: im}, nil
	}
	fs, err := extfs.Open(im.Reader, im.Offset)
	if err != nil {
		return nil, err
	}
	return &volume{fs: fs, im: im}, nil
}

type volume struct {
	fs *extfs.FS
	im *diskimage.Image
	// drained is how many of the filesystem's diagnostics have been reported.
	// extfs accumulates rather than clearing, so the drain is a position.
	drained int
}

// Bytes is the volume's own length, from the superblock rather than from the
// image, since on a partitioned image the two differ.
func (v *volume) Bytes() int64 {
	return int64(v.fs.Superblock().BlocksCount) * int64(v.fs.BlockSize())
}

func (v *volume) Close() error { return nil }

// key is what Open and Damage need to reach the inode again without resolving
// the path a second time.
type key struct {
	ino *extfs.Inode
}

func (v *volume) Walk(fn func(fstool.Entry) error) ([]fstool.Diagnostic, error) {
	issues, err := v.fs.Walk(func(path string, _ *extfs.DirEntry, ino *extfs.Inode) error {
		return fn(entryFor(path, ino))
	})
	diags := make([]fstool.Diagnostic, 0, len(issues))
	for _, wi := range issues {
		diags = append(diags, fstool.Diagnostic{
			Path: wi.Path, Kind: "walk", Damage: true, Text: wi.Err.Error(),
		})
	}
	return diags, err
}

func entryFor(path string, ino *extfs.Inode) fstool.Entry {
	e := fstool.Entry{
		Path:    strings.TrimPrefix(path, "/"),
		Dir:     ino.IsDir(),
		Size:    ino.Size,
		ModTime: ino.MTime,
		Mode:    modeString(ino),
		Key:     key{ino: ino},
	}
	switch {
	case ino.IsDir():
		e.Size = 0
	case ino.IsSymlink():
		// Reported but never recreated, so an image cannot place a link the
		// operator did not choose. The target is not resolved here, since that
		// would read the medium, which the listing has not asked for.
		e.Symlink = "…"
	case !ino.IsRegular():
		e.Special = true
	}
	return e
}

// Lookup has no index to use: ext2 resolves a path by walking the directory
// tree, which the shared selection already does. Returning ErrNoLookup keeps
// one code path rather than a second, slower copy of the same walk.
func (v *volume) Lookup(string) (fstool.Entry, error) {
	return fstool.Entry{}, fstool.ErrNoLookup
}

func (v *volume) Open(e fstool.Entry) (fstool.Reader, error) {
	k, ok := e.Key.(key)
	if !ok {
		return nil, fmt.Errorf("extdrv: %s has no inode", e.Path)
	}
	return v.fs.ReadInodeData(k.ino)
}

// Damage reports what the rescue map says the entry lost, without reading it.
//
// ext2 keeps a file's block addresses in the inode and in pointer blocks, so an
// exact answer would mean resolving the whole block list. That reads the
// medium, and this method must report damage before anything has been read. It
// reports nothing rather than guessing.
func (v *volume) Damage(fstool.Entry) (int64, int) { return 0, 0 }

// Diagnostics drains what the filesystem noticed. extfs accumulates its
// diagnostics rather than clearing them, so the drain is tracked here and each
// is reported once.
func (v *volume) Diagnostics() []fstool.Diagnostic {
	all := v.fs.Diagnostics()
	if v.drained >= len(all) {
		return nil
	}
	fresh := all[v.drained:]
	v.drained = len(all)
	out := make([]fstool.Diagnostic, 0, len(fresh))
	for _, d := range fresh {
		out = append(out, fstool.Diagnostic{Kind: "open", Text: d})
	}
	return out
}

func (v *volume) Info(w io.Writer) error {
	sb := v.fs.Superblock()
	fmt.Fprintf(w, "Volume name: %s\n", sb.VolumeName)             //nolint:errcheck // report output
	fmt.Fprintf(w, "Block size:  %d\n", v.fs.BlockSize())          //nolint:errcheck // report output
	fmt.Fprintf(w, "Blocks:      %d\n", sb.BlocksCount)            //nolint:errcheck // report output
	fmt.Fprintf(w, "Free blocks: %d\n", sb.FreeBlocksCount)        //nolint:errcheck // report output
	fmt.Fprintf(w, "Inodes:      %d\n", sb.InodesCount)            //nolint:errcheck // report output
	fmt.Fprintf(w, "Free inodes: %d\n", sb.FreeInodesCount)        //nolint:errcheck // report output
	fmt.Fprintf(w, "Size:        %s\n", humanize.Bytes(v.Bytes())) //nolint:errcheck // report output
	if sb.BackupGroup != 0 {
		fmt.Fprintf(w, "Superblock:  backup copy from group %d\n", sb.BackupGroup) //nolint:errcheck // report output
	}
	if sb.Mtime != 0 {
		fmt.Fprintf(w, "Last mounted: %s\n", time.Unix(int64(sb.Mtime), 0).Format(time.DateTime)) //nolint:errcheck // report output
	}
	return nil
}

// Verify adds ext2's own structural check to the shared walk.
func (v *volume) Verify(rep *fstool.Report, _ bool) {
	res, err := v.fs.ValidateFS()
	if err != nil {
		rep.Errorf("structural check: %s", err)
		return
	}
	for _, is := range res.Issues {
		rep.Warnf("%-18s inode %-8d %s", is.Kind, is.InodeNum, is.Detail)
	}
	rep.Printf("Inodes:      %d total, %d readable", res.TotalInodes, res.ReadableInodes)
	rep.Printf("Readable:    %d files", res.ReadableFiles)
}

// modeString renders an inode's mode as ls does.
func modeString(ino *extfs.Inode) string {
	var b strings.Builder
	switch {
	case ino.IsDir():
		b.WriteByte('d')
	case ino.IsSymlink():
		b.WriteByte('l')
	default:
		b.WriteByte('-')
	}
	for i := 8; i >= 0; i-- {
		if ino.Mode&(1<<i) != 0 {
			b.WriteByte("rwx"[(8-i)%3])
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}
