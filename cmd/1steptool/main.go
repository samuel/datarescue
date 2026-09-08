// Command 1steptool inspects and restores Iomega 1-Step Backup archives.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"samuelks.com/datarescue/ddrescue"
	"samuelks.com/datarescue/internal/humanize"
	"samuelks.com/datarescue/internal/subcmd"
	"samuelks.com/datarescue/onestep"
)

const tool = "1steptool"

func main() {
	os.Exit(subcmd.Run(tool, "<file.1-Step ...>", []subcmd.Command{
		{Name: "info", Summary: "show the header, job and disk set details", Run: runInfo},
		{Name: "ls", Summary: "list the files in the backup", Run: runLS},
		{Name: "cat", Summary: "write one file's contents to stdout", Run: runCat},
		{Name: "extract", Summary: "restore files to a directory or an archive", Run: runExtract},
		{Name: "verify", Summary: "check every file against its recorded checksums", Run: runVerify},
		{Name: "catalog", Summary: "write the catalog's members out as separate files", Run: runCatalog},
		{Name: "dbf", Summary: "dump a dBase table, such as one written by catalog", Run: runDBF},
	}))
}

// pathList collects a repeatable -path flag.
type pathList []string

func (p *pathList) String() string { return strings.Join(*p, ", ") }

func (p *pathList) Set(v string) error {
	*p = append(*p, v)
	return nil
}

// selector decides which files a command acts on. With no -path it takes all
// of them; a path matches a file exactly or is a directory containing it.
type selector struct {
	paths pathList
}

func (s *selector) register(fs *flag.FlagSet) {
	fs.Var(&s.paths, "path", "restrict to this path in the backup, or to the directory tree under it (repeatable)")
}

func (s *selector) wants(p string) bool {
	if len(s.paths) == 0 {
		return true
	}
	have := normalize(p)
	for _, want := range s.paths {
		w := normalize(want)
		if have == w || strings.HasPrefix(have, w+`\`) {
			return true
		}
	}
	return false
}

func normalize(p string) string {
	return strings.ToUpper(strings.Trim(strings.ReplaceAll(p, "/", `\`), `\`))
}

// withArchive wires the flag parsing, archive open and diagnostics reporting
// that every subcommand needs. Every positional argument is one disk of the
// set, which is why paths are selected with a flag rather than by trailing
// arguments as in the single-image tools.
func withArchive(name, summary string, setup func(*flag.FlagSet), run func(*onestep.Archive, []string) int) func([]string) int {
	return func(args []string) int {
		fs := flag.NewFlagSet(name, flag.ExitOnError)
		// Only the caveat is specific to this tool: every positional argument
		// is a disk, so a single map given with -map cannot cover a set. The
		// rest of the wording is shared so the flag reads the same everywhere.
		mapFile := fs.String("map", "", "ddrescue .map for the image; only with a single image, which otherwise uses disk1.1-Step.map or disk1.map")
		noMap := fs.Bool("no-map", false, ddrescue.NoMapUsage)
		if setup != nil {
			setup(fs)
		}
		fs.Usage = func() {
			fmt.Fprintf(os.Stderr, "Usage: %s %s [flags] <file.1-Step ...>\n", tool, name)
			fmt.Fprintln(os.Stderr, summary)
			fs.PrintDefaults()
		}
		_ = fs.Parse(args)
		if fs.NArg() < 1 {
			fs.Usage()
			return subcmd.ExitUsage
		}
		if *mapFile != "" && fs.NArg() != 1 {
			fmt.Fprintf(os.Stderr, "%s %s: -map takes a single image; put each disk's map beside it instead\n", tool, name)
			return subcmd.ExitUsage
		}
		if *mapFile != "" && *noMap {
			fmt.Fprintf(os.Stderr, "%s %s: a map was named and also asked to be ignored\n", tool, name)
			return subcmd.ExitUsage
		}

		srcs := make([]onestep.Source, 0, fs.NArg())
		for _, p := range fs.Args() {
			srcs = append(srcs, onestep.Source{Path: p, Map: *mapFile, NoMap: *noMap})
		}
		a, err := onestep.OpenSources(srcs)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s %s: %s\n", tool, name, err)
			return subcmd.ExitFailure
		}
		defer a.Close() //nolint:errcheck // read-only; close errors are unactionable

		reportDiagnostics(name, a)
		return run(a, fs.Args())
	}
}

func reportDiagnostics(name string, a *onestep.Archive) int {
	return subcmd.ReportDiagnostics(tool+" "+name, a.Diagnostics(),
		func(d onestep.Diagnostic) onestep.DiagnosticKind { return d.Kind })
}

func runInfo(args []string) int {
	return withArchive("info", "Shows the file header, the job record and the disks of the set.", nil,
		func(a *onestep.Archive, _ []string) int {
			j := a.Job()
			disks := a.Disks()
			fmt.Printf("Job:          %d\n", j.Number)
			fmt.Printf("Description:  %s\n", j.Description)
			fmt.Printf("Created:      %s\n", stamp(j.Created))
			if len(disks) > 0 {
				// The header's timestamp is the set identity: every disk of one
				// backup carries it, so it is what sorts a directory of images
				// into sets.
				fmt.Printf("Set:          %s\n", stamp(disks[0].Header.Created))
			}
			fmt.Printf("Disks in set: %d\n", j.Disks)
			fmt.Printf("Compressed:   %s\n", yesNo(j.Compressed))
			if j.HasPassword {
				fmt.Printf("Password:     yes\n")
			}

			fmt.Printf("\nDisks supplied:\n")
			for _, d := range disks {
				fmt.Printf("  %d: %s\n", d.Header.Disk, d.Path)
				fmt.Printf("     %s, %s of data", humanize.Bytes(d.Size), humanize.Bytes(d.DataLength))
				if d.Header.CatalogOffset > 0 {
					fmt.Printf(", catalog at 0x%x", d.Header.CatalogOffset)
				}
				fmt.Println()
				if d.FirstChunk > 0 {
					fmt.Printf("     chunks %d-%d\n", d.FirstChunk, d.LastChunk)
				}
				if short := d.ExpectedDataLength - d.DataLength; short > 0 {
					fmt.Printf("     truncated: %s short of the %s the catalog puts here\n",
						humanize.Bytes(short), humanize.Bytes(d.ExpectedDataLength))
				}
				if d.MapPath != "" {
					fmt.Printf("     map: %s\n", d.MapPath)
				}
			}

			if drives := a.Drives(); len(drives) > 0 {
				fmt.Printf("\nSource drives:\n")
				for _, d := range drives {
					fmt.Printf("  %s %-12s used %s, free %s\n", d.Letter, d.Label,
						humanize.Bytes(d.Used), humanize.Bytes(d.Free))
				}
			}
			if sel := a.Selections(); len(sel) > 0 {
				fmt.Printf("\nSelected for backup:\n")
				for _, s := range sel {
					fmt.Printf("  %s\n", s.Path)
				}
			}
			if sess := a.Sessions(); len(sess) > 0 {
				fmt.Printf("\nSessions:\n")
				for _, s := range sess {
					fmt.Printf("  disk %d: chunks %d-%d, %s\n", s.Disk, s.FirstChunk, s.LastChunk, stamp(s.Date))
				}
			}

			files := a.Files()
			var chunks int
			var total int64
			for _, f := range files {
				chunks += len(f.Chunks)
				total += f.Size
			}
			fmt.Printf("\nContents:     %d files, %d chunks, %s\n", len(files), chunks, humanize.Bytes(total))
			return subcmd.ExitOK
		})(args)
}

func runLS(args []string) int {
	var long bool
	var sel selector
	return withArchive("ls", "Lists the files the catalog records.",
		func(fs *flag.FlagSet) {
			fs.BoolVar(&long, "l", false, "long format: size, timestamp and storage")
			sel.register(fs)
		},
		func(a *onestep.Archive, _ []string) int {
			var shown int
			for i := range a.Files() {
				f := &a.Files()[i]
				if !sel.wants(f.Path) {
					continue
				}
				shown++
				if !long {
					fmt.Println(f.Path)
					continue
				}
				mark := " "
				if !f.Recoverable() {
					mark = "!"
				}
				fmt.Printf("%s %10d  %s  %-9s %s\n", mark, f.Size, stamp(f.Modified), storage(f), f.Path)
			}
			if shown == 0 {
				fmt.Fprintf(os.Stderr, "%s ls: nothing matched\n", tool)
				return subcmd.ExitFailure
			}
			return subcmd.ExitOK
		})(args)
}

// storage describes how a file's chunks are held, since a compressed backup
// can still store a chunk verbatim when compressing it did not make it smaller.
func storage(f *onestep.File) string {
	var stored, deflated int
	for _, c := range f.Chunks {
		switch {
		case c.Empty():
			// A chunk holding nothing is neither.
		case c.Stored():
			stored++
		default:
			deflated++
		}
	}
	switch {
	case stored == 0 && deflated == 0:
		return "empty"
	case deflated == 0:
		return "stored"
	case stored == 0:
		return "deflated"
	default:
		return "mixed"
	}
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Format("2006-01-02 15:04:05")
}
