package onestep

import (
	"fmt"
	"sync"
)

// DiagnosticKind classifies a non-fatal problem found while reading a backup.
type DiagnosticKind string

const (
	DiagDirectoryUnreadable DiagnosticKind = "directory_unreadable"
	DiagDirectoryScan       DiagnosticKind = "directory_scan"
	DiagTableMissing        DiagnosticKind = "table_missing"
	DiagTableDamaged        DiagnosticKind = "table_damaged"
	DiagDiskMismatch        DiagnosticKind = "disk_mismatch"
	DiagDiskMissing         DiagnosticKind = "disk_missing"
	DiagDiskTruncated       DiagnosticKind = "disk_truncated"
	DiagSessionMismatch     DiagnosticKind = "session_mismatch"
	DiagOffsetChainBroken   DiagnosticKind = "offset_chain_broken"
	DiagChunkUnplaced       DiagnosticKind = "chunk_unplaced"
	DiagChunkCountMismatch  DiagnosticKind = "chunk_count_mismatch"
	DiagPathUnresolved      DiagnosticKind = "path_unresolved"
	DiagUnknownCompression  DiagnosticKind = "unknown_compression"
	DiagPasswordProtected   DiagnosticKind = "password_protected"
)

// Diagnostic is a non-fatal problem noticed while reading a backup. They are
// returned as data rather than logged, so a caller can render or serialize
// them instead of scraping a log stream.
type Diagnostic struct {
	Kind    DiagnosticKind
	Message string
}

func (d Diagnostic) String() string { return d.Message }

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
