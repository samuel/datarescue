package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"samuelks.com/datarescue/cdrom"
	"samuelks.com/datarescue/cuebin"
	"samuelks.com/datarescue/ddrescue"
	"samuelks.com/datarescue/internal/subcmd"
	"samuelks.com/datarescue/internal/testiso"
)

// readMode lists the three ways the rip can ask for a sector, so the fake can
// fail one without failing the others: the pregap fallback depends on a data
// sector that reads only in audio mode.
type readMode int

const (
	modeCooked readMode = iota
	modeRawData
	modeRawAudio
)

func (m readMode) String() string {
	switch m {
	case modeCooked:
		return "cooked"
	case modeRawData:
		return "raw-data"
	default:
		return "raw-audio"
	}
}

type readKey struct {
	lba  int64
	mode readMode
}

// fakeDevice is an in-memory CD. Sectors it does not hold read as errors, which
// is how it models both a sector outside the disc and one the drive cannot
// recover; fail marks a sector the disc holds but will not return in a given
// mode.
type fakeDevice struct {
	toc     cdrom.TOC
	sectors map[readKey][]byte
	fail    map[readKey]bool
	reads   map[readKey]int
	closed  bool
}

var errFakeUnreadable = errors.New("fake: unreadable sector")

func newFakeDevice(toc cdrom.TOC) *fakeDevice {
	return &fakeDevice{
		toc:     toc,
		sectors: make(map[readKey][]byte),
		fail:    make(map[readKey]bool),
		reads:   make(map[readKey]int),
	}
}

func (d *fakeDevice) put(lba int64, mode readMode, data []byte) {
	d.sectors[readKey{lba, mode}] = data
}

func (d *fakeDevice) markBad(lba int64, modes ...readMode) {
	for _, m := range modes {
		d.fail[readKey{lba, m}] = true
	}
}

func (d *fakeDevice) TOC() (cdrom.TOC, error) { return d.toc, nil }

func (d *fakeDevice) Close() error {
	d.closed = true
	return nil
}

func (d *fakeDevice) read(lba int64, mode readMode) ([]byte, error) {
	key := readKey{lba, mode}
	d.reads[key]++
	if d.fail[key] {
		return nil, errFakeUnreadable
	}
	data, ok := d.sectors[key]
	if !ok {
		return nil, errFakeUnreadable
	}
	return bytes.Clone(data), nil
}

func (d *fakeDevice) ReadSector(lba int64) ([]byte, error) {
	return d.read(lba, modeCooked)
}

func (d *fakeDevice) ReadRawSector(lba int64, trackType cdrom.TrackType) ([]byte, error) {
	if trackType == cdrom.TrackTypeAudio {
		return d.read(lba, modeRawAudio)
	}
	return d.read(lba, modeRawData)
}

// markSector stamps the LBA into a sector so a test can tell which sector of
// the disc ended up at a given offset in the output.
func markSector(lba int64, size int) []byte {
	sec := make([]byte, size)
	binary.LittleEndian.PutUint64(sec[:8], uint64(lba)) //nolint:gosec // test LBAs are small and non-negative
	for i := 8; i < size; i++ {
		sec[i] = byte(lba)
	}
	return sec
}

func sectorLBA(sec []byte) int64 {
	return int64(binary.LittleEndian.Uint64(sec[:8])) //nolint:gosec // round-trips markSector's own value
}

// isoDisc is a single data track holding the shared test image, the disc shape
// qemu can emulate and the default ISO path handles.
func isoDisc() *fakeDevice {
	d := newFakeDevice(cdrom.TOC{
		FirstTrack: 1,
		LastTrack:  1,
		LeadOutLBA: testiso.Sectors,
		Tracks: []cdrom.Track{
			{Number: 1, Session: 1, Type: cdrom.TrackTypeData, StartLBA: 0},
		},
	})
	img := testiso.Build()
	for lba := range int64(testiso.Sectors) {
		d.put(lba, modeCooked, img[lba*testiso.SectorSize:(lba+1)*testiso.SectorSize])
	}
	return d
}

func readMap(t *testing.T, path string) []ddrescue.Block {
	t.Helper()
	blocks, err := ddrescue.ParseMap(path)
	if err != nil {
		t.Fatalf("parse map %s: %v", path, err)
	}
	return blocks
}

// badRanges returns the sector ranges the map marks as not recovered.
func badRanges(blocks []ddrescue.Block, sectorSize int64) [][2]int64 {
	var out [][2]int64
	for _, b := range blocks {
		if b.Status == ddrescue.BlockStatusFinished {
			continue
		}
		out = append(out, [2]int64{b.Offset / sectorSize, b.Size / sectorSize})
	}
	return out
}

// finishedRanges is the complement, which is what the cooked-provenance map
// carries: there, "finished" means the sector fell back to a block read.
func finishedRanges(blocks []ddrescue.Block, sectorSize int64) [][2]int64 {
	var out [][2]int64
	for _, b := range blocks {
		if b.Status != ddrescue.BlockStatusFinished {
			continue
		}
		out = append(out, [2]int64{b.Offset / sectorSize, b.Size / sectorSize})
	}
	return out
}

func TestRipISOClean(t *testing.T) {
	dev := isoDisc()
	out := filepath.Join(t.TempDir(), "disc.iso")

	if _, err := ripISO(dev, dev.toc, out, 0, 3, true, false, false, false); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, testiso.Build()) {
		t.Errorf("ripped image differs from the disc (%d bytes vs %d)", len(got), testiso.Sectors*testiso.SectorSize)
	}
	blocks := readMap(t, out+".map")
	if len(blocks) != 1 || blocks[0].Status != ddrescue.BlockStatusFinished ||
		blocks[0].Offset != 0 || blocks[0].Size != testiso.Sectors*testiso.SectorSize {
		t.Errorf("map = %+v; want one finished block covering the whole image", blocks)
	}
}

func TestRipISOZeroFillsBadSectors(t *testing.T) {
	dev := isoDisc()
	// Two separate damaged runs, so the map has to coalesce each and keep them
	// apart rather than merging everything after the first failure.
	dev.markBad(5, modeCooked)
	dev.markBad(6, modeCooked)
	dev.markBad(12, modeCooked)
	out := filepath.Join(t.TempDir(), "disc.iso")

	if _, err := ripISO(dev, dev.toc, out, 0, 2, true, false, false, false); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != testiso.Sectors*testiso.SectorSize {
		t.Fatalf("image is %d bytes; want %d", len(got), testiso.Sectors*testiso.SectorSize)
	}
	want := testiso.Build()
	zero := make([]byte, testiso.SectorSize)
	for lba := range int64(testiso.Sectors) {
		sec := got[lba*testiso.SectorSize : (lba+1)*testiso.SectorSize]
		expect := want[lba*testiso.SectorSize : (lba+1)*testiso.SectorSize]
		if lba == 5 || lba == 6 || lba == 12 {
			expect = zero
		}
		if !bytes.Equal(sec, expect) {
			t.Errorf("sector %d does not match", lba)
		}
	}

	gotBad := badRanges(readMap(t, out+".map"), testiso.SectorSize)
	wantBad := [][2]int64{{5, 2}, {12, 1}}
	if len(gotBad) != len(wantBad) {
		t.Fatalf("map bad ranges = %v; want %v", gotBad, wantBad)
	}
	for i, w := range wantBad {
		if gotBad[i] != w {
			t.Errorf("map bad range %d = %v; want %v", i, gotBad[i], w)
		}
	}
}

// sessionDisc is a two-session disc whose second session starts with an audio
// track, so selecting that session has to walk back past it to the session
// start rather than starting at its first data track.
func sessionDisc() *fakeDevice {
	d := newFakeDevice(cdrom.TOC{
		FirstTrack: 1,
		LastTrack:  3,
		LeadOutLBA: 200,
		Tracks: []cdrom.Track{
			{Number: 1, Session: 1, Type: cdrom.TrackTypeData, StartLBA: 0},
			{Number: 2, Session: 2, Type: cdrom.TrackTypeAudio, StartLBA: 100},
			{Number: 3, Session: 2, Type: cdrom.TrackTypeData, StartLBA: 150},
		},
	})
	for lba := range int64(200) {
		d.put(lba, modeCooked, markSector(lba, testiso.SectorSize))
	}
	return d
}

func TestRipISOSessionSelection(t *testing.T) {
	tests := []struct {
		name         string
		session      uint8
		wantFirstLBA int64
		wantSectors  int64
	}{
		// Session 1 ends where session 2 begins, not at the lead-out.
		{"session 1 stops at the next session", 1, 0, 100},
		// Selecting a session rips all of it, audio track included: the
		// operator asked for the session, not for the filesystem in it.
		{"session 2 starts at the session start", 2, 100, 100},
		// Auto picks the last data session and starts at its first *data*
		// track. Starting at the session's audio track would hand the ISO
		// writer music to read as a filesystem.
		{"auto picks the last data session's data track", 0, 150, 50},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dev := sessionDisc()
			out := filepath.Join(t.TempDir(), "disc.iso")
			if _, err := ripISO(dev, dev.toc, out, tc.session, 1, false, false, false, false); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			if int64(len(got)) != tc.wantSectors*testiso.SectorSize {
				t.Fatalf("image is %d sectors; want %d",
					int64(len(got))/testiso.SectorSize, tc.wantSectors)
			}
			if got := sectorLBA(got); got != tc.wantFirstLBA {
				t.Errorf("first sector came from LBA %d; want %d", got, tc.wantFirstLBA)
			}
			last := got[(tc.wantSectors-1)*testiso.SectorSize:]
			if got := sectorLBA(last); got != tc.wantFirstLBA+tc.wantSectors-1 {
				t.Errorf("last sector came from LBA %d; want %d",
					got, tc.wantFirstLBA+tc.wantSectors-1)
			}
		})
	}
}

func TestRipISONoDataTracks(t *testing.T) {
	dev := newFakeDevice(cdrom.TOC{
		FirstTrack: 1, LastTrack: 1, LeadOutLBA: 100,
		Tracks: []cdrom.Track{{Number: 1, Session: 1, Type: cdrom.TrackTypeAudio}},
	})
	out := filepath.Join(t.TempDir(), "disc.iso")
	if _, err := ripISO(dev, dev.toc, out, 0, 1, false, false, false, false); err == nil {
		t.Error("ripISO succeeded on an audio-only disc; want an error")
	}
	if _, err := ripISO(dev, dev.toc, out, 2, 1, false, false, false, false); err == nil {
		t.Error("ripISO succeeded for a session with no data tracks; want an error")
	}
}

// mixedDisc is a data track followed by an audio track with a 150-sector
// pregap, the shape that makes the tool choose cuebin output.
func mixedDisc() *fakeDevice {
	d := newFakeDevice(cdrom.TOC{
		FirstTrack: 1,
		LastTrack:  2,
		LeadOutLBA: 400,
		Tracks: []cdrom.Track{
			{Number: 1, Session: 1, Type: cdrom.TrackTypeData, StartLBA: 0},
			{Number: 2, Session: 1, Type: cdrom.TrackTypeAudio, StartLBA: 300, Index0LBA: 150},
		},
	})
	for lba := range int64(400) {
		mode := modeRawData
		if lba >= 150 {
			// The pregap and the audio track that follows it read as audio.
			mode = modeRawAudio
		}
		d.put(lba, mode, markSector(lba, cdrom.RawSectorSize))
	}
	return d
}

func TestRipCueBinMixedMode(t *testing.T) {
	dev := mixedDisc()
	stem := filepath.Join(t.TempDir(), "disc")

	if _, err := ripCueBin(dev, dev.toc, stem+".bin", 2, true, false, false); err != nil {
		t.Fatal(err)
	}

	bin, err := os.ReadFile(stem + ".bin")
	if err != nil {
		t.Fatal(err)
	}
	if len(bin) != 400*cdrom.RawSectorSize {
		t.Fatalf("bin is %d sectors; want 400", len(bin)/cdrom.RawSectorSize)
	}
	// Every sector must land at its own disc position, whichever mode read it.
	for lba := range int64(400) {
		sec := bin[lba*cdrom.RawSectorSize:]
		if got := sectorLBA(sec); got != lba {
			t.Fatalf("bin sector %d came from LBA %d", lba, got)
		}
	}

	// The pregap sectors, which only read in audio mode, went through the
	// fallback rather than being written off as bad.
	if n := dev.reads[readKey{200, modeRawData}]; n == 0 {
		t.Error("pregap sector was never attempted in data mode")
	}
	if n := dev.reads[readKey{200, modeRawAudio}]; n == 0 {
		t.Error("pregap sector was never retried in audio mode")
	}

	f, err := os.Open(stem + ".cue")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck // read-only
	sheet, err := cuebin.ParseSheet(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(sheet.Files) != 1 || len(sheet.Files[0].Tracks) != 2 {
		t.Fatalf("cue has %d files; want one with 2 tracks", len(sheet.Files))
	}
	tracks := sheet.Files[0].Tracks
	if tracks[0].Mode != "MODE1/2352" || tracks[1].Mode != "AUDIO" {
		t.Errorf("track modes = %q, %q; want MODE1/2352, AUDIO", tracks[0].Mode, tracks[1].Mode)
	}
	// Positions in the CUE are offsets into the BIN, and the BIN starts at
	// track 1, so track 2's INDEX 01 is at sector 300 and its INDEX 00 at 150.
	start, err := tracks[1].StartLBA()
	if err != nil {
		t.Fatal(err)
	}
	if start != 300 {
		t.Errorf("track 2 INDEX 01 = %d; want 300", start)
	}
	var index0 int64 = -1
	for _, idx := range tracks[1].Indices {
		if idx.Number == 0 {
			index0 = idx.Position.LBA()
		}
	}
	if index0 != 150 {
		t.Errorf("track 2 INDEX 00 = %d; want 150", index0)
	}

	if bad := badRanges(readMap(t, stem+".bin.map"), cdrom.RawSectorSize); len(bad) != 0 {
		t.Errorf("map reports bad ranges %v on a fully readable disc", bad)
	}
}

func TestRipCueBinUnreadablePregapIsNotBad(t *testing.T) {
	dev := mixedDisc()
	// A pregap sector that reads in neither mode is expected, not damage: some
	// drives will not return the pregap at all.
	dev.markBad(200, modeRawData, modeRawAudio)
	// A sector inside the data track proper is real damage.
	dev.markBad(50, modeRawData, modeRawAudio)
	stem := filepath.Join(t.TempDir(), "disc")

	if _, err := ripCueBin(dev, dev.toc, stem+".bin", 2, true, false, false); err != nil {
		t.Fatal(err)
	}

	bin, err := os.ReadFile(stem + ".bin")
	if err != nil {
		t.Fatal(err)
	}
	for _, lba := range []int64{50, 200} {
		sec := bin[lba*cdrom.RawSectorSize : (lba+1)*cdrom.RawSectorSize]
		if !bytes.Equal(sec, make([]byte, cdrom.RawSectorSize)) {
			t.Errorf("sector %d is not zero-filled", lba)
		}
	}

	// Only the damaged sector is bad; the missing pregap is recorded finished,
	// so a later verify does not report the disc as damaged where it is not.
	gotBad := badRanges(readMap(t, stem+".bin.map"), cdrom.RawSectorSize)
	if len(gotBad) != 1 || gotBad[0] != [2]int64{50, 1} {
		t.Errorf("map bad ranges = %v; want just sector 50", gotBad)
	}
}

func TestRipCueBinNoTracks(t *testing.T) {
	dev := newFakeDevice(cdrom.TOC{LeadOutLBA: 100})
	stem := filepath.Join(t.TempDir(), "disc")
	if _, err := ripCueBin(dev, dev.toc, stem+".bin", 1, false, false, false); err == nil {
		t.Error("ripCueBin succeeded on a disc with no tracks; want an error")
	}
}

func TestReadWithRetryAttemptCount(t *testing.T) {
	dev := isoDisc()
	dev.markBad(3, modeCooked)

	if _, err := readWithRetry(dev.ReadSector, 3, 4); err == nil {
		t.Error("readWithRetry succeeded on an unreadable sector")
	}
	if n := dev.reads[readKey{3, modeCooked}]; n != 4 {
		t.Errorf("unreadable sector was read %d times; want 4", n)
	}
	// A sector that reads first time must not be retried.
	if _, err := readWithRetry(dev.ReadSector, 4, 4); err != nil {
		t.Fatal(err)
	}
	if n := dev.reads[readKey{4, modeCooked}]; n != 1 {
		t.Errorf("readable sector was read %d times; want 1", n)
	}
}

// withDevice points run's device seam at dev for the duration of the test.
func withDevice(t *testing.T, dev cdrom.Device) {
	t.Helper()
	prev := openDevice
	openDevice = func(string) (cdrom.Device, error) { return dev, nil }
	t.Cleanup(func() { openDevice = prev })
}

func TestRipWritesAnImageAndClosesTheDevice(t *testing.T) {
	dev := isoDisc()
	withDevice(t, dev)
	out := filepath.Join(t.TempDir(), "disc.iso")

	if code := runRip([]string{"/dev/fake", out}); code != subcmd.ExitOK {
		t.Fatalf("runRip = %d; want 0", code)
	}
	if !dev.closed {
		t.Error("runRip did not close the device")
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, testiso.Build()) {
		t.Error("ripped image differs from the disc")
	}
	if _, err := os.Stat(out + ".map"); err != nil {
		t.Errorf("-map wrote no map file: %v", err)
	}

	// Without -f a second rip must not destroy the first.
	if code := runRip([]string{"/dev/fake", out}); code != subcmd.ExitFailure {
		t.Errorf("re-rip = %d; want %d (an existing image must not be truncated)", code, subcmd.ExitFailure)
	}
	if code := runRip([]string{"-f", "/dev/fake", out}); code != subcmd.ExitOK {
		t.Errorf("re-rip with -f = %d; want 0", code)
	}
}

func TestRipAutoFormatFollowsTheTOC(t *testing.T) {
	dir := t.TempDir()

	withDevice(t, isoDisc())
	if code := runRip([]string{"/dev/fake", filepath.Join(dir, "data.iso")}); code != subcmd.ExitOK {
		t.Fatalf("data-only rip = %d; want 0", code)
	}
	if _, err := os.Stat(filepath.Join(dir, "data.iso")); err != nil {
		t.Errorf("auto format on a data-only disc wrote no ISO: %v", err)
	}

	// A disc with audio must come out as cuebin, since an ISO cannot hold it.
	withDevice(t, mixedDisc())
	if code := runRip([]string{"/dev/fake", filepath.Join(dir, "mixed.bin")}); code != subcmd.ExitOK {
		t.Fatalf("mixed-mode rip = %d; want 0", code)
	}
	for _, name := range []string{"mixed.bin", "mixed.cue"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("auto format on a mixed-mode disc wrote no %s: %v", name, err)
		}
	}
}

func TestInfoReportsWithoutWriting(t *testing.T) {
	dev := isoDisc()
	withDevice(t, dev)
	dir := t.TempDir()

	if code := runInfo([]string{"/dev/fake"}); code != subcmd.ExitOK {
		t.Fatalf("info = %d; want 0", code)
	}
	if !dev.closed {
		t.Error("info did not close the device")
	}
	// info takes only the device, and must write nothing.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("info wrote %d files", len(entries))
	}
	// The arg count is the subcommand's own now, not a consequence of a flag.
	if code := runInfo(nil); code != subcmd.ExitUsage {
		t.Error("info with no device did not report a usage error")
	}
	if code := runInfo([]string{"/dev/fake", filepath.Join(dir, "extra.iso")}); code != subcmd.ExitUsage {
		t.Error("info with an extra argument did not report a usage error")
	}
}

// The medium decides how the disc is read, so info has to say which it is
// before an operator commits to a rip.
func TestInfoNamesTheMedium(t *testing.T) {
	tests := []struct {
		name string
		toc  cdrom.TOC
		want string
	}{
		{"CD", isoDisc().toc, "medium: CD"},
		{"device reports non-CD", nonCDTOC(), "not a CD (the device reports non-CD media)"},
		{"lead-out past CD range", dvdSizedTOC(), "not a CD (lead-out LBA"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			withDevice(t, newFakeDevice(tc.toc))
			var buf bytes.Buffer
			log.SetOutput(&buf)
			t.Cleanup(func() { log.SetOutput(os.Stderr) })

			if code := runInfo([]string{"/dev/fake"}); code != subcmd.ExitOK {
				t.Fatalf("info = %d; want 0", code)
			}
			if !strings.Contains(buf.String(), tc.want) {
				t.Errorf("info did not report %q; got:\n%s", tc.want, buf.String())
			}
		})
	}
}

func TestRipRejectsBadFlags(t *testing.T) {
	// A rejected command line must not reach the drive: spinning up a disc only
	// to reject the arguments wastes the operator's time.
	openDevice = func(string) (cdrom.Device, error) {
		t.Error("runRip opened the device despite an invalid command line")
		return nil, errors.New("must not be called")
	}
	t.Cleanup(func() { openDevice = cdrom.Open })

	out := filepath.Join(t.TempDir(), "disc.iso")
	tests := []struct {
		name string
		args []string
	}{
		{"no arguments", nil},
		{"missing output", []string{"/dev/fake"}},
		{"extra argument", []string{"/dev/fake", out, "extra"}},
		{"zero retries", []string{"-retries", "0", "/dev/fake", out}},
		{"negative retries", []string{"-retries", "-1", "/dev/fake", out}},
		{"session out of range", []string{"-session", "100", "/dev/fake", out}},
		{"unknown format", []string{"-format", "bluray", "/dev/fake", out}},
		// This one is why the subcommands keep flag.ContinueOnError: under the
		// ExitOnError the other tools use, a bad flag would end this process
		// rather than return a status.
		{"unknown flag", []string{"-nope", "/dev/fake", out}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if code := runRip(tc.args); code != subcmd.ExitUsage {
				t.Errorf("runRip(%q) = %d; want 2", strings.Join(tc.args, " "), code)
			}
		})
	}
}

func TestRipReportsAnUnopenableDevice(t *testing.T) {
	openDevice = func(string) (cdrom.Device, error) { return nil, errors.New("no such device") }
	t.Cleanup(func() { openDevice = cdrom.Open })

	out := filepath.Join(t.TempDir(), "disc.iso")
	if code := runRip([]string{"/dev/fake", out}); code != subcmd.ExitFailure {
		t.Errorf("runRip on an unopenable device = %d; want %d", code, subcmd.ExitFailure)
	}
}

// The map a rip writes has to be the one the tool that reads the rip looks for.
// cuetool resolves it from the CUE's own FILE line, so the check follows the
// same path rather than assuming the rip's file names: parse the sheet,
// resolve the BIN beside it, and ask for the map that belongs to it.
func TestRipMapIsFoundByCuetoolsLookup(t *testing.T) {
	dir := t.TempDir()
	withDevice(t, mixedDisc())
	if code := runRip([]string{"/dev/fake", filepath.Join(dir, "mixed.bin")}); code != subcmd.ExitOK {
		t.Fatalf("rip = %d; want 0", code)
	}

	cuePath := filepath.Join(dir, "mixed.cue")
	cf, err := os.Open(cuePath)
	if err != nil {
		t.Fatal(err)
	}
	defer cf.Close() //nolint:errcheck // read-only
	sheet, err := cuebin.ParseSheet(cf)
	if err != nil {
		t.Fatal(err)
	}
	if len(sheet.Files) != 1 {
		t.Fatalf("the rip's cue names %d files; want 1", len(sheet.Files))
	}
	binPath := cuebin.BinPath(cuePath, sheet.Files[0].Path)

	want := filepath.Join(dir, "mixed.bin.map")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("the rip wrote no map at %s: %v", want, err)
	}
	if got := ddrescue.FindMap(binPath); got != want {
		t.Errorf("the map lookup for %s found %q; want the rip's %q", binPath, got, want)
	}
}

// The same for an ISO rip, whose map is named after the image it describes.
func TestISORipMapIsFoundByLookup(t *testing.T) {
	dir := t.TempDir()
	withDevice(t, isoDisc())
	out := filepath.Join(dir, "data.iso")
	if code := runRip([]string{"/dev/fake", out}); code != subcmd.ExitOK {
		t.Fatalf("rip = %d; want 0", code)
	}
	if got := ddrescue.FindMap(out); got != out+".map" {
		t.Errorf("the map lookup for %s found %q; want %q", out, got, out+".map")
	}
}

// fakeCookedDevice adds a block-level fallback to fakeDevice, the same way a
// real Device optionally implements cdrom.CookedFallbackReader. fakeDevice
// itself deliberately does not implement it, matching most real devices.
type fakeCookedDevice struct {
	*fakeDevice
	cookedSectors map[int64][]byte
	cookedFail    map[int64]bool
	cookedReads   map[int64]int
}

func newFakeCookedDevice(d *fakeDevice) *fakeCookedDevice {
	return &fakeCookedDevice{
		fakeDevice:    d,
		cookedSectors: make(map[int64][]byte),
		cookedFail:    make(map[int64]bool),
		cookedReads:   make(map[int64]int),
	}
}

func (d *fakeCookedDevice) putCooked(lba int64, data []byte) {
	d.cookedSectors[lba] = data
}

func (d *fakeCookedDevice) markCookedBad(lba int64) {
	d.cookedFail[lba] = true
}

func (d *fakeCookedDevice) ReadSectorCooked(lba int64) ([]byte, error) {
	d.cookedReads[lba]++
	if d.cookedFail[lba] {
		return nil, errFakeUnreadable
	}
	data, ok := d.cookedSectors[lba]
	if !ok {
		return nil, errFakeUnreadable
	}
	return bytes.Clone(data), nil
}

// On a medium with no low-level read path, a block read is not a fallback --
// it is how the medium is read -- so nothing is flagged as one, and the
// low-level command is never even attempted.
func TestSectorReaderCookedOnlySkipsLowLevel(t *testing.T) {
	dev := isoDisc()
	cooked := newFakeCookedDevice(dev)
	cooked.putCooked(0, markSector(0, cdrom.DataSectorSize))
	sr := sectorReader{dev: dev, cooked: cooked, cookedOnly: true, retries: 3}

	data, viaFallback, err := sr.read(0)
	if err != nil {
		t.Fatal(err)
	}
	if viaFallback {
		t.Error("viaFallback = true on a medium whose only read path is the block read")
	}
	if sectorLBA(data) != 0 {
		t.Error("data did not come from the cooked reader")
	}
	if n := dev.reads[readKey{0, modeCooked}]; n != 0 {
		t.Errorf("low-level ReadSector was called %d times for a cooked-only read; want 0", n)
	}
	if n := cooked.cookedReads[0]; n != 1 {
		t.Errorf("ReadSectorCooked called %d times; want 1", n)
	}
}

func TestSectorReaderLowLevelSucceeds(t *testing.T) {
	dev := isoDisc()
	cooked := newFakeCookedDevice(dev)
	cooked.putCooked(0, markSector(999, cdrom.DataSectorSize)) // must not be used
	sr := sectorReader{dev: dev, cooked: cooked, retries: 1}

	_, viaFallback, err := sr.read(0)
	if err != nil {
		t.Fatal(err)
	}
	if viaFallback {
		t.Error("viaFallback = true when the low-level read succeeded")
	}
	if n := cooked.cookedReads[0]; n != 0 {
		t.Errorf("ReadSectorCooked was called %d times when unnecessary; want 0", n)
	}
}

func TestSectorReaderReactiveFallback(t *testing.T) {
	dev := isoDisc()
	dev.markBad(5, modeCooked)
	cooked := newFakeCookedDevice(dev)
	cooked.putCooked(5, markSector(5, cdrom.DataSectorSize))
	sr := sectorReader{dev: dev, cooked: cooked, retries: 2}

	data, viaFallback, err := sr.read(5)
	if err != nil {
		t.Fatal(err)
	}
	if !viaFallback {
		t.Error("viaFallback = false; want true")
	}
	if sectorLBA(data) != 5 {
		t.Error("data did not come from the cooked reader")
	}
	if n := dev.reads[readKey{5, modeCooked}]; n != 2 {
		t.Errorf("low-level ReadSector was retried %d times; want 2 (retries exhausted before falling back)", n)
	}
}

func TestSectorReaderFallbackAlsoFails(t *testing.T) {
	dev := isoDisc()
	dev.markBad(5, modeCooked)
	cooked := newFakeCookedDevice(dev)
	cooked.markCookedBad(5)
	sr := sectorReader{dev: dev, cooked: cooked, retries: 1}

	_, viaFallback, err := sr.read(5)
	if err == nil {
		t.Fatal("read succeeded even though both the low-level and fallback reads fail")
	}
	if viaFallback {
		t.Error("viaFallback = true even though the fallback failed")
	}
}

func TestSectorReaderNoCookedReader(t *testing.T) {
	dev := isoDisc()
	dev.markBad(5, modeCooked)
	sr := sectorReader{dev: dev, retries: 1}

	_, viaFallback, err := sr.read(5)
	if err == nil {
		t.Fatal("read succeeded on a bad sector with no fallback available")
	}
	if viaFallback {
		t.Error("viaFallback = true with no cooked reader")
	}
}

// dvdSizedTOC is a single data track whose lead-out exceeds cdrom.MaxLBA, one
// of the two signals ripISO uses to detect non-CD media. It is only ever used
// to exercise the guard that runs before the rip loop, so its data never needs
// to be populated -- ripping it for real would mean iterating ~450,000
// sectors.
func dvdSizedTOC() cdrom.TOC {
	return cdrom.TOC{
		FirstTrack: 1,
		LastTrack:  1,
		LeadOutLBA: cdrom.MaxLBA + 1,
		Tracks: []cdrom.Track{
			{Number: 1, Session: 1, Type: cdrom.TrackTypeData, StartLBA: 0},
		},
	}
}

// nonCDTOC is the other signal, and the one no size test can find: a medium
// the device itself reported is not a CD, small enough to fit inside CD range.
// A DVD-R with a short burn looks exactly like this, and on macOS -- where the
// CD ioctls are attached to CD media only -- the low-level path cannot read a
// single sector of it.
func nonCDTOC() cdrom.TOC {
	toc := isoDisc().toc
	toc.NotCD = true
	return toc
}

// A non-CD medium is unreadable on a platform whose device offers no block
// read, and no flag changes that -- so the error must not send the operator
// looking for one. Both signals and both flag states are covered; a plain
// fakeDevice does not implement cdrom.CookedFallbackReader, standing in for
// such a platform.
func TestRipISONonCDWithoutBlockReadSupportFails(t *testing.T) {
	for _, medium := range []struct {
		name string
		toc  cdrom.TOC
	}{
		{"lead-out past CD range", dvdSizedTOC()},
		{"device reports non-CD", nonCDTOC()},
	} {
		for _, flag := range []struct {
			name           string
			cookedFallback bool
		}{
			{"flag absent", false},
			{"flag set", true},
		} {
			t.Run(medium.name+", "+flag.name, func(t *testing.T) {
				dev := newFakeDevice(medium.toc)
				out := filepath.Join(t.TempDir(), "disc.iso")

				_, err := ripISO(dev, dev.toc, out, 0, 1, false, false, false, flag.cookedFallback)
				if err == nil {
					t.Fatal("ripISO succeeded on non-CD media with no block-read support; want an error")
				}
				if strings.Contains(err.Error(), "-cooked-fallback") {
					t.Errorf("error tells the operator to pass a flag that cannot help: %q", err.Error())
				}
				if _, statErr := os.Stat(out); statErr == nil {
					t.Error("ripISO created an output file despite failing before the rip loop")
				}
			})
		}
	}
}

func TestRipISOCookedFallbackNoEffectWhenUnsupported(t *testing.T) {
	// A normal (non-DVD) disc with -cooked-fallback set but no device support:
	// behavior must be exactly as if the flag were absent.
	dev := isoDisc()
	dev.markBad(5, modeCooked)
	out := filepath.Join(t.TempDir(), "disc.iso")

	if _, err := ripISO(dev, dev.toc, out, 0, 1, true, false, false, true); err != nil {
		t.Fatal(err)
	}
	gotBad := badRanges(readMap(t, out+".map"), testiso.SectorSize)
	if len(gotBad) != 1 || gotBad[0] != [2]int64{5, 1} {
		t.Errorf("map bad ranges = %v; want just sector 5", gotBad)
	}
	cookedBlocks := readMap(t, out+".cooked.map")
	if len(cookedBlocks) != 0 {
		t.Errorf("cooked map = %+v; want no entries when the fallback is unsupported", cookedBlocks)
	}
}

func TestRipISOReactiveFallbackRecoversASector(t *testing.T) {
	dev := isoDisc()
	dev.markBad(5, modeCooked)
	cooked := newFakeCookedDevice(dev)
	cooked.putCooked(5, markSector(5, testiso.SectorSize))
	out := filepath.Join(t.TempDir(), "disc.iso")

	if _, err := ripISO(cooked, cooked.toc, out, 0, 1, true, false, false, true); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if sectorLBA(got[5*testiso.SectorSize:]) != 5 {
		t.Error("recovered sector 5 does not hold the fallback's data")
	}

	// The sector must read as good in the main map, since it was recovered.
	if bad := badRanges(readMap(t, out+".map"), testiso.SectorSize); len(bad) != 0 {
		t.Errorf("main map bad ranges = %v; a fallback-recovered sector must not be bad", bad)
	}
	// It must be the only entry in the cooked-provenance map.
	cookedFinished := finishedRanges(readMap(t, out+".cooked.map"), testiso.SectorSize)
	if len(cookedFinished) != 1 || cookedFinished[0] != [2]int64{5, 1} {
		t.Errorf("cooked map finished ranges = %v; want just sector 5", cookedFinished)
	}
}

func TestRipRejectsCuebinForDVDSizedDisc(t *testing.T) {
	dev := newFakeDevice(dvdSizedTOC())
	withDevice(t, dev)
	out := filepath.Join(t.TempDir(), "disc")

	if code := runRip([]string{"-format", "cuebin", "/dev/fake", out}); code != subcmd.ExitFailure {
		t.Errorf("runRip with -format cuebin on a DVD-sized disc = %d; want %d", code, subcmd.ExitFailure)
	}
}

// 2048-byte block reads are a DVD's native and only read form, so ripping one
// takes no flag. Every sector must come through that path, the low-level
// command must never be attempted, and no sector may be recorded as a fallback,
// since there was nothing to fall back from. Its lead-out is inside CD range,
// so only NotCD marks the medium as not a CD.
func TestRipISONonCDRipsWithNoFlag(t *testing.T) {
	dev := isoDisc()
	cooked := newFakeCookedDevice(dev)
	cooked.toc.NotCD = true
	sectors := cooked.toc.LeadOutLBA
	for lba := range sectors {
		cooked.putCooked(lba, markSector(lba, testiso.SectorSize))
	}
	out := filepath.Join(t.TempDir(), "disc.iso")

	// cookedFallback is false: the flag governs the CD case alone.
	if _, err := ripISO(cooked, cooked.toc, out, 0, 1, true, false, false, false); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(got)) != sectors*testiso.SectorSize {
		t.Fatalf("image is %d bytes; want %d", len(got), sectors*testiso.SectorSize)
	}
	for lba := range sectors {
		if sectorLBA(got[lba*testiso.SectorSize:]) != lba {
			t.Fatalf("sector %d does not hold the block reader's data", lba)
		}
		if n := cooked.cookedReads[lba]; n != 1 {
			t.Fatalf("LBA %d was read %d times through the block path; want 1", lba, n)
		}
		if n := dev.reads[readKey{lba, modeCooked}]; n != 0 {
			t.Fatalf("low-level ReadSector was called for LBA %d on non-CD media", lba)
		}
	}
	if bad := badRanges(readMap(t, out+".map"), testiso.SectorSize); len(bad) != 0 {
		t.Errorf("main map bad ranges = %v; every sector was read", bad)
	}
	// No sector here is second-class, so there is nothing for the provenance
	// sidecar to distinguish and it must not be written at all: it would
	// duplicate the map above and imply verification was skipped rather than
	// unavailable.
	if _, err := os.Stat(out + ".cooked.map"); err == nil {
		t.Error("a non-CD rip wrote a .cooked.map; the medium has no low-level path to contrast with")
	} else if !os.IsNotExist(err) {
		t.Errorf("stat .cooked.map: %v", err)
	}
}

// On a CD the sidecar is written whether or not anything fell back, so its
// presence is keyed to the medium rather than to whether damage happened.
func TestRipISOCleanCDWritesAnEmptyCookedMap(t *testing.T) {
	dev := isoDisc()
	cooked := newFakeCookedDevice(dev)
	out := filepath.Join(t.TempDir(), "disc.iso")

	if _, err := ripISO(cooked, cooked.toc, out, 0, 1, true, false, false, true); err != nil {
		t.Fatal(err)
	}
	if got := finishedRanges(readMap(t, out+".cooked.map"), testiso.SectorSize); len(got) != 0 {
		t.Errorf("cooked map finished ranges = %v; a clean rip fell back nowhere", got)
	}
}

// The whole command on a DVD, through the fake device: the ripISO tests above
// bypass flag parsing, so this test proves an operator who passes no flags
// gets an image.
func TestRipCommandRipsNonCDMediaWithNoFlag(t *testing.T) {
	dev := isoDisc()
	cooked := newFakeCookedDevice(dev)
	cooked.toc.NotCD = true
	for lba := range cooked.toc.LeadOutLBA {
		cooked.putCooked(lba, markSector(lba, testiso.SectorSize))
	}
	withDevice(t, cooked)
	out := filepath.Join(t.TempDir(), "disc.iso")

	if code := runRip([]string{"/dev/fake", out}); code != subcmd.ExitOK {
		t.Fatalf("runRip on non-CD media without -cooked-fallback = %d; want 0", code)
	}
	if !cooked.closed {
		t.Error("runRip did not close the device")
	}
	if _, err := os.Stat(out + ".map"); err != nil {
		t.Errorf("-map wrote no map file: %v", err)
	}
	if _, err := os.Stat(out + ".cooked.map"); !os.IsNotExist(err) {
		t.Errorf("stat .cooked.map = %v; a non-CD rip must not write one", err)
	}
}

func TestRipRejectsCuebinForNonCDDisc(t *testing.T) {
	dev := newFakeDevice(nonCDTOC())
	withDevice(t, dev)
	out := filepath.Join(t.TempDir(), "disc")

	if code := runRip([]string{"-format", "cuebin", "/dev/fake", out}); code != subcmd.ExitFailure {
		t.Errorf("runRip with -format cuebin on non-CD media = %d; want %d", code, subcmd.ExitFailure)
	}
}

// TestRipStatusSeparatesTheDiscFromTheRun covers the three answers a rip can
// give. A disc that lost sectors yields a usable image and says so. A .map that
// cannot be written is a run failure, not a warning, since a rip zero-fills
// what it cannot read and the map is the only record of those zeros.
func TestRipStatusSeparatesTheDiscFromTheRun(t *testing.T) {
	t.Run("clean disc", func(t *testing.T) {
		withDevice(t, isoDisc())
		out := filepath.Join(t.TempDir(), "disc.iso")
		if code := runRip([]string{"/dev/fake", out}); code != subcmd.ExitOK {
			t.Errorf("rip of a clean disc = %d; want %d", code, subcmd.ExitOK)
		}
	})

	t.Run("bad sectors", func(t *testing.T) {
		dev := isoDisc()
		dev.markBad(5, modeCooked)
		withDevice(t, dev)
		out := filepath.Join(t.TempDir(), "disc.iso")
		if code := runRip([]string{"-retries", "1", "/dev/fake", out}); code != subcmd.ExitDegraded {
			t.Errorf("rip over a bad sector = %d; want %d", code, subcmd.ExitDegraded)
		}
		// The image is still there and usable, so ExitDegraded is the right
		// answer rather than a failure.
		if _, err := os.Stat(out); err != nil {
			t.Errorf("no image was written: %v", err)
		}
		if bad := badRanges(readMap(t, out+".map"), testiso.SectorSize); len(bad) != 1 {
			t.Errorf("map bad ranges = %v; want the one sector", bad)
		}
	})

	t.Run("map cannot be written", func(t *testing.T) {
		withDevice(t, isoDisc())
		dir := t.TempDir()
		out := filepath.Join(dir, "disc.iso")
		// A directory where the sidecar belongs: nothing can create the file
		// over it, with or without -f.
		if err := os.Mkdir(out+".map", 0o755); err != nil {
			t.Fatal(err)
		}
		if code := runRip([]string{"/dev/fake", out}); code != subcmd.ExitFailure {
			t.Errorf("rip whose map could not be written = %d; want %d", code, subcmd.ExitFailure)
		}
	})
}

// A rip zero-fills what it cannot read, so the map recording those zeros is not
// optional and is written by default.
func TestRipWritesTheMapByDefault(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want bool
	}{
		{"iso, default", nil, true},
		{"iso, -no-map", []string{"-no-map"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withDevice(t, isoDisc())
			out := filepath.Join(t.TempDir(), "disc.iso")
			if code := runRip(append(tc.args, "/dev/fake", out)); code != subcmd.ExitOK {
				t.Fatalf("runRip = %d; want 0", code)
			}
			_, err := os.Stat(out + ".map")
			if got := err == nil; got != tc.want {
				t.Errorf("map written = %v; want %v (%v)", got, tc.want, err)
			}
		})
	}
	for _, tc := range []struct {
		name string
		args []string
		want bool
	}{
		{"cuebin, default", nil, true},
		{"cuebin, -no-map", []string{"-no-map"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withDevice(t, mixedDisc())
			stem := filepath.Join(t.TempDir(), "disc")
			if code := runRip(append(tc.args, "/dev/fake", stem+".bin")); code != subcmd.ExitOK {
				t.Fatalf("runRip = %d; want 0", code)
			}
			_, err := os.Stat(stem + ".bin.map")
			if got := err == nil; got != tc.want {
				t.Errorf("map written = %v; want %v (%v)", got, tc.want, err)
			}
		})
	}
}

// A session with an empty data range must not produce an empty image and exit
// 0: there was nothing to rip, and the tool must say so.
func TestRipRefusesAnEmptyRange(t *testing.T) {
	dev := newFakeDevice(cdrom.TOC{
		FirstTrack: 1, LastTrack: 1, LeadOutLBA: 0,
		Tracks: []cdrom.Track{{Number: 1, Session: 1, Type: cdrom.TrackTypeData, StartLBA: 0}},
	})
	withDevice(t, dev)
	out := filepath.Join(t.TempDir(), "disc.iso")
	if code := runRip([]string{"/dev/fake", out}); code != subcmd.ExitFailure {
		t.Errorf("runRip over an empty range = %d; want %d", code, subcmd.ExitFailure)
	}
}
