package cuebin

import (
	"strings"
	"testing"
)

func TestParseSheetDataOnly(t *testing.T) {
	cue := `FILE "disc.bin" BINARY
  TRACK 01 MODE1/2352
    INDEX 01 00:00:00
`
	sheet, err := ParseSheet(strings.NewReader(cue))
	if err != nil {
		t.Fatal(err)
	}
	if len(sheet.Files) != 1 {
		t.Fatalf("files = %d; want 1", len(sheet.Files))
	}
	cf := sheet.Files[0]
	if cf.Path != "disc.bin" {
		t.Errorf("path = %q; want disc.bin", cf.Path)
	}
	if cf.Format != "BINARY" {
		t.Errorf("format = %q; want BINARY", cf.Format)
	}
	if len(cf.Tracks) != 1 {
		t.Fatalf("tracks = %d; want 1", len(cf.Tracks))
	}
	tr := cf.Tracks[0]
	if tr.Number != 1 {
		t.Errorf("track number = %d; want 1", tr.Number)
	}
	if tr.Mode != "MODE1/2352" {
		t.Errorf("mode = %q; want MODE1/2352", tr.Mode)
	}
	lba, err := tr.StartLBA()
	if err != nil {
		t.Fatal(err)
	}
	if lba != 0 {
		t.Errorf("start LBA = %d; want 0", lba)
	}
}

func TestParseSheetMixedMode(t *testing.T) {
	cue := `FILE "disc.bin" BINARY
  TRACK 01 MODE1/2352
    INDEX 01 00:00:00
  TRACK 02 AUDIO
    PREGAP 00:02:00
    INDEX 01 25:43:22
  TRACK 03 AUDIO
    INDEX 00 48:12:10
    INDEX 01 48:14:10
`
	sheet, err := ParseSheet(strings.NewReader(cue))
	if err != nil {
		t.Fatal(err)
	}
	cf := sheet.Files[0]
	if len(cf.Tracks) != 3 {
		t.Fatalf("tracks = %d; want 3", len(cf.Tracks))
	}

	if cf.Tracks[0].Mode != "MODE1/2352" {
		t.Errorf("track 1 mode = %q; want MODE1/2352", cf.Tracks[0].Mode)
	}
	if cf.Tracks[1].Mode != "AUDIO" {
		t.Errorf("track 2 mode = %q; want AUDIO", cf.Tracks[1].Mode)
	}
	if cf.Tracks[1].Pregap == nil {
		t.Error("track 2 has no PREGAP")
	}

	// Track 3 has INDEX 00 (pregap in bin) and INDEX 01
	tr3 := cf.Tracks[2]
	if len(tr3.Indices) != 2 {
		t.Fatalf("track 3 indices = %d; want 2", len(tr3.Indices))
	}
	if tr3.Indices[0].Number != 0 {
		t.Errorf("track 3 first index = %d; want 0", tr3.Indices[0].Number)
	}
	if tr3.Indices[1].Number != 1 {
		t.Errorf("track 3 second index = %d; want 1", tr3.Indices[1].Number)
	}
}

func TestParseSheetREMAndMetadata(t *testing.T) {
	cue := `REM GENRE Rock
REM DATE 1994
PERFORMER "Some Artist"
TITLE "Some Album"
FILE "disc.bin" BINARY
  TRACK 01 MODE1/2352
    INDEX 01 00:00:00
`
	sheet, err := ParseSheet(strings.NewReader(cue))
	if err != nil {
		t.Fatal(err)
	}
	if len(sheet.Files) != 1 || len(sheet.Files[0].Tracks) != 1 {
		t.Fatalf("unexpected parse result: %+v", sheet)
	}
}

func TestMSFLBA(t *testing.T) {
	tests := []struct {
		msf  MSF
		want int64
	}{
		{MSF{0, 0, 0}, 0},
		{MSF{0, 0, 1}, 1},
		{MSF{0, 1, 0}, 75},
		{MSF{1, 0, 0}, 4500},
		{MSF{74, 59, 74}, 337499}, // 74*4500 + 59*75 + 74
	}
	for _, tt := range tests {
		if got := tt.msf.LBA(); got != tt.want {
			t.Errorf("MSF%v.LBA() = %d; want %d", tt.msf, got, tt.want)
		}
	}
}

func TestBinSectorSize(t *testing.T) {
	tests := []struct {
		name   string
		tracks []CueTrack
		want   int
	}{
		{"raw", []CueTrack{{Mode: "MODE1/2352"}, {Mode: "AUDIO"}}, 2352},
		{"cooked", []CueTrack{{Mode: "MODE1/2048"}}, 2048},
		{"mode2 form2", []CueTrack{{Mode: "MODE2/2336"}}, 2336},
		{"cd+g", []CueTrack{{Mode: "CDG"}}, 2448},
		{"lower case", []CueTrack{{Mode: "mode1/2352"}}, 2352},
	}
	for _, tc := range tests {
		got, err := BinSectorSize(tc.tracks)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: BinSectorSize = %d; want %d", tc.name, got, tc.want)
		}
	}
}

// A mode the parser does not know is an error, not 2352. Guessing would make a
// legal MODE2/2336 sheet silently drift 16 bytes per sector, and tracks that
// disagree would give one sector index two positions.
func TestBinSectorSizeRefusals(t *testing.T) {
	tests := []struct {
		name   string
		tracks []CueTrack
	}{
		{"no tracks", nil},
		{"unknown mode", []CueTrack{{Number: 1, Mode: "MODE3/9999"}}},
		{"empty mode", []CueTrack{{Number: 1, Mode: ""}}},
		{"disagreeing strides", []CueTrack{{Number: 1, Mode: "MODE1/2048"}, {Number: 2, Mode: "AUDIO"}}},
	}
	for _, tc := range tests {
		if got, err := BinSectorSize(tc.tracks); err == nil {
			t.Errorf("%s: BinSectorSize = %d, nil; want an error", tc.name, got)
		}
	}
}
