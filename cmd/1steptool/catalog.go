package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"samuelks.com/datarescue/ddrescue"
	"samuelks.com/datarescue/internal/dbf"
	"samuelks.com/datarescue/internal/humanize"
	"samuelks.com/datarescue/internal/safepath"
	"samuelks.com/datarescue/internal/subcmd"
	"samuelks.com/datarescue/internal/xio"
	"samuelks.com/datarescue/onestep"
)

func runCatalog(args []string) int {
	var (
		outDir string
		force  bool
		list   bool
	)
	return withArchive("catalog", "Writes the catalog's members out as separate files, or lists them with -l.",
		func(fs *flag.FlagSet) {
			fs.StringVar(&outDir, "o", ".", "output directory")
			fs.BoolVar(&force, "f", false, "overwrite existing output files")
			fs.BoolVar(&list, "l", false, "list the members instead of writing them")
		},
		func(a *onestep.Archive, _ []string) int {
			members := a.Members()
			if len(members) == 0 {
				fmt.Fprintf(os.Stderr, "%s catalog: the catalog directory could not be read, so its members cannot be named\n", tool)
				return subcmd.ExitFailure
			}
			if list {
				for _, m := range members {
					fmt.Printf("%-16s %10d  %s  0x%x\n", m.Name, m.Size, stamp(m.Modified), m.Offset)
				}
				return subcmd.ExitOK
			}
			if err := xio.EnsureDir(outDir); err != nil {
				fmt.Fprintf(os.Stderr, "%s catalog: %s\n", tool, err)
				return subcmd.ExitFailure
			}
			var written int
			var failed int
			var bytes int64
			for _, m := range members {
				outPath, err := safepath.Join(outDir, m.Name)
				if err != nil {
					fmt.Fprintf(os.Stderr, "%s catalog: %s\n", tool, err)
					failed++
					continue
				}
				out, err := xio.Create(outPath, force)
				if err != nil {
					fmt.Fprintf(os.Stderr, "%s catalog: %s\n", tool, err)
					failed++
					continue
				}
				n, copyErr := io.Copy(out, a.OpenMember(m))
				bytes += n
				if err := xio.CloseAfter(out, copyErr); err != nil {
					fmt.Fprintf(os.Stderr, "%s catalog: %s: recovered %d of %d bytes before failing: %s\n",
						tool, m.Name, n, m.Size, err)
					failed++
					continue
				}
				written++
				rel, _ := filepath.Rel(outDir, outPath)
				fmt.Fprintf(os.Stderr, "%s -> %s (%d bytes)\n", m.Name, rel, n)
			}
			fmt.Fprintf(os.Stderr, "wrote %d member(s), %s", written, humanize.Bytes(bytes))
			if failed > 0 {
				fmt.Fprintf(os.Stderr, "; %d failed", failed)
			}
			fmt.Fprintln(os.Stderr)
			if failed > 0 {
				return subcmd.ExitFailure
			}
			return subcmd.ExitOK
		})(args)
}

// runDBF reads a dBase table on its own, which covers both a member written
// out by the catalog command and the IOA*.DBF configuration database that
// 1-Step Backup kept on the machine the backup ran from.
func runDBF(args []string) int {
	fs := flag.NewFlagSet("dbf", flag.ExitOnError)
	var src ddrescue.Flags
	src.Register(fs)
	offset := fs.Int64("offset", 0, "byte offset of the table within the file")
	rows := fs.Int("rows", 0, "print at most this many rows (0 = all)")
	fields := fs.Bool("fields", false, "print the field definitions only")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s dbf [flags] <file.dbf>\n", tool)
		fmt.Fprintln(os.Stderr, "Dumps a dBase III table.")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		fs.Usage()
		return subcmd.ExitUsage
	}
	if err := src.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "%s dbf: %s\n", tool, err)
		return subcmd.ExitUsage
	}

	// A table rescued out of a disk image arrives with its own map beside it.
	// Reading through that map makes a range that was never recovered fail
	// the read instead of decoding as records.
	source, err := src.Open(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s dbf: %s\n", tool, err)
		return subcmd.ExitFailure
	}
	defer source.Close() //nolint:errcheck // read-only; close errors are unactionable

	if *offset < 0 || *offset > source.Size {
		fmt.Fprintf(os.Stderr, "%s dbf: -offset %d is outside the %d-byte file\n", tool, *offset, source.Size)
		return subcmd.ExitUsage
	}
	t, err := dbf.Open(source.Checked(), *offset, source.Size-*offset)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s dbf: %s\n", tool, err)
		return subcmd.ExitFailure
	}

	fmt.Printf("Version:      0x%02x\n", t.Version)
	fmt.Printf("Updated:      %s\n", stamp(t.Updated))
	fmt.Printf("Record size:  %d\n", t.RecordSize)
	fmt.Printf("Records:      %d", t.Len())
	if t.Len() != t.DeclaredLen() {
		fmt.Printf(" (header says %d)", t.DeclaredLen())
	}
	fmt.Println()
	fmt.Println("Fields:")
	for i, fl := range t.Fields {
		fmt.Printf("  %2d %-12s %s %4d at %d\n", i+1, fl.Name, fl.Type, fl.Length, fl.Offset)
	}
	if *fields {
		return subcmd.ExitOK
	}

	limit := t.Len()
	if *rows > 0 && *rows < limit {
		limit = *rows
	}
	status := subcmd.ExitOK
	fmt.Println()
	// A table over a bad region of the medium fails one record per
	// unrecovered sector, producing thousands of identical lines; cap them the
	// way the archive diagnostics are capped.
	var badRecords int
	for i := range limit {
		r, err := t.Record(i)
		if err != nil {
			badRecords++
			if badRecords <= subcmd.MaxPerKind {
				fmt.Fprintf(os.Stderr, "%s dbf: %s\n", tool, err)
			}
			// A record that was never recovered is damage the map reports;
			// the table itself is sound and the rest of it still decodes.
			status = subcmd.ExitDegraded
			continue
		}
		var parts []string
		for _, fl := range t.Fields {
			v, err := r.String(fl.Name)
			if err != nil {
				v = "?"
			}
			parts = append(parts, fl.Name+"="+strconv.Quote(v))
		}
		var mark string
		if r.Deleted() {
			mark = " (deleted)"
		}
		fmt.Printf("[%d]%s %s\n", i, mark, strings.Join(parts, " "))
	}
	if badRecords > subcmd.MaxPerKind {
		fmt.Fprintf(os.Stderr, "%s dbf: and %d more unreadable record(s)\n", tool, badRecords-subcmd.MaxPerKind)
	}
	return status
}
