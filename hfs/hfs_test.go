package hfs

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"samuelks.com/datarescue/internal/testfixture"
)

// testImagePath is the HFS fixture every test in this package reads.
const testImagePath = "testdata/disk.img.gz"

func TestHFS(t *testing.T) {
	t.Parallel()
	v, err := openFixture(t)
	if err != nil {
		t.Fatal(err)
	}
	mdb := v.MDB

	t.Logf("Created %s", MacTime(mdb.CreatedTimestamp).Time(time.UTC))
	t.Logf("Modified %s", MacTime(mdb.ModifiedTimestamp).Time(time.UTC))
	t.Logf("Volume label %s", mdb.Label())
	if label := mdb.Label(); label != "disk" {
		t.Errorf("Volume label = %q; want %q", label, "disk")
	}

	files, issues, err := v.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, ci := range issues {
		t.Errorf("catalog issue: %s", ci)
	}

	var foundCaper bool
	for _, f := range files {
		t.Logf("%s (CNID %d, Size %d)", strings.Join(f.Path, "/"), f.CNID, f.DataSize)
		if strings.Join(f.Path, "/") == "TwinPeaksTrivia" {
			foundCaper = true
			if f.DataSize != 327680 {
				t.Errorf("TwinPeaksTrivia size = %d; want 327680", f.DataSize)
			}
			wantExtents := ExtentsRecord{{StartBlock: 3447, BlockCount: 640}}
			if f.DataExtents != wantExtents {
				t.Errorf("TwinPeaksTrivia extents = %+#v; want %+#v", f.DataExtents, wantExtents)
			}
			if f.CNID != 27 {
				t.Errorf("TwinPeaksTrivia CNID = %d; want 27", f.CNID)
			}
		}
	}

	if !foundCaper {
		t.Error("TwinPeaksTrivia not found in file list")
	}

	expectedCount := 1
	if len(files) < expectedCount {
		t.Errorf("got %d files, want at least %d", len(files), expectedCount)
	}
}

// openFixture opens the committed HFS volume. It is a bare volume: no partition
// map, so it starts at byte 0 and runs the length of the image.
func openFixture(t *testing.T) (*Volume, error) {
	t.Helper()
	return openBytes(testfixture.Bytes(t, testImagePath))
}

func openBytes(data []byte) (*Volume, error) {
	return Open(bytes.NewReader(data), 0, int64(len(data)))
}
