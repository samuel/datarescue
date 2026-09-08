package humanize

import "testing"

func TestBytes(t *testing.T) {
	tests := []struct {
		in   int64
		want string
	}{
		{0, "0 bytes"},
		{512, "512 bytes"},
		{1024, "1.00 KiB (1024 bytes)"},
		{1536, "1.50 KiB (1536 bytes)"},
		{1 << 20, "1.00 MiB (1048576 bytes)"},
		{1 << 30, "1.00 GiB (1073741824 bytes)"},
	}
	for _, tc := range tests {
		if got := Bytes(tc.in); got != tc.want {
			t.Errorf("Bytes(%d) = %q; want %q", tc.in, got, tc.want)
		}
	}
}

func TestMB(t *testing.T) {
	if got := MB(2_000_000); got != 2 {
		t.Errorf("MB(2000000) = %v; want 2", got)
	}
}
