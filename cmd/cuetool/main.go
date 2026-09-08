package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"samuelks.com/datarescue/cuebin"
	"samuelks.com/datarescue/ddrescue"
	"samuelks.com/datarescue/internal/humanize"
	"samuelks.com/datarescue/internal/subcmd"
	"samuelks.com/datarescue/internal/xio"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("cuetool: ")

	os.Exit(subcmd.Run("cuetool", "<file.cue>", []subcmd.Command{
		{Name: "verify", Summary: "verify the structural integrity of a CUE/BIN rip", Run: runVerify},
		{Name: "export", Summary: "export tracks from a CUE/BIN image to ISO/WAV files", Run: runExport},
	}))
}

// ---- verify subcommand ----

func runVerify(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	var src ddrescue.Flags
	src.Register(fs)
	var (
		full    = fs.Bool("full", false, "read and verify every file's data (slow)")
		verbose = fs.Bool("v", false, "verbose per-sector output")
		jsonOut = fs.Bool("json", false, "output JSON report")
	)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: cuetool verify [flags] <file.cue>")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)

	if fs.NArg() != 1 {
		fs.Usage()
		return subcmd.ExitUsage
	}

	code, err := verify(fs.Arg(0), &src, *full, *verbose, *jsonOut)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cuetool verify:", err)
		return subcmd.ExitFor(err)
	}
	return code
}

// loadSheet parses the cue sheet and returns the path of the BIN it describes.
// The rescue map is resolved against the BIN, never against the cue, since the
// cue is a text file nothing was rescued from.
func loadSheet(cuePath string) (*cuebin.Sheet, *cuebin.CueFile, string, error) {
	f, err := os.Open(cuePath)
	if err != nil {
		return nil, nil, "", fmt.Errorf("open cue: %w", err)
	}
	sheet, err := cuebin.ParseSheet(f)
	f.Close() //nolint:errcheck // read-only
	if err != nil {
		return nil, nil, "", fmt.Errorf("parse cue: %w", err)
	}
	if len(sheet.Files) == 0 {
		return nil, nil, "", errors.New("cue sheet has no FILE entries")
	}
	if len(sheet.Files) > 1 {
		return nil, nil, "", errors.New("multi-file cue sheets are not supported")
	}
	cf := &sheet.Files[0]
	return sheet, cf, cuebin.BinPath(cuePath, cf.Path), nil
}

func verify(cuePath string, srcFlags *ddrescue.Flags, full, verbose, jsonOut bool) (int, error) {
	sheet, cueFile, binPath, err := loadSheet(cuePath)
	if err != nil {
		return subcmd.ExitFailure, err
	}
	if err := srcFlags.Validate(); err != nil {
		return subcmd.ExitFailure, subcmd.Usage(err)
	}
	src, err := cuebin.OpenSource(binPath, srcFlags.Options())
	if err != nil {
		return subcmd.ExitFailure, err
	}
	defer src.Close() //nolint:errcheck // read-only

	vr, err := cuebin.VerifyBIN(src, cueFile.Tracks)
	if err != nil {
		return subcmd.ExitFailure, fmt.Errorf("verify: %w", err)
	}

	fsr := cuebin.CheckFilesystem(src, cueFile.Tracks, vr)
	if full {
		cuebin.DeepVerifyFiles(src, cueFile.Tracks, fsr)
	}

	bounds, err := cuebin.TrackBounds(sheet, src.Size)
	if err != nil {
		return subcmd.ExitFailure, fmt.Errorf("track bounds: %w", err)
	}
	sectorSize, err := cuebin.BinSectorSize(cueFile.Tracks)
	if err != nil {
		return subcmd.ExitFailure, err
	}

	var audioResults []*cuebin.AudioResult
	for i, tr := range cueFile.Tracks {
		if !cuebin.IsAudio(tr.Mode) {
			continue
		}
		audioResults = append(audioResults, cuebin.AnalyzeAudioTrack(src, tr.Number, bounds[i][0], bounds[i][1], sectorSize, vr))
	}

	if jsonOut {
		if err := printJSON(binPath, src.MapPath, vr, fsr, audioResults); err != nil {
			// Failing to write to stdout is a run failure, not a usage error.
			return subcmd.ExitFailure, err
		}
		return verifyExitCode(vr, fsr), nil
	}

	printReport(binPath, src.MapPath, cueFile.Tracks, vr, fsr, audioResults, verbose)
	return verifyExitCode(vr, fsr), nil
}

// discModeString describes what the BIN holds, for the report's header line. It
// is based on the track modes, not the sector size: an audio-only disc has no
// MODE1 sectors even though its stride is 2352 bytes.
func discModeString(tracks []cuebin.CueTrack) string {
	for _, tr := range tracks {
		if !cuebin.IsAudio(tr.Mode) {
			return tr.Mode
		}
	}
	if len(tracks) > 0 {
		return "AUDIO"
	}
	return "no tracks"
}

// verifyExitCode returns the exit code for a verify run. Everything it weighs
// is a finding about the medium (a broken sync pattern, a sector that would not
// read, a catalog that will not parse), so any of it yields ExitDegraded, and
// the report states the severity as CLEAN, DEGRADED or FAILED. It never
// returns ExitUsage, since a wrapper reads that as a mistyped command line.
func verifyExitCode(vr *cuebin.VerifyResult, fsr *cuebin.FSResult) int {
	var st subcmd.Status
	switch {
	// A file that is not a whole number of sectors is the first thing the
	// report prints, and it must affect the exit code too: a BIN cut
	// mid-sector is a short rip, not a formatting quirk.
	case vr.SizeUnaligned || vr.BadSync > 0 || vr.BadMSF > 0 || vr.Unreadable > 0:
		st.Degraded()
	// Filesystem damage must count: a disc whose catalog will not parse yields
	// no affected files and would otherwise be reported clean.
	case fsr.AffectedFiles > 0 || vr.LostSectors() > 0 || len(fsr.Issues) > 0:
		st.Degraded()
	case fsr.Type != cuebin.FSUnknown && len(fsr.Files) == 0 && fsr.DeclaredSectors == 0:
		st.Degraded()
	}
	return st.Code()
}

func printReport(binPath, mapFile string, tracks []cuebin.CueTrack, vr *cuebin.VerifyResult, fsr *cuebin.FSResult, audio []*cuebin.AudioResult, verbose bool) {
	fmt.Printf("%s  %d sectors  %s\n", filepath.Base(binPath), vr.TotalSectors, discModeString(tracks))
	if mapFile != "" {
		fmt.Printf("%s  %d bad sectors\n", filepath.Base(mapFile), vr.BadMap)
	}
	if vr.SizeUnaligned {
		fmt.Printf("WARNING: file size is not a multiple of %d bytes\n", vr.SectorSize)
	}
	fmt.Println()

	fmt.Println("Structural")
	printCheck("sync headers", vr.TotalSectors-vr.BadSync, vr.TotalSectors)
	printCheck("MSF sequence", vr.TotalSectors-vr.BadMSF, vr.TotalSectors)

	edcOK := vr.TotalSectors - vr.BadEDC
	if vr.BadEDC == 0 {
		printCheck("EDC", vr.TotalSectors, vr.TotalSectors)
	} else {
		var suffix string
		if mapFile != "" {
			switch {
			case vr.EDCBadMapGood > 0 && vr.EDCGoodMapBad > 0:
				suffix = fmt.Sprintf("  (%d fail; %d corrupt but map says recovered, %d map says bad but EDC intact)",
					vr.BadEDC, vr.EDCBadMapGood, vr.EDCGoodMapBad)
			case vr.EDCBadMapGood > 0:
				suffix = fmt.Sprintf("  (%d fail; %d corrupt but map says recovered)", vr.BadEDC, vr.EDCBadMapGood)
			case vr.EDCGoodMapBad > 0:
				suffix = fmt.Sprintf("  (%d fail; %d map says bad but EDC intact — stale map?)", vr.BadEDC, vr.EDCGoodMapBad)
			default:
				suffix = fmt.Sprintf("  (%d fail, all agree with map)", vr.BadEDC)
			}
		} else {
			suffix = fmt.Sprintf("  (%d fail)", vr.BadEDC)
		}
		fmt.Printf("  %-14s %d/%d%s\n", "EDC:", edcOK, vr.TotalSectors, suffix)
	}
	if vr.Unreadable > 0 {
		fmt.Printf("  %-14s %d sector(s) could not be read from the BIN\n", "unreadable:", vr.Unreadable)
	}

	if verbose {
		fmt.Println()
		fmt.Println("Per-sector detail:")
		for i, s := range vr.Sectors {
			if s.BadSync || s.BadMSF || s.BadEDC || s.MapBad {
				var issues []string
				if s.BadSync {
					issues = append(issues, "sync")
				}
				if s.BadMSF {
					issues = append(issues, "msf")
				}
				if s.BadEDC {
					issues = append(issues, "edc")
				}
				if s.MapBad {
					issues = append(issues, "map")
				}
				fmt.Printf("  sector %6d: %s\n", i, strings.Join(issues, ","))
			}
		}
	}

	fmt.Println()
	if fsr.Type != cuebin.FSUnknown {
		fmt.Printf("Filesystem: %s %q  %d sectors declared\n",
			fsr.Type, fsr.VolumeLabel, fsr.DeclaredSectors)
		fmt.Printf("  %d files, %d affected:\n", len(fsr.Files), fsr.AffectedFiles)
		for _, is := range fsr.Issues {
			fmt.Printf("  ! %s\n", is)
		}
		for i := range fsr.Files {
			fe := &fsr.Files[i]
			if len(fe.BadSectors) == 0 {
				continue
			}
			status := "PARTIAL"
			if fe.FullyLost() {
				status = "UNRECOVERABLE"
			}
			first := fe.BadSectors[0]
			last := fe.BadSectors[len(fe.BadSectors)-1]
			fmt.Printf("    %-13s  %-40s  sectors %d–%d (%d bad)\n",
				status, fe.Path, first, last, len(fe.BadSectors))
		}
	} else {
		fmt.Println("Filesystem: unknown")
	}

	if len(audio) > 0 {
		fmt.Println()
		fmt.Println("Audio tracks:")
		for _, ar := range audio {
			dur := ar.DurationSeconds()
			mins := int(dur) / 60
			sec := dur - float64(mins*60)
			var pct float64
			if ar.TotalSectors > 0 {
				pct = float64(ar.BadSectors) / float64(ar.TotalSectors) * 100
			}
			fmt.Printf("  Track %02d  %d:%05.2f  %d/%d sectors bad (%.1f%%)\n",
				ar.TrackNumber, mins, sec, ar.BadSectors, ar.TotalSectors, pct)
			if ar.UnreadableSectors > 0 {
				fmt.Printf("    %d sector(s) would not read and were not analyzed\n", ar.UnreadableSectors)
			}
			for _, sr := range ar.SilentRuns {
				fmt.Printf("    silence at LBA %d: %d samples (~%.0fms)\n",
					sr.StartLBA, sr.Samples, float64(sr.Samples)/88.2)
			}
			if len(ar.RepeatedSectors) > 0 {
				fmt.Printf("    %d repeated sectors detected\n", len(ar.RepeatedSectors))
			}
		}
	}

	fmt.Println()
	totalBad := vr.LostSectors()
	switch {
	case vr.Unreadable > 0:
		fmt.Printf("Result: FAILED — %d sector(s) could not be read from the BIN\n", vr.Unreadable)
	case vr.BadSync > 0 || vr.BadMSF > 0:
		fmt.Printf("Result: FAILED — structural errors in the BIN\n")
	case fsr.AffectedFiles > 0:
		fmt.Printf("Result: DEGRADED — %d sectors lost, %d file(s) unrecoverable\n",
			totalBad, fsr.AffectedFiles)
	case totalBad > 0:
		fmt.Printf("Result: DEGRADED — %d sectors lost\n", totalBad)
	case len(fsr.Issues) > 0:
		fmt.Printf("Result: DEGRADED — filesystem damage: %s\n", fsr.Issues[0])
	case fsr.Type != cuebin.FSUnknown && len(fsr.Files) == 0 && fsr.DeclaredSectors == 0:
		fmt.Printf("Result: UNKNOWN FILESYSTEM — detected %s but traversal not implemented\n", fsr.Type)
	default:
		fmt.Println("Result: CLEAN")
	}
}

func printCheck(name string, ok, total int64) {
	if ok == total {
		fmt.Printf("  %-14s %d/%d ✓\n", name+":", ok, total)
	} else {
		fmt.Printf("  %-14s %d/%d  (%d fail)\n", name+":", ok, total, total-ok)
	}
}

type jsonReport struct {
	BinFile      string          `json:"bin_file"`
	MapFile      string          `json:"map_file,omitempty"`
	TotalSectors int64           `json:"total_sectors"`
	SectorSize   int             `json:"sector_size"`
	Structural   jsonStructural  `json:"structural"`
	Filesystem   *jsonFilesystem `json:"filesystem,omitempty"`
	AudioTracks  []jsonAudio     `json:"audio_tracks,omitempty"`
}

type jsonStructural struct {
	BadSync       int64 `json:"bad_sync"`
	BadMSF        int64 `json:"bad_msf"`
	BadEDC        int64 `json:"bad_edc"`
	BadMap        int64 `json:"bad_map"`
	Unreadable    int64 `json:"unreadable"`
	EDCBadMapGood int64 `json:"edc_bad_map_good"`
	EDCGoodMapBad int64 `json:"edc_good_map_bad"`
}

type jsonFilesystem struct {
	Type            string     `json:"type"`
	VolumeLabel     string     `json:"volume_label"`
	DeclaredSectors int64      `json:"declared_sectors"`
	TotalFiles      int        `json:"total_files"`
	AffectedFiles   int        `json:"affected_files"`
	Issues          []string   `json:"issues,omitempty"`
	Files           []jsonFile `json:"files,omitempty"`
}

type jsonFile struct {
	Path       string  `json:"path"`
	StartLBA   int64   `json:"start_lba"`
	EndLBA     int64   `json:"end_lba"`
	BadSectors []int64 `json:"bad_sectors,omitempty"`
}

type jsonAudio struct {
	TrackNumber     int             `json:"track"`
	StartLBA        int64           `json:"start_lba"`
	EndLBA          int64           `json:"end_lba"`
	DurationSec     float64         `json:"duration_sec"`
	BadSectors      int64           `json:"bad_sectors"`
	Unreadable      int64           `json:"unreadable_sectors,omitempty"`
	RepeatedSectors []int64         `json:"repeated_sectors,omitempty"`
	SilentRuns      []jsonSilentRun `json:"silent_runs,omitempty"`
}

type jsonSilentRun struct {
	StartLBA int64 `json:"start_lba"`
	Samples  int64 `json:"samples"`
}

func printJSON(binPath, mapFile string, vr *cuebin.VerifyResult, fsr *cuebin.FSResult, audio []*cuebin.AudioResult) error {
	var mapBase string
	if mapFile != "" {
		mapBase = filepath.Base(mapFile)
	}
	report := jsonReport{
		BinFile:      filepath.Base(binPath),
		MapFile:      mapBase,
		TotalSectors: vr.TotalSectors,
		SectorSize:   vr.SectorSize,
		Structural: jsonStructural{
			BadSync:       vr.BadSync,
			BadMSF:        vr.BadMSF,
			BadEDC:        vr.BadEDC,
			BadMap:        vr.BadMap,
			Unreadable:    vr.Unreadable,
			EDCBadMapGood: vr.EDCBadMapGood,
			EDCGoodMapBad: vr.EDCGoodMapBad,
		},
	}

	if fsr.Type != cuebin.FSUnknown {
		jfs := &jsonFilesystem{
			Type:            fsr.Type.String(),
			VolumeLabel:     fsr.VolumeLabel,
			DeclaredSectors: fsr.DeclaredSectors,
			TotalFiles:      len(fsr.Files),
			AffectedFiles:   fsr.AffectedFiles,
			Issues:          fsr.Issues,
		}
		for i := range fsr.Files {
			fe := &fsr.Files[i]
			jfs.Files = append(jfs.Files, jsonFile{
				Path:       fe.Path,
				StartLBA:   fe.StartLBA,
				EndLBA:     fe.EndLBA,
				BadSectors: fe.BadSectors,
			})
		}
		report.Filesystem = jfs
	}

	for _, ar := range audio {
		jat := jsonAudio{
			TrackNumber:     ar.TrackNumber,
			StartLBA:        ar.StartLBA,
			EndLBA:          ar.EndLBA,
			DurationSec:     ar.DurationSeconds(),
			BadSectors:      ar.BadSectors,
			Unreadable:      ar.UnreadableSectors,
			RepeatedSectors: ar.RepeatedSectors,
		}
		for _, sr := range ar.SilentRuns {
			jat.SilentRuns = append(jat.SilentRuns, jsonSilentRun{
				StartLBA: sr.StartLBA,
				Samples:  sr.Samples,
			})
		}
		report.AudioTracks = append(report.AudioTracks, jat)
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(report)
}

// ---- export subcommand ----

func runExport(args []string) int {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	var src ddrescue.Flags
	src.Register(fs)
	var (
		outDir    = fs.String("o", "", "output directory (default: same as .cue file)")
		trackList = fs.String("tracks", "", "comma-separated track numbers to export (default: all)")
		swapAudio = fs.Bool("swap-audio", false, "byte-swap audio samples (for big-endian source images)")
		force     = fs.Bool("f", false, "overwrite existing output files")
	)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: cuetool export [flags] <file.cue>\n\n")
		fmt.Fprintf(os.Stderr, "Exports tracks from a cue/bin image to ISO and WAV files.\n\n")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)

	if fs.NArg() != 1 {
		fs.Usage()
		return subcmd.ExitUsage
	}
	cuePath := fs.Arg(0)

	sheet, cf, binPath, err := loadSheet(cuePath)
	if err != nil {
		log.Print(err)
		return subcmd.ExitFailure
	}
	if err := src.Validate(); err != nil {
		log.Print(err)
		return subcmd.ExitFor(subcmd.Usage(err))
	}
	binSS, err := cuebin.BinSectorSize(cf.Tracks)
	if err != nil {
		log.Print(err)
		return subcmd.ExitFailure
	}

	dir := *outDir
	if dir == "" {
		dir = filepath.Dir(cuePath)
	}
	// Check the output directory up front, so a path that can never work
	// fails once rather than once per track.
	if err := xio.EnsureDir(dir); err != nil {
		log.Print(err)
		return subcmd.ExitFailure
	}

	wanted := map[int]bool{}
	if *trackList != "" {
		for s := range strings.SplitSeq(*trackList, ",") {
			n, err := strconv.Atoi(strings.TrimSpace(s))
			if err != nil {
				log.Printf("invalid track number %q", s)
				return subcmd.ExitUsage
			}
			wanted[n] = true
		}
	}

	// One source for the whole run rather than an open per track.
	source, err := cuebin.OpenSource(binPath, src.Options())
	if err != nil {
		log.Print(err)
		return subcmd.ExitFailure
	}
	defer source.Close() //nolint:errcheck // read-only

	bounds, err := cuebin.TrackBounds(sheet, source.Size)
	if err != nil {
		log.Printf("compute track bounds: %v", err)
		return subcmd.ExitFailure
	}

	var exported, failed, damaged int
	for i, tr := range cf.Tracks {
		if len(wanted) > 0 && !wanted[tr.Number] {
			continue
		}
		startLBA, endLBA := bounds[i][0], bounds[i][1]

		var outPath string
		var tm *cuebin.TrackMap
		var err error
		if cuebin.IsAudio(tr.Mode) {
			outPath = filepath.Join(dir, fmt.Sprintf("track%02d.wav", tr.Number))
			log.Printf("track %02d  AUDIO  LBA %d–%d  → %s", tr.Number, startLBA, endLBA, outPath)
			tm, err = exportAudio(source, startLBA, endLBA, binSS, *swapAudio, outPath, *force)
		} else {
			outPath = filepath.Join(dir, fmt.Sprintf("track%02d.iso", tr.Number))
			log.Printf("track %02d  %-12s  LBA %d–%d  → %s", tr.Number, tr.Mode, startLBA, endLBA, outPath)
			tm, err = exportData(source, startLBA, endLBA, tr.Mode, binSS, outPath, *force)
		}
		// The map is written even when the export then failed, since an
		// artifact that zero-filled and then stopped is where it matters most. A
		// source with no map produces none, since nothing was measured.
		if tm != nil {
			mapPath := outPath + ".map"
			if err := writeTrackMap(tm, mapPath, *force); err != nil {
				log.Printf("error: %v", err)
				failed++
			} else if tm.Damaged() {
				damaged++
				log.Printf("  %s of the track was never recovered; zero-filled and recorded in %s",
					humanize.Bytes(tm.Lost()), filepath.Base(mapPath))
			}
		}
		if err != nil {
			log.Printf("error: %v", err)
			failed++
			continue
		}
		exported++
	}

	// A full disk, an unwritable directory or a corrupt BIN is a run failure,
	// even if some tracks did export.
	if failed > 0 {
		log.Printf("%d track(s) exported, %d failed", exported, failed)
		return subcmd.ExitFailure
	}
	if exported == 0 {
		log.Printf("no tracks matched")
		return subcmd.ExitFailure
	}
	// The artifact and the map beside it are the correct output for a damaged
	// rip, so every track was exported. The disc was damaged, not the run, and
	// the status says so.
	if damaged > 0 {
		log.Printf("%d track(s) exported, %d with holes, each with a .map beside it", exported, damaged)
		return subcmd.ExitDegraded
	}
	log.Printf("%d track(s) exported", exported)
	return subcmd.ExitOK
}

func exportData(src *cuebin.Source, startLBA, endLBA int64, mode string, binSectorSize int, outPath string, force bool) (*cuebin.TrackMap, error) {
	f, err := xio.Create(outPath, force)
	if err != nil {
		return nil, err
	}
	tm, err := cuebin.ExtractDataTrack(src.Raw(), src.Blocks, startLBA, endLBA, mode, binSectorSize, f)
	return tm, xio.CloseAfter(f, err)
}

func exportAudio(src *cuebin.Source, startLBA, endLBA int64, binSectorSize int, swapBytes bool, outPath string, force bool) (*cuebin.TrackMap, error) {
	f, err := xio.Create(outPath, force)
	if err != nil {
		return nil, err
	}
	tm, err := cuebin.ExtractAudioTrack(src.Raw(), src.Blocks, startLBA, endLBA, binSectorSize, swapBytes, f)
	return tm, xio.CloseAfter(f, err)
}

// writeTrackMap writes an exported track's rescue map beside it, using the
// sidecar name every tool here looks for: track01.iso.map beside track01.iso.
func writeTrackMap(tm *cuebin.TrackMap, mapPath string, force bool) error {
	f, err := xio.Create(mapPath, force)
	if err != nil {
		return err
	}
	_, err = tm.WriteTo(f)
	return xio.CloseAfter(f, err)
}
