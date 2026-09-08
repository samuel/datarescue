// Package extractrun handles the files an extract command recovers.
//
// Five tools pull files off damaged media into an outsink.Sink. The rule that
// turns what happened into an exit code is the same for all of them, and easy
// to get subtly different: a finding about the medium exits ExitDegraded, a
// failure of the run exits ExitFailure, and a failure of the run outranks a
// finding about the medium. Keeping the counters, that classification and the
// report in one place stops the same bad sector from exiting 1 in one tool and
// 3 in another. Each tool keeps only the logic specific to its own format.
package extractrun

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"samuelks.com/datarescue/ddrescue"
	"samuelks.com/datarescue/internal/humanize"
	"samuelks.com/datarescue/internal/outsink"
	"samuelks.com/datarescue/internal/subcmd"
)

// Outcome is what became of one file.
type Outcome uint8

const (
	// Whole is a file that came back at the length the medium records.
	Whole Outcome = iota
	// Filled is one written at its full length with the holes zero-filled and
	// a .map beside it recording where they are.
	Filled
	// Partial is one whose read stopped part way. What came back is kept: a
	// partial recovery is still evidence of what the medium held, and throwing
	// it away recovers nothing.
	Partial
	// Lost is one of which nothing was recovered. It is a finding about the
	// medium and not a failure of the run: the run did what was asked and the
	// bytes were not there.
	Lost
	// Unsound is one delivered whole that is not what the medium records — a
	// checksum that did not agree, or a length that did not.
	Unsound
	// Skipped is one the tool does not write by design: a symlink, a device
	// node. Nothing is wrong with either the medium or the run.
	Skipped
	// Failed is one the run could not write: a full disk, a duplicate member,
	// a map that could not be written. It is the only outcome that is not a
	// finding about the medium.
	Failed

	numOutcomes = iota
)

// Degraded reports whether the outcome describes the medium rather than the
// run. The exit code follows from this classification alone.
func (o Outcome) Degraded() bool {
	switch o {
	case Filled, Partial, Lost, Unsound:
		return true
	}
	return false
}

// outcomeText is the summary's wording for each outcome, so every tool
// describes the same outcome the same way.
var outcomeText = [numOutcomes]string{
	Filled:  "with holes, each with a .map beside it",
	Partial: "stopped part way",
	Lost:    "the medium gave up nothing of",
	Unsound: "not what the medium records",
	Skipped: "skipped",
	Failed:  "could not be written",
}

// Run counts what an extract command wrote and turns it into a report and an
// exit code.
type Run struct {
	// Verbose prints each file as it is written.
	Verbose bool
	// RequireSelection makes a run that selected no file at all a failure of
	// the run rather than an empty success. Set it where the selection is a
	// path prefix rather than an exact name, since nothing else then notices
	// that the operator asked for a tree the archive does not hold.
	RequireSelection bool

	tool string
	sink outsink.Sink
	log  *outsink.Log

	selected  int
	unmatched int
	bytes     int64
	counts    [numOutcomes]int
	// damage counts the findings about the medium that belong to no one file:
	// a directory that would not read, a walk that stopped, a volume descriptor
	// that was never recovered.
	damage int
	// want is the paths the operator gave and how many entries each matched.
	want map[string]int
}

// New starts a run writing to sink. tool is the program ("isotool") and command
// the subcommand ("extract"); together they prefix every line of the report.
func New(tool, command string, sink outsink.Sink) *Run {
	return &Run{tool: tool, sink: sink, log: &outsink.Log{Prefix: tool + " " + command}}
}

// Log returns the run's log, which writes to stderr and keeps the report the
// sink carries, for the lines only the tool itself can write.
func (r *Run) Log() *outsink.Log { return r.log }

// Want registers the exact paths the operator asked for. A path that matches
// nothing means the run did not do what was asked; Finish reports it and fails.
func (r *Run) Want(paths ...string) {
	if r.want == nil {
		r.want = make(map[string]int, len(paths))
	}
	for _, p := range paths {
		r.want[p] = 0
	}
}

// Wants reports whether path was asked for, counting the match. With nothing
// registered, every path is wanted.
func (r *Run) Wants(path string) bool {
	if len(r.want) == 0 {
		return true
	}
	if _, ok := r.want[path]; !ok {
		return false
	}
	r.want[path]++
	return true
}

// Selected counts a file the selection matched, whether or not it is then
// written.
func (r *Run) Selected() { r.selected++ }

// Note adds a line to the report against one path.
func (r *Run) Note(path, format string, args ...any) {
	r.log.Printf("%s: %s", path, fmt.Sprintf(format, args...))
}

// Damage records a finding about the medium that belongs to no single file: a
// directory that would not read, a walk that stopped part way, a descriptor
// never recovered. It reaches the exit code the way a damaged file does.
func (r *Run) Damage(format string, args ...any) {
	r.damage++
	r.log.Printf("%s", fmt.Sprintf(format, args...))
}

// Record counts an outcome the caller settled without writing anything: a
// symlink it will not recreate, a file the catalog records no data for, an
// inode whose data can never be read.
func (r *Run) Record(o Outcome) { r.counts[o]++ }

// Reclassify moves one file from one outcome to another, for a check that can
// only run after the bytes are read, such as a chunk checksum.
func (r *Run) Reclassify(from, to Outcome) {
	if r.counts[from] > 0 {
		r.counts[from]--
	}
	r.counts[to]++
}

// Merge folds two recorded outcomes into one, keeping the worse, and returns
// it.
//
// A file with more than one stream — a Mac file's data fork and its resource
// fork — is written twice and is still one file. Counting both would report
// every forked file as two, and report a whole data fork beside a damaged
// resource fork as one clean file and one damaged one rather than as the single
// damaged file it is.
func (r *Run) Merge(a, b Outcome) Outcome {
	keep := a
	if severity(b) > severity(a) {
		keep = b
	}
	for _, o := range [2]Outcome{a, b} {
		if r.counts[o] > 0 {
			r.counts[o]--
		}
	}
	r.counts[keep]++
	return keep
}

// severity orders outcomes by how bad they are, so folding two into one keeps
// the one worth reporting.
func severity(o Outcome) int {
	switch o {
	case Whole:
		return 0
	case Skipped:
		return 1
	case Filled:
		return 2
	case Partial:
		return 3
	case Unsound:
		return 4
	case Lost:
		return 5
	case Failed:
		return 6
	}
	return 0
}

// Write writes one recovered file to the sink, with the map recording any holes
// in it beside it, and reports what happened. size is the file's length as the
// medium records it, which a short read is measured against; pass -1 where the
// format records none.
//
// This is the only route an extract command has to a gap-filling reader, since
// outsink.WriteFilled is what arms the log: a file cannot be written with holes
// in it unless the map recording them goes into the same sink beside it.
func (r *Run) Write(e outsink.Entry, size int64, open func(*ddrescue.GapLog) (io.Reader, error)) (outsink.Written, Outcome) {
	w, runErr := outsink.WriteFilled(r.sink, e, open)
	r.bytes += w.Bytes
	// Every finding below is about the medium, so it uses the medium's name for
	// the file. Where the written name differs, it is w.Dest, which the verbose
	// line already shows.
	src := origin(e)

	// A failure of the run is reported first and outranks damage to the
	// medium: a run failure means the output cannot be trusted at all, while
	// damage only says how much of the medium was there. WriteFilled folds a
	// map that could not be written into that error, so a file that filled
	// holes and then could not have them recorded lands here rather than among
	// the damaged.
	if runErr != nil {
		r.Note(src, "%s", runErr)
		if w.Err != nil {
			r.Note(src, "%s", w.Err)
		}
		return w, r.count(Failed)
	}
	switch {
	case w.Err != nil && w.Bytes == 0:
		// Nothing of the file was recovered. That is damage to the medium, not
		// a failure of the run.
		r.Note(src, "%s", w.Err)
		return w, r.count(Lost)
	case w.Err != nil:
		r.Note(src, "kept %s of %s: %s", humanize.Bytes(w.Bytes), declared(size), w.Err)
		return w, r.count(Partial)
	case w.Damaged():
		r.Note(src, "%s", w.GapMessage())
		return w, r.count(Filled)
	case size >= 0 && w.Bytes != size:
		r.Note(src, "%s came back where the medium records %s",
			humanize.Bytes(w.Bytes), humanize.Bytes(size))
		return w, r.count(Partial)
	}
	if r.Verbose {
		r.log.Plainf("%s -> %s (%s)", src, w.Dest, humanize.Bytes(w.Bytes))
	}
	return w, r.count(Whole)
}

func (r *Run) count(o Outcome) Outcome {
	r.counts[o]++
	return o
}

// declared renders the length the medium records, for the formats that record
// one.
func declared(size int64) string {
	if size < 0 {
		return "an unrecorded length"
	}
	return humanize.Bytes(size)
}

// Finish reports what the run came to, hands the sink the report, closes it and
// returns the exit code.
func (r *Run) Finish(sources []string) int {
	// A requested path that is not on the medium means the command line was
	// wrong about the medium, not that the medium is damaged: the output is not
	// what was asked for. The paths are sorted, so two runs over one image
	// report the same thing in the same order.
	for _, p := range slices.Sorted(maps.Keys(r.want)) {
		if r.want[p] == 0 {
			r.log.Printf("%s: no such path in the image", p)
			r.unmatched++
		}
	}
	r.log.Plainf("%s", r.summary())

	st := r.status()
	if err := r.sink.Manifest(outsink.ManifestText(r.tool, sources, r.log.Text())); err != nil {
		r.log.Printf("%s", err)
		st.Failed()
	}
	// A close that fails leaves a truncated archive, whatever the files did.
	if err := r.sink.Close(); err != nil {
		r.log.Printf("%s", err)
		st.Failed()
	}
	return st.Code()
}

// summary is the final line of the run's report.
func (r *Run) summary() string {
	var handled int
	for _, n := range r.counts {
		handled += n
	}
	if handled == 0 && r.damage == 0 {
		return "nothing matched"
	}
	written := r.counts[Whole] + r.counts[Filled] + r.counts[Partial] + r.counts[Unsound]

	var b strings.Builder
	fmt.Fprintf(&b, "wrote %d file(s), %s", written, humanize.Bytes(r.bytes))
	for o := range numOutcomes {
		if Outcome(o) == Whole {
			continue
		}
		if n := r.counts[o]; n > 0 {
			fmt.Fprintf(&b, "; %d %s", n, outcomeText[o])
		}
	}
	if r.damage > 0 {
		fmt.Fprintf(&b, "; %d problem(s) with the medium itself", r.damage)
	}
	return b.String()
}

// status weighs the counts. The classification is one question — does this
// finding describe the medium, or the run? A file written at its full length
// with a map beside it, one that stopped part way, one with nothing recovered
// and one that does not match what the medium records are all the medium.
// A sink that would not take a file, a report or a close that failed, and a
// selection that matched nothing are all the run.
func (r *Run) status() subcmd.Status {
	var st subcmd.Status
	if r.damage > 0 {
		st.Degraded()
	}
	for o := range numOutcomes {
		if r.counts[o] > 0 && Outcome(o).Degraded() {
			st.Degraded()
		}
	}
	if r.counts[Failed] > 0 || r.unmatched > 0 {
		st.Failed()
	}
	if r.RequireSelection && r.selected == 0 {
		st.Failed()
	}
	return st
}

// origin is the name a message about the medium uses for an entry: the
// medium's own name, when the file was written under a name this tool derived
// from other metadata on the medium.
func origin(e outsink.Entry) string {
	if e.Origin != "" {
		return e.Origin
	}
	return e.Path
}
