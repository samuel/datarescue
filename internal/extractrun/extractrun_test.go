package extractrun

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"samuelks.com/datarescue/ddrescue"
	"samuelks.com/datarescue/internal/outsink"
	"samuelks.com/datarescue/internal/subcmd"
)

// sink is an outsink.Sink that keeps what it is given in memory and can be told
// to refuse a named entry, which is how the run's own failures are produced.
type sink struct {
	files    map[string][]byte
	refuse   map[string]bool
	manifest string
	closeErr error
	discards int
}

func newSink() *sink {
	return &sink{files: map[string][]byte{}, refuse: map[string]bool{}}
}

type member struct {
	bytes.Buffer
	s    *sink
	name string
}

func (m *member) Close() error {
	m.s.files[m.name] = append([]byte(nil), m.Bytes()...)
	return nil
}

func (s *sink) Create(e outsink.Entry) (io.WriteCloser, string, error) {
	if s.refuse[e.Path] {
		return nil, "", errors.New("the sink refuses " + e.Path)
	}
	return &member{s: s, name: e.Path}, e.Path, nil
}

func (s *sink) Discard(e outsink.Entry) error {
	s.discards++
	delete(s.files, e.Path)
	return nil
}

func (s *sink) Manifest(text string) error { s.manifest = text; return nil }
func (s *sink) Close() error               { return s.closeErr }

// reader delivers stop bytes and then fails, zero-filling the rest when it has
// an armed log, which is how a real reader behaves over a bad sector. A
// negative stop never fails.
type reader struct {
	data []byte
	stop int
	off  int
	fill *ddrescue.GapLog
	// nofill models damage whose length is not arithmetic, which every reader
	// in the toolkit latches on rather than guessing at.
	nofill bool
}

func (r *reader) Read(p []byte) (int, error) {
	if r.off >= len(r.data) {
		return 0, io.EOF
	}
	fail := errors.New("the sector would not read")
	if r.stop >= 0 && r.off >= r.stop {
		if r.nofill || !r.fill.Filling() {
			return 0, fail
		}
		hole := int64(len(r.data) - r.off)
		r.fill.Add(int64(r.off), hole, fail)
		n := min(len(p), int(hole))
		clear(p[:n])
		r.off += n
		return n, nil
	}
	end := len(r.data)
	if r.stop >= 0 {
		end = min(r.stop, end)
	}
	n := copy(p, r.data[r.off:end])
	r.off += n
	return n, nil
}

// write recovers one file through a reader that stops after stop bytes, filling
// the rest unless nofill says the hole cannot be measured.
func write(r *Run, path string, data []byte, stop int, nofill bool) Outcome {
	_, o := r.Write(outsink.Entry{Path: path}, int64(len(data)), func(l *ddrescue.GapLog) (io.Reader, error) {
		return &reader{data: data, stop: stop, fill: l, nofill: nofill}, nil
	})
	return o
}

// The classification is one question — does this finding describe the medium,
// or the run? Everything else about the exit code follows from it.
func TestOutcomes(t *testing.T) {
	data := bytes.Repeat([]byte("datarescue"), 100)
	for _, tc := range []struct {
		name   string
		stop   int
		nofill bool
		want   Outcome
	}{
		{"whole", -1, false, Whole},
		{"filled", 400, false, Filled},
		{"partial", 400, true, Partial},
		{"lost", 0, true, Lost},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSink()
			r := New("tool", "extract", s)
			if got := write(r, tc.name, data, tc.stop, tc.nofill); got != tc.want {
				t.Errorf("outcome = %d; want %d", got, tc.want)
			}
			if code := r.Finish(nil); (code == subcmd.ExitOK) != (tc.want == Whole) {
				t.Errorf("exit = %d for outcome %d", code, tc.want)
			}
		})
	}
}

// A file the medium gave nothing for is what the medium cost, not the run
// failing to do what was asked, so it exits degraded and not failed.
func TestNothingRecoveredIsDegraded(t *testing.T) {
	s := newSink()
	r := New("tool", "extract", s)
	if got := write(r, "gone.bin", []byte("a file"), 0, true); got != Lost {
		t.Fatalf("outcome = %d; want Lost", got)
	}
	if code := r.Finish(nil); code != subcmd.ExitDegraded {
		t.Errorf("exit = %d; want %d — the medium was short, not the run", code, subcmd.ExitDegraded)
	}
}

// A sink that will not take the file is the run failing, and that outranks any
// finding about the medium.
func TestSinkRefusalIsFailure(t *testing.T) {
	s := newSink()
	s.refuse["blocked.bin"] = true
	r := New("tool", "extract", s)
	if got := write(r, "blocked.bin", []byte("a file"), -1, false); got != Failed {
		t.Fatalf("outcome = %d; want Failed", got)
	}
	if code := r.Finish(nil); code != subcmd.ExitFailure {
		t.Errorf("exit = %d; want %d", code, subcmd.ExitFailure)
	}
}

// A file that filled holes and then stopped, whose map could not be written, is
// the case where both go wrong at once. The map outranks the damage: unrecorded
// zeros are worse than a short file, so the data is taken back and the run
// fails.
func TestFilledButUnrecordedIsFailure(t *testing.T) {
	data := bytes.Repeat([]byte("datarescue"), 100)
	s := newSink()
	s.refuse["half.bin"+outsink.MapSuffix] = true
	r := New("tool", "extract", s)

	if got := write(r, "half.bin", data, 400, false); got != Failed {
		t.Fatalf("outcome = %d; want Failed, the map having gone unwritten", got)
	}
	if s.discards == 0 {
		t.Error("the full-length file was left standing with nothing to explain its zeros")
	}
	if _, ok := s.files["half.bin"]; ok {
		t.Error("the data was kept although its map could not be written")
	}
	if code := r.Finish(nil); code != subcmd.ExitFailure {
		t.Errorf("exit = %d; want %d", code, subcmd.ExitFailure)
	}
}

// A path the operator gave that matches nothing means the run did not do what
// was asked, however healthy the medium is.
func TestUnmatchedSelectionFails(t *testing.T) {
	s := newSink()
	r := New("tool", "extract", s)
	r.Want("/present", "/absent")
	if !r.Wants("/present") {
		t.Fatal("a registered path was not wanted")
	}
	if r.Wants("/other") {
		t.Error("an unregistered path was wanted although a selection was given")
	}
	write(r, "/present", []byte("here"), -1, false)

	code := r.Finish(nil)
	if code != subcmd.ExitFailure {
		t.Errorf("exit = %d; want %d", code, subcmd.ExitFailure)
	}
	if !strings.Contains(s.manifest, "/absent") {
		t.Errorf("the report does not name the path that matched nothing:\n%s", s.manifest)
	}
}

// A close that fails leaves a truncated archive, whatever the files did.
func TestCloseFailureFails(t *testing.T) {
	s := newSink()
	s.closeErr = errors.New("the archive could not be finished")
	r := New("tool", "extract", s)
	write(r, "ok.bin", []byte("here"), -1, false)
	if code := r.Finish(nil); code != subcmd.ExitFailure {
		t.Errorf("exit = %d; want %d", code, subcmd.ExitFailure)
	}
}

// A run that selected nothing is only a failure where the selection is a prefix
// and nothing else would notice.
func TestRequireSelection(t *testing.T) {
	for _, require := range []bool{false, true} {
		s := newSink()
		r := New("tool", "extract", s)
		r.RequireSelection = require
		code := r.Finish(nil)
		want := subcmd.ExitOK
		if require {
			want = subcmd.ExitFailure
		}
		if code != want {
			t.Errorf("RequireSelection=%v exit = %d; want %d", require, code, want)
		}
	}
}
