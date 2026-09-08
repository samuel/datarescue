package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"samuelks.com/datarescue/cdrom"
	"samuelks.com/datarescue/cuebin"
	"samuelks.com/datarescue/internal/humanize"
	"samuelks.com/datarescue/internal/subcmd"
	"samuelks.com/datarescue/iso"
)

// maxTrackNumber bounds -session; a CD carries at most 99 tracks, so it cannot
// have more sessions than that.
const maxTrackNumber = 99

func runRip(args []string) int {
	fs := flag.NewFlagSet("rip", flag.ContinueOnError)
	var (
		format  = fs.String("format", "auto", "output format: auto, iso, cuebin")
		session = fs.Uint("session", 0, "session to rip for ISO output (0 = last data session)")
		noMap   = fs.Bool("no-map", false,
			"do not write the ddrescue .map beside the output; a rip zero-fills what it cannot "+
				"read, so without the map those zeros are indistinguishable from data")
		retries = fs.Int("retries", 3,
			"per-sector attempts before marking a sector bad. The device retries a failed read "+
				"a few times of its own before reporting it, so this multiplies that")
		force          = fs.Bool("f", false, "overwrite an existing output image")
		verbose        = fs.Bool("verbose", false, "log each bad sector")
		cookedFallback = fs.Bool("cooked-fallback", false,
			"on a CD, read a sector the low-level command could not through a plain block read "+
				"instead; trades the raw sector's own EDC for the data, and is recorded in "+
				"<output>.cooked.map. Not needed for a DVD, which has no low-level read path")
	)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: disctool rip [flags] <device> <output>\n\n")
		fmt.Fprintf(os.Stderr, "  <device>  CD/DVD device (e.g. /dev/sr0, /dev/disk2)\n")
		fmt.Fprintf(os.Stderr, "  <output>  output path (.iso) or stem for cuebin (.bin+.cue)\n\n")
		fs.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nA session that does not begin at LBA 0 gives an ISO whose own\n"+
			"offsets start at 0 while its internal directory records still hold\n"+
			"absolute disc LBAs; the same limitation applies to a data track\n"+
			"exported with \"cuetool export\".\n")
	}
	if code, ok := parseFlags(fs, args, 2); !ok {
		return code
	}
	devicePath, outputPath := fs.Arg(0), fs.Arg(1)

	// Validate the flags before touching the drive: spinning up a disc only to
	// reject the command line wastes the operator's time, and -retries 0 would
	// make readWithRetry return a nil-data "success".
	if *retries < 1 {
		fmt.Fprintln(os.Stderr, "disctool rip: -retries must be at least 1")
		return subcmd.ExitUsage
	}
	if *session > maxTrackNumber {
		fmt.Fprintf(os.Stderr, "disctool rip: -session %d is out of range (0 = auto, otherwise 1-%d)\n",
			*session, maxTrackNumber)
		return subcmd.ExitUsage
	}
	switch *format {
	case "auto", "iso", "cuebin":
	default:
		fmt.Fprintf(os.Stderr, "disctool rip: unknown -format %q; choose auto, iso, or cuebin\n", *format)
		return subcmd.ExitUsage
	}

	dev, toc, err := openDisc(devicePath)
	if err != nil {
		log.Printf("rip: %v", err)
		return subcmd.ExitFailure
	}
	defer dev.Close() //nolint:errcheck // read-only device; close errors are unactionable at program exit

	printTOC(toc)

	useFormat := *format
	if useFormat == "auto" {
		if toc.HasAudio() {
			useFormat = "cuebin"
		} else {
			useFormat = "iso"
		}
	}
	log.Printf("output format: %s", useFormat)

	// cuebin needs raw sectors, which have no plain-block equivalent, so a
	// medium the low-level CD read command cannot address can never be ripped
	// this way. Reject it here rather than reporting every sector bad.
	if useFormat == "cuebin" && toc.NeedsCookedReads() {
		log.Printf("cuebin output requires CD media, and this disc is not one (%s) -- use -format iso",
			notCDReason(toc))
		return subcmd.ExitFailure
	}

	var st subcmd.Status
	switch useFormat {
	case "iso":
		//nolint:gosec // -session was range-checked against maxTrackNumber above
		st, err = ripISO(dev, toc, outputPath, uint8(*session), *retries, !*noMap, *force, *verbose, *cookedFallback)
	default: // cuebin; the format was validated above
		st, err = ripCueBin(dev, toc, outputPath, *retries, !*noMap, *force, *verbose)
	}
	if err != nil {
		log.Printf("rip: %v", err)
		st.Failed()
	}
	return st.Code()
}

func ripISO(dev cdrom.Device, toc cdrom.TOC, outputPath string, sessionNum uint8, retries int, writeMap, force, verbose, cookedFallback bool) (st subcmd.Status, err error) {
	if toc.HasAudio() {
		log.Printf("warning: disc has audio tracks; they will be skipped in ISO output")
	}

	// A medium the low-level CD commands cannot address has only one read
	// path, so no flag is required. -cooked-fallback covers the other case: a
	// CD whose low-level read failed, where a block read recovers the data but
	// loses the raw sector's EDC.
	cookedOnly := toc.NeedsCookedReads()
	cr, deviceCanCook := dev.(cdrom.CookedFallbackReader)

	var cookedReader cdrom.CookedFallbackReader
	switch {
	case cookedOnly:
		if !deviceCanCook {
			return st, fmt.Errorf("this disc is not CD media (%s), so plain block reads are the only "+
				"way to read it, and this platform's device does not support them", notCDReason(toc))
		}
		cookedReader = cr
	case cookedFallback && deviceCanCook:
		cookedReader = cr
	case cookedFallback:
		log.Printf("warning: -cooked-fallback has no effect: this platform's device does not " +
			"support plain block reads")
	}

	_, firstTrackIdx := toc.LastDataSession()
	if firstTrackIdx < 0 {
		return st, errors.New("no data tracks found on disc")
	}
	// Override if user specified a session.
	if sessionNum != 0 {
		firstTrackIdx = -1
		for i, tr := range toc.Tracks {
			if tr.Session == sessionNum && tr.Type == cdrom.TrackTypeData {
				if firstTrackIdx < 0 || tr.StartLBA < toc.Tracks[firstTrackIdx].StartLBA {
					firstTrackIdx = i
				}
			}
		}
		if firstTrackIdx < 0 {
			return st, fmt.Errorf("no data tracks found in session %d", sessionNum)
		}
		// Walk back to session start.
		for i := firstTrackIdx - 1; i >= 0; i-- {
			if toc.Tracks[i].Session == sessionNum {
				firstTrackIdx = i
			} else {
				break
			}
		}
	}

	startLBA := toc.Tracks[firstTrackIdx].StartLBA
	endLBA := toc.LeadOutLBA

	// If there's a subsequent session, end at its start instead.
	targetSession := toc.Tracks[firstTrackIdx].Session
	for _, tr := range toc.Tracks {
		if tr.Session > targetSession && tr.StartLBA < endLBA {
			endLBA = tr.StartLBA
		}
	}

	total := endLBA - startLBA
	if total <= 0 {
		return st, fmt.Errorf("the disc's data runs from LBA %d to %d, so there is nothing to rip",
			startLBA, endLBA)
	}
	log.Printf("ripping LBA %d–%d (%d sectors, %.1f MB) to %s",
		startLBA, endLBA, total, humanize.MB(total*cdrom.DataSectorSize), outputPath)

	w, err := iso.NewWriter(outputPath, force)
	if err != nil {
		return st, err
	}
	// The map records which sectors are real and which are zero fill. A rip that
	// dies at 90% without one leaves data that cannot be interpreted, so it is
	// written on every exit path, not just after the loop completes.
	defer func() {
		if !writeMap {
			return
		}
		if err := w.WriteMap(outputPath + ".map"); err != nil {
			log.Printf("write map: %v", err)
			st.Failed()
		}
		// The cooked sidecar distinguishes sectors within one image, so it is
		// written only on a CD, where the low-level path applies and any given
		// sector may or may not have fallen back to a block read. On a medium
		// with no low-level path there is nothing to distinguish: the file would
		// duplicate the map above byte for byte and imply verification was
		// skipped rather than unavailable.
		if cookedOnly {
			return
		}
		if err := w.WriteCookedMap(outputPath + ".cooked.map"); err != nil {
			log.Printf("write cooked map: %v", err)
			st.Failed()
		}
	}()

	sr := sectorReader{
		dev:        dev,
		cooked:     cookedReader,
		cookedOnly: cookedOnly,
		retries:    retries,
		verbose:    verbose,
	}

	var badCount, fallbackCount int64
	start := time.Now()
	for lba := startLBA; lba < endLBA; lba++ {
		outLBA := lba - startLBA // ISO is zero-based
		data, viaFallback, err := sr.read(lba)
		switch {
		case err != nil:
			if verbose {
				log.Printf("bad sector LBA %d: %v", lba, err)
			}
			badCount++
			if werr := w.WriteZeroSector(outLBA); werr != nil {
				_ = w.Close() //nolint:errcheck // write already failed; closing to release fd before returning
				return st, werr
			}
		case viaFallback:
			fallbackCount++
			if werr := w.WriteCookedSector(outLBA, data); werr != nil {
				_ = w.Close() //nolint:errcheck // write already failed; closing to release fd before returning
				return st, werr
			}
		default:
			if werr := w.WriteSector(outLBA, data); werr != nil {
				_ = w.Close() //nolint:errcheck // write already failed; closing to release fd before returning
				return st, werr
			}
		}
		printProgress(lba-startLBA+1, total, badCount, cdrom.DataSectorSize, start)
	}
	fmt.Fprintf(os.Stderr, "\n")
	if badCount > 0 {
		st.Degraded()
	}
	if cookedOnly {
		// Every sector came through the medium's only read path, so a fallback
		// count here would equal the sector count and be meaningless.
		log.Printf("done: %d sectors, %d bad", total, badCount)
	} else {
		log.Printf("done: %d sectors, %d bad, %d recovered by block read (not low-level verified)",
			total, badCount, fallbackCount)
	}

	return st, w.Close()
}

// sectorReader is one rip's read policy: which device, whether the low-level
// path applies to this medium at all, and what to do when it fails. Keeping
// these fixed settings here keeps them out of the per-sector call signature.
type sectorReader struct {
	dev        cdrom.Device
	cooked     cdrom.CookedFallbackReader
	cookedOnly bool
	retries    int
	verbose    bool
}

// read returns one data sector for ISO output, and whether it came from a block
// read that stood in for a failed low-level read.
//
// On a medium with no low-level path the low-level attempt is skipped: it would
// fail identically for every sector, which on a DVD is millions of ioctls that
// cannot succeed. Those sectors are not fallbacks either, since block reads are
// the medium's normal read path, so the flag stays false and nothing records
// them as second-class.
func (r sectorReader) read(lba int64) ([]byte, bool, error) {
	if r.cookedOnly && r.cooked != nil {
		data, err := readWithRetry(r.cooked.ReadSectorCooked, lba, r.retries)
		return data, false, err
	}
	data, err := readWithRetry(r.dev.ReadSector, lba, r.retries)
	if err == nil {
		return data, false, nil
	}
	if r.cooked == nil {
		return nil, false, err
	}
	fbData, fbErr := readWithRetry(r.cooked.ReadSectorCooked, lba, r.retries)
	if fbErr != nil {
		return nil, false, err // report the original low-level error
	}
	if r.verbose {
		log.Printf("LBA %d: low-level read failed (%v); recovered by block read "+
			"(not low-level verified)", lba, err)
	}
	return fbData, true, nil
}

func ripCueBin(dev cdrom.Device, toc cdrom.TOC, outputPath string, retries int, writeMap, force, verbose bool) (st subcmd.Status, err error) {
	stem := cuebin.Stem(outputPath)

	var startLBA, endLBA int64
	if len(toc.Tracks) == 0 {
		return st, errors.New("no tracks found on disc")
	}
	startLBA = toc.Tracks[0].StartLBA
	endLBA = toc.LeadOutLBA

	total := endLBA - startLBA
	if total <= 0 {
		return st, fmt.Errorf("the disc's tracks run from LBA %d to %d, so there is nothing to rip",
			startLBA, endLBA)
	}
	log.Printf("ripping LBA %d–%d (%d sectors, %.1f MB) to %s.bin",
		startLBA, endLBA, total, humanize.MB(total*cdrom.RawSectorSize), stem)

	w, err := cuebin.NewWriter(stem, toc, force)
	if err != nil {
		return st, err
	}
	defer func() {
		if writeMap {
			if err := w.WriteMap(stem + ".bin.map"); err != nil {
				log.Printf("write map: %v", err)
				st.Failed()
			}
		}
	}()

	var badCount, pregapCount int64
	start := time.Now()
	for lba := startLBA; lba < endLBA; lba++ {
		trackType := toc.TrackTypeAt(lba)
		readRaw := func(lba int64) ([]byte, error) { return dev.ReadRawSector(lba, trackType) }
		data, readErr := readWithRetry(readRaw, lba, retries)

		var writeErr error
		switch {
		case readErr == nil:
			writeErr = w.WriteRawSector(lba, data)
		case trackType == cdrom.TrackTypeData && toc.InPregap(lba):
			// A pregap that will not read as data may still read as audio. If
			// neither works, zero-fill without marking it bad.
			readAudio := func(lba int64) ([]byte, error) { return dev.ReadRawSector(lba, cdrom.TrackTypeAudio) }
			audioData, audioErr := readWithRetry(readAudio, lba, retries)
			if audioErr == nil {
				if verbose {
					log.Printf("pregap sector LBA %d: recovered in audio mode", lba)
				}
				writeErr = w.WriteRawSector(lba, audioData)
			} else {
				if verbose {
					log.Printf("pregap sector LBA %d: unreadable in both modes", lba)
				}
				pregapCount++
				writeErr = w.WritePregapSector(lba)
			}
		default:
			if verbose {
				log.Printf("bad sector LBA %d: %v", lba, readErr)
			}
			badCount++
			writeErr = w.WriteZeroSector(lba)
		}

		if writeErr != nil {
			_ = w.Close() //nolint:errcheck // write already failed; closing to release fd before returning
			return st, writeErr
		}
		printProgress(lba-startLBA+1, total, badCount, cdrom.RawSectorSize, start)
	}
	fmt.Fprintf(os.Stderr, "\n")
	if badCount > 0 {
		st.Degraded()
	}
	log.Printf("done: %d sectors, %d bad, %d pregap", total, badCount, pregapCount)

	return st, w.Close()
}

// readWithRetry calls read for the same lba up to retries times, returning
// the first success or the last error. It takes the read function itself,
// rather than a Device plus a raw/trackType pair, so one loop can drive
// ReadSector, ReadRawSector or a CookedFallbackReader's ReadSectorCooked, and
// -retries applies to all three the same way.
func readWithRetry(read func(lba int64) ([]byte, error), lba int64, retries int) ([]byte, error) {
	var lastErr error
	for range retries {
		data, err := read(lba)
		if err == nil {
			return data, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// printProgress renders the progress line. sectorSize is the size of a sector
// in this rip's output (2048 for an ISO, 2352 for a BIN); using the wrong one
// would make the throughput figure 15% off on every cuebin rip.
func printProgress(done, total, bad int64, sectorSize int64, start time.Time) {
	if done%256 != 0 && done != total {
		return
	}
	elapsed := time.Since(start).Seconds()
	var rate float64
	if elapsed > 0 {
		rate = float64(done) * float64(sectorSize) / elapsed / 1e6
	}
	var eta string
	if done > 0 && rate > 0 {
		remaining := humanize.MB((total-done)*sectorSize) / rate
		eta = fmt.Sprintf("%.0fs", remaining)
	} else {
		eta = "?"
	}
	fmt.Fprintf(os.Stderr, "\r[%d / %d sectors] bad=%d  %.1f MB/s  ETA=%s    ",
		done, total, bad, rate, eta)
}
