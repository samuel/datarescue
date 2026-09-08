// Package fatdrv reads FAT12/16/32 volumes for the filesystem tool.
package fatdrv

import (
	"errors"
	"fmt"
	"io"
	"math"
	"strings"

	"samuelks.com/datarescue/diskimage"
	"samuelks.com/datarescue/fat"
	"samuelks.com/datarescue/internal/fstool"
	"samuelks.com/datarescue/internal/humanize"
	"samuelks.com/datarescue/mbr"
)

// Driver reads FAT.
type Driver struct {
	policy     string
	contiguous bool
	deleted    bool
}

// New returns the FAT driver.
func New() *Driver { return &Driver{} }

func (*Driver) Name() string    { return "fat" }
func (*Driver) Summary() string { return "FAT12/16/32" }

// Flags registers the three FAT-specific flags.
func (d *Driver) Flags(fs *fstool.FlagSet) {
	fs.StringVar(&d.policy, "fat-policy", "",
		"which FAT copy to trust when they disagree: strict, copy0, copy1, best-effort")
	fs.BoolVar(&d.contiguous, "contiguous", false,
		"extract: read a file with a broken chain as a straight run of clusters")
	fs.BoolVar(&d.deleted, "deleted", false, "ls: include deleted entries")
}

// Probe reports whether a FAT boot sector sits at off.
//
// A boot sector that reads cleanly but does not describe a FAT volume is some
// other format. One that would not read is the case the rescue map exists for,
// and the two must be reported differently.
func (*Driver) Probe(r io.ReadSeeker, off int64) fstool.Verdict {
	f, err := fat.Open(r, off)
	switch {
	case err == nil:
		return fstool.Recognized(f)
	case errors.Is(err, fat.ErrNotFAT):
		return fstool.Rejected()
	}
	return fstool.Unrecovered(err)
}

// PartitionMatches narrows auto-detection to the FAT partition types.
func (*Driver) PartitionMatches(t byte) bool { return mbr.IsFAT(t) }

// Open opens the volume. hint is the *fat.FAT the probe already built.
func (d *Driver) Open(im *diskimage.Image, hint any) (fstool.Volume, error) {
	f, ok := hint.(*fat.FAT)
	if !ok || f == nil {
		var err error
		if f, err = fat.Open(im.Reader, im.Offset); err != nil {
			return nil, err
		}
	}
	if d.policy != "" {
		p, err := fat.ParseChainPolicy(d.policy)
		if err != nil {
			return nil, err
		}
		f.ChainPolicy = p
	}
	return &volume{f: f, im: im, d: d}, nil
}

type volume struct {
	f  *fat.FAT
	im *diskimage.Image
	d  *Driver
}

// Bytes is the volume's own length, from the boot sector rather than from the
// image, since on a partitioned image the two differ.
func (v *volume) Bytes() int64 {
	return int64(v.f.TotalSectors) * int64(v.f.SectorSize)
}

func (v *volume) Close() error { return nil }

// key carries the directory entry, so Open and Damage reach the cluster chain
// without resolving the path again.
type key struct{ e *fat.DirectoryEntry }

func (v *volume) Walk(fn func(fstool.Entry) error) ([]fstool.Diagnostic, error) {
	issues, err := v.f.Walk(func(path string, e *fat.DirectoryEntry) error {
		return fn(entryFor(path, e))
	})
	diags := make([]fstool.Diagnostic, 0, len(issues))
	for _, wi := range issues {
		diags = append(diags, fstool.Diagnostic{
			Path: wi.Path, Kind: kindOf(wi), Damage: true, Text: wi.Err.Error(),
		})
	}
	return diags, err
}

// kindOf separates a directory that would not read from a problem the reader
// merely noticed while reading it, so similar findings group together.
func kindOf(wi fat.WalkIssue) string {
	if wi.Diag {
		return "diagnostic"
	}
	return "directory"
}

func entryFor(path string, e *fat.DirectoryEntry) fstool.Entry {
	return fstool.Entry{
		Path:    strings.Trim(path, "/"),
		Dir:     e.Subdirectory,
		Size:    int64(e.FileSize),
		ModTime: e.LastModified,
		Mode:    formatAttrs(e),
		Deleted: e.Deleted,
		Key:     key{e: e},
	}
}

// Lookup resolves a path through the directory tree.
//
// It recovers a requested file from a volume whose tree will not enumerate,
// and keeps the two failures apart: fat.ErrNotFound means the volume does not
// hold what was asked for, and anything else is a directory on the way that
// would not read. Asking for a file by path must not turn damage to the medium
// into a failure of the run.
func (v *volume) Lookup(path string) (fstool.Entry, error) {
	e, err := v.resolve(path)
	if err != nil {
		return fstool.Entry{}, err
	}
	return entryFor(path, e), nil
}

// resolve walks to the entry at path. fat.FAT.Open returns a reader rather
// than the entry, and the entry is what carries the size, the date and the
// chain.
func (v *volume) resolve(path string) (*fat.DirectoryEntry, error) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	cluster := uint32(0)
	for i, part := range parts {
		dir, err := v.f.ReadDirectory(cluster, true)
		if err != nil && len(dir) == 0 {
			return nil, err
		}
		var found *fat.DirectoryEntry
		for _, e := range dir {
			if e.VolumeLabel || e.Name == "." || e.Name == ".." {
				continue
			}
			if strings.EqualFold(e.Name, part) || strings.EqualFold(e.ShortName, part) {
				found = e
				break
			}
		}
		if found == nil {
			return nil, fmt.Errorf("%w: %s", fat.ErrNotFound, path)
		}
		if i == len(parts)-1 {
			return found, nil
		}
		if !found.Subdirectory {
			return nil, fmt.Errorf("%w: %s is not a directory", fat.ErrNotFound, part)
		}
		cluster = found.FirstCluster
	}
	return nil, fmt.Errorf("%w: %s", fat.ErrNotFound, path)
}

func (v *volume) Open(e fstool.Entry) (fstool.Reader, error) {
	k, ok := e.Key.(key)
	if !ok {
		return nil, fmt.Errorf("fatdrv: %s has no directory entry", e.Path)
	}
	de := k.e
	// An empty file needs no special case: OpenClusterChain returns an empty
	// reader whatever the entry's first cluster says.
	r, err := v.f.OpenClusterChain(de.FirstCluster, int64(de.FileSize))
	if err == nil {
		return r, nil
	}
	if !v.d.contiguous || de.FirstCluster < 2 || de.FileSize == 0 {
		return nil, err
	}
	// The chain is unusable, but the data may still be contiguous from the
	// first cluster, which is common for a file written once to a freshly
	// formatted volume. Compute in a wide type, since both terms come from
	// the image.
	count := (int64(de.FileSize) + int64(v.f.ClusterSize) - 1) / int64(v.f.ClusterSize)
	if count <= 0 || count > math.MaxUint32 {
		return nil, fmt.Errorf("implausible size %d", de.FileSize)
	}
	//nolint:gosec // count is bounded against MaxUint32 immediately above
	return v.f.OpenContiguousChain(de.FirstCluster, uint32(count), int64(de.FileSize))
}

// Damage is how much of an entry's data lies in ranges the rescue never
// recovered, and in how many separate ranges.
//
// It reads nothing. The cluster chain gives where the file lives and the map
// gives what was recovered, so a bad region of the medium can be attributed to
// the files it affected before anything reads them.
func (v *volume) Damage(e fstool.Entry) (int64, int) {
	k, ok := e.Key.(key)
	if !ok || v.im.MapPath == "" || k.e.FirstCluster < 2 {
		return 0, 0
	}
	size := int64(k.e.FileSize)
	if k.e.Subdirectory {
		size = 0 // a directory declares no size; take the whole chain
	}
	extents, err := v.f.ChainExtents(k.e.FirstCluster, size)
	if err != nil && len(extents) == 0 {
		return 0, 0 // the chain itself is unusable; the read path reports that
	}
	var lost int64
	var ranges int
	for _, ex := range extents {
		for _, b := range v.im.Unrecovered(ex.ImageOffset, ex.Length) {
			lost += b.Size
			ranges++
		}
	}
	return lost, ranges
}

func (v *volume) Diagnostics() []fstool.Diagnostic {
	ds := v.f.Diagnostics()
	out := make([]fstool.Diagnostic, 0, len(ds))
	for _, d := range ds {
		out = append(out, fstool.Diagnostic{Kind: string(d.Kind), Damage: true, Text: d.String()})
	}
	return out
}

// Orphans lists the cluster chains allocated in the FAT that no directory entry
// points to, which is where a deleted or unlinked file's data remains.
func (v *volume) Orphans(w io.Writer, verbose bool) (int, error) {
	orphans, err := v.f.FindOrphans()
	if err != nil {
		return 0, err
	}
	if len(orphans) == 0 {
		fmt.Fprintln(w, "No orphaned cluster chains found.") //nolint:errcheck // report output
		return 0, nil
	}
	fmt.Fprintf(w, "Found %d orphaned cluster chain(s):\n\n", len(orphans)) //nolint:errcheck // report output
	for i, o := range orphans {
		kind := "FILE"
		if o.LooksLikeDir {
			kind = "DIR"
		}
		fmt.Fprintf(w, "[%d] cluster=%-6d  clusters=%-4d  size=%-12s  %s\n", //nolint:errcheck // report output
			i+1, o.StartCluster, len(o.Clusters), humanize.Bytes(o.Size), kind)
		if verbose && o.LooksLikeDir {
			for _, e := range o.Entries {
				if e.Name == "." || e.Name == ".." {
					continue
				}
				fmt.Fprintf(w, "      %s %10d  %s\n", formatAttrs(e), e.FileSize, e.Name) //nolint:errcheck // report output
			}
		}
	}
	return len(orphans), nil
}

// formatAttrs renders the entry's attributes as "drhsaD" — directory,
// read-only, hidden, system, archive, deleted.
func formatAttrs(e *fat.DirectoryEntry) string {
	pick := func(b bool, c byte) byte {
		if b {
			return c
		}
		return '-'
	}
	return string([]byte{
		pick(e.Subdirectory, 'd'),
		pick(e.ReadOnly, 'r'),
		pick(e.Hidden, 'h'),
		pick(e.System, 's'),
		pick(e.Archive, 'a'),
		pick(e.Deleted, 'D'),
	})
}

// formatSerial renders a FAT volume serial number as XXXX-XXXX.
func formatSerial(b [4]byte) string {
	return fmt.Sprintf("%02X%02X-%02X%02X", b[3], b[2], b[1], b[0])
}
