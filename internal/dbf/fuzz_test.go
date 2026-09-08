package dbf

import (
	"bytes"
	"testing"
)

// The header, the field descriptors and the record count all come off damaged
// media, and every one of them is used to size or index something. The
// invariant is the same as in the other parsers here: decode anything, panic
// on nothing.

func FuzzOpen(f *testing.F) {
	f.Add(sample().bytes())

	wide := table{
		fields: []field{
			{name: "SERIAL", typ: 'N', length: 12},
			{name: "DESCR", typ: 'C', length: 0, decimals: 1},
		},
		rows: [][]byte{[]byte(" " + "           1" + "text")},
	}
	f.Add(wide.bytes())

	// The shapes that have to be rejected rather than trusted.
	lying := sample()
	lying.count = 0xFFFFFFFF
	f.Add(lying.bytes())
	noTerm := sample()
	noTerm.terminator = 0xFF
	f.Add(noTerm.bytes())
	f.Add([]byte{Version3})
	f.Add(bytes.Repeat([]byte{Version3}, 64))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		tab, err := Open(bytes.NewReader(data), 0, int64(len(data)))
		if err != nil {
			return
		}
		if tab.RecordSize < 1 {
			t.Fatalf("record size %d", tab.RecordSize)
		}
		if tab.Len() < 0 || tab.Len() > tab.DeclaredLen() {
			t.Fatalf("Len %d, DeclaredLen %d", tab.Len(), tab.DeclaredLen())
		}
		// Every record the table admits to holding must be addressable, and
		// every field of it must lie inside the record.
		for i := range tab.Len() {
			r, err := tab.Record(i)
			if err != nil {
				continue
			}
			for _, fl := range tab.Fields {
				if _, err := r.Raw(fl.Name); err != nil {
					t.Fatalf("field %s of record %d is not inside the record: %s", fl.Name, i, err)
				}
				if _, err := r.Int(fl.Name); err != nil && fl.Type == TypeNumeric {
					// A numeric field holding something other than a number is
					// a corrupt value, not a parser failure.
					continue
				}
			}
		}
	})
}
