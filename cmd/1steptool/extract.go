package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"samuelks.com/datarescue/ddrescue"
	"samuelks.com/datarescue/internal/extractrun"
	"samuelks.com/datarescue/internal/humanize"
	"samuelks.com/datarescue/internal/outsink"
	"samuelks.com/datarescue/internal/subcmd"
	"samuelks.com/datarescue/onestep"
)

func runCat(args []string) int {
	var sel selector
	return withArchive("cat", "Writes the selected file's contents to stdout.",
		sel.register,
		func(a *onestep.Archive, _ []string) int {
			if len(sel.paths) != 1 {
				fmt.Fprintf(os.Stderr, "%s cat: name exactly one file with -path\n", tool)
				return subcmd.ExitUsage
			}
			r, err := a.Open(sel.paths[0])
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s cat: %s\n", tool, err)
				return subcmd.ExitFailure
			}
			n, err := io.Copy(os.Stdout, r)
			var st subcmd.Status
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s cat: recovered %d bytes before failing: %s\n", tool, n, err)
				st.Degraded()
			}
			for _, e := range r.Errors() {
				fmt.Fprintf(os.Stderr, "%s cat: %s\n", tool, e)
				st.Degraded()
			}
			return st.Code()
		})(args)
}

func runExtract(args []string) int {
	var (
		dest    outsink.Flags
		verbose bool
		sel     selector
	)
	return withArchive("extract", "Restores files to a directory or an archive, keeping whatever was recovered from a damaged one.",
		func(fs *flag.FlagSet) {
			dest.Register(fs)
			fs.BoolVar(&verbose, "v", false, "print each file as it is written")
			sel.register(fs)
		},
		func(a *onestep.Archive, sources []string) int {
			sink, err := dest.Open()
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s extract: %s\n", tool, err)
				return subcmd.ExitFor(err)
			}
			// A path selects a whole tree rather than one entry, so only the
			// match count shows a selection the backup does not hold.
			run := extractrun.New(tool, "extract", sink)
			run.Verbose = verbose
			run.RequireSelection = true

			for i := range a.Files() {
				f := &a.Files()[i]
				if !sel.wants(f.Path) {
					continue
				}
				run.Selected()
				extract(run, a, f)
			}
			return run.Finish(sources)
		})(args)
}

// extract writes one file out, keeping every byte at its file offset: a chunk
// that will not read or decompress becomes zeros, with a map beside the file
// recording them, written in the same call. The catalog records each chunk's
// original size, so the hole is measured rather than guessed and every chunk
// after it still lands where the file says, including the chunks of a disk that
// was never supplied.
func extract(run *extractrun.Run, a *onestep.Archive, f *onestep.File) {
	if len(f.Chunks) == 0 && f.Size > 0 {
		// The catalog lists the file but records nothing to read for it. That
		// is damage to the backup, not a failure of the run.
		run.Note(f.Path, "the catalog records no data for it")
		run.Record(extractrun.Lost)
		return
	}
	var r *onestep.Reader
	_, outcome := run.Write(outsink.Entry{Path: f.Path, ModTime: f.Modified}, f.Size,
		func(l *ddrescue.GapLog) (io.Reader, error) {
			r = a.OpenFile(f)
			r.Fill(l)
			return r, nil
		})
	// A checksum mismatch means the bytes are not what was backed up. They are
	// kept anyway, since they are closer to the file than nothing, and the
	// mismatch is known only after all the bytes are read. The reader is nil
	// where the sink refused the file before anything was read.
	if r == nil {
		return
	}
	if errs := r.Errors(); len(errs) > 0 {
		for _, e := range errs {
			run.Note(f.Path, "%s", e)
		}
		if outcome == extractrun.Whole {
			run.Reclassify(extractrun.Whole, extractrun.Unsound)
		}
	}
}

func runVerify(args []string) int {
	var (
		verbose bool
		sel     selector
	)
	return withArchive("verify", "Decompresses every selected file and checks it against the catalog, writing nothing.",
		func(fs *flag.FlagSet) {
			fs.BoolVar(&verbose, "v", false, "print each file as it is checked")
			sel.register(fs)
		},
		func(a *onestep.Archive, _ []string) int {
			var checked, bad, unreachable int
			var chunks, unchecked int
			var bytes int64
			for i := range a.Files() {
				f := &a.Files()[i]
				if !sel.wants(f.Path) {
					continue
				}
				if !f.Recoverable() {
					fmt.Fprintf(os.Stderr, "%s verify: %s: data is not on any supplied disk\n", tool, f.Path)
					unreachable++
					continue
				}
				for _, c := range f.Chunks {
					chunks++
					if !c.HasChecksum() {
						unchecked++
					}
				}
				c := a.Check(f)
				checked++
				bytes += c.Bytes
				if !c.OK() {
					bad++
					for _, e := range c.Errors {
						fmt.Fprintf(os.Stderr, "%s verify: %s: %s\n", tool, f.Path, e)
					}
					if c.Bytes != f.Size {
						fmt.Fprintf(os.Stderr, "%s verify: %s: recovered %d of %d bytes\n", tool, f.Path, c.Bytes, f.Size)
					}
					continue
				}
				if verbose {
					fmt.Fprintf(os.Stderr, "%s: %d bytes OK\n", f.Path, c.Bytes)
				}
			}
			if checked == 0 && unreachable == 0 {
				fmt.Fprintf(os.Stderr, "%s verify: nothing matched\n", tool)
				return subcmd.ExitFailure
			}

			// Not every chunk carries a checksum, so say how many were actually
			// checked rather than implying every byte was proven.
			fmt.Printf("checked %d file(s), %s\n", checked, humanize.Bytes(bytes))
			fmt.Printf("%d chunk(s): %d checksummed, %d without a checksum\n", chunks, chunks-unchecked, unchecked)
			if unreachable > 0 {
				fmt.Printf("%d file(s) on a disk that was not supplied\n", unreachable)
			}
			if bad > 0 {
				fmt.Printf("%d file(s) did not verify\n", bad)
			}
			var st subcmd.Status
			if bad+unreachable > 0 {
				st.Degraded()
			}
			return st.Code()
		})(args)
}
