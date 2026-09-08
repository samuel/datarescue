// Command fstool inspects and extracts from the filesystems this toolkit reads
// inside a disk image: FAT, ext2/ext3, ISO 9660 and Apple HFS.
//
// It probes the image for a filesystem rather than making the operator specify
// one. With a directory of rescued images, the operator should not have to
// know or remember each one's format.
package main

import (
	"os"

	"samuelks.com/datarescue/internal/fstool"
	"samuelks.com/datarescue/internal/fstool/extdrv"
	"samuelks.com/datarescue/internal/fstool/fatdrv"
	"samuelks.com/datarescue/internal/fstool/hfsdrv"
	"samuelks.com/datarescue/internal/fstool/isodrv"
	"samuelks.com/datarescue/internal/subcmd"
)

const tool = "fstool"

// drivers is the registry, and the order the probe tries them in.
//
// It lives here rather than in internal/fstool because each driver package
// imports internal/fstool, so putting the list there would be an import cycle.
//
// Drivers are ordered by how specific their format's signature is. "CD001"
// five bytes into a fixed sector cannot plausibly be anything else. HFS's 'BD'
// and ext2's 0xEF53 are each checked against the geometry they imply. FAT
// accepts the loosest evidence, a boot sector with a plausible parameter block,
// so it goes last as the fallback. A hybrid disc with both an ISO 9660 and an
// HFS volume is therefore reported as ISO, and -fs hfs reaches the HFS side.
func drivers() []fstool.Driver {
	return []fstool.Driver{isodrv.New(), hfsdrv.New(), extdrv.New(), fatdrv.New()}
}

func main() {
	// Operator-facing messages carry no timestamp, level or key-value pairs,
	// since they are addressed to the person who ran the tool. Structured output
	// is Diagnostics, returned as data.
	os.Exit(subcmd.Run(tool, "<image-file>", commands()))
}

func commands() []subcmd.Command {
	var list fstool.ListFlags
	var extract fstool.ExtractFlags
	var verifyVerbose, orphansVerbose bool

	defs := []fstool.Command{
		{
			Name: "info", Usage: "<image-file>", Summary: "show filesystem information",
			MinArgs: 1, MaxArgs: 1,
			Run: fstool.Info,
		},
		{
			Name: "ls", Usage: "<image-file> [path-in-image ...]", Summary: "list files",
			MinArgs: 1, MaxArgs: -1,
			Setup: list.Register,
			Run:   func(c *fstool.Context) int { return fstool.List(c, &list) },
		},
		{
			Name: "cat", Usage: "<image-file> <path-in-image>",
			Summary: "write one file's contents to stdout",
			MinArgs: 2, MaxArgs: 2,
			Run: fstool.Cat,
		},
		{
			Name: "extract", Usage: "<image-file> [path-in-image ...]",
			Summary: "extract files to a directory or an archive",
			MinArgs: 1, MaxArgs: -1,
			Setup: extract.Register,
			Run:   func(c *fstool.Context) int { return fstool.Extract(c, &extract) },
		},
		{
			Name: "verify", Usage: "<image-file>",
			Summary: "check the filesystem and read every file through",
			MinArgs: 1, MaxArgs: 1,
			Setup: func(fs *fstool.FlagSet) {
				fs.BoolVar(&verifyVerbose, "v", false, "print each file as it is checked")
			},
			Run: func(c *fstool.Context) int { return fstool.Verify(c, verifyVerbose) },
		},
		{
			Name: "orphans", Usage: "<image-file>",
			Summary: "list data allocated but named by no directory entry",
			MinArgs: 1, MaxArgs: 1,
			Setup: func(fs *fstool.FlagSet) {
				fs.BoolVar(&orphansVerbose, "v", false, "list the contents of an orphaned directory")
			},
			Run: func(c *fstool.Context) int { return fstool.Orphans(c, orphansVerbose) },
		},
	}

	cmds := make([]subcmd.Command, 0, len(defs))
	for _, d := range defs {
		cmds = append(cmds, subcmd.Command{
			Name:    d.Name,
			Summary: d.Summary,
			Run:     fstool.Dispatch(tool, d, drivers),
		})
	}
	return cmds
}
