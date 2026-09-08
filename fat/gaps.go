package fat

import "fmt"

// Extent is a contiguous run of a file's bytes and where they live in the
// image. A chain of clusters that follow one another on disk is one extent, so
// cross-referencing a file against a ddrescue map costs a handful of lookups
// rather than one per cluster.
type Extent struct {
	FileOffset  int64
	ImageOffset int64
	Length      int64
}

// ChainExtents returns where in the image the chain starting at cluster keeps
// its data, clipped to size when size is positive. Clusters adjacent on disk
// are coalesced.
func (f *FAT) ChainExtents(cluster uint32, size int64) ([]Extent, error) {
	clusters, err := f.resolveClusterChain(cluster)
	if err != nil {
		return nil, err
	}
	return f.clusterExtents(clusters, size)
}

// clusterExtents maps a cluster list to image ranges.
func (f *FAT) clusterExtents(clusters []uint32, size int64) ([]Extent, error) {
	cs := int64(f.ClusterSize)
	if cs <= 0 {
		return nil, fmt.Errorf("fat: cluster size %d is not usable", f.ClusterSize)
	}
	if size > 0 {
		if need := (size + cs - 1) / cs; need < int64(len(clusters)) {
			clusters = clusters[:need]
		}
	}
	var out []Extent
	var fileOff int64
	for _, c := range clusters {
		off, err := f.ClusterOffset(c)
		if err != nil {
			return out, err
		}
		if n := len(out); n > 0 && out[n-1].ImageOffset+out[n-1].Length == off {
			out[n-1].Length += cs
		} else {
			out = append(out, Extent{FileOffset: fileOff, ImageOffset: off, Length: cs})
		}
		fileOff += cs
	}
	// The last cluster is only partly the file's when the size does not fill it.
	if size > 0 && fileOff > size {
		last := &out[len(out)-1]
		last.Length -= fileOff - size
		if last.Length <= 0 {
			out = out[:len(out)-1]
		}
	}
	return out, nil
}

// clusterBase is the file offset the current cluster starts at.
func (r *Reader) clusterBase() int64 {
	return int64(r.currentIndex) * int64(r.f.ClusterSize)
}
