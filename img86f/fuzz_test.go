package img86f

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// The track offset table comes straight off the medium, and every length the
// decoder derives is computed from it: the distance to the next track, the
// bitcell count, the nominal length the disk flags imply. The invariant: parse
// any input without panicking and without reading outside the image.

func FuzzOpen(f *testing.F) {
	// A header with one track whose data starts right after the offset table.
	hdr := func(flags uint16, offsets map[int]uint32, tail int) []byte {
		sides := 1
		if flags&diskFlagDoubleSided != 0 {
			sides = 2
		}
		n := maxTracksPerSide * sides
		b := make([]byte, fileHeaderSize+n*4+tail)
		copy(b, Magic)
		binary.LittleEndian.PutUint16(b[6:], flags)
		for i, off := range offsets {
			binary.LittleEndian.PutUint32(b[fileHeaderSize+i*4:], off)
		}
		return b
	}
	start := uint32(fileHeaderSize + maxTracksPerSide*4)
	f.Add(hdr(0, map[int]uint32{0: start}, 4096))
	f.Add(hdr(diskFlagDoubleSided, map[int]uint32{0: start * 2, 1: start*2 + 1024}, 8192))
	// A track claiming to start inside the offset table, and one past the end.
	f.Add(hdr(0, map[int]uint32{0: 4}, 512))
	f.Add(hdr(0, map[int]uint32{0: 0xFFFFFFFF}, 512))
	// The bitcell count is the one length not taken from the table.
	f.Add(hdr(diskFlagBitcellCount, map[int]uint32{0: start}, 64))
	f.Add(make([]byte, fileHeaderSize))
	f.Add([]byte{})

	// A track carrying real MFM sector data, so the fuzzer reaches the decoder
	// rather than only the offset table: the IDAM's size code is the exponent
	// of the sector length, and the mutations that matter are one byte deep.
	withTrack := func(sectors ...[]byte) []byte {
		var track []byte
		track = append(track, 0, 0, 0, 0, 0, 0) // flags(2) + index hole(4)
		for _, s := range sectors {
			track = append(track, s...)
		}
		b := hdr(0, map[int]uint32{0: start}, len(track))
		copy(b[start:], track)
		return b
	}
	f.Add(withTrack(mfmSector(0, 0, 1, 2, make([]byte, 512), false)))
	f.Add(withTrack(mfmSector(0, 0, 1, 2, make([]byte, 512), true)))
	f.Add(withTrack(mfmSector(0, 0, 1, maxSizeCode, make([]byte, 128<<maxSizeCode), false)))
	f.Add(withTrack(
		mfmSector(0, 0, 1, 0, make([]byte, 128), false),
		mfmSector(0, 0, 2, 2, make([]byte, 512), false)))

	f.Fuzz(func(t *testing.T, data []byte) {
		d, err := OpenReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return
		}
		tracks, sides := d.Geometry()
		if tracks < 0 || tracks > maxTracksPerSide || sides < 1 || sides > 2 {
			t.Fatalf("Geometry() = (%d, %d)", tracks, sides)
		}
		// Only the first few tracks: every track decodes the same way, and
		// decoding a whole disk per input only costs time.
		for track := range min(tracks, 3) {
			for side := range sides {
				sectors, err := d.ReadTrack(track, side)
				if err != nil {
					continue
				}
				for _, sec := range sectors {
					if want := 128 << sec.SizeCode; len(sec.Data) > want {
						t.Fatalf("sector %+v holds %d bytes, more than its size code allows (%d)",
							sec.SectorNum, len(sec.Data), want)
					}
					if len(sec.Data) > len(data) {
						t.Fatalf("a sector of %d bytes came out of a %d-byte image", len(sec.Data), len(data))
					}
				}
			}
		}
		// The geometry the boot sector declares need not match the sectors the
		// tracks actually hold, which is what the linear reader has to survive.
		spt, heads, bps, err := DetectGeometry(d)
		if err != nil {
			return
		}
		lr, err := NewLinearReader(d, spt, heads, bps)
		if err != nil {
			return
		}
		if _, err := lr.Seek(0, 0); err != nil {
			return
		}
		_, _ = lr.Read(make([]byte, min(bps, 1<<16)))
	})
}
