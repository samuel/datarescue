// Package humanize formats sizes for the command-line tools.
package humanize

import "fmt"

const (
	kib = 1024
	mib = kib * 1024
	gib = mib * 1024
)

// Bytes renders a byte count in binary units with the exact count alongside,
// which matters when the reader is comparing against an on-disk figure.
func Bytes(n int64) string {
	switch {
	case n >= gib:
		return fmt.Sprintf("%.2f GiB (%d bytes)", float64(n)/gib, n)
	case n >= mib:
		return fmt.Sprintf("%.2f MiB (%d bytes)", float64(n)/mib, n)
	case n >= kib:
		return fmt.Sprintf("%.2f KiB (%d bytes)", float64(n)/kib, n)
	default:
		return fmt.Sprintf("%d bytes", n)
	}
}

// MB returns a byte count in decimal megabytes, the unit the ripping progress
// and throughput lines are quoted in.
func MB(n int64) float64 { return float64(n) / 1e6 }
