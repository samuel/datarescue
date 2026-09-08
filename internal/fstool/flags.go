package fstool

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"

	"samuelks.com/datarescue/ddrescue"
	"samuelks.com/datarescue/diskimage"
	"samuelks.com/datarescue/internal/subcmd"
)

// FlagSet is a flag.FlagSet that remembers which driver each flag belongs to.
//
// The format is not known until the image is open, long after flag.Parse, so
// every driver's flags are registered on every subcommand and one that does not
// apply can only be caught afterwards. It is still the operator's mistake and
// not the medium's, so it is reported as a subcmd.UsageError, the same as
// -geometry given for an image type that has no geometry.
type FlagSet struct {
	*flag.FlagSet
	// owner maps a flag name to the driver that registered it. A flag with no
	// entry here is everybody's.
	owner map[string]string
	// order is the drivers in registration order, so the grouped help and a
	// rejection message read the same way on two runs.
	order []string
}

// NewFlagSet returns a flag set for one subcommand.
func NewFlagSet(name string) *FlagSet {
	return &FlagSet{
		FlagSet: flag.NewFlagSet(name, flag.ExitOnError),
		owner:   make(map[string]string),
	}
}

// Own registers a driver's flags and records them against it.
//
// If two drivers register the same flag name, the flag package panics before
// this sees it, and its message includes the flag, which is what a driver
// author needs.
func (fs *FlagSet) Own(driver string, register func(*FlagSet)) {
	before := make(map[string]struct{})
	fs.VisitAll(func(f *flag.Flag) { before[f.Name] = struct{}{} })

	register(fs)

	fs.order = append(fs.order, driver)
	fs.VisitAll(func(f *flag.Flag) {
		if _, had := before[f.Name]; had {
			return
		}
		fs.owner[f.Name] = driver
	})
}

// Check reports a flag that was given and belongs to a driver other than the
// one the image turned out to hold.
//
// Only flags actually given are considered (flag.Visit walks those rather than
// every registered default). They are checked in name order so two runs report
// the same one first.
func (fs *FlagSet) Check(chosen, image, summary string) error {
	var wrong []string
	fs.Visit(func(f *flag.Flag) {
		if owner, ok := fs.owner[f.Name]; ok && owner != chosen {
			wrong = append(wrong, f.Name)
		}
	})
	if len(wrong) == 0 {
		return nil
	}
	slices.Sort(wrong)
	name := wrong[0]
	return subcmd.Usage(fmt.Errorf("-%s applies only to a %s volume; %s holds %s (-fs names a format outright)",
		name, fs.owner[name], image, summary))
}

// PrintDefaultsGrouped prints the shared flags, then each driver's own under a
// heading. With every driver's flags on every subcommand, a flat -h would be
// too long to read, and the grouping shows which flag belongs to which format.
func (fs *FlagSet) PrintDefaultsGrouped(w io.Writer) {
	byOwner := make(map[string][]*flag.Flag)
	fs.VisitAll(func(f *flag.Flag) {
		byOwner[fs.owner[f.Name]] = append(byOwner[fs.owner[f.Name]], f)
	})
	print := func(flags []*flag.Flag) {
		for _, f := range flags {
			def := ""
			if f.DefValue != "" && f.DefValue != "false" && f.DefValue != "0" {
				def = fmt.Sprintf(" (default %s)", f.DefValue)
			}
			fmt.Fprintf(w, "  -%-14s %s%s\n", f.Name, f.Usage, def) //nolint:errcheck // usage text
		}
	}
	print(byOwner[""])
	for _, d := range fs.order {
		if len(byOwner[d]) == 0 {
			continue
		}
		fmt.Fprintf(w, "\n %s only:\n", d) //nolint:errcheck // usage text
		print(byOwner[d])
	}
}

// CommonFlags are the flags every subcommand takes whatever the format: where
// the filesystem is in the image, and which map to read it through.
type CommonFlags struct {
	Offset    int64
	Partition int
	Geometry  string
	Map       string
	NoMap     bool
	// FS forces a format rather than probing for one.
	FS string
}

// Register adds the common flags. The map flags take ddrescue's own usage
// strings, so they read the same here as in every other tool.
func (cf *CommonFlags) Register(fs *FlagSet, formats string) {
	fs.Int64Var(&cf.Offset, "offset", -1, "byte offset of the filesystem within the image (-1 auto-detects)")
	fs.IntVar(&cf.Partition, "partition", 0, "partition to use (1-based, 0 auto-detects)")
	fs.StringVar(&cf.Geometry, "geometry", "", "SPT:HEADS:BPS for an .86f image, which carries no geometry")
	fs.StringVar(&cf.Map, "map", "", ddrescue.MapUsage)
	fs.BoolVar(&cf.NoMap, "no-map", false, ddrescue.NoMapUsage)
	fs.StringVar(&cf.FS, "fs", "", "filesystem to read rather than probing for one: "+formats)
}

// Is86F reports whether the path is an 86Box floppy image, which is located
// by geometry rather than by offset and has no partition table and no map.
func Is86F(path string) bool {
	return strings.HasSuffix(strings.ToLower(path), ".86f")
}

// Validate rejects the flag combinations that cannot work, before any of the
// image is read. Each is the operator's mistake, so each is a usage error.
func (cf *CommonFlags) Validate(imagePath string) error {
	is86F := Is86F(imagePath)
	switch {
	case cf.Offset >= 0 && cf.Partition != 0:
		return errors.New("-offset and -partition are mutually exclusive")
	case cf.Map != "" && cf.NoMap:
		return errors.New("-map and -no-map are mutually exclusive")
	case cf.Partition < 0:
		return fmt.Errorf("-partition %d: must be 1 or greater, or 0 to auto-detect", cf.Partition)
	case is86F && cf.Map != "":
		return errors.New("-map does not apply to an 86F image, whose sectors are decoded from track data rather than read at a byte offset")
	case is86F && cf.Offset >= 0:
		return errors.New("-offset does not apply to an 86F image, which has no partition table")
	case is86F && cf.Partition != 0:
		return errors.New("-partition does not apply to an 86F image, which has no partition table")
	case !is86F && cf.Geometry != "":
		return errors.New("-geometry applies only to .86f images")
	}
	if cf.Geometry != "" {
		if _, _, _, err := diskimage.ParseGeometry(cf.Geometry); err != nil {
			return err
		}
	}
	return nil
}

// options turns the common flags into the options diskimage.Open takes.
func (cf *CommonFlags) options() diskimage.Options {
	return diskimage.Options{
		Offset:    cf.Offset,
		Partition: cf.Partition,
		Geometry:  cf.Geometry,
		Map:       cf.Map,
		NoMap:     cf.NoMap,
	}
}
