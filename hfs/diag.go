package hfs

import (
	"errors"
	"fmt"
)

// DiagnosticKind groups near-identical findings so a report can cap them: one
// bad region of the medium produces one finding per file it affects, and
// thousands of near-identical lines bury every other finding.
type DiagnosticKind string

const (
	// DiagAlternateMDB reports that the volume was opened from its spare master
	// directory block because the primary would not read.
	DiagAlternateMDB DiagnosticKind = "alternate_mdb"
	// DiagMDBMismatch reports that the two master directory blocks disagree, or
	// that the spare could not be read to check.
	DiagMDBMismatch DiagnosticKind = "mdb_mismatch"
	// DiagAllocationAreaTruncated reports a master directory block whose
	// allocation area extends past the volume's or partition's own end; the
	// block count is clamped to what the volume holds.
	DiagAllocationAreaTruncated DiagnosticKind = "allocation_area_truncated"
	// DiagNoExtentsOverflow reports a volume with no extents overflow file,
	// which is normal and only matters when a fork needs one.
	DiagNoExtentsOverflow DiagnosticKind = "no_extents_overflow"
	// DiagOverflowTruncated reports that the extents overflow file did not
	// parse in full, so records that exist on disk were never loaded.
	DiagOverflowTruncated DiagnosticKind = "extents_overflow_truncated"
	// DiagExtentOutOfRange reports an extent referring to allocation blocks the
	// volume does not have.
	DiagExtentOutOfRange DiagnosticKind = "extent_out_of_range"
	// DiagForkTailLost reports a fork whose extents cover less than its
	// recorded length, with no overflow record to say where the rest is.
	DiagForkTailLost DiagnosticKind = "fork_tail_lost"
	// DiagForkSizeAbsurd reports a fork longer than the volume can hold.
	DiagForkSizeAbsurd DiagnosticKind = "fork_size_absurd"
	// DiagCatalog reports damage found walking the catalog.
	DiagCatalog DiagnosticKind = "catalog"
)

// Diagnostic is something the volume noticed. Problems are returned as data
// attributed to what produced them, rather than written to a log a report would
// have to be scraped out of.
type Diagnostic struct {
	Kind DiagnosticKind
	// CNID is the file or folder the finding belongs to, 0 when it is the
	// volume's own.
	CNID    uint32
	Message string
}

func (d Diagnostic) String() string { return d.Message }

// diag records a finding.
func (v *Volume) diag(kind DiagnosticKind, cnid uint32, format string, args ...any) {
	v.diags = append(v.diags, Diagnostic{Kind: kind, CNID: cnid, Message: fmt.Sprintf(format, args...)})
}

// Diagnostics returns the problems noticed so far and clears the list, so a
// caller can attribute them to the operation that produced them. One left
// pending is reported against whichever file is read next.
func (v *Volume) Diagnostics() []Diagnostic {
	d := v.diags
	v.diags = nil
	return d
}

// EmbeddedSignature is drEmbedSigWord for an HFS volume that is only a wrapper
// around an embedded HFS+ one.
const EmbeddedSignature uint16 = 0x482B // 'H+'

// ErrEmbeddedHFSPlus reports an HFS wrapper around an HFS+ volume.
var ErrEmbeddedHFSPlus = errors.New("hfs: this is an HFS wrapper around an HFS+ volume, which this package does not read")

// EmbeddedHFSPlusError says where the embedded volume is, in bytes from the
// front of the reader the wrapper was opened over, so an operator — or a later
// HFS+ reader — can go straight to it.
type EmbeddedHFSPlusError struct {
	Offset, Size int64
}

func (e *EmbeddedHFSPlusError) Error() string {
	return fmt.Sprintf("%s; the embedded volume is %d bytes at byte %d", ErrEmbeddedHFSPlus, e.Size, e.Offset)
}

// Is reports a match against ErrEmbeddedHFSPlus, so a caller can test for the
// condition without reaching for the type.
func (e *EmbeddedHFSPlusError) Is(target error) bool { return target == ErrEmbeddedHFSPlus }

// embedded is where an embedded HFS+ volume lives inside this wrapper. The
// extent descriptor is packed into a uint32: the start block in the high half
// and the block count in the low, both in the wrapper's allocation blocks.
func (v *Volume) embedded() (offset, size int64) {
	start := uint16(v.MDB.EmbeddedVolumeExtentDescriptor >> 16)
	count := int64(v.MDB.EmbeddedVolumeExtentDescriptor & 0xFFFF)
	return v.AllocBlockOffset(start), count * v.blockBytes()
}

// Embedded returns where an embedded HFS+ volume lives inside this wrapper, for
// a caller that deliberately wants the wrapper rather than the volume. Open
// refuses such a disk, so this is reachable only through a caller that went
// looking.
func (v *Volume) Embedded() (offset, size int64, ok bool) {
	if v.MDB.EmbeddedVolumeSignature != EmbeddedSignature {
		return 0, 0, false
	}
	off, n := v.embedded()
	return off, n, true
}
