package qic

import (
	"fmt"
	"sync"
)

// DiagnosticKind classifies a non-fatal problem found while reading an archive.
type DiagnosticKind string

const (
	DiagVTBLDamaged         DiagnosticKind = "vtbl_damaged"
	DiagVTBLInconsistent    DiagnosticKind = "vtbl_inconsistent"
	DiagVariantAssumed      DiagnosticKind = "variant_assumed"
	DiagUnknownCompression  DiagnosticKind = "unknown_compression"
	DiagSegmentUnreadable   DiagnosticKind = "segment_unreadable"
	DiagSegmentInconsistent DiagnosticKind = "segment_inconsistent"
	DiagSegmentDecompress   DiagnosticKind = "segment_decompress"
	DiagSegmentShort        DiagnosticKind = "segment_short"
	DiagDataRegionShort     DiagnosticKind = "data_region_short"
	DiagCatalogUnreadable   DiagnosticKind = "catalog_unreadable"
	DiagCatalogTruncated    DiagnosticKind = "catalog_truncated"
	DiagRecordDamaged       DiagnosticKind = "record_damaged"
	DiagRecordResync        DiagnosticKind = "record_resync"
	DiagTreeBroken          DiagnosticKind = "tree_broken"
	DiagPathMismatch        DiagnosticKind = "path_mismatch"
	DiagDataOffsetDrift     DiagnosticKind = "data_offset_drift"
	DiagDataOffsetLost      DiagnosticKind = "data_offset_lost"
	DiagDataOffsetUnchecked DiagnosticKind = "data_offset_unchecked"
	DiagDataScan            DiagnosticKind = "data_scan"
	DiagSizeMismatch        DiagnosticKind = "size_mismatch"
	DiagSpansCartridges     DiagnosticKind = "spans_cartridges"
)

// Diagnostic is a non-fatal problem noticed while reading an archive. They are
// returned as data rather than logged, so a caller can render or serialize
// them instead of scraping a log stream.
type Diagnostic struct {
	Kind    DiagnosticKind
	Message string
}

func (d Diagnostic) String() string { return d.Message }

// diagFunc is how the free functions in this package report a problem, so they
// need no reference to the Archive collecting them.
type diagFunc func(kind DiagnosticKind, format string, args ...any)

// diagnostics collects Diagnostic values from any goroutine reading the
// archive.
type diagnostics struct {
	mu    sync.Mutex
	diags []Diagnostic
}

func (d *diagnostics) add(kind DiagnosticKind, format string, args ...any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.diags = append(d.diags, Diagnostic{Kind: kind, Message: fmt.Sprintf(format, args...)})
}

// drain returns the problems noticed so far and clears the list, so a caller
// can attribute them to the operation that produced them.
func (d *diagnostics) drain() []Diagnostic {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := d.diags
	d.diags = nil
	return out
}
