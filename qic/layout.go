package qic

import "errors"

// layout is where the two regions of a volume start and how much of the file
// they cover: where the data begins and where the catalog begins. The format
// states this three different ways, which have to be reconciled.
type layout struct {
	headerBytes  int64 // volume table records, plus an MDID when present
	dataStart    int64
	catalogStart int64
	segments     int // segments in the whole grid
	dataSegments int
	catalogSegs  int
	compressed   bool
	variant      Variant
}

// resolveLayout places the regions. For a single volume, and that is every
// archive Windows 95 wrote, the catalog is found by backing whole segments off
// the end of the file: the directory needs ceil(dirSz/payload) of them, where
// the payload is what one segment holds once a compressed archive's segment
// header is deducted.
//
// The reference implementation divides dirSz by the whole segment size even
// for a compressed archive, which asks for one segment too few whenever dirSz
// lands near a multiple of it, and its documentation then backs off the file
// by a multiple of the payload instead of the grid, which does not land on a
// segment boundary at all. The segment count has to use the payload size, and
// the backing off has to use the grid.
//
// Windows 98 and ME with several volumes state the regions outright, in QFA
// block numbers counted from 3.
func resolveLayout(v Volume, volumes int, variant Variant, size int64, diag diagFunc) (layout, error) {
	lay := layout{
		headerBytes: int64(volumes) * VTBLSize,
		compressed:  v.Compressed,
		variant:     variant,
	}
	if variant == VariantWin98 {
		lay.headerBytes += VTBLSize
	}
	if size <= lay.headerBytes {
		return lay, errors.New("qic: the file holds a header region and nothing else")
	}

	grid := size - lay.headerBytes
	lay.segments = int(grid / SegmentSize)
	if lay.segments == 0 {
		return lay, errors.New("qic: the file holds no complete segment past its header region")
	}
	if grid%SegmentSize != 0 {
		diag(DiagVTBLInconsistent,
			"the file is %d bytes past its header region, %d short of a whole %d-byte segment, so its last segment is truncated",
			grid, SegmentSize-grid%SegmentSize, SegmentSize)
	}
	if want := int(v.Segments[1]) - int(v.Segments[0]) + 1; want != lay.segments {
		diag(DiagVTBLInconsistent,
			"the volume table names QFA blocks %d to %d, which is %d segments, but the file holds %d",
			v.Segments[0], v.Segments[1], want, lay.segments)
	}

	lay.dataStart = lay.headerBytes
	payload := int64(SegmentSize)
	if v.Compressed {
		payload = segPayload
	}

	switch {
	case volumes > 1 && variant == VariantWin98:
		// QFA block numbers count from 3, so block 3 is the first segment past
		// the header region.
		lay.catalogStart = lay.headerBytes + (int64(v.QFAEnd)-3)*SegmentSize
		lay.dataStart = lay.headerBytes + (int64(v.QFAStart)-3)*SegmentSize
	default:
		segs := (v.DirSize + payload - 1) / payload
		lay.catalogStart = size - segs*SegmentSize
	}

	if lay.catalogStart <= lay.dataStart || lay.catalogStart >= size ||
		(lay.catalogStart-lay.headerBytes)%SegmentSize != 0 {
		diag(DiagVTBLInconsistent,
			"a directory of %d bytes puts the catalog at 0x%x, which is not a segment boundary inside the data, so it is taken as the last segment instead",
			v.DirSize, lay.catalogStart)
		lay.catalogStart = lay.headerBytes + int64(lay.segments-1)*SegmentSize
	}
	if lay.dataStart < lay.headerBytes || lay.dataStart >= lay.catalogStart ||
		(lay.dataStart-lay.headerBytes)%SegmentSize != 0 {
		diag(DiagVTBLInconsistent,
			"the volume table puts the data at 0x%x, which is not a segment boundary before the catalog, so it is taken to start at 0x%x",
			lay.dataStart, lay.headerBytes)
		lay.dataStart = lay.headerBytes
	}

	lay.dataSegments = int((lay.catalogStart - lay.dataStart) / SegmentSize)
	lay.catalogSegs = int((size - lay.catalogStart) / SegmentSize)
	return lay, nil
}
