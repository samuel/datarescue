// Package subcmd is the shared "tool <command> [flags] <args>" dispatcher used
// by the command-line tools.
package subcmd

import (
	"cmp"
	"errors"
	"flag"
	"fmt"
	"maps"
	"os"
	"runtime/debug"
	"slices"
)

// Exit statuses, used by every tool so a script can treat them alike.
//
// They answer two separate questions: what the tool found on the medium, and
// whether the run itself worked. ExitOK means a faithful copy, anything up to
// ExitDegraded means the artifacts are usable, and anything above means they
// cannot be trusted.
const (
	// ExitOK means the command did what was asked and found nothing wrong.
	ExitOK = 0
	// ExitDegraded means the command did what was asked and the medium was
	// damaged: holes zero-filled with a .map recording them, a file that
	// stopped part way, a checksum that did not match, a directory that would
	// not read, sectors that would not come back. The damage was handled as
	// designed and written down, and the output is as good as the medium
	// allowed.
	ExitDegraded = 1
	// ExitUsage means the command line itself was wrong.
	ExitUsage = 2
	// ExitFailure means the run could not do what was asked: an image that
	// would not open, an output or a .map that would not write, an archive left
	// truncated, a selection that matched nothing. This says nothing about the
	// medium; it means the result is not what was asked for.
	ExitFailure = 3
)

// UsageError marks an error the command line caused rather than the medium.
// Some flag combinations can only be judged once the image is in hand — a
// partition number against the table it is not in, -geometry against an image
// type that has no geometry — which is too late for flag.Parse to catch, but
// they are still the operator's mistake and not the disc's.
type UsageError struct{ Err error }

func (e *UsageError) Error() string { return e.Err.Error() }

func (e *UsageError) Unwrap() error { return e.Err }

// Usage marks err as caused by the command line. A nil error stays nil.
func Usage(err error) error {
	if err == nil {
		return nil
	}
	return &UsageError{Err: err}
}

// ExitFor is the status for an error that stopped a command before it could do
// any work: ExitUsage when the command line caused it, and otherwise
// ExitFailure, since the run could not do what was asked.
func ExitFor(err error) int {
	if _, ok := errors.AsType[*UsageError](err); ok {
		return ExitUsage
	}
	return ExitFailure
}

// Status accumulates what a run found, so every tool turns the same findings
// into the same exit code. The zero value is ExitOK.
type Status struct {
	degraded bool
	failed   bool
}

// Degraded records a finding about the medium: damage the command handled as
// designed and wrote down.
func (s *Status) Degraded() { s.degraded = true }

// Failed records that the run could not do what was asked.
func (s *Status) Failed() { s.failed = true }

// Add folds another status in, for a command whose work is split across
// functions.
func (s *Status) Add(o Status) {
	s.degraded = s.degraded || o.degraded
	s.failed = s.failed || o.failed
}

// Code is the process exit status. A failure of the run outranks a finding
// about the medium, since it says the output cannot be trusted at all, where a
// finding only says how much of the medium was there. ExitUsage is not part of
// that ordering: a bad command line is settled before any work begins.
func (s Status) Code() int {
	switch {
	case s.failed:
		return ExitFailure
	case s.degraded:
		return ExitDegraded
	}
	return ExitOK
}

// Version is the tool version, overridable at build time with
// -ldflags "-X samuelks.com/datarescue/internal/subcmd.Version=...". Left
// unset, it is the module version the binary was built from, which is what
// "go install samuelks.com/datarescue/cmd/fstool@v0.1.0" records.
var Version = ""

// CurrentVersion is Version, else the module version from the build info, else "dev"
// for a build from a working tree.
func CurrentVersion() string {
	if Version != "" {
		return Version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

// Command is one subcommand. Run receives the arguments after the command name
// and returns the process exit status.
type Command struct {
	Name string
	// Summary is the one-line description shown in the usage listing.
	Summary string
	Run     func(args []string) int
}

// Run parses the command name from os.Args, dispatches to the matching command
// and returns its exit status. An unknown or missing command prints the usage
// listing and returns ExitUsage.
func Run(tool, argSummary string, cmds []Command) int {
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Usage = func() { usage(tool, argSummary, cmds) }
	flag.Parse()

	if *showVersion {
		fmt.Printf("%s %s\n", tool, CurrentVersion())
		return ExitOK
	}
	if flag.NArg() < 1 {
		flag.Usage()
		return ExitUsage
	}
	name := flag.Arg(0)
	for _, c := range cmds {
		if c.Name == name {
			return c.Run(flag.Args()[1:])
		}
	}
	fmt.Fprintf(os.Stderr, "%s: unknown command %q\n\n", tool, name)
	flag.Usage()
	return ExitUsage
}

func usage(tool, argSummary string, cmds []Command) {
	fmt.Fprintf(os.Stderr, "Usage: %s <command> [flags] %s\n\n", tool, argSummary)
	fmt.Fprintln(os.Stderr, "Commands:")
	defer fmt.Fprintf(os.Stderr, "\nRun \"%s <command> -h\" for a command's flags.\n", tool)
	var width int
	for _, c := range cmds {
		width = max(width, len(c.Name))
	}
	for _, c := range cmds {
		fmt.Fprintf(os.Stderr, "  %-*s  %s\n", width, c.Name, c.Summary)
	}
}

// MaxPerKind bounds how many diagnostics of one kind ReportDiagnostics prints.
// One lost segment or one missing disk produces a diagnostic per affected file,
// and thousands of near-identical lines would bury every other finding.
const MaxPerKind = 10

// ReportDiagnostics prints a reader's diagnostics to stderr under prefix,
// capping how many of each kind it shows and following them with a tally of
// what was left out. kind groups them, and the tally follows that order, so two
// runs over one image print the same report.
//
// It returns how many diagnostics there were, including the ones not printed.
func ReportDiagnostics[D fmt.Stringer, K cmp.Ordered](prefix string, diags []D, kind func(D) K) int {
	over := make(map[K]int)
	seen := make(map[K]int, len(diags))
	for _, d := range diags {
		k := kind(d)
		seen[k]++
		if seen[k] <= MaxPerKind {
			fmt.Fprintf(os.Stderr, "%s: %s\n", prefix, d)
			continue
		}
		over[k]++
	}
	for _, k := range slices.Sorted(maps.Keys(over)) {
		fmt.Fprintf(os.Stderr, "%s: and %d more %v\n", prefix, over[k], k)
	}
	return len(diags)
}
