package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"samuelks.com/datarescue/ddrescue"
	"samuelks.com/datarescue/internal/extractrun"
	"samuelks.com/datarescue/internal/humanize"
	"samuelks.com/datarescue/internal/outsink"
	"samuelks.com/datarescue/internal/subcmd"
	"samuelks.com/datarescue/qic"
)

// selection restricts a command to some of the archive. A path selects the
// file itself or, when it is a directory, everything under it.
type selection struct{ paths pathList }

type pathList []string

func (p *pathList) String() string { return strings.Join(*p, ",") }

func (p *pathList) Set(v string) error {
	*p = append(*p, normalize(v))
	return nil
}

func (s *selection) register(fs *flag.FlagSet) {
	fs.Var(&s.paths, "path", "restrict to this path in the backup, or to the tree under it (repeatable)")
}

func (s *selection) wants(p string) bool {
	if len(s.paths) == 0 {
		return true
	}
	p = normalize(p)
	for _, want := range s.paths {
		if p == want || strings.HasPrefix(p, want+`\`) {
			return true
		}
	}
	return false
}

func normalize(p string) string {
	p = strings.ReplaceAll(p, "/", `\`)
	return strings.Trim(strings.ToUpper(p), `\`)
}

func runExtract(args []string) int {
	var (
		flags   outsink.Flags
		sel     selection
		verbose bool
	)
	setup := func(fs *flag.FlagSet) {
		flags.Register(fs)
		sel.register(fs)
		fs.BoolVar(&verbose, "v", false, "print each file as it is written")
	}
	return withArchive("extract", "Restores files to a directory or an archive.", setup,
		func(a *qic.Archive, _ []string) int {
			sink, err := flags.Open()
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s extract: %s\n", tool, err)
				return subcmd.ExitFor(err)
			}
			// A path can select a whole tree rather than one entry, so only
			// the count shows that a selection matched nothing in the archive.
			run := extractrun.New(tool, "extract", sink)
			run.Verbose = verbose
			run.RequireSelection = true

			for i := range a.Files() {
				f := &a.Files()[i]
				if f.Dir || !sel.wants(f.Path) {
					continue
				}
				run.Selected()
				// The file keeps its own offsets: a segment that will not expand
				// becomes zeros and a map beside the file saying so, written in
				// the same call. Every segment records the count of uncompressed
				// bytes preceding it, so the length of what was lost is the
				// distance to where the next segment says the stream resumes —
				// measured, not guessed — and the file after the damage is
				// unaffected either way.
				run.Write(outsink.Entry{Path: f.Path, ModTime: f.Modified}, f.Size,
					func(l *ddrescue.GapLog) (io.Reader, error) {
						r := a.OpenFile(f)
						r.Fill(l)
						return r, nil
					})
			}
			return run.Finish([]string{a.Source().Path})
		})(args)
}

func runVerify(args []string) int {
	var (
		sel     selection
		verbose bool
	)
	setup := func(fs *flag.FlagSet) {
		sel.register(fs)
		fs.BoolVar(&verbose, "v", false, "print each file as it is checked")
	}
	return withArchive("verify", "Reads every file through without writing and reports what is damaged.", setup,
		func(a *qic.Archive, _ []string) int {
			var checked, bad, unreachable int
			var bytes int64
			for i := range a.Files() {
				f := &a.Files()[i]
				if f.Dir || !sel.wants(f.Path) {
					continue
				}
				c := a.Check(f)
				checked++
				bytes += c.Bytes
				switch {
				case c.OK():
					if verbose {
						fmt.Printf("ok   %s\n", f.Path)
					}
				case c.Bytes == 0:
					unreachable++
					fmt.Printf("lost %s\n", f.Path)
				default:
					bad++
					fmt.Printf("part %s: %s of %s\n", f.Path, humanize.Bytes(c.Bytes), humanize.Bytes(f.Size))
				}
				for _, e := range c.Errors {
					fmt.Fprintf(os.Stderr, "%s verify: %s\n", tool, e)
				}
			}

			// The segment offsets are checked as well. They place every file,
			// and a damaged archive can get them wrong without any single file
			// looking wrong.
			segs := badSegments(a)
			short := shortSegments(a)

			fmt.Printf("\nchecked %d file(s), %s\n", checked, humanize.Bytes(bytes))
			fmt.Printf("%d whole, %d stopped part way, %d unreachable\n", checked-bad-unreachable, bad, unreachable)
			fmt.Printf("%d of %d data segment(s) did not yield what their offsets account for\n", len(short), a.DataSegments())
			if len(short) > 0 {
				fmt.Printf("  %s\n", summarize(short))
			}
			if len(segs) > 0 {
				fmt.Printf("%d segment(s) could not be placed: %s\n", len(segs), summarize(segs))
			}
			reportDiagnostics("verify", a)

			// Each of these is a finding about the archive, which is what
			// verify reports.
			var st subcmd.Status
			if bad+unreachable+len(segs)+len(short) > 0 {
				st.Degraded()
			}
			if checked == 0 {
				// A -path that matched nothing means the command line is wrong
				// about the archive, not a finding about it; extract exits the
				// same way through RequireSelection.
				st.Failed()
			}
			return st.Code()
		})(args)
}

// shortSegments reads every data segment and reports the ones whose stream
// does not produce the bytes their offsets account for. This is the check the
// format makes possible and the reference implementation has no equivalent of.
func shortSegments(a *qic.Archive) []int {
	var out []int
	buf := make([]byte, 1)
	r := a.DataReaderAt()
	for _, s := range a.Segments()[:a.DataSegments()] {
		if s.Length <= 0 {
			continue
		}
		// Reading the segment's last byte forces the whole segment through the
		// decompressor and reports a stream that ends early.
		if _, err := r.ReadAt(buf, s.End()-1); err != nil {
			out = append(out, s.Index)
		}
	}
	return out
}
