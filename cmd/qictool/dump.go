package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"samuelks.com/datarescue/ddrescue"
	"samuelks.com/datarescue/internal/humanize"
	"samuelks.com/datarescue/internal/subcmd"
	"samuelks.com/datarescue/internal/xio"
	"samuelks.com/datarescue/qic"
)

func runSegments(args []string) int {
	var damagedOnly bool
	setup := func(fs *flag.FlagSet) {
		fs.BoolVar(&damagedOnly, "damaged", false, "show only the segments that could not be placed")
	}
	return withArchive("segments", "Shows the segment map, which says what a bad region of the medium cost.", setup,
		func(a *qic.Archive, _ []string) int {
			fmt.Printf("%6s  %10s  %12s  %10s  %-8s %s\n",
				"seg", "file", "stream", "length", "payload", "state")
			for _, s := range a.Segments() {
				if damagedOnly && (s.State == qic.SegmentOK || s.State == qic.SegmentTerminator) {
					continue
				}
				kind := "compressed"
				switch {
				case s.Raw && s.Catalog:
					kind = "catalog"
				case s.Raw:
					kind = "stored"
				}
				fmt.Printf("%6d  0x%08x  %12d  %10d  %-8d %s, %s\n",
					s.Index, s.FileOffset, s.Start, s.Length, s.Payload, kind, s.State)
			}
			return subcmd.ExitOK
		})(args)
}

// openOutput makes the file a decompress writes to. It is a variable so a test
// can hand back an output that stops taking bytes part way and reach the map
// written below after a failed run.
var openOutput = func(path string, force bool) (io.WriteCloser, error) {
	return xio.Create(path, force)
}

func runDecompress(args []string) int {
	var (
		out   string
		force bool
	)
	setup := func(fs *flag.FlagSet) {
		fs.StringVar(&out, "o", "", "write to this file, or - for stdout")
		fs.BoolVar(&force, "f", false, "overwrite an existing output file")
	}
	return withArchive("decompress", "Writes the whole decompressed data region to a file.", setup,
		func(a *qic.Archive, _ []string) int {
			if out == "" {
				fmt.Fprintf(os.Stderr, "%s decompress: -o is required\n", tool)
				return subcmd.ExitUsage
			}
			d := region{a: a, stream: out == "-", mapping: ddrescue.NewMapBuilder(1)}
			w := io.WriteCloser(nopCloser{os.Stdout})
			if !d.stream {
				f, err := openOutput(out, force)
				if err != nil {
					fmt.Fprintf(os.Stderr, "%s decompress: %s\n", tool, err)
					return subcmd.ExitFailure
				}
				w = f
			}

			err := d.write(w)
			if cerr := w.Close(); err == nil {
				err = cerr
			}
			mapPath := out + ".map"
			if !d.stream {
				// The map records which bytes of the output came from the
				// archive and which are zeros standing in for what would not
				// expand. It is written however the run ended, since a write
				// that stopped part way is when the output's holes most need
				// recording; without it nothing distinguishes the made-up
				// bytes from real ones.
				if merr := writeRegionMap(d.mapping, mapPath); merr != nil {
					err = errors.Join(err, merr)
				}
			}
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s decompress: %s\n", tool, err)
				return subcmd.ExitFailure
			}

			fmt.Fprintf(os.Stderr, "%s decompress: wrote %s", tool, humanize.Bytes(d.written))
			if d.lost > 0 {
				fmt.Fprintf(os.Stderr, "; %s in %d segment(s) was never recovered, zero-filled and recorded in %s",
					humanize.Bytes(d.filled), d.lost, filepath.Base(mapPath))
			}
			fmt.Fprintln(os.Stderr)
			if d.lost > 0 {
				// The archive was damaged, not the run.
				return subcmd.ExitDegraded
			}
			return subcmd.ExitOK
		})(args)
}

// region writes the uncompressed data region out, writing zeros for what would
// not expand so that every later byte keeps the offset the archive gives it,
// and records both in a ddrescue map beside the output.
//
// A stream has nowhere to keep that map, so it stops at the damage instead:
// unrecorded zeros are worse than a short file. cat latches for the same
// reason.
type region struct {
	a       *qic.Archive
	stream  bool
	mapping *ddrescue.MapBuilder
	written int64
	filled  int64
	lost    int
}

// chunkSize bounds one read of the region. A segment's span reaches past its
// own payload wherever the segments after it lost their headers, so the span is
// not a buffer size.
const chunkSize = 1 << 20

func (d *region) write(w io.Writer) error {
	total := d.a.DataSize()
	r := d.a.DataReaderAt()
	buf := make([]byte, chunkSize)

	var pos int64
	for _, s := range d.a.Segments()[:d.a.DataSegments()] {
		// A stretch no segment accounts for: the headers that would have
		// placed it were lost, and its length is exactly the distance between
		// the two recorded offsets around it.
		if err := d.fill(w, &pos, min(s.Start, total), "segment %d is not accounted for", s.Index); err != nil {
			return err
		}
		end := min(s.End(), total)
		for pos < end {
			n, rerr := r.ReadAt(buf[:min(end-pos, chunkSize)], pos)
			if n > 0 {
				if err := d.put(w, buf[:n]); err != nil {
					return err
				}
				d.mapping.RecordRange(pos, int64(n), false)
				pos += int64(n)
			}
			if rerr == nil {
				continue
			}
			if err := d.fill(w, &pos, end, "%s", rerr); err != nil {
				return err
			}
			break
		}
	}
	return d.fill(w, &pos, total, "the data region is not accounted for past %d", pos)
}

// fill writes zeros from *pos up to end, records them and advances *pos.
func (d *region) fill(w io.Writer, pos *int64, end int64, format string, args ...any) error {
	length := end - *pos
	if length <= 0 {
		return nil
	}
	if d.stream {
		return fmt.Errorf("%s: %s could not be recovered, and a stream has nowhere to keep the map that would record the zeros standing in for it; write to a file with -o <path>",
			fmt.Sprintf(format, args...), humanize.Bytes(length))
	}
	d.lost++
	d.filled += length
	d.mapping.RecordRange(*pos, length, true)
	fmt.Fprintf(os.Stderr, "%s decompress: %s zero-filled at %d: %s\n",
		tool, humanize.Bytes(length), *pos, fmt.Sprintf(format, args...))

	zeros := make([]byte, min(length, chunkSize))
	for length > 0 {
		n := min(length, int64(len(zeros)))
		if err := d.put(w, zeros[:n]); err != nil {
			return err
		}
		*pos += n
		length -= n
	}
	return nil
}

func (d *region) put(w io.Writer, b []byte) error {
	n, err := w.Write(b)
	d.written += int64(n)
	return err
}

// writeRegionMap writes the sidecar listing the output's own holes. The output
// itself was gated by -f; a stale map beside a fresh output is worse than
// none.
func writeRegionMap(b *ddrescue.MapBuilder, path string) error {
	f, err := xio.Create(path, true)
	if err != nil {
		return err
	}
	_, err = b.WriteTo(f)
	return xio.CloseAfter(f, err)
}

func runCatalog(args []string) int {
	var (
		long  bool
		limit int
	)
	setup := func(fs *flag.FlagSet) {
		fs.BoolVar(&long, "l", false, "show the flag and attribute bits and the record's place in the data region")
		fs.IntVar(&limit, "n", 0, "show at most this many records")
	}
	return withArchive("catalog", "Dumps the catalog records with their undecoded fields.", setup,
		func(a *qic.Archive, _ []string) int {
			for i := range a.Files() {
				if limit > 0 && i >= limit {
					break
				}
				f := &a.Files()[i]
				if !long {
					fmt.Printf("%-4d %s\n", i, f.Path)
					continue
				}
				fmt.Printf("%-4d flag=0x%02x attr=0x%02x depth=%d size=%-10d record=%-10d data=%-10d %s\n",
					i, f.Flags, f.Attributes, f.Depth, f.Size, f.RecordOffset(), f.DataOffset(), f.Path)
				fmt.Printf("       short=%-14s created=%s accessed=%s modified=%s\n",
					orDash(f.ShortName), stamp(f.Created), stamp(f.Accessed), stamp(f.Modified))
			}
			return subcmd.ExitOK
		})(args)
}

type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }
