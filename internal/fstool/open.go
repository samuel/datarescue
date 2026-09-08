package fstool

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"samuelks.com/datarescue/diskimage"
	"samuelks.com/datarescue/internal/subcmd"
)

// prober is the Accept predicate diskimage.Open takes, asked about one
// candidate offset at a time.
//
// It is one predicate over every driver rather than one diskimage.Open per
// driver. Four opens would read the partition table four times and, worse,
// would let driver order override partition order, so a FAT partition late in
// the table would win over an ext2 partition early in it. Here the first
// offset any driver claims wins, which is the rule diskimage already applies
// with a single-format Accept.
type prober struct {
	drivers []Driver
	hit     Driver
	hint    any
	// damaged is every driver that reported the bytes it needed were never
	// recovered, kept so a detection that finds nothing can say why rather than
	// "no filesystem found".
	damaged []probeDamage
}

type probeDamage struct {
	driver Driver
	off    int64
	err    error
}

func (p *prober) accept(r io.ReadSeeker, off int64) bool {
	for _, d := range p.drivers {
		switch v := d.Probe(r, off); v.Kind {
		case Yes:
			p.hit, p.hint = d, v.Hint
			return true
		case Damaged:
			p.damaged = append(p.damaged, probeDamage{driver: d, off: off, err: v.Err})
		case No:
		}
	}
	return false
}

// Detect locates the volume and the driver that reads it. force is the -fs
// value, empty to probe.
//
// The image is opened once. On failure the returned image is nil, but a
// diskimage.NotFoundError carries the map, so a caller can still report what
// the image lost, which matters most on the damaged images this tool is for.
func Detect(path string, cf *CommonFlags, drivers []Driver) (Driver, *diskimage.Image, any, error) {
	if err := cf.Validate(path); err != nil {
		return nil, nil, nil, subcmd.Usage(err)
	}

	if cf.FS != "" {
		d := driverNamed(drivers, cf.FS)
		if d == nil {
			return nil, nil, nil, subcmd.Usage(fmt.Errorf("-fs %q: unknown filesystem; %s", cf.FS, formatList(drivers)))
		}
		opts := cf.options()
		// With an offset or partition given, the operator has said where the
		// volume is and the probe could only disagree. Without one, the probe
		// locates the volume, narrowed to the partition types this format uses.
		if cf.Offset < 0 && cf.Partition == 0 {
			opts.Accept = func(r io.ReadSeeker, off int64) bool { return d.Probe(r, off).Kind == Yes }
			opts.PartitionMatches = d.PartitionMatches
			if m, ok := d.(APMMatcher); ok {
				opts.APMPartitionMatches = m.APMPartitionMatches
			}
		}
		im, err := diskimage.Open(path, opts)
		if err != nil {
			if errors.Is(err, diskimage.ErrNotFound) {
				// -fs specified the format, so say that this image does not
				// hold one rather than repeating diskimage's generic answer.
				return d, nil, nil, fmt.Errorf("%s: no %s volume at offset 0 or in the partition table: %w",
					path, d.Name(), err)
			}
			return d, nil, nil, wrapOpen(path, err)
		}
		return d, im, nil, nil
	}

	p := &prober{drivers: drivers}
	opts := cf.options()
	if cf.Offset < 0 && cf.Partition == 0 {
		opts.Accept = p.accept
		// No PartitionMatches: across every driver it would be "every used
		// partition", and with every driver probing, the probe decides rather
		// than the type byte.
	}
	im, err := diskimage.Open(path, opts)
	if err != nil {
		// A driver that could not decide outranks "found nothing": the bytes
		// it needed are gone, and saying no filesystem is here would be a
		// different and much worse claim.
		if len(p.damaged) > 0 {
			d := p.damaged[0]
			// The map explains why the filesystem could not be located, so it
			// is kept with the verdict rather than dropped with diskimage's
			// answer.
			return nil, nil, nil, fmt.Errorf("%s: could not determine whether this is %s: %w: %w",
				path, d.driver.Name(), d.err, err)
		}
		// No driver claimed any offset diskimage offered. That is the probe's
		// answer, not diskimage's, so say what the operator can do about it and
		// keep the underlying error so a caller can still reach the map.
		if errors.Is(err, diskimage.ErrNotFound) {
			return nil, nil, nil, fmt.Errorf("%s: %w; %s", path, err, hint(drivers))
		}
		return nil, nil, nil, wrapOpen(path, err)
	}
	if p.hit == nil {
		// An explicit -offset or -partition skips the probe, so nothing has
		// identified the format yet. Ask each driver about the given offset.
		for _, d := range p.drivers {
			switch v := d.Probe(im.Reader, im.Offset); v.Kind {
			case Yes:
				return d, im, v.Hint, nil
			case Damaged:
				im.Close() //nolint:errcheck // read-only
				return nil, nil, nil, fmt.Errorf("%s: could not determine whether this is %s: %w",
					path, d.Name(), v.Err)
			case No:
			}
		}
		im.Close() //nolint:errcheck // read-only
		return nil, nil, nil, fmt.Errorf("%s: no %s volume at %s; %s",
			path, orList(driverNames(drivers)), im.Source, hint(drivers))
	}
	return p.hit, im, p.hint, nil
}

// wrapOpen turns diskimage's failures into the tool's. A partition number the
// table does not hold is the operator asking for something that is not there,
// which is a usage error however late it is noticed. Everything else is a
// problem with the image.
func wrapOpen(path string, err error) error {
	switch {
	case errors.Is(err, diskimage.ErrNoSuchPartition):
		return subcmd.Usage(err)
	case errors.Is(err, diskimage.ErrNotFound):
		return fmt.Errorf("%s: %w", path, err)
	}
	return err
}

// hint adds a line to "nothing found" that the operator can act on. A probe
// that finds nothing on a damaged image is common and recoverable, so the
// message lists the flags that help.
func hint(drivers []Driver) string {
	return fmt.Sprintf("-fs %s names a format outright, and -offset or -partition says where to look",
		Formats(drivers))
}

func driverNamed(drivers []Driver, name string) Driver {
	for _, d := range drivers {
		if d.Name() == name {
			return d
		}
	}
	return nil
}

func driverNames(drivers []Driver) []string {
	names := make([]string, len(drivers))
	for i, d := range drivers {
		names[i] = d.Summary()
	}
	return names
}

// Formats is the -fs help text: the driver names, in the order they are tried.
func Formats(drivers []Driver) string {
	names := make([]string, len(drivers))
	for i, d := range drivers {
		names[i] = d.Name()
	}
	return strings.Join(names, "|")
}

func formatList(drivers []Driver) string {
	return "known filesystems are " + Formats(drivers)
}

func orList(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " or " + items[len(items)-1]
}

// Context is what a subcommand is handed once the volume is open.
type Context struct {
	Driver Driver
	Volume Volume
	Image  *diskimage.Image
	// Path is the image path as the operator gave it, for messages.
	Path string
	// Args are the positional arguments after the image.
	Args  []string
	Flags *FlagSet
	// Tool and Command identify the run, for message prefixes.
	Tool, Command string
}

// Errorf prints one operator-facing message, prefixed the way every tool here
// prefixes them.
func (c *Context) Errorf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "%s %s: %s\n", c.Tool, c.Command, fmt.Sprintf(format, args...)) //nolint:errcheck // stderr
}

// Command is one of the tool's subcommands, written once over the driver
// interface.
type Command struct {
	Name    string
	Usage   string
	Summary string
	// MinArgs and MaxArgs bound the positional arguments after the subcommand,
	// the image included. MaxArgs below zero means no upper bound.
	MinArgs, MaxArgs int
	// Setup registers the subcommand's own flags.
	Setup func(*FlagSet)
	Run   func(*Context) int
}

// Dispatch turns a Command into the function subcmd.Run takes, wrapping it in
// the preamble every subcommand shares: parse, validate, detect, reject a flag
// that cannot apply, open, drain what opening noticed, run.
func Dispatch(tool string, cmd Command, drivers func() []Driver) func([]string) int {
	return func(args []string) int {
		ds := drivers()
		fset := NewFlagSet(cmd.Name)
		var cf CommonFlags
		cf.Register(fset, Formats(ds))
		if cmd.Setup != nil {
			cmd.Setup(fset)
		}
		for _, d := range ds {
			fset.Own(d.Name(), d.Flags)
		}
		fset.Usage = func() {
			fmt.Fprintf(os.Stderr, "Usage: %s %s [flags] %s\n", tool, cmd.Name, cmd.Usage) //nolint:errcheck // usage
			fset.PrintDefaultsGrouped(os.Stderr)
		}
		_ = fset.Parse(args)
		if fset.NArg() < cmd.MinArgs || (cmd.MaxArgs >= 0 && fset.NArg() > cmd.MaxArgs) {
			fset.Usage()
			return subcmd.ExitUsage
		}

		path := fset.Arg(0)
		drv, im, hint, err := Detect(path, &cf, ds)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s %s: %s\n", tool, cmd.Name, err) //nolint:errcheck // stderr
			// The map explains why a volume could not be located, so it is
			// still reported. This is the case the map exists for.
			var nf *diskimage.NotFoundError
			if errors.As(err, &nf) && nf.MapPath != "" {
				printMapInfo(os.Stderr, nf.MapPath, nf.Blocks, 0, nf.Size, "")
			}
			return subcmd.ExitFor(err)
		}
		defer im.Close() //nolint:errcheck // read-only

		// A flag belonging to another format is refused before the volume is
		// opened, so nothing is half-honored.
		if err := fset.Check(drv.Name(), path, drv.Summary()); err != nil {
			fmt.Fprintf(os.Stderr, "%s %s: %s\n", tool, cmd.Name, err) //nolint:errcheck // stderr
			return subcmd.ExitFor(err)
		}

		vol, err := drv.Open(im, hint)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s %s: opening %s at %s: %s\n", tool, cmd.Name, drv.Name(), im.Source, err) //nolint:errcheck // stderr
			return subcmd.ExitFor(err)
		}
		defer vol.Close() //nolint:errcheck // read-only

		c := &Context{
			Driver: drv, Volume: vol, Image: im, Path: path,
			Args: fset.Args()[1:], Flags: fset, Tool: tool, Command: cmd.Name,
		}
		// Drained here rather than at the first read: Diagnostics clears the
		// list, and one left pending would be reported against whichever file
		// is read next.
		for _, d := range vol.Diagnostics() {
			c.Errorf("%s", d)
		}
		return cmd.Run(c)
	}
}
