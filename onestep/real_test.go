package onestep_test

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"samuelks.com/datarescue/onestep"
)

// realDirEnv points to a directory of *.1-Step files to test against. Real
// archives cannot be committed, so this stays skipped unless an operator
// points it at their own.
const realDirEnv = "DATARESCUE_1STEP_DIR"

// TestRealArchives verifies every file of every backup set found in the
// directory realDirEnv points to, grouping the files into sets by the job
// number and set timestamp in their headers.
func TestRealArchives(t *testing.T) {
	dir := os.Getenv(realDirEnv)
	if dir == "" {
		t.Skipf("%s is not set", realDirEnv)
	}
	paths, err := filepath.Glob(filepath.Join(dir, "*.1-Step"))
	if err != nil {
		t.Fatalf("glob: %s", err)
	}
	if len(paths) == 0 {
		t.Skipf("no *.1-Step files in %s", dir)
	}
	sort.Strings(paths)

	// Group by set. Every disk carries the job number and the set timestamp in
	// its header, which is the only thing a disk without the catalog can be
	// grouped by.
	sets := map[string][]string{}
	for _, p := range paths {
		h, err := onestep.ReadHeader(p)
		if err != nil {
			t.Errorf("%s: %s", p, err)
			continue
		}
		key := fmt.Sprintf("%s-job%d", h.Created.Format("20060102-150405"), h.Job)
		sets[key] = append(sets[key], p)
	}

	for key, members := range sets {
		t.Run(key, func(t *testing.T) {
			a, err := onestep.Open(members...)
			if err != nil {
				t.Fatalf("Open: %s", err)
			}
			defer a.Close() //nolint:errcheck // read-only
			for _, d := range a.Diagnostics() {
				t.Logf("diagnostic [%s] %s", d.Kind, d.Message)
			}

			var checked, unreachable, damaged int
			var bytes int64
			for i := range a.Files() {
				f := &a.Files()[i]
				if !f.Recoverable() {
					unreachable++
					continue
				}
				c := a.Check(f)
				checked++
				bytes += c.Bytes
				switch {
				case c.OK():
				case len(c.Errors) > 0:
					// Damage in the medium is what these archives are kept
					// for, so it is logged rather than failed: whether any is
					// present is a property of the operator's own files.
					damaged++
					for _, e := range c.Errors {
						t.Logf("%s: %s", f.Path, e)
					}
				default:
					// Nothing went wrong and the size is still wrong, which is
					// the reader disagreeing with the catalog rather than the
					// medium losing anything.
					t.Errorf("%s: recovered %d of %d bytes with no error reported", f.Path, c.Bytes, f.Size)
				}
			}
			t.Logf("job %d over %d disk(s): %d file(s) checked, %d bytes, %d damaged, %d unreachable",
				a.Job().Number, len(members), checked, bytes, damaged, unreachable)
			if checked == 0 {
				t.Error("no file could be checked")
			}
		})
	}
}
