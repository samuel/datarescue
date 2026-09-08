package fstool

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"samuelks.com/datarescue/ddrescue"
	"samuelks.com/datarescue/internal/appledouble"
	"samuelks.com/datarescue/internal/extractrun"
	"samuelks.com/datarescue/internal/humanize"
	"samuelks.com/datarescue/internal/outsink"
	"samuelks.com/datarescue/internal/subcmd"
)

// normalize puts a path in the form paths are matched in: no leading or
// trailing slash. Every driver normalizes its entries this way and every path
// given on the command line goes through it, so a name typed with or without a
// leading slash selects the same file.
func normalize(p string) string { return strings.Trim(p, "/") }

// ---- info ------------------------------------------------------------------

// Info reports what the volume says about itself. It describes rather than
// judges, so it prints what the map left unrecovered and still exits OK: the
// count is a fact about the medium, not a verdict on it.
func Info(c *Context) int {
	fmt.Printf("Image:       %s\n", c.Path)
	fmt.Printf("Filesystem:  %s\n", c.Driver.Summary())
	fmt.Printf("Located:     %s\n", c.Image.Source)
	if err := c.Volume.Info(os.Stdout); err != nil {
		c.Errorf("%s", err)
		return subcmd.ExitFailure
	}
	PrintMapInfo(os.Stdout, c, fmt.Sprintf("run '%s verify' for the files it cost", c.Tool))
	return subcmd.ExitOK
}

// ---- ls --------------------------------------------------------------------

// ListFlags are what a listing takes beyond the common flags.
type ListFlags struct {
	Long    bool
	Grouped bool
}

// Register adds the listing flags.
func (lf *ListFlags) Register(fs *FlagSet) {
	fs.BoolVar(&lf.Long, "l", false, "long format: mode, size and date")
	fs.BoolVar(&lf.Grouped, "g", false, "group the listing by directory")
}

// List writes the volume's entries.
//
// A listing describes the medium rather than judging it, so it exits OK even
// when the map shows heavy damage. It marks the affected entries and reports
// how many at the end. It exits non-zero only when it tried to read something
// and could not, which the walk's own diagnostics report.
func List(c *Context, lf *ListFlags) int {
	var st subcmd.Status
	sel := selection(c.Args, true)

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	var group string
	var damaged int
	first := true

	// Decided once, before the walk, not per entry. tabwriter aligns by cell
	// index, so whether the column exists has to be settled for the whole
	// listing. A format that records no kind outside the name gets no kind
	// column.
	tv, typed := c.Volume.(Typed)

	diags, err := c.Volume.Walk(func(e Entry) error {
		if !sel.wants(e.Path) {
			return nil
		}
		if lf.Grouped {
			// Every driver walks in tree order, so grouping by parent needs no
			// tree in memory: the header changes exactly when the directory
			// does.
			if dir := path.Dir(e.Path); dir != group {
				w.Flush() //nolint:errcheck // stdout
				if !first {
					fmt.Println()
				}
				if dir == "." {
					fmt.Println("/:")
				} else {
					fmt.Printf("%s:\n", dir)
				}
				group = dir
			}
			first = false
		}
		lost, _ := c.Volume.Damage(e)
		if lost > 0 {
			damaged++
		}
		var kind FileKind
		if typed {
			kind = tv.Kind(e)
		}
		printEntry(w, e, lf.Long, lost, typed, kind)
		return nil
	})
	w.Flush() //nolint:errcheck // stdout

	for _, d := range diags {
		c.Errorf("%s", d)
		if d.Damage {
			st.Degraded()
		}
	}
	if err != nil {
		c.Errorf("walk stopped: %s", err)
		st.Degraded()
	}
	for _, d := range c.Volume.Diagnostics() {
		c.Errorf("%s", d)
		if d.Damage {
			st.Degraded()
		}
	}
	if damaged > 0 {
		fmt.Printf("\n%d entr%s touch unrecovered parts of the image\n", damaged, plural(damaged, "y", "ies"))
	}
	if n := sel.unmatched(); len(n) > 0 {
		// A path that matched nothing is a failure of the run, unless the walk
		// hit damage. Then the tool could not look everywhere and cannot say
		// the path is absent, which is a problem with the medium. The two get
		// different exit codes.
		blind := st.Code() == subcmd.ExitDegraded
		for _, p := range n {
			if blind {
				c.Errorf("%s: not found, but part of the volume would not read", p)
				continue
			}
			c.Errorf("%s: no such path in the image", p)
		}
		if !blind {
			st.Failed()
		}
	}
	return st.Code()
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// printEntry writes one listing line. The loss is given in the entry's own
// bytes, the same unit as its size: a figure in medium bytes next to one in
// file bytes invites the wrong subtraction.
//
// typed says whether the volume records a kind outside the name. It decides
// whether the column is printed, rather than kind being non-empty: a folder
// records no kind and would otherwise end the column for every row after it.
func printEntry(w *tabwriter.Writer, e Entry, long bool, lost int64, typed bool, kind FileKind) {
	name := e.Path
	if e.Dir {
		name += "/"
	}
	mark := ""
	if lost > 0 {
		mark = fmt.Sprintf("  [%s unrecovered]", humanize.Bytes(lost))
	}
	if !long {
		fmt.Fprintf(w, "%s%s\n", name, mark) //nolint:errcheck // in-memory tabwriter
		return
	}
	size := "<DIR>"
	if !e.Dir {
		size = "-"
		if e.Size >= 0 {
			size = fmt.Sprintf("%d", e.Size)
		}
	}
	date := strings.Repeat(" ", len(time.DateTime))
	if !e.ModTime.IsZero() {
		date = e.ModTime.Format(time.DateTime)
	}
	if e.Symlink != "" {
		name += " -> " + e.Symlink
	}
	cells := []string{e.Mode, fmt.Sprintf("%10s", size), date}
	if typed {
		cells = append(cells, kind.Code)
	}
	// The name is last so no row grows trailing whitespace: tabwriter pads every
	// cell but the one after the final tab.
	cells = append(cells, name+mark)
	fmt.Fprintln(w, strings.Join(cells, "\t")) //nolint:errcheck // in-memory tabwriter
}

// ---- cat -------------------------------------------------------------------

// Cat writes one file's data to stdout.
//
// It never fills gaps: stdout has nowhere to put the map that would record
// them, and unexplained zeros are worse than a short file. So it stops at the
// damage and reports how much was read.
func Cat(c *Context) int {
	e, err := resolve(c, c.Args[0])
	if err != nil {
		c.Errorf("%s", err)
		if errors.Is(err, ErrNotFound) {
			// The volume does not hold it, which is a failure of the run.
			return subcmd.ExitFailure
		}
		return subcmd.ExitDegraded
	}
	if e.Dir {
		c.Errorf("%s is a directory", e.Path)
		return subcmd.ExitFailure
	}
	r, err := c.Volume.Open(e)
	if err != nil {
		c.Errorf("%s", err)
		return subcmd.ExitDegraded
	}
	n, err := io.Copy(os.Stdout, r)
	for _, d := range c.Volume.Diagnostics() {
		c.Errorf("%s", d)
	}
	if err != nil {
		c.Errorf("recovered %d bytes before failing: %s", n, err)
		return subcmd.ExitDegraded
	}
	return subcmd.ExitOK
}

// resolve finds one requested path, through the volume's index where it has one
// and by walking where it does not.
func resolve(c *Context, want string) (Entry, error) {
	want = normalize(want)
	e, err := c.Volume.Lookup(want)
	if !errors.Is(err, ErrNoLookup) {
		return e, err
	}
	// No index: walk and stop at the match. The sentinel is swallowed here, so
	// a caller cannot mistake a successful stop for a failure.
	var found Entry
	var hit bool
	stop := errors.New("found")
	if _, werr := c.Volume.Walk(func(e Entry) error {
		if normalize(e.Path) == want {
			found, hit = e, true
			return stop
		}
		return nil
	}); werr != nil && !errors.Is(werr, stop) {
		return Entry{}, werr
	}
	if !hit {
		return Entry{}, fmt.Errorf("%w: %s", ErrNotFound, want)
	}
	return found, nil
}

// ---- selection -------------------------------------------------------------

// sel is the set of paths given on the command line. An empty set selects
// everything.
//
// The two modes matter. A directory given to a listing means "show me what is
// under it", so it matches by prefix. A directory given to an extract is
// something extract will not write, and matching its children by prefix would
// quietly turn one path into a whole subtree. Extract therefore matches
// exactly and says so.
type sel struct {
	want   map[string]int
	prefix bool
}

func selection(args []string, prefix bool) *sel {
	s := &sel{prefix: prefix}
	if len(args) == 0 {
		return s
	}
	s.want = make(map[string]int, len(args))
	for _, a := range args {
		s.want[normalize(a)] = 0
	}
	return s
}

// wants reports whether the path was asked for, counting the match.
func (s *sel) wants(p string) bool {
	if s.want == nil {
		return true
	}
	p = normalize(p)
	if _, ok := s.want[p]; ok {
		s.want[p]++
		return true
	}
	if !s.prefix {
		return false
	}
	for w := range s.want {
		if w == "" || strings.HasPrefix(p, w+"/") {
			s.want[w]++
			return true
		}
	}
	return false
}

// unmatched is every requested path that selected nothing, in order, so two
// runs report the same one first. A selection that matched nothing is a
// failure of the run.
func (s *sel) unmatched() []string {
	var out []string
	for w, n := range s.want {
		if n == 0 {
			out = append(out, w)
		}
	}
	sort.Strings(out)
	return out
}

// ---- extract ---------------------------------------------------------------

// Fork policies for a volume whose files carry more than one stream.
const (
	// ForksAppleDouble writes the data fork as the file and the resource fork
	// as a "._Name" companion. macOS itself writes this onto a volume that
	// cannot hold two forks, so the pair is rejoined when copied back to a Mac.
	ForksAppleDouble = "appledouble"
	// ForksSeparate writes the resource fork as "Name.rsrc", a plain second
	// file. Nothing reassembles the pair, and the Finder information has
	// nowhere to go.
	ForksSeparate = "separate"
	// ForksData writes the data fork only and reports what that dropped, which
	// on a classic Mac disk is often most of the content.
	ForksData = "data"
	// ForksNone writes the data fork only and says nothing.
	ForksNone = "none"
)

// ExtractFlags are what an extract takes beyond the common and sink flags.
type ExtractFlags struct {
	Dest    outsink.Flags
	Verbose bool
	Forks   string
	Ext     bool
}

// Register adds the extract flags.
func (xf *ExtractFlags) Register(fs *FlagSet) {
	xf.Dest.Register(fs.FlagSet)
	fs.BoolVar(&xf.Verbose, "v", false, "print each file as it is written")
	fs.StringVar(&xf.Forks, "forks", ForksAppleDouble,
		"what to do with a resource fork: appledouble, separate, data (drop and report), none")
	// Shared across drivers, as -forks is. A driver-owned flag given against
	// another format is a usage error, and an operator extracting a directory of
	// rescued images should not have to know which format each one is before
	// passing the flag.
	fs.BoolVar(&xf.Ext, "ext", false,
		"append a filename extension the volume's own record implies, where the name carries none (HFS)")
}

// validate checks the fork policy before the image is touched.
func (xf *ExtractFlags) validate() error {
	switch xf.Forks {
	case ForksAppleDouble, ForksSeparate, ForksData, ForksNone:
		return nil
	}
	return subcmd.Usage(fmt.Errorf("-forks %q: want %s, %s, %s or %s",
		xf.Forks, ForksAppleDouble, ForksSeparate, ForksData, ForksNone))
}

// Extract writes the volume's files to the sink.
func Extract(c *Context, xf *ExtractFlags) int {
	if err := xf.validate(); err != nil {
		c.Errorf("%s", err)
		return subcmd.ExitFor(err)
	}
	sink, err := xf.Dest.Open()
	if err != nil {
		c.Errorf("%s", err)
		return subcmd.ExitFor(err)
	}
	run := extractrun.New(c.Tool, c.Command, sink)
	dn := derivedNames{}
	run.Verbose = xf.Verbose

	named := selectionPaths(c.Args)
	if len(named) > 0 {
		run.Want(named...)
		// A volume with an index resolves each requested path directly. That
		// recovers a file from a tree that will not enumerate, and keeps the
		// two failures apart: the volume not holding the path is a failure of
		// the run, a directory on the way that would not read is the medium.
		if done := extractNamed(c, run, xf, dn, named); done {
			return run.Finish([]string{c.Path})
		}
	}

	sel := selection(c.Args, false)
	diags, werr := c.Volume.Walk(func(e Entry) error {
		if !sel.wants(e.Path) {
			return nil
		}
		// Counted here as well as matched: Finish fails the run for a requested
		// path that selected nothing, and a path this walk did write must not
		// be reported as one of them.
		run.Wants(e.Path)
		if e.Dir {
			if len(named) > 0 {
				run.Note(e.Path, "is a directory; name the files inside it")
			}
			return nil
		}
		if e.Symlink != "" || e.Special {
			// Neither is recreated, so an image cannot place a link or a device
			// node the operator did not choose.
			run.Record(extractrun.Skipped)
			return nil
		}
		writeEntry(c, run, xf, dn, e)
		return nil
	})

	for _, d := range diags {
		if d.Damage {
			run.Damage("%s", d)
		} else {
			run.Note(d.Path, "%s", d.Text)
		}
	}
	if werr != nil {
		run.Damage("walk stopped: %s", werr)
	}
	for _, d := range c.Volume.Diagnostics() {
		if d.Damage {
			run.Damage("%s", d)
		} else {
			run.Note(d.Path, "%s", d.Text)
		}
	}
	return run.Finish([]string{c.Path})
}

// selectionPaths is the requested paths in the form Run.Want records, so an
// unmatched one is reported as typed, after normalizing.
func selectionPaths(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = normalize(a)
	}
	return out
}

// extractNamed writes each requested path through the volume's index. It
// reports whether it handled the selection; for a volume with no index the
// caller uses the walk instead.
func extractNamed(c *Context, run *extractrun.Run, xf *ExtractFlags, dn derivedNames, named []string) bool {
	for i, p := range named {
		e, err := c.Volume.Lookup(p)
		if errors.Is(err, ErrNoLookup) {
			if i > 0 {
				// A volume answers the same way for every path, so this cannot
				// happen part way through. The guard keeps a buggy driver from
				// writing half the selection twice.
				run.Damage("%s: this volume answered for %d path(s) and then said it has no index", c.Path, i)
			}
			return false
		}
		switch {
		case errors.Is(err, ErrNotFound):
			// Left unmatched: Finish reports it and fails the run, which gives
			// one message and one count rather than two.
			continue
		case err != nil:
			// The medium, not the run: asking for the file by path must not turn
			// exit 1 into exit 3 for damage the whole-volume walk would also hit.
			run.Note(p, "%s", err)
			run.Record(extractrun.Lost)
			continue
		}
		run.Wants(e.Path)
		if e.Dir {
			run.Note(e.Path, "is a directory; name the files inside it")
			continue
		}
		if e.Symlink != "" || e.Special {
			run.Record(extractrun.Skipped)
			continue
		}
		writeEntry(c, run, xf, dn, e)
	}
	return true
}

// writeEntry writes one recovered file. The reader is opened inside the call
// that writes the map, so a file can only be written with holes if the map
// recording them goes into the same sink beside it.
func writeEntry(c *Context, run *extractrun.Run, xf *ExtractFlags, dn derivedNames, e Entry) {
	n := nameFor(c, xf, e)
	// Two image names can become one output name, and a derived extension is one
	// more way that can happen. The name is checked before the write claims it, so
	// the note is printed only when an earlier entry already wrote it. Any other
	// write failure is a separate run error, reported on its own.
	collision := dn[n.data()]
	dn.claim(n)
	_, outcome := run.Write(n.entry(n.data(), e.ModTime), e.Size,
		func(l *ddrescue.GapLog) (io.Reader, error) {
			r, err := c.Volume.Open(e)
			if err != nil {
				return nil, err
			}
			r.Fill(l)
			return r, nil
		})
	// The tool caused the collision, so the message says how to avoid it.
	if outcome == extractrun.Failed && collision {
		run.Note(n.src, "%s is a name derived from what the volume records a file holds; -ext=false writes the volume's own names",
			path.Base(n.data()))
	}
	// Drained right after the read that produced them: Diagnostics clears
	// the list, so one left pending would be reported against the next file.
	for _, d := range c.Volume.Diagnostics() {
		run.Note(n.src, "%s", d.Text)
	}
	writeResourceFork(c, run, xf, e, n, outcome)
}

// writeResourceFork writes the second half of a file that has one.
//
// A file is one file however many streams it carries, so the fork's outcome is
// merged into the data fork's rather than counted again: an intact data fork
// with a damaged resource fork is reported once, as damaged. Counting them
// separately would report every forked file twice.
func writeResourceFork(c *Context, run *extractrun.Run, xf *ExtractFlags, e Entry, n outName, was extractrun.Outcome) {
	fv, ok := c.Volume.(Forked)
	if !ok || xf.Forks == ForksNone {
		return
	}
	size := fv.ResourceSize(e)
	if size <= 0 {
		return
	}
	if xf.Forks == ForksData {
		// Reported rather than dropped silently: on a classic Mac volume a
		// resource-only file extracts as a zero-byte file, which is accurate
		// for the data fork and useless to the operator.
		run.Note(n.src, "%s of resource fork not written; -forks %s writes it",
			humanize.Bytes(size), ForksAppleDouble)
		return
	}

	// A companion written after a header needs a reader that can shift the
	// holes it records. Without one the map would mark the header as lost and
	// put every hole one header length too early, so the fork is written as a
	// plain file instead: a second file with a correct map is better than a
	// companion with a wrong one.
	appleDouble := xf.Forks == ForksAppleDouble
	if appleDouble && !forkReaderShifts(fv, e) {
		run.Note(n.src, "this format's reader cannot record a companion's holes; writing the fork as a plain file")
		appleDouble = false
	}

	// Both names use the one stem nameFor reserved room for, so the companion
	// and its fork cannot be shortened to different lengths.
	dest := n.rsrc()
	var hdr appledouble.Header
	var base int64
	declared := size
	if appleDouble {
		dest = n.sidecar()
		hdr = fv.Sidecar(e, path.Base(n.data()))
		base = hdr.Size()
		declared = base + size
	}

	_, outcome := run.Write(n.entry(dest, e.ModTime), declared,
		func(l *ddrescue.GapLog) (io.Reader, error) {
			r, err := fv.OpenResource(e)
			if err != nil {
				return nil, err
			}
			if base > 0 {
				// The fork's bytes start at base in the written file. Only the
				// reader knows the offsets it reports, so it adds the base
				// itself. The header is the tool's own bytes and is always
				// recorded as recovered.
				r.(ShiftedFiller).FillAt(l, base) //nolint:errcheck,forcetypeassert // canShift was checked above
				return hdr.Reader(r), nil
			}
			r.Fill(l)
			return r, nil
		})
	for _, d := range c.Volume.Diagnostics() {
		run.Note(n.src, "%s", d.Text)
	}
	// One file, one outcome: the worse of the two stands.
	run.Merge(was, outcome)
}

// ---- verify ----------------------------------------------------------------

// Verify checks the volume's structure and reads every file, reporting what
// was lost to damage.
//
// Every finding is about the medium, so the code is ExitDegraded: a run that
// could not do what was asked has already failed before reaching here.
func Verify(c *Context, verbose bool) int {
	rep := NewReport(os.Stdout)
	rep.Preamble(c)

	if v, ok := c.Volume.(Verifier); ok {
		v.Verify(rep, verbose)
	}

	var dirs, files, damaged int
	diags, err := c.Volume.Walk(func(e Entry) error {
		if e.Dir {
			dirs++
			return nil
		}
		if e.Symlink != "" || e.Special {
			return nil
		}
		files++
		if lost, ranges := c.Volume.Damage(e); lost > 0 {
			damaged++
			rep.Warnf("%s: %s in %d range(s) was never recovered", e.Path, humanize.Bytes(lost), ranges)
		}
		// Read the whole file. The map only says what was lost before
		// reading; a read can fail on more, and a verify that skipped the
		// read would pass any volume that has no map.
		verifyRead(c, rep, e, verbose)
		// Drained after the read that produced them, not before: Diagnostics
		// clears the list, so draining earlier would report anything still
		// pending against the wrong entry.
		for _, d := range c.Volume.Diagnostics() {
			rep.Warnf("%s: %s", e.Path, d.Text)
		}
		return nil
	})
	for _, d := range diags {
		rep.Warnf("%s", d)
	}
	if err != nil {
		rep.Errorf("walk stopped: %s", err)
	}
	for _, d := range c.Volume.Diagnostics() {
		rep.Warnf("%s", d)
	}

	if tv, ok := c.Volume.(TailVerifier); ok {
		tv.VerifyTail(rep)
	}

	rep.Printf("")
	rep.Printf("Directories: %d", dirs)
	rep.Printf("Files:       %d", files)
	if c.Image.MapPath != "" {
		rep.Printf("Damaged:     %d entries touch unrecovered parts of the image", damaged)
	}
	return rep.Verdict()
}

// verifyRead reads one whole file and reports what stopped it. Nothing is
// kept: the question is whether the bytes can be read, not what they are.
func verifyRead(c *Context, rep *Report, e Entry, verbose bool) {
	r, err := c.Volume.Open(e)
	if err != nil {
		rep.Errorf("%s: %s", e.Path, err)
		return
	}
	n, err := io.Copy(io.Discard, r)
	if err != nil {
		rep.Errorf("%s: read stopped after %s: %s", e.Path, humanize.Bytes(n), err)
		return
	}
	if e.Size >= 0 && n != e.Size {
		rep.Warnf("%s: %s came back where the volume records %s",
			e.Path, humanize.Bytes(n), humanize.Bytes(e.Size))
		return
	}
	if verbose {
		rep.Infof("OK   %s (%s)", e.Path, humanize.Bytes(n))
	}
}

// ---- orphans ---------------------------------------------------------------

// Orphans lists data that is allocated but not reachable from any directory
// entry. Only some formats support it. For one that does not, the operator has
// asked for something this image cannot provide, which is a usage error
// however late it is noticed, the same as a flag that does not apply.
func Orphans(c *Context, verbose bool) int {
	o, ok := c.Volume.(Orphaner)
	if !ok {
		c.Errorf("%s", subcmd.Usage(fmt.Errorf(
			"orphans applies only to a volume that tracks allocation outside its directory tree; %s holds %s",
			c.Path, c.Driver.Summary())))
		return subcmd.ExitUsage
	}
	found, err := o.Orphans(os.Stdout, verbose)
	if err != nil {
		c.Errorf("%s", err)
		return subcmd.ExitFailure
	}
	if found > 0 {
		return subcmd.ExitDegraded
	}
	return subcmd.ExitOK
}
