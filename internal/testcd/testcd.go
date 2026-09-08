// Package testcd frames a payload as the raw 2352-byte sectors a CD carries it
// in, with a correct sync pattern, address and EDC.
//
// It is a normal package because the cuebin and cuetool tests need the same
// disc. It computes the EDC bit by bit rather than sharing cuebin's table: a
// fixture that borrowed the implementation under test would agree with it even
// if both were wrong.
package testcd

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	// RawSectorSize is a sector as it sits on the medium.
	RawSectorSize = 2352
	// DataSectorSize is the user data inside a MODE1 sector.
	DataSectorSize = 2048
	// edcCoverage is how many leading bytes of a MODE1 sector its EDC covers.
	edcCoverage = 2064
)

var syncPattern = [12]byte{0x00, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0x00}

// EDC computes the CRC-32/CD (polynomial 0xD8018001, reflected) over b, a bit
// at a time.
func EDC(b []byte) uint32 {
	var crc uint32
	for _, x := range b {
		crc ^= uint32(x)
		for range 8 {
			if crc&1 != 0 {
				crc = crc>>1 ^ 0xD8018001
			} else {
				crc >>= 1
			}
		}
	}
	return crc
}

// MODE1 frames payload as MODE1/2352 sectors starting at startLBA, padding the
// last sector with zeros. The ECC area is left zero: nothing here checks it,
// and the EDC is what a verifier looks at.
func MODE1(payload []byte, startLBA int64) []byte {
	sectors := (len(payload) + DataSectorSize - 1) / DataSectorSize
	out := make([]byte, sectors*RawSectorSize)
	for i := range sectors {
		sec := out[i*RawSectorSize : (i+1)*RawSectorSize]
		copy(sec[:12], syncPattern[:])
		m, s, f := lbaToMSF(startLBA + int64(i))
		sec[12], sec[13], sec[14] = bcd(m), bcd(s), bcd(f)
		sec[15] = 0x01 // MODE1
		copy(sec[16:16+DataSectorSize], payload[i*DataSectorSize:])
		binary.LittleEndian.PutUint32(sec[2064:2068], EDC(sec[:edcCoverage]))
	}
	return out
}

// Audio frames payload as audio sectors, which carry no structure at all: the
// bytes are the samples, padded to a whole sector.
func Audio(payload []byte) []byte {
	sectors := (len(payload) + RawSectorSize - 1) / RawSectorSize
	out := make([]byte, sectors*RawSectorSize)
	copy(out, payload)
	return out
}

// lbaToMSF converts a logical block address to its absolute disc address, which
// starts two seconds in.
func lbaToMSF(lba int64) (m, s, f int) {
	a := lba + 150
	return int(a / 4500), int(a / 75 % 60), int(a % 75)
}

func bcd(n int) byte {
	return byte((n/10)<<4 | n%10) //nolint:gosec // the values are minutes, seconds and frames
}

// Cue is a single-track cue sheet for binName in the given mode.
func Cue(binName, mode string) string {
	return fmt.Sprintf("FILE %q BINARY\n  TRACK 01 %s\n    INDEX 01 00:00:00\n", binName, mode)
}

// Write puts bin and its cue sheet in dir under stem, and returns the cue path.
func Write(dir, stem, mode string, bin []byte) (string, error) {
	binName := stem + ".bin"
	if err := os.WriteFile(filepath.Join(dir, binName), bin, 0o600); err != nil {
		return "", err
	}
	cuePath := filepath.Join(dir, stem+".cue")
	if err := os.WriteFile(cuePath, []byte(Cue(binName, mode)), 0o600); err != nil {
		return "", err
	}
	return cuePath, nil
}

// MapText renders a ddrescue map over a file of size bytes marking the given
// sectors unrecovered, for a caller that has already damaged them.
func MapText(size int64, sectorSize int64, lbas ...int64) string {
	var b strings.Builder
	b.WriteString("# Mapfile. Created by testcd.\n")
	b.WriteString("0x00000000     +               1\n")
	b.WriteString("#      Start        Size  Status\n")
	var pos int64
	for _, lba := range lbas {
		start, end := lba*sectorSize, (lba+1)*sectorSize
		if start > pos {
			fmt.Fprintf(&b, "0x%08x  0x%08x  +\n", pos, start-pos)
		}
		fmt.Fprintf(&b, "0x%08x  0x%08x  -\n", start, end-start)
		pos = end
	}
	if pos < size {
		fmt.Fprintf(&b, "0x%08x  0x%08x  +\n", pos, size-pos)
	}
	return b.String()
}
