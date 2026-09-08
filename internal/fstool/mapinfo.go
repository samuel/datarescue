package fstool

import (
	"fmt"
	"io"

	"samuelks.com/datarescue/ddrescue"
	"samuelks.com/datarescue/internal/humanize"
)

// PrintMapInfo writes the two or three lines that say which rescue map the
// image was read through and what it left unrecovered.
//
// hint suggests the command that would attribute the loss to files, and is
// empty where there is none.
func PrintMapInfo(w io.Writer, c *Context, hint string) {
	printMapInfo(w, c.Image.MapPath, c.Image.Blocks, c.Image.Offset, c.Volume.Bytes(), hint)
}

// printMapInfo takes the pieces rather than a Context, so the same lines can be
// printed for an image whose filesystem was never located. There is no volume
// to ask then, and the map is the only explanation of why.
func printMapInfo(w io.Writer, mapPath string, blocks []ddrescue.Block, off, size int64, hint string) {
	fmt.Fprintln(w) //nolint:errcheck // report output
	if mapPath == "" {
		fmt.Fprintln(w, "Rescue map:      none (the image is read as it is)") //nolint:errcheck // report output
		return
	}
	fmt.Fprintf(w, "Rescue map:      %s\n", mapPath) //nolint:errcheck // report output
	lost := unrecoveredBytes(blocks, off, size)
	if lost == 0 {
		fmt.Fprintln(w, "Unrecovered:     none of the volume") //nolint:errcheck // report output
		return
	}
	if hint == "" {
		fmt.Fprintf(w, "Unrecovered:     %s\n", humanize.Bytes(lost)) //nolint:errcheck // report output
		return
	}
	fmt.Fprintf(w, "Unrecovered:     %s — %s\n", humanize.Bytes(lost), hint) //nolint:errcheck // report output
}

// unrecoveredBytes is how many of the volume's bytes the map does not mark
// finished.
//
// The range is measured from where the filesystem starts, not from the start of
// the image. On a partitioned image those differ, and counting from 0 would
// report damage in whatever precedes the partition as this filesystem's.
// Without a map nothing is known to be missing.
func unrecoveredBytes(blocks []ddrescue.Block, off, size int64) int64 {
	if len(blocks) == 0 || size <= 0 {
		return 0
	}
	var lost int64
	for _, b := range ddrescue.Unrecovered(blocks, off, size) {
		lost += b.Size
	}
	return lost
}

// VolumeUnrecovered is how many of the volume's bytes were never recovered.
func VolumeUnrecovered(c *Context) int64 {
	return unrecoveredBytes(c.Image.Blocks, c.Image.Offset, c.Volume.Bytes())
}
