package fat_test

import (
	"bytes"
	"io"
	"testing"

	"samuelks.com/datarescue/fat"
	"samuelks.com/datarescue/internal/testfat"
)

// Every size in a FAT volume is derived from the boot sector: the table's
// length, the cluster count, the entry width the count then selects, the
// directory region's extent. FAT12 unpacking in particular reads the two bytes
// at n*3/2, so an entry count off by one indexes past the buffer. The invariant
// is the same one the other parsers are held to: parse anything, panic on
// nothing, and return no size or cluster the volume cannot hold.

func FuzzOpen(f *testing.F) {
	sound := testfat.Build(testfat.Options{
		Files: []testfat.File{{Name: "A.BIN", Size: 3 * testfat.ClusterSize}},
		Dirs:  []testfat.Dir{{Name: "SUB", Files: []testfat.File{{Name: "B.BIN", Size: 40}}}},
	})
	// Only the front of the image: the fuzzer mutates the boot sector and the
	// tables, and carrying 10 MiB of data region into every iteration buys
	// nothing.
	f.Add(sound.Data[:64<<10])
	f.Add(sound.Data[:2048])
	f.Add(make([]byte, 512))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		fs, err := fat.Open(bytes.NewReader(data), 0)
		if err != nil {
			return
		}
		if fs.ClusterSize <= 0 || fs.SectorSize <= 0 {
			t.Fatalf("opened a volume with %d-byte sectors and %d-byte clusters", fs.SectorSize, fs.ClusterSize)
		}
		if len(fs.Tables) == 0 {
			t.Fatal("opened a volume with no FAT copy")
		}
		entries, _ := fs.ReadDirectory(0, true)
		for _, e := range entries {
			if _, err := fs.ClusterOffset(e.FirstCluster); err != nil {
				continue
			}
			r, err := fs.OpenClusterChain(e.FirstCluster, int64(e.FileSize))
			if err != nil {
				continue
			}
			n, err := io.Copy(io.Discard, io.LimitReader(r, 1<<20))
			if err == nil && n > int64(e.FileSize) {
				t.Fatalf("%q declared %d bytes and delivered %d", e.Name, e.FileSize, n)
			}
		}
		summary, err := fs.AllocationSummary()
		if err == nil && int64(summary.Total) != fs.DataClusters() {
			t.Fatalf("the summary counts %d clusters where the data region holds %d",
				summary.Total, fs.DataClusters())
		}

		// The FAT is a whole number of sectors, so it has entries past the
		// last real cluster. An orphan chain running into them would be
		// reported at a size the volume cannot hold.
		last := fs.DataClusters() + 1
		orphans, err := fs.FindOrphans()
		if err == nil {
			for _, o := range orphans {
				for _, c := range o.Clusters {
					if int64(c) > last {
						t.Fatalf("orphan chain from %d reaches cluster %d, past the last of %d",
							o.StartCluster, c, last)
					}
				}
				if o.Size != int64(len(o.Clusters))*int64(fs.ClusterSize) {
					t.Fatalf("orphan chain from %d holds %d clusters and claims %d bytes",
						o.StartCluster, len(o.Clusters), o.Size)
				}
			}
		}
	})
}
