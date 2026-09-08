package apm

import (
	"bytes"
	"encoding/binary"
	"testing"

	"samuelks.com/datarescue/internal/testapm"
)

// The partition map is the first structure read off an Apple disk, and every
// offset it returns comes straight off the medium. As with every parser here,
// any input must parse without a panic, and no offset returned may fall outside
// the image.

func FuzzPartitions(f *testing.F) {
	sound, _ := testapm.Build(testapm.Options{MapBlocks: 10, Fill: 0xFA},
		testapm.Partition{Type: TypeHFS, Name: "ISIS", Blocks: 40})
	f.Add(sound)

	// A block size no device uses, which must not be taken as a stride.
	for _, bs := range []uint16{0, 3, 1536, 0xFFFF} {
		img := append([]byte(nil), sound...)
		binary.BigEndian.PutUint16(img[2:4], bs)
		f.Add(img)
	}
	// A declared entry count at both extremes.
	for _, n := range []uint32{0, 0xFFFFFFFF} {
		img := append([]byte(nil), sound...)
		binary.BigEndian.PutUint32(img[512+4:], n)
		f.Add(img)
	}
	// An entry claiming the whole address space, and one claiming block 0.
	huge := append([]byte(nil), sound...)
	binary.BigEndian.PutUint32(huge[2*512+8:], 0xFFFFFFFF)
	binary.BigEndian.PutUint32(huge[2*512+12:], 0xFFFFFFFF)
	f.Add(huge)
	zero := append([]byte(nil), sound...)
	binary.BigEndian.PutUint32(zero[2*512+8:], 0)
	f.Add(zero)
	// A driver count larger than the table can hold.
	drv := append([]byte(nil), sound...)
	binary.BigEndian.PutUint16(drv[16:18], 0xFFFF)
	f.Add(drv)

	noDDR, _ := testapm.Build(testapm.Options{NoDDR: true},
		testapm.Partition{Type: TypeHFS, Blocks: 8})
	f.Add(noDDR)
	f.Add(make([]byte, EntrySize))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		size := int64(len(data))
		m, err := ReadMap(bytes.NewReader(data), size)
		if err != nil {
			return
		}
		if !plausibleBlockSize(m.BlockSize) {
			t.Fatalf("block size %d is not one the map is ever written at", m.BlockSize)
		}
		if len(m.Entries) > maxEntries {
			t.Fatalf("returned %d entries; the cap is %d", len(m.Entries), maxEntries)
		}
		for _, e := range m.Entries {
			off, span := e.Offset(), e.Size()
			switch {
			case off <= 0:
				t.Fatalf("entry %d has offset %d; block 0 holds the record", e.Index, off)
			case span < 0:
				t.Fatalf("entry %d has size %d", e.Index, span)
			case off+span < off:
				t.Fatalf("entry %d overflows: %d+%d", e.Index, off, span)
			case off >= size:
				t.Fatalf("entry %d starts at %d, past the %d-byte image", e.Index, off, size)
			case e.Index < 1:
				t.Fatalf("entry with index %d", e.Index)
			case len(e.PartName()) > 32 || len(e.TypeName()) > 32:
				t.Fatalf("entry %d has an unbounded name or type", e.Index)
			}
		}
		// Partitions filters to what is worth looking inside, and nothing it
		// returns may be free space or a zero-length entry.
		parts, err := Partitions(bytes.NewReader(data), size)
		if err != nil {
			return
		}
		for _, p := range parts {
			if p.Unused() {
				t.Fatalf("an unused entry was returned as a partition: %+v", p)
			}
		}
	})
}
