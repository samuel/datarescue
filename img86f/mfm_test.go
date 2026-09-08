package img86f

import (
	"errors"
	"testing"
)

// encodeMFMByte is decodeMFMByte's inverse: each data bit sits in the low bit
// of its two-bitcell pair. The clock bits between them are left zero, which the
// decoder ignores — it reads only the data positions.
func encodeMFMByte(b byte) (hi, lo byte) {
	for i := range 4 {
		if b&(1<<(7-i)) != 0 {
			hi |= 1 << (6 - 2*i)
		}
		if b&(1<<(3-i)) != 0 {
			lo |= 1 << (6 - 2*i)
		}
	}
	return hi, lo
}

// mfmAddressMark renders one A1 A1 A1 sync sequence followed by the given
// decoded bytes: a mark, its fields, and its two CRC bytes.
//
// The sync bytes are mfmSync verbatim rather than encoded, because a real A1
// sync mark carries a deliberate clock violation — that is what makes it
// findable in a bitstream — and the decoder locates a mark by that pattern.
func mfmAddressMark(fields ...byte) []byte {
	out := append([]byte(nil), mfmSync...)
	for _, b := range fields {
		hi, lo := encodeMFMByte(b)
		out = append(out, hi, lo)
	}
	return out
}

// mfmSector renders an IDAM and its DAM: a whole readable sector, or one with a
// deliberately wrong data CRC when badCRC is set.
func mfmSector(cyl, head, sec, sizeCode byte, data []byte, badCRC bool) []byte {
	idam := []byte{markIDAM, cyl, head, sec, sizeCode}
	crc := crc16(append([]byte{0xA1, 0xA1, 0xA1}, idam...))
	out := mfmAddressMark(append(idam, byte(crc>>8), byte(crc))...)

	body := append([]byte{markDAM}, data...)
	dcrc := crc16(append([]byte{0xA1, 0xA1, 0xA1}, body...))
	if badCRC {
		dcrc ^= 0xFFFF
	}
	return append(out, mfmAddressMark(append(body, byte(dcrc>>8), byte(dcrc))...)...)
}

// The size code is the exponent of the sector length, and the format defines
// 0-7. A code of 56 makes 128<<56 overflow int to a negative length, the bounds
// check in decodeMFMSlice wrap, and make panic — and the IDAM's own CRC is
// checked after the shift, so it cannot be what catches it.
func TestDecodeMFMRejectsAnImpossibleSizeCode(t *testing.T) {
	t.Parallel()
	for _, code := range []byte{8, 25, 32, 56, 255} {
		stream := mfmSector(0, 0, 1, code, make([]byte, 512), false)
		if got := decodeMFMTrack(stream); len(got) != 0 {
			t.Errorf("size code %d yielded %d sector(s); a length that impossible is not a sector", code, len(got))
		}
	}
	// Every code the format does define still decodes.
	for code := byte(0); code <= maxSizeCode; code++ {
		stream := mfmSector(0, 0, 1, code, make([]byte, 128<<code), false)
		got := decodeMFMTrack(stream)
		if len(got) != 1 {
			t.Fatalf("size code %d yielded %d sector(s); want 1", code, len(got))
		}
		if len(got[0].Data) != 128<<code {
			t.Errorf("size code %d gave %d bytes of data; want %d", code, len(got[0].Data), 128<<code)
		}
	}
}

// The decoder must not carry a rejected IDAM's size into the data mark that
// follows it, or the DAM decodes at the wrong length.
func TestDecodeMFMDataMarkWithoutAnIDAM(t *testing.T) {
	t.Parallel()
	body := append([]byte{markDAM}, make([]byte, 512)...)
	crc := crc16(append([]byte{0xA1, 0xA1, 0xA1}, body...))
	stream := mfmAddressMark(append(body, byte(crc>>8), byte(crc))...)
	if got := decodeMFMTrack(stream); len(got) != 0 {
		t.Errorf("a data mark with no preceding IDAM yielded %d sector(s)", len(got))
	}
}

// A sector whose own IDAM declares a length the geometry does not is a read
// error for that sector. Assuming they agree would copy past the end of the
// decoded data.
func TestLinearReaderSectorSizeMismatch(t *testing.T) {
	t.Parallel()
	// One 128-byte sector where the reader is told sectors are 512.
	sectors := decodeMFMTrack(mfmSector(0, 0, 1, 0, make([]byte, 128), false))
	if len(sectors) != 1 {
		t.Fatalf("built %d sectors; want 1", len(sectors))
	}
	r := &linearReader{
		sectorsPerTrack: 18,
		heads:           2,
		sectorSize:      512,
		size:            512,
		cache:           map[trackKey][]Sector{{0, 0}: sectors},
	}
	n, err := r.Read(make([]byte, 512))
	if err == nil {
		t.Fatalf("read of a short sector succeeded with %d bytes; want an error for that sector", n)
	}
	if n != 0 {
		t.Errorf("read returned %d bytes from a sector it could not use", n)
	}
}

// ErrCRC is the sentinel a caller matches to tell a sector that was found but
// failed its CRC from one that did not read at all, so it has to survive the
// reader's error wrapping.
func TestErrCRCReachesTheCaller(t *testing.T) {
	t.Parallel()
	sectors := decodeMFMTrack(mfmSector(0, 0, 1, 2, make([]byte, 512), true))
	if len(sectors) != 1 {
		t.Fatalf("built %d sectors; want 1", len(sectors))
	}
	if !sectors[0].CRCError {
		t.Fatal("the sector with a deliberately wrong data CRC decoded as sound")
	}
	r := &linearReader{
		sectorsPerTrack: 18,
		heads:           2,
		sectorSize:      512,
		size:            512,
		cache:           map[trackKey][]Sector{{0, 0}: sectors},
	}
	buf := make([]byte, 512)
	n, err := r.Read(buf)
	if !errors.Is(err, ErrCRC) {
		t.Fatalf("Read = %v; want an error matching ErrCRC", err)
	}
	if n != 512 {
		t.Errorf("read %d bytes; the sector's data is delivered alongside the CRC failure", n)
	}
}
