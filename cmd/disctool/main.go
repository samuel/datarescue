// Command disctool rips a CD or DVD to an ISO or CUE/BIN image, recording the
// sectors that would not read in a ddrescue map beside it.
package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"os"

	"samuelks.com/datarescue/cdrom"
	"samuelks.com/datarescue/internal/subcmd"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("disctool: ")

	os.Exit(subcmd.Run("disctool", "<device>", []subcmd.Command{
		{Name: "info", Summary: "show the disc's table of contents and medium type", Run: runInfo},
		{Name: "rip", Summary: "rip the disc to an ISO or CUE/BIN image", Run: runRip},
	}))
}

// openDevice is replaced in tests so a rip can run without a drive.
var openDevice = cdrom.Open

// parseFlags parses args, then requires exactly nargs positional arguments.
// The flag sets here use ContinueOnError rather than the ExitOnError the other
// tools use, so a malformed flag returns a status the caller can test instead
// of ending the process.
func parseFlags(fs *flag.FlagSet, args []string, nargs int) (int, bool) {
	if err := fs.Parse(args); err != nil {
		// Parse has already printed the reason and the usage.
		if errors.Is(err, flag.ErrHelp) {
			return subcmd.ExitOK, false
		}
		return subcmd.ExitUsage, false
	}
	if fs.NArg() != nargs {
		fs.Usage()
		return subcmd.ExitUsage, false
	}
	return subcmd.ExitOK, true
}

// openDisc opens the drive and reads its table of contents, the two steps every
// subcommand starts with.
func openDisc(path string) (cdrom.Device, cdrom.TOC, error) {
	dev, err := openDevice(path)
	if err != nil {
		return nil, cdrom.TOC{}, fmt.Errorf("open device: %w", err)
	}
	log.Printf("reading table of contents from %s", path)
	toc, err := dev.TOC()
	if err != nil {
		_ = dev.Close() //nolint:errcheck // read-only device, and the TOC read has already failed
		return nil, cdrom.TOC{}, fmt.Errorf("read TOC: %w", err)
	}
	return dev, toc, nil
}

// printTOC reports the disc's layout, including the medium type. A disc the
// low-level CD commands cannot address is read through a different path, and
// an operator should learn that before starting a rip rather than from the
// summary afterwards.
func printTOC(toc cdrom.TOC) {
	// FirstTrack and LastTrack are track numbers, so the session range has to
	// come from the tracks themselves.
	firstSession, lastSession := uint8(0), uint8(0)
	for _, tr := range toc.Tracks {
		if firstSession == 0 || tr.Session < firstSession {
			firstSession = tr.Session
		}
		lastSession = max(lastSession, tr.Session)
	}
	log.Printf("tracks: %d (numbered %d-%d), sessions: %d-%d, lead-out LBA: %d",
		len(toc.Tracks), toc.FirstTrack, toc.LastTrack, firstSession, lastSession, toc.LeadOutLBA)
	for _, tr := range toc.Tracks {
		log.Printf("  track %02d  session=%d  type=%-5s  lba=%d",
			tr.Number, tr.Session, tr.Type, tr.StartLBA)
	}
	if toc.NeedsCookedReads() {
		log.Printf("medium: not a CD (%s); read as 2048-byte sectors through plain block reads",
			notCDReason(toc))
		return
	}
	log.Printf("medium: CD")
}

// notCDReason reports which of the two signals decided the medium is not a CD,
// so the operator can tell a lead-out measured past CD range from a device
// node that answered no CD command at all.
func notCDReason(toc cdrom.TOC) string {
	if toc.NotCD {
		return "the device reports non-CD media"
	}
	return fmt.Sprintf("lead-out LBA %d exceeds CD range (max %d), so this is a DVD",
		toc.LeadOutLBA, cdrom.MaxLBA)
}

func runInfo(args []string) int {
	fs := flag.NewFlagSet("info", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: disctool info <device>\n\n")
		fmt.Fprintf(os.Stderr, "  <device>  CD/DVD device (e.g. /dev/sr0, /dev/disk2)\n\n")
		fs.PrintDefaults()
	}
	if code, ok := parseFlags(fs, args, 1); !ok {
		return code
	}

	dev, toc, err := openDisc(fs.Arg(0))
	if err != nil {
		log.Printf("info: %v", err)
		return subcmd.ExitFailure
	}
	defer dev.Close() //nolint:errcheck // read-only device; close errors are unactionable at program exit

	printTOC(toc)
	return subcmd.ExitOK
}
