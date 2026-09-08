// Command qictool inspects and restores Windows 9x MS Backup archives.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"samuelks.com/datarescue/ddrescue"
	"samuelks.com/datarescue/internal/humanize"
	"samuelks.com/datarescue/internal/subcmd"
	"samuelks.com/datarescue/qic"
)

const tool = "qictool"

func main() {
	os.Exit(subcmd.Run(tool, "<file.qic>", []subcmd.Command{
		{Name: "info", Summary: "show the volume, region and segment details", Run: runInfo},
		{Name: "ls", Summary: "list the files in the backup", Run: runLS},
		{Name: "cat", Summary: "write one file's contents to stdout", Run: runCat},
		{Name: "extract", Summary: "restore files to a directory or an archive", Run: runExtract},
		{Name: "verify", Summary: "read every file through without writing and report what is damaged", Run: runVerify},
		{Name: "segments", Summary: "show the segment map, which says what a bad region of the medium cost", Run: runSegments},
		{Name: "decompress", Summary: "write the whole decompressed data region to a file", Run: runDecompress},
		{Name: "catalog", Summary: "dump the catalog records with their undecoded fields", Run: runCatalog},
	}))
}

// withArchive parses the shared flags, opens the archive given as the first
// argument and hands it to run. Running the command inside a closure means the
// deferred close happens before the status reaches main.
func withArchive(name, summary string, setup func(*flag.FlagSet), run func(*qic.Archive, []string) int) func([]string) int {
	return func(args []string) int {
		fs := flag.NewFlagSet(name, flag.ExitOnError)
		mapFile := fs.String("map", "", ddrescue.MapUsage)
		noMap := fs.Bool("no-map", false, ddrescue.NoMapUsage)
		volume := fs.Int("volume", 0, "which volume table record to read, 1-based; Windows 98 and ME write one per source drive")
		mode := fs.String("source", "auto", "where the file list comes from: auto, catalog or scan")
		if setup != nil {
			setup(fs)
		}
		fs.Usage = func() {
			fmt.Fprintf(os.Stderr, "Usage: %s %s [flags] <file.qic>\n", tool, name)
			fmt.Fprintln(os.Stderr, summary)
			fs.PrintDefaults()
		}
		_ = fs.Parse(args)
		if fs.NArg() < 1 {
			fs.Usage()
			return subcmd.ExitUsage
		}

		if *mapFile != "" && *noMap {
			fmt.Fprintf(os.Stderr, "%s %s: a map was named and also asked to be ignored\n", tool, name)
			return subcmd.ExitUsage
		}

		var opts qic.Options
		opts.Volume = *volume
		switch *mode {
		case "auto":
			opts.Mode = qic.ModeAuto
		case "catalog":
			opts.Mode = qic.ModeCatalog
		case "scan":
			opts.Mode = qic.ModeScan
		default:
			fmt.Fprintf(os.Stderr, "%s %s: -source must be auto, catalog or scan\n", tool, name)
			return subcmd.ExitUsage
		}

		a, err := qic.OpenSource(qic.Source{Path: fs.Arg(0), Map: *mapFile, NoMap: *noMap}, opts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s %s: %s\n", tool, name, err)
			return subcmd.ExitFailure
		}
		defer a.Close() //nolint:errcheck // read-only; close errors are unactionable

		reportDiagnostics(name, a)
		return run(a, fs.Args()[1:])
	}
}

func reportDiagnostics(name string, a *qic.Archive) {
	subcmd.ReportDiagnostics(tool+" "+name, a.Diagnostics(),
		func(d qic.Diagnostic) qic.DiagnosticKind { return d.Kind })
}

func runInfo(args []string) int {
	return withArchive("info", "Shows the volume table, the regions and what the archive holds.", nil,
		func(a *qic.Archive, _ []string) int {
			v := a.Volume()
			fmt.Printf("Label:        %s\n", v.Label)
			fmt.Printf("Written by:   %s (%s), revision %d.%d\n", v.Variant, v.OSTypeName(), v.Revision[0], v.Revision[1])
			fmt.Printf("Created:      %s\n", stamp(v.Created))
			fmt.Printf("Drive:        %s\n", orDash(a.Drive()))
			if v.SourceLabel != "" {
				fmt.Printf("Source label: %s\n", v.SourceLabel)
			}
			fmt.Printf("Compression:  %s\n", compression(v))
			if flags := v.FlagNames(); len(flags) > 0 {
				fmt.Printf("Flags:        %s\n", strings.Join(flags, ", "))
			}
			if v.Sequence > 1 {
				fmt.Printf("Cartridge:    %d\n", v.Sequence)
			}

			dataStart, catalogStart := a.Layout()
			fmt.Printf("\nRegions:\n")
			fmt.Printf("  data:       0x%x, %d segments, %s decompressed\n",
				dataStart, a.DataSegments(), humanize.Bytes(a.DataSize()))
			fmt.Printf("  catalog:    0x%x, %d segments, %s\n",
				catalogStart, len(a.Segments())-a.DataSegments(), humanize.Bytes(a.CatalogSize()))
			if v.DataSize != a.DataSize() {
				fmt.Printf("  the volume table records %s of data\n", humanize.Bytes(v.DataSize))
			}
			fmt.Printf("  segment:    %d bytes, %d QFA blocks from %d\n",
				qic.SegmentSize, int(v.Segments[1])-int(v.Segments[0])+1, v.Segments[0])

			if len(a.Volumes()) > 1 {
				fmt.Printf("\nVolumes:\n")
				for _, x := range a.Volumes() {
					fmt.Printf("  %d: %s, drive %s, %s\n", x.Index, orDash(x.Label), orDash(x.Drive), humanize.Bytes(x.DataSize))
				}
			}

			if id := a.MediumID(); id != nil {
				fmt.Printf("\nMedium:\n")
				fmt.Printf("  id:         %s\n", orDash(id.MediumID))
				fmt.Printf("  label:      %s\n", orDash(id.Label))
				fmt.Printf("  created:    %s\n", stamp(id.Created))
				for _, k := range []string{"VR", "CS", "FM"} {
					if val := id.Keys[k]; val != "" {
						fmt.Printf("  %-11s %s\n", k+":", val)
					}
				}
			}

			files, dirs, damaged, total := tally(a)
			fmt.Printf("\nContents:     %d files, %d directories, %s\n", files, dirs, humanize.Bytes(total))
			if a.FromScan() {
				fmt.Printf("              recovered by scanning the data region; the catalog could not be read\n")
			}
			if damaged > 0 {
				fmt.Printf("              %d file(s) overlap a segment that could not be placed\n", damaged)
			}
			if bad := badSegments(a); len(bad) > 0 {
				fmt.Printf("              %d segment(s) could not be placed: %s\n", len(bad), summarize(bad))
			}
			return subcmd.ExitOK
		})(args)
}

func runLS(args []string) int {
	var long, dirs bool
	setup := func(fs *flag.FlagSet) {
		fs.BoolVar(&long, "l", false, "show size, modification time and whether the file can be recovered")
		fs.BoolVar(&dirs, "d", false, "list directories as well as files")
	}
	return withArchive("ls", "Lists the files in the backup.", setup,
		func(a *qic.Archive, _ []string) int {
			for i := range a.Files() {
				f := &a.Files()[i]
				if f.Dir && !dirs {
					continue
				}
				if !long {
					fmt.Println(f.Path)
					continue
				}
				mark := " "
				if !f.Dir && !f.Recoverable() {
					mark = "!"
				}
				kind := "-"
				if f.Dir {
					kind = "d"
				}
				fmt.Printf("%s%s %10d  %s  %s\n", mark, kind, f.Size, stamp(f.Modified), f.Path)
			}
			return subcmd.ExitOK
		})(args)
}

func runCat(args []string) int {
	return withArchive("cat", "Writes one file's contents to stdout.", nil,
		func(a *qic.Archive, paths []string) int {
			if len(paths) != 1 {
				fmt.Fprintf(os.Stderr, "%s cat: expected one path inside the backup\n", tool)
				return subcmd.ExitUsage
			}
			r, err := a.Open(paths[0])
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s cat: %s\n", tool, err)
				return subcmd.ExitFailure
			}
			n, err := io.Copy(os.Stdout, r)
			var st subcmd.Status
			for _, e := range r.Errors() {
				fmt.Fprintf(os.Stderr, "%s cat: %s\n", tool, e)
				st.Degraded()
			}
			if err != nil {
				// cat writes to stdout and has nowhere to put a map, so it
				// latches at the damage. Reporting how much came back tells a
				// short file from an empty one.
				fmt.Fprintf(os.Stderr, "%s cat: %s after %s\n", tool, err, humanize.Bytes(n))
				st.Degraded()
			}
			return st.Code()
		})(args)
}

func tally(a *qic.Archive) (files, dirs, damaged int, total int64) {
	for i := range a.Files() {
		f := &a.Files()[i]
		if f.Dir {
			dirs++
			continue
		}
		files++
		total += f.Size
		if !f.Recoverable() {
			damaged++
		}
	}
	return files, dirs, damaged, total
}

// badSegments lists the segments the index could not place from the archive's
// own offsets. A segment whose stream will not decompress is not among them:
// that is only found by reading it, which verify does.
func badSegments(a *qic.Archive) []int {
	var out []int
	for _, s := range a.Segments() {
		if s.State != qic.SegmentOK && s.State != qic.SegmentTerminator {
			out = append(out, s.Index)
		}
	}
	return out
}

// summarize renders a list of segment numbers as runs, since damage on a
// medium arrives in stretches.
func summarize(nums []int) string {
	var parts []string
	for i := 0; i < len(nums); {
		j := i
		for j+1 < len(nums) && nums[j+1] == nums[j]+1 {
			j++
		}
		if j == i {
			parts = append(parts, fmt.Sprintf("%d", nums[i]))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", nums[i], nums[j]))
		}
		i = j + 1
		if len(parts) == 8 && i < len(nums) {
			parts = append(parts, "...")
			break
		}
	}
	return strings.Join(parts, ", ")
}

func compression(v qic.Volume) string {
	if !v.Compressed {
		return "none"
	}
	if v.Algorithm == 1 {
		return "QIC-122"
	}
	return fmt.Sprintf("algorithm %d, which is not known", v.Algorithm)
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Format("2006-01-02 15:04:05")
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
