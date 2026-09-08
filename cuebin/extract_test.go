package cuebin

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"samuelks.com/datarescue/cdrom"
	"samuelks.com/datarescue/ddrescue"
)

// rawSectors builds a BIN whose sectors are each filled with one byte, the
// sector index plus one, so a byte says which sector it came from and no
// sector's data can be mistaken for a zero-filled hole.
func rawSectors(numSectors, sectorSize int) []byte {
	buf := make([]byte, numSectors*sectorSize)
	for i := range numSectors {
		for j := range sectorSize {
			buf[i*sectorSize+j] = byte(i + 1)
		}
	}
	return buf
}

// bad marks [off, off+size) unrecovered, everything else in a file of size
// bytes recovered.
func bad(size, off, length int64) []ddrescue.Block {
	var blocks []ddrescue.Block
	if off > 0 {
		blocks = append(blocks, ddrescue.Block{Offset: 0, Size: off, Status: ddrescue.BlockStatusFinished})
	}
	blocks = append(blocks, ddrescue.Block{Offset: off, Size: length, Status: ddrescue.BlockStatusFailedBlockBadSectors})
	if end := off + length; end < size {
		blocks = append(blocks, ddrescue.Block{Offset: end, Size: size - end, Status: ddrescue.BlockStatusFinished})
	}
	return blocks
}

// mapBlocks writes tm out and parses it back, which is the only check that the
// map an operator gets is the one the export meant.
func mapBlocks(t *testing.T, tm *TrackMap) []ddrescue.Block {
	t.Helper()
	var b bytes.Buffer
	if _, err := tm.WriteTo(&b); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "track.map")
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	blocks, err := ddrescue.ParseMap(path)
	if err != nil {
		t.Fatalf("the exported map does not parse: %v\n%s", err, b.String())
	}
	return blocks
}

func TestExtractDataTrackMODE1(t *testing.T) {
	t.Parallel()
	// Each MODE1/2352 sector: 12 sync + 4 header + 2048 data + 288 ECC.
	const numSectors = 3
	bin := make([]byte, numSectors*cdrom.RawSectorSize)
	for s := range numSectors {
		sec := bin[s*cdrom.RawSectorSize:]
		for i := 12; i < 16; i++ {
			sec[i] = 0x01
		}
		for i := 16; i < 16+2048; i++ {
			sec[i] = byte(0xAA + s)
		}
		for i := 16 + 2048; i < cdrom.RawSectorSize; i++ {
			sec[i] = 0xFF
		}
	}

	var out bytes.Buffer
	tm, err := ExtractDataTrack(bytes.NewReader(bin), nil, 0, numSectors, "MODE1/2352", cdrom.RawSectorSize, &out)
	if err != nil {
		t.Fatal(err)
	}
	if tm != nil {
		t.Error("a source with no rescue map produced one; nothing was measured to put in it")
	}
	if out.Len() != numSectors*2048 {
		t.Fatalf("output size = %d; want %d", out.Len(), numSectors*2048)
	}
	data := out.Bytes()
	for s := range numSectors {
		for i := range 2048 {
			if data[s*2048+i] != byte(0xAA+s) {
				t.Fatalf("sector %d byte %d = 0x%02x; want 0x%02x", s, i, data[s*2048+i], byte(0xAA+s))
			}
		}
	}
}

// MODE2/2352 carries 2336 bytes of user data, not 2048, so both the stride of
// the exported file and the window read from each sector differ from MODE1.
func TestExtractDataTrackMODE2(t *testing.T) {
	t.Parallel()
	const numSectors = 3
	bin := rawSectors(numSectors, cdrom.RawSectorSize)

	var out bytes.Buffer
	if _, err := ExtractDataTrack(bytes.NewReader(bin), nil, 0, numSectors, "MODE2/2352", cdrom.RawSectorSize, &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() != numSectors*2336 {
		t.Fatalf("output size = %d; want %d (2336 bytes of user data per MODE2 sector)", out.Len(), numSectors*2336)
	}
	// Each sector's bytes are its own index, so a wrong window or stride shows
	// as the wrong byte at a sector boundary.
	data := out.Bytes()
	for s := range numSectors {
		if got := data[s*2336]; got != byte(s+1) {
			t.Errorf("first byte of exported sector %d = 0x%02x; want 0x%02x", s, got, byte(s+1))
		}
	}
}

// A hole in a sector's payload becomes zeros in the exported file and a range
// in its map, translated from the disc's offsets into the file's own.
func TestExtractDataTrackTranslatesHoles(t *testing.T) {
	t.Parallel()
	const numSectors = 4
	bin := rawSectors(numSectors, cdrom.RawSectorSize)
	size := int64(len(bin))
	// 50 bytes lost, 100 into sector 2's payload.
	const holeIn = 2*cdrom.RawSectorSize + 16 + 100
	blocks := bad(size, holeIn, 50)

	var out bytes.Buffer
	tm, err := ExtractDataTrack(bytes.NewReader(bin), blocks, 0, numSectors, "MODE1/2352", cdrom.RawSectorSize, &out)
	if err != nil {
		t.Fatal(err)
	}
	if !tm.Damaged() || tm.Lost() != 50 {
		t.Fatalf("TrackMap lost %d bytes; want 50", tm.Lost())
	}

	data := out.Bytes()
	const outHole = 2*2048 + 100
	for i := outHole; i < outHole+50; i++ {
		if data[i] != 0 {
			t.Fatalf("exported byte %d = 0x%02x; want 0 (the range was never recovered)", i, data[i])
		}
	}
	if data[outHole-1] != 3 || data[outHole+50] != 3 {
		t.Error("the bytes either side of the hole were not the sector's own")
	}

	// The map has to say the same thing, in the exported file's offsets.
	got := ddrescue.Unrecovered(mapBlocks(t, tm), 0, int64(len(data)))
	if len(got) != 1 || got[0].Offset != outHole || got[0].Size != 50 {
		t.Fatalf("map reports %v; want one range of 50 bytes at %d", got, outHole)
	}
}

// A track that does not start at LBA 0 is exported from its own offset zero,
// which is what the map must be written in.
func TestExtractDataTrackOffsetTrack(t *testing.T) {
	t.Parallel()
	const numSectors = 6
	bin := rawSectors(numSectors, cdrom.RawSectorSize)
	blocks := bad(int64(len(bin)), 4*cdrom.RawSectorSize+16, 2048)

	var out bytes.Buffer
	tm, err := ExtractDataTrack(bytes.NewReader(bin), blocks, 3, numSectors, "MODE1/2352", cdrom.RawSectorSize, &out)
	if err != nil {
		t.Fatal(err)
	}
	got := ddrescue.Unrecovered(mapBlocks(t, tm), 0, int64(out.Len()))
	if len(got) != 1 || got[0].Offset != 2048 || got[0].Size != 2048 {
		t.Fatalf("map reports %v; want the second exported sector, at offset 2048", got)
	}
}

// A sector whose ECC was never recovered but whose payload was contributes real
// bytes to the exported file, which does not carry the ECC at all. Reporting it
// lost would call a sound file damaged.
func TestExtractDataTrackECCHoleIsNotUserData(t *testing.T) {
	t.Parallel()
	const numSectors = 2
	bin := rawSectors(numSectors, cdrom.RawSectorSize)
	blocks := bad(int64(len(bin)), 16+2048, 288) // sector 0's ECC area

	var out bytes.Buffer
	tm, err := ExtractDataTrack(bytes.NewReader(bin), blocks, 0, numSectors, "MODE1/2352", cdrom.RawSectorSize, &out)
	if err != nil {
		t.Fatal(err)
	}
	if tm.Damaged() {
		t.Errorf("an unrecovered ECC area cost the exported file %d bytes; it holds no ECC", tm.Lost())
	}
	for i, b := range out.Bytes()[:2048] {
		if b != 1 {
			t.Fatalf("exported byte %d = 0x%02x; want 0x01: the payload was zeroed although only the ECC was lost", i, b)
		}
	}
}

// A BIN that stops mid-track is a truncated file, not a range the rescue never
// recovered: the export would simply be short, so it is reported.
func TestExtractDataTrackTruncatedBIN(t *testing.T) {
	t.Parallel()
	bin := rawSectors(2, cdrom.RawSectorSize)
	var out bytes.Buffer
	if _, err := ExtractDataTrack(bytes.NewReader(bin), nil, 0, 4, "MODE1/2352", cdrom.RawSectorSize, &out); err == nil {
		t.Error("a BIN shorter than the track exported without complaint")
	}
}

func TestExtractAudioTrackWAV(t *testing.T) {
	t.Parallel()
	const numSectors = 2
	bin := rawSectors(numSectors, cdrom.RawSectorSize)

	var out bytes.Buffer
	if _, err := ExtractAudioTrack(bytes.NewReader(bin), nil, 0, numSectors, cdrom.RawSectorSize, false, &out); err != nil {
		t.Fatal(err)
	}

	hdr := out.Bytes()
	if string(hdr[0:4]) != "RIFF" {
		t.Error("missing RIFF header")
	}
	if string(hdr[8:12]) != "WAVE" {
		t.Error("missing WAVE marker")
	}
	dataSize := binary.LittleEndian.Uint32(hdr[40:44])
	want := uint32(numSectors * cdrom.RawSectorSize)
	if dataSize != want {
		t.Errorf("WAV data size = %d; want %d", dataSize, want)
	}
	if got := binary.LittleEndian.Uint32(hdr[28:32]); got != 44100*4 {
		t.Errorf("WAV byte rate = %d; want %d", got, 44100*4)
	}
	totalWant := wavHeaderSize + numSectors*cdrom.RawSectorSize
	if out.Len() != totalWant {
		t.Errorf("output total = %d; want %d", out.Len(), totalWant)
	}
}

// The WAV header is the tool's own bytes, so every hole in the exported file
// sits 44 bytes further on than it does in the BIN.
func TestExtractAudioTrackHeaderOffset(t *testing.T) {
	t.Parallel()
	const numSectors = 3
	bin := rawSectors(numSectors, cdrom.RawSectorSize)
	blocks := bad(int64(len(bin)), cdrom.RawSectorSize, cdrom.RawSectorSize)

	var out bytes.Buffer
	tm, err := ExtractAudioTrack(bytes.NewReader(bin), blocks, 0, numSectors, cdrom.RawSectorSize, false, &out)
	if err != nil {
		t.Fatal(err)
	}
	got := ddrescue.Unrecovered(mapBlocks(t, tm), 0, int64(out.Len()))
	if len(got) != 1 || got[0].Offset != wavHeaderSize+cdrom.RawSectorSize || got[0].Size != cdrom.RawSectorSize {
		t.Fatalf("map reports %v; want one sector lost at %d", got, wavHeaderSize+cdrom.RawSectorSize)
	}
	if tm.Lost() != cdrom.RawSectorSize {
		t.Errorf("Lost = %d; want %d", tm.Lost(), cdrom.RawSectorSize)
	}
}

// Swapping moves each byte to the other half of its 2-byte sample, so a hole
// that starts or ends mid-sample has to be recorded outward to the sample
// boundary: otherwise a real byte lands where the map says lost.
func TestExtractAudioTrackSwapRoundsHoles(t *testing.T) {
	t.Parallel()
	const numSectors = 1
	bin := rawSectors(numSectors, cdrom.RawSectorSize)
	blocks := bad(int64(len(bin)), 101, 5) // odd offset, odd length

	for _, swap := range []bool{false, true} {
		var out bytes.Buffer
		tm, err := ExtractAudioTrack(bytes.NewReader(bin), blocks, 0, numSectors, cdrom.RawSectorSize, swap, &out)
		if err != nil {
			t.Fatal(err)
		}
		got := ddrescue.Unrecovered(mapBlocks(t, tm), 0, int64(out.Len()))
		if len(got) != 1 {
			t.Fatalf("swap=%v: map reports %v; want one range", swap, got)
		}
		wantOff, wantSize := int64(wavHeaderSize+101), int64(5)
		if swap {
			wantOff, wantSize = wavHeaderSize+100, 6
		}
		if got[0].Offset != wantOff || got[0].Size != wantSize {
			t.Errorf("swap=%v: lost range = %d+%d; want %d+%d", swap, got[0].Offset, got[0].Size, wantOff, wantSize)
		}
	}
}

func TestTrackBounds(t *testing.T) {
	t.Parallel()
	cueText := `FILE "disc.bin" BINARY
  TRACK 01 MODE1/2352
    INDEX 01 00:00:00
  TRACK 02 AUDIO
    PREGAP 00:02:00
    INDEX 01 00:10:00
  TRACK 03 AUDIO
    INDEX 00 00:20:00
    INDEX 01 00:22:00
`
	sheet, err := ParseSheet(strings.NewReader(cueText))
	if err != nil {
		t.Fatal(err)
	}

	// The last track INDEX 01 is at 00:22:00, LBA 1650, so 2000 sectors is
	// enough of a file for all three.
	const totalSectors = 2000
	bounds, err := TrackBounds(sheet, totalSectors*cdrom.RawSectorSize)
	if err != nil {
		t.Fatal(err)
	}
	if len(bounds) != 3 {
		t.Fatalf("bounds = %d entries; want 3", len(bounds))
	}

	// Track 1 ends where track 2's bytes begin, which is its INDEX 01 at
	// 00:10:00 = LBA 750. Track 2's PREGAP is silence the writer generates and
	// is not in the file, so it moves nothing.
	if bounds[0][0] != 0 || bounds[0][1] != 750 {
		t.Errorf("track 1 bounds = %v; want [0, 750]", bounds[0])
	}
	// Track 2 ends at track 3's INDEX 00 @ 00:20:00 = LBA 1500, which *is* in
	// the file.
	if bounds[1][0] != 750 || bounds[1][1] != 1500 {
		t.Errorf("track 2 bounds = %v; want [750, 1500]", bounds[1])
	}
	// Track 3: INDEX 00 @ 00:20:00 = LBA 1500; ends at totalSectors = 2000.
	if bounds[2][0] != 1500 || bounds[2][1] != totalSectors {
		t.Errorf("track 3 bounds = %v; want [1500, %d]", bounds[2], totalSectors)
	}
}

// A track carrying both PREGAP and INDEX 00 used to subtract the pregap from
// the first index rather than from INDEX 01, which put the previous track's end
// before its start — and verify then indexed a per-sector slice at -150.
func TestTrackBoundsPregapAndIndex00(t *testing.T) {
	t.Parallel()
	cueText := `FILE "disc.bin" BINARY
  TRACK 01 MODE1/2352
    INDEX 01 00:00:00
  TRACK 02 AUDIO
    PREGAP 00:02:00
    INDEX 00 00:00:00
    INDEX 01 00:02:00
`
	sheet, err := ParseSheet(strings.NewReader(cueText))
	if err != nil {
		t.Fatal(err)
	}
	bounds, err := TrackBounds(sheet, 1000*cdrom.RawSectorSize)
	if err != nil {
		t.Fatal(err)
	}
	for i, b := range bounds {
		if b[0] < 0 || b[1] < b[0] {
			t.Errorf("track %d bounds = %v; a track cannot end before it starts", i+1, b)
		}
	}
}

// A cue sheet's MSF fields are the arithmetic every bound rests on, so a
// negative or overflowing one is refused where it is parsed rather than
// yielding a negative LBA.
func TestParseSheetBoundsMSF(t *testing.T) {
	t.Parallel()
	for _, pos := range []string{"-1:00:00", "00:-1:00", "00:00:-1", "00:60:00", "00:00:75", "100:00:00"} {
		cueText := "FILE \"disc.bin\" BINARY\n  TRACK 01 AUDIO\n    INDEX 01 " + pos + "\n"
		if _, err := ParseSheet(strings.NewReader(cueText)); err == nil {
			t.Errorf("ParseSheet accepted INDEX 01 %s", pos)
		}
	}
}
