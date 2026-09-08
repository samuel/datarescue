package extfs

import (
	"encoding/binary"
	"testing"
)

// Every panic these parsers can reach is reachable from bytes alone, so the
// invariant each target asserts is the same: parse anything, panic on nothing.

func FuzzParseSuperblock(f *testing.F) {
	// A minimally valid superblock, plus shapes that break a careless parser.
	good := make([]byte, 1024)
	binary.LittleEndian.PutUint16(good[56:], SuperblockMagic)
	binary.LittleEndian.PutUint32(good[32:], 8192) // BlocksPerGroup
	binary.LittleEndian.PutUint32(good[40:], 1024) // InodesPerGroup
	binary.LittleEndian.PutUint32(good[4:], 4096)  // BlocksCount
	binary.LittleEndian.PutUint32(good[20:], 1)    // FirstDataBlock
	f.Add(good)
	f.Add(make([]byte, 1024))
	f.Add([]byte{})

	hugeBlocks := append([]byte(nil), good...)
	binary.LittleEndian.PutUint32(hugeBlocks[24:], 0xFFFFFFFF) // LogBlockSize
	f.Add(hugeBlocks)

	f.Fuzz(func(t *testing.T, data []byte) {
		sb, err := parseSuperblock(data)
		if err != nil {
			return
		}
		// A superblock that parses must be usable without overflowing anything
		// derived from it.
		if sb.LogBlockSize > 6 {
			t.Fatalf("accepted LogBlockSize %d", sb.LogBlockSize)
		}
		if sb.BlocksPerGroup == 0 || sb.InodesPerGroup == 0 {
			t.Fatal("accepted a zero group size")
		}
	})
}

func FuzzParseInode(f *testing.F) {
	good := make([]byte, 256)
	binary.LittleEndian.PutUint16(good[0:], ModeIFREG|0o644)
	binary.LittleEndian.PutUint32(good[4:], 1024)
	f.Add(good, true)
	f.Add(make([]byte, 128), false)
	f.Add([]byte{}, true)

	dirWithACL := make([]byte, 256)
	binary.LittleEndian.PutUint16(dirWithACL[0:], ModeIFDIR|0o755)
	binary.LittleEndian.PutUint32(dirWithACL[108:], 0xFFFFFFFF)
	f.Add(dirWithACL, true)

	f.Fuzz(func(t *testing.T, data []byte, largeFile bool) {
		ino, err := parseInode(data, largeFile)
		if err != nil {
			return
		}
		if ino.Size < 0 {
			t.Fatalf("negative size %d", ino.Size)
		}
		// i_size_high is i_dir_acl on a directory, so it must never widen one.
		if ino.IsDir() && ino.Size > 1<<32 {
			t.Fatalf("directory size %d came from i_dir_acl", ino.Size)
		}
	})
}

func FuzzParseDirData(f *testing.F) {
	good := make([]byte, 1024)
	binary.LittleEndian.PutUint32(good[0:], 11)
	binary.LittleEndian.PutUint16(good[4:], 1024)
	good[6] = 5
	copy(good[8:], "hello")
	f.Add(good, 1024, true, false)
	f.Add(make([]byte, 64), 64, false, true)
	f.Add([]byte{}, 1024, true, true)

	// A corrupt name_len that would splice bytes out of the entries after it.
	overrun := make([]byte, 256)
	binary.LittleEndian.PutUint32(overrun[0:], 11)
	binary.LittleEndian.PutUint16(overrun[4:], 12)
	overrun[6] = 200
	f.Add(overrun, 256, false, true)

	f.Fuzz(func(t *testing.T, data []byte, blkSize int, hasFiletype, includeDeleted bool) {
		if blkSize <= 0 || blkSize > 1<<16 {
			t.Skip()
		}
		entries, _ := parseDirData(data, blkSize, hasFiletype, includeDeleted)
		for _, e := range entries {
			// A name must have come from inside the buffer it was parsed from.
			if len(e.Name) > len(data) {
				t.Fatalf("name of %d bytes from a %d-byte buffer", len(e.Name), len(data))
			}
		}
	})
}

func FuzzParseGroupDescriptor(f *testing.F) {
	f.Add(make([]byte, 32))
	f.Add(make([]byte, 17))
	f.Add([]byte{})
	f.Fuzz(func(_ *testing.T, data []byte) {
		_, _ = parseGroupDescriptor(data)
	})
}
