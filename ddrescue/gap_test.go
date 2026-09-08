package ddrescue

import (
	"bytes"
	"errors"
	"testing"
)

var errUnreadable = errors.New("unreadable")

// A log a caller builds for itself must never fill. Only Fill arms one, and it
// writes the map in the same call, so the type itself guarantees zeros appear
// only where a map records them.
func TestUnarmedLogDoesNotFill(t *testing.T) {
	t.Parallel()
	var l GapLog
	if l.Filling() {
		t.Error("the zero value of a GapLog is armed; it must not be")
	}
	var nilLog *GapLog
	if nilLog.Filling() {
		t.Error("a nil GapLog is armed; it must not be")
	}
	l.Limit(100)
	l.Add(0, 10, errUnreadable)
	if len(l.Gaps()) != 0 || l.Bytes() != 0 {
		t.Errorf("an unarmed log recorded %v; it must record nothing", l.Gaps())
	}
	// The same holds through Recover, which a reader reaches by another path.
	buf := make([]byte, 8)
	l.Recover(Chunk{Buf: buf, Unit: 4, Read: failingRead}, 0, errUnreadable)
	if len(l.Gaps()) != 0 {
		t.Errorf("an unarmed log recorded %v through Recover", l.Gaps())
	}
}

func failingRead(int64, []byte) (int, error) { return 0, errUnreadable }

func TestGapLogAddClipsAndCoalesces(t *testing.T) {
	t.Parallel()
	l := &GapLog{armed: true}
	l.Limit(1000)

	l.Add(0, 100, errUnreadable)
	l.Add(100, 50, errUnreadable) // meets the previous one: they merge
	l.Add(400, 100, errUnreadable)
	l.Add(900, 500, errUnreadable) // runs past the file: clipped to its end
	l.Add(1000, 10, errUnreadable) // wholly past the file: dropped
	l.Add(500, 0, errUnreadable)   // empty: dropped

	want := []Gap{
		{FileOffset: 0, Length: 150},
		{FileOffset: 400, Length: 100},
		{FileOffset: 900, Length: 100},
	}
	got := l.Gaps()
	if len(got) != len(want) {
		t.Fatalf("Gaps() = %v; want %v", got, want)
	}
	for i := range got {
		if got[i].FileOffset != want[i].FileOffset || got[i].Length != want[i].Length {
			t.Errorf("gap %d = %+v; want offset %d length %d", i, got[i], want[i].FileOffset, want[i].Length)
		}
	}
	if l.Bytes() != 350 {
		t.Errorf("Bytes() = %d; want 350", l.Bytes())
	}
}

// scriptedMedium fails the units whose start offset is in bad, and fills
// everything else with a byte derived from the offset.
type scriptedMedium struct {
	bad  map[int64]bool
	unit int64
}

func (m scriptedMedium) read(off int64, buf []byte) (int, error) {
	if m.bad[off] {
		return 0, errUnreadable
	}
	for i := range buf {
		buf[i] = byte(off/m.unit) + 1
	}
	return len(buf), nil
}

func TestRecover(t *testing.T) {
	t.Parallel()
	const (
		unit  = 4
		chunk = 16 // four units
	)
	tests := []struct {
		name string
		have int // bytes the whole-chunk read delivered before failing
		bad  []int64
		want []Gap
	}{
		{"the first unit is lost", 0, []int64{0},
			[]Gap{{FileOffset: 0, Length: unit}}},
		{"a middle unit is lost", 4, []int64{4},
			[]Gap{{FileOffset: 4, Length: unit}}},
		{"the last unit is lost", 12, []int64{12},
			[]Gap{{FileOffset: 12, Length: unit}}},
		{"two runs are kept apart", 0, []int64{0, 8},
			[]Gap{{FileOffset: 0, Length: unit}, {FileOffset: 8, Length: unit}}},
		{"adjacent units coalesce", 0, []int64{4, 8},
			[]Gap{{FileOffset: 4, Length: 2 * unit}}},
		// The read stopped mid-unit, so the rest of that unit is lost with it
		// and the retry resumes at the next boundary.
		{"a failure mid-unit loses the rest of it", 6, []int64{4},
			[]Gap{{FileOffset: 6, Length: 2}}},
		{"everything after a mid-unit failure is lost", 6, []int64{4, 8, 12},
			[]Gap{{FileOffset: 6, Length: chunk - 6}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := scriptedMedium{bad: map[int64]bool{}, unit: unit}
			for _, b := range tc.bad {
				m.bad[b] = true
			}
			l := &GapLog{armed: true}
			buf := make([]byte, chunk)
			for i := range buf {
				buf[i] = 0xFF // stale data from a previous chunk
			}
			l.Recover(Chunk{Buf: buf, Unit: unit, Read: m.read}, tc.have, errUnreadable)

			got := l.Gaps()
			if len(got) != len(tc.want) {
				t.Fatalf("Gaps() = %v; want %v", got, tc.want)
			}
			for i := range got {
				if got[i].FileOffset != tc.want[i].FileOffset || got[i].Length != tc.want[i].Length {
					t.Errorf("gap %d = %+v; want offset %d length %d",
						i, got[i], tc.want[i].FileOffset, tc.want[i].Length)
				}
			}
			// Every byte a gap covers is zero, and no byte outside one is.
			for i := tc.have; i < chunk; i++ {
				var inGap bool
				for _, g := range got {
					if int64(i) >= g.FileOffset && int64(i) < g.FileOffset+g.Length {
						inGap = true
					}
				}
				if inGap && buf[i] != 0 {
					t.Fatalf("byte %d is in a gap but is %#x, not zero", i, buf[i])
				}
				if !inGap && buf[i] == 0 {
					t.Fatalf("byte %d was recovered but is zero", i)
				}
			}
		})
	}
}

// A chunk with no smaller unit is all or nothing.
func TestRecoverWholeChunk(t *testing.T) {
	t.Parallel()
	l := &GapLog{armed: true}
	buf := make([]byte, 8)
	l.Recover(Chunk{FileOffset: 100, Buf: buf, Unit: 0, Read: failingRead}, 0, errUnreadable)
	want := Gap{FileOffset: 100, Length: 8}
	got := l.Gaps()
	if len(got) != 1 || got[0].FileOffset != want.FileOffset || got[0].Length != want.Length {
		t.Errorf("Gaps() = %v; want [%+v]", got, want)
	}
	if !bytes.Equal(buf, make([]byte, 8)) {
		t.Error("the unrecovered chunk was not zeroed")
	}
}

// A ReadFunc is allowed to come back short without an error, and the bytes it
// did not deliver are zeros. Recover exists to make sure zeros are recorded, so
// treating a short read as complete here would leave unrecorded zeros in the
// file.
func TestRecoverRecordsAShortReadWithNoError(t *testing.T) {
	t.Parallel()
	l := &GapLog{armed: true}
	buf := make([]byte, 8)
	short := func(_ int64, b []byte) (int, error) { return len(b) / 2, nil }
	l.Recover(Chunk{FileOffset: 0, Buf: buf, Unit: 4, Read: short}, 0, errUnreadable)

	want := []Gap{{FileOffset: 2, Length: 2}, {FileOffset: 6, Length: 2}}
	got := l.Gaps()
	if len(got) != len(want) {
		t.Fatalf("Gaps() = %v; want %v", got, want)
	}
	for i := range got {
		if got[i].FileOffset != want[i].FileOffset || got[i].Length != want[i].Length {
			t.Errorf("gap %d = %+v; want offset %d length %d", i, got[i], want[i].FileOffset, want[i].Length)
		}
	}
	for _, g := range want {
		if !bytes.Equal(buf[g.FileOffset:g.FileOffset+g.Length], make([]byte, g.Length)) {
			t.Errorf("the bytes at %d are not zeros", g.FileOffset)
		}
	}
}

// A caller claiming more bytes came back than the chunk holds is wrong about
// the read; that must not cause a panic in the type the safety guarantee
// depends on.
func TestRecoverClampsHaveToTheBuffer(t *testing.T) {
	t.Parallel()
	l := &GapLog{armed: true}
	buf := make([]byte, 8)
	l.Recover(Chunk{Buf: buf, Unit: 4, Read: failingRead}, 100, errUnreadable)
	if len(l.Gaps()) != 0 {
		t.Errorf("Gaps() = %v; the whole chunk was claimed recovered, so there is no hole", l.Gaps())
	}
}
