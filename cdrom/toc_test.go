package cdrom

import "testing"

// mixedTOC is a two-session disc: a data track, then two audio tracks with
// known pregaps, then a data track in a second session.
func mixedTOC() TOC {
	return TOC{
		FirstTrack: 1,
		LastTrack:  4,
		LeadOutLBA: 30000,
		Tracks: []Track{
			{Number: 1, Session: 1, Type: TrackTypeData, StartLBA: 0},
			{Number: 2, Session: 1, Type: TrackTypeAudio, StartLBA: 10000, Index0LBA: 9850},
			{Number: 3, Session: 1, Type: TrackTypeAudio, StartLBA: 20000, Index0LBA: 19850},
			{Number: 4, Session: 2, Type: TrackTypeData, StartLBA: 25000},
		},
	}
}

func TestTrackTypeAt(t *testing.T) {
	t.Parallel()
	toc := mixedTOC()
	tests := []struct {
		lba  int64
		want TrackType
	}{
		{0, TrackTypeData},
		{9999, TrackTypeData},
		{10000, TrackTypeAudio},
		{19999, TrackTypeAudio},
		{25000, TrackTypeData},
		{29999, TrackTypeData},
	}
	for _, tc := range tests {
		if got := toc.TrackTypeAt(tc.lba); got != tc.want {
			t.Errorf("TrackTypeAt(%d) = %s; want %s", tc.lba, got, tc.want)
		}
	}
	// A TOC with no tracks must not index out of range.
	empty := TOC{}
	if got := empty.TrackTypeAt(0); got != TrackTypeData {
		t.Errorf("TrackTypeAt on an empty TOC = %s", got)
	}
}

func TestNextTrackAfter(t *testing.T) {
	t.Parallel()
	toc := mixedTOC()
	tr, ok := toc.NextTrackAfter(0)
	if !ok || tr.Number != 2 {
		t.Errorf("NextTrackAfter(0) = %+v, %v; want track 2", tr, ok)
	}
	if _, ok := toc.NextTrackAfter(29999); ok {
		t.Error("NextTrackAfter past the last track reported a track")
	}
}

func TestInPregapWithIndex0(t *testing.T) {
	t.Parallel()
	toc := mixedTOC()
	tests := []struct {
		lba  int64
		want bool
	}{
		{9849, false},
		{9850, true},   // start of track 2's pregap
		{9999, true},   // last pregap sector
		{10000, false}, // INDEX 01: the track proper
		{19850, true},
		{24999, false}, // track 4 has no INDEX 00
	}
	for _, tc := range tests {
		if got := toc.InPregap(tc.lba); got != tc.want {
			t.Errorf("InPregap(%d) = %v; want %v", tc.lba, got, tc.want)
		}
	}
}

func TestInPregapWithoutIndex0(t *testing.T) {
	t.Parallel()
	// With no INDEX 00 anywhere, the answer falls back to whether the next
	// track is audio: only audio pregaps are stored in the image.
	toc := TOC{
		LeadOutLBA: 30000,
		Tracks: []Track{
			{Number: 1, Type: TrackTypeData, StartLBA: 0},
			{Number: 2, Type: TrackTypeAudio, StartLBA: 10000},
			{Number: 3, Type: TrackTypeData, StartLBA: 20000},
		},
	}
	if !toc.InPregap(9999) {
		t.Error("InPregap before an audio track = false")
	}
	if toc.InPregap(19999) {
		t.Error("InPregap before a data track = true")
	}
	if toc.InPregap(25000) {
		t.Error("InPregap past the last track = true")
	}
}

func TestHasAudio(t *testing.T) {
	t.Parallel()
	toc := mixedTOC()
	if !toc.HasAudio() {
		t.Error("HasAudio on a mixed disc = false")
	}
	dataOnly := TOC{Tracks: []Track{{Number: 1, Type: TrackTypeData}}}
	if dataOnly.HasAudio() {
		t.Error("HasAudio on a data-only disc = true")
	}
}

func TestLastDataSession(t *testing.T) {
	t.Parallel()
	toc := mixedTOC()
	session, idx := toc.LastDataSession()
	if session != 2 || idx != 3 {
		t.Errorf("LastDataSession = (%d, %d); want (2, 3)", session, idx)
	}
	audioOnly := TOC{Tracks: []Track{{Number: 1, Type: TrackTypeAudio}}}
	if _, idx := audioOnly.LastDataSession(); idx != -1 {
		t.Errorf("LastDataSession on an audio-only disc = %d; want -1", idx)
	}
}

func TestNeedsCookedReads(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		leadOutLBA int64
		notCD      bool
		want       bool
	}{
		{name: "within CD range", leadOutLBA: MaxLBA, want: false},
		{name: "one past CD range", leadOutLBA: MaxLBA + 1, want: true},
		{name: "DVD-sized lead-out", leadOutLBA: 1151849, want: true},
		// A DVD holding less than CD range fits every size test, so only the
		// device saying so can distinguish it -- the macOS case.
		{name: "small medium the device calls non-CD", leadOutLBA: 1000, notCD: true, want: true},
		{name: "DVD-sized and reported non-CD", leadOutLBA: 2481232, notCD: true, want: true},
	}
	for _, tc := range tests {
		toc := TOC{LeadOutLBA: tc.leadOutLBA, NotCD: tc.notCD}
		if got := toc.NeedsCookedReads(); got != tc.want {
			t.Errorf("%s: NeedsCookedReads() = %v; want %v", tc.name, got, tc.want)
		}
	}
}

func TestBlockStatusHelpers(t *testing.T) {
	t.Parallel()
	if TrackTypeAudio.String() != "audio" || TrackTypeData.String() != "data" {
		t.Errorf("TrackType strings: %s %s", TrackTypeAudio, TrackTypeData)
	}
}

// A caller ripping an ISO needs the last data session's *data* track. For a
// session that opens with audio and carries data after it, returning the first
// track would give the caller the audio track's LBA and it would read music as
// a filesystem.
func TestLastDataSessionPicksADataTrack(t *testing.T) {
	t.Parallel()
	toc := TOC{
		FirstTrack: 1, LastTrack: 3, LeadOutLBA: 200,
		Tracks: []Track{
			{Number: 1, Session: 1, Type: TrackTypeData, StartLBA: 0},
			{Number: 2, Session: 2, Type: TrackTypeAudio, StartLBA: 100},
			{Number: 3, Session: 2, Type: TrackTypeData, StartLBA: 150},
		},
	}
	session, idx := toc.LastDataSession()
	if session != 2 {
		t.Errorf("session = %d; want 2", session)
	}
	if idx != 2 {
		t.Fatalf("first track index = %d; want 2, the session's data track", idx)
	}
	if lba := toc.Tracks[idx].StartLBA; lba != 150 {
		t.Errorf("it starts at LBA %d; want 150", lba)
	}

	// A session whose data track comes first still reports that one.
	toc.Tracks[1], toc.Tracks[2] = Track{Number: 2, Session: 2, Type: TrackTypeData, StartLBA: 100},
		Track{Number: 3, Session: 2, Type: TrackTypeAudio, StartLBA: 150}
	if _, idx := toc.LastDataSession(); idx != 1 {
		t.Errorf("first track index = %d; want 1", idx)
	}
}

// An MSF address inside the 150-frame lead-in has no LBA, and a negative LBA
// would be used to index every per-sector slice a caller builds from the TOC.
func TestMSFToLBAClamps(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		m, s, f byte
		want    int64
	}{
		{0, 0, 0, 0},
		{0, 1, 0, 0},
		{0, 1, 74, 0},
		{0, 2, 0, 0},
		{0, 2, 1, 1},
		{1, 0, 0, 4350},
	} {
		if got := MSFToLBA(tc.m, tc.s, tc.f); got != tc.want {
			t.Errorf("MSFToLBA(%d, %d, %d) = %d; want %d", tc.m, tc.s, tc.f, got, tc.want)
		}
	}
}

// LBAToMSF clamps rather than wrapping, so the round trip has to come back at
// the address it started from for every LBA a CD can hold.
func TestLBAToMSFRoundTrip(t *testing.T) {
	t.Parallel()
	for _, lba := range []int64{0, 1, 74, 75, 4499, 4500, 100000} {
		m, s, f := LBAToMSF(lba)
		if got := MSFToLBA(m, s, f); got != lba {
			t.Errorf("LBA %d round-tripped through MSF(%d, %d, %d) to %d", lba, m, s, f, got)
		}
	}
}

// TrackAt is the lookup TrackTypeAt is built on, and both read the track list
// as ascending.
func TestTrackAt(t *testing.T) {
	t.Parallel()
	toc := TOC{Tracks: []Track{
		{Number: 1, Type: TrackTypeData, StartLBA: 0},
		{Number: 2, Type: TrackTypeAudio, StartLBA: 100},
	}}
	for _, tc := range []struct {
		lba  int64
		want uint8
		ok   bool
	}{
		{0, 1, true},
		{99, 1, true},
		{100, 2, true},
		{1000, 2, true},
		{-1, 0, false},
	} {
		tr, ok := toc.TrackAt(tc.lba)
		if ok != tc.ok || tr.Number != tc.want {
			t.Errorf("TrackAt(%d) = %d, %v; want %d, %v", tc.lba, tr.Number, ok, tc.want, tc.ok)
		}
	}
	var empty TOC
	if _, ok := empty.TrackAt(0); ok {
		t.Error("TrackAt on a TOC with no tracks found one")
	}
}
