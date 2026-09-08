package fstool

import (
	"fmt"
	"io"

	"samuelks.com/datarescue/internal/humanize"
	"samuelks.com/datarescue/internal/subcmd"
)

// Report is a verify run's findings. Every format's own checks write through
// it, so the WARNING/ERROR shape, the issue count and the verdict are the same
// whatever was checked.
type Report struct {
	W io.Writer
	// issues is what reaches the verdict. Every Warnf and Errorf counts one.
	issues int
}

// NewReport returns a report writing to w.
func NewReport(w io.Writer) *Report { return &Report{W: w} }

// Warnf records something wrong that the tool worked around.
func (r *Report) Warnf(format string, args ...any) {
	fmt.Fprintf(r.W, "WARNING: "+format+"\n", args...) //nolint:errcheck // report output
	r.issues++
}

// Errorf records something wrong that the tool could not work around.
func (r *Report) Errorf(format string, args ...any) {
	fmt.Fprintf(r.W, "ERROR:   "+format+"\n", args...) //nolint:errcheck // report output
	r.issues++
}

// Infof records something worth saying that is not a finding, so it does not
// reach the verdict.
func (r *Report) Infof(format string, args ...any) {
	fmt.Fprintf(r.W, "         "+format+"\n", args...) //nolint:errcheck // report output
}

// Printf writes a plain line.
func (r *Report) Printf(format string, args ...any) {
	fmt.Fprintf(r.W, format+"\n", args...) //nolint:errcheck // report output
}

// Issues is how many findings were recorded.
func (r *Report) Issues() int { return r.issues }

// Preamble writes what every verify begins with: the format, where it was
// found, and what the rescue map says the volume lost. A map that leaves any of
// the volume unrecovered is itself a finding: the medium is incomplete, which
// is exactly what this command is asked to judge.
func (r *Report) Preamble(c *Context) {
	r.Printf("Filesystem:  %s", c.Driver.Summary())
	r.Printf("Located:     %s", c.Image.Source)
	if c.Image.MapPath == "" {
		return
	}
	r.Infof("Rescue map: %s", c.Image.MapPath)
	if lost := VolumeUnrecovered(c); lost > 0 {
		r.Warnf("the rescue map leaves %s of the volume unrecovered", humanize.Bytes(lost))
	}
}

// Verdict writes the issue count and the result, and returns the exit code.
//
// Every finding a verify makes is about the medium (a structure that does not
// check out, a range that was never recovered), so the code is ExitDegraded and
// never ExitFailure. A run that could not do what was asked has already failed
// before reaching here.
func (r *Report) Verdict() int {
	r.Printf("Issues:      %d", r.issues)
	if r.issues == 0 {
		r.Printf("Result:      OK")
		return subcmd.ExitOK
	}
	r.Printf("Result:      ISSUES FOUND")
	return subcmd.ExitDegraded
}
