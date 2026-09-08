package subcmd

import "testing"

func TestUsageListing(t *testing.T) {
	// The listing pads the command column to the longest name, so a short name
	// and a long one line up.
	cmds := []Command{
		{Name: "ls", Summary: "list files"},
		{Name: "extract", Summary: "extract files to a directory"},
	}
	usage("tool", "<image>", cmds)
}

func TestExitCodesAreDistinct(t *testing.T) {
	codes := map[string]int{"ok": ExitOK, "degraded": ExitDegraded, "usage": ExitUsage, "failure": ExitFailure}
	seen := map[int]string{}
	for name, code := range codes {
		if other, dup := seen[code]; dup {
			t.Errorf("%s and %s are both %d; a caller cannot tell them apart", name, other, code)
		}
		seen[code] = name
	}
	if ExitOK != 0 {
		t.Errorf("ExitOK = %d; a successful run must exit 0", ExitOK)
	}
	// A caller testing "are the artifacts usable" writes code <= ExitDegraded,
	// so a finding about the medium has to sort below a failure of the run.
	if ExitDegraded >= ExitFailure {
		t.Errorf("ExitDegraded = %d, ExitFailure = %d; damage must sort below a failed run",
			ExitDegraded, ExitFailure)
	}
}

// Status turns findings into codes, and a failure of the run outranks a finding
// about the medium: the first says the output cannot be trusted, the second
// only says how much of the medium was there.
func TestStatusCode(t *testing.T) {
	var zero Status
	if zero.Code() != ExitOK {
		t.Errorf("the zero Status = %d; want %d", zero.Code(), ExitOK)
	}

	var degraded Status
	degraded.Degraded()
	if degraded.Code() != ExitDegraded {
		t.Errorf("a degraded Status = %d; want %d", degraded.Code(), ExitDegraded)
	}

	var failed Status
	failed.Failed()
	if failed.Code() != ExitFailure {
		t.Errorf("a failed Status = %d; want %d", failed.Code(), ExitFailure)
	}

	var both Status
	both.Degraded()
	both.Failed()
	if both.Code() != ExitFailure {
		t.Errorf("a run that found damage and then failed = %d; want %d", both.Code(), ExitFailure)
	}

	// Add folds one in without losing either finding.
	var outer Status
	outer.Degraded()
	outer.Add(failed)
	if outer.Code() != ExitFailure {
		t.Errorf("after Add = %d; want %d", outer.Code(), ExitFailure)
	}
}

// TestCurrentVersion checks that a version set at link time wins, and that a
// test binary, which carries no module version, reports dev.
func TestCurrentVersion(t *testing.T) {
	if got := CurrentVersion(); got != "dev" {
		t.Errorf("CurrentVersion() = %q with nothing set, want dev", got)
	}
	Version = "v1.2.3"
	t.Cleanup(func() { Version = "" })
	if got := CurrentVersion(); got != "v1.2.3" {
		t.Errorf("CurrentVersion() = %q, want the linked version", got)
	}
}
