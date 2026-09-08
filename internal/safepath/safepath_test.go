package safepath

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestComponent(t *testing.T) {
	t.Parallel()
	tests := []struct{ in, want string }{
		{"hello.txt", "hello.txt"},
		{"", "_"},
		{".", "_"},
		{"..", "_"},
		{"a/b", "a_b"},
		{`a\b`, "a_b"},
		{"a\x00b", "ab"},
		{"a\nb", "a_b"},
		{"C:", "C_"},
		{"stream.txt:hidden", "stream.txt_hidden"},
		{"trailing.", "trailing"},
		{"trailing   ", "trailing"},
		{"CON", "_CON"},
		{"con.txt", "_con.txt"},
		{"NUL", "_NUL"},
		{"COM9.dat", "_COM9.dat"},
		{"comic.txt", "comic.txt"}, // not reserved
		{strings.Repeat("x", 300), strings.Repeat("x", 255)},
	}
	for _, tc := range tests {
		if got := Component(tc.in); got != tc.want {
			t.Errorf("Component(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
}

func TestPathEscapes(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "out")
	escapes := []string{
		"../../etc/passwd",
		"/etc/passwd",
		`..\..\windows\system32\cmd.exe`,
		"a/../../../b",
		"////etc/passwd",
		"./././../x",
		"..",
		"",
	}
	for _, p := range escapes {
		got, err := Join(root, p)
		if err != nil {
			t.Errorf("Join(%q) = %v", p, err)
			continue
		}
		rel, err := filepath.Rel(root, got)
		if err != nil || rel == ".." || strings.HasPrefix(rel, "..") {
			t.Errorf("Join(root, %q) = %q, which escapes root", p, got)
		}
		t.Logf("%-40q -> %s", p, rel)
	}
}

func TestPathKeepsStructure(t *testing.T) {
	t.Parallel()
	tests := []struct{ in, want string }{
		{"dir/file.txt", filepath.Join("dir", "file.txt")},
		{`C:\DOS\ATTRIB.EXE`, filepath.Join("C_", "DOS", "ATTRIB.EXE")},
		{"a/b/c.txt", filepath.Join("a", "b", "c.txt")},
		{"/leading/slash.txt", filepath.Join("leading", "slash.txt")},
		{"dir//double.txt", filepath.Join("dir", "double.txt")},
	}
	for _, tc := range tests {
		if got := Path(tc.in); got != tc.want {
			t.Errorf("Path(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
}

func TestSlash(t *testing.T) {
	t.Parallel()
	tests := []struct{ in, want string }{
		{"dir/file.txt", "dir/file.txt"},
		{`C:\DOS\ATTRIB.EXE`, "C_/DOS/ATTRIB.EXE"},
		{"a/b/c.txt", "a/b/c.txt"},
		{"/leading/slash.txt", "leading/slash.txt"},
		{"dir//double.txt", "dir/double.txt"},
		{"../../etc/passwd", "etc/passwd"},
		{"/etc/passwd", "etc/passwd"},
		{`..\..\windows\system32\cmd.exe`, "windows/system32/cmd.exe"},
		{"a/../../../b", "a/b"},
		{"..", "_"},
		{"", "_"},
	}
	for _, tc := range tests {
		if got := Slash(tc.in); got != tc.want {
			t.Errorf("Slash(%q) = %q; want %q", tc.in, got, tc.want)
		}
		// An archive member name is never absolute and never climbs out.
		if got := Slash(tc.in); strings.HasPrefix(got, "/") || strings.Contains(got, "..") {
			t.Errorf("Slash(%q) = %q, which is not a safe member name", tc.in, got)
		}
	}
}

// TestPathIsSlashLocalized keeps the two from drifting: a member name and a
// local path must sanitize identically, only their separators differing.
func TestPathIsSlashLocalized(t *testing.T) {
	t.Parallel()
	for _, p := range []string{"dir/file.txt", `C:\DOS\ATTRIB.EXE`, "../x", "", "a//b"} {
		if got, want := Path(p), filepath.FromSlash(Slash(p)); got != want {
			t.Errorf("Path(%q) = %q; want %q", p, got, want)
		}
	}
}

// The length cut comes before the trailing dots and spaces are trimmed.
// Trimming first does not fix the problem: the cut can then land on a dot the
// name already held at byte 255, and the name Windows silently renames is
// written after all.
func TestComponentTrimsAfterTruncating(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
	}{
		{
			// The 255-byte cut lands inside a run of dots, which trimming
			// beforehand never sees.
			name: "dots at the cut",
			in:   strings.Repeat("a", 250) + "....." + strings.Repeat("b", 50) + ".",
		},
		{
			name: "spaces at the cut",
			in:   strings.Repeat("a", 250) + "     " + strings.Repeat("b", 50),
		},
		{
			name: "nothing but dots and spaces",
			in:   strings.Repeat(". ", 300),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Component(tc.in)
			if got == "" {
				t.Fatal("Component returned an empty component")
			}
			if len(got) > 255 {
				t.Errorf("Component returned %d bytes; want at most 255", len(got))
			}
			if strings.HasSuffix(got, ".") || strings.HasSuffix(got, " ") {
				t.Errorf("Component = %q; it still ends in a dot or a space", got)
			}
		})
	}
}

// The prefix that keeps a name from matching a Windows device must not push the
// component past the bound it was just cut to.
func TestComponentReservedNameStaysInBounds(t *testing.T) {
	t.Parallel()
	got := Component("NUL." + strings.Repeat("x", 300))
	if len(got) > 255 {
		t.Errorf("Component returned %d bytes; want at most 255", len(got))
	}
	if !strings.HasPrefix(got, "_NUL") {
		t.Errorf("Component = %q; want the reserved name prefixed", got[:min(len(got), 16)])
	}
}

// The cut has to come out of the name the medium supplied. Otherwise a caller
// that appends its own bytes afterwards would lose them, wholly or, worse,
// partly: a ".tif" left as ".ti" claims a file type nothing recorded.
func TestStemReservesRoomForWhatTheCallerAdds(t *testing.T) {
	long := strings.Repeat("a", 300)
	dir, stem := Stem(long, len(".tif"))
	if dir != "" {
		t.Errorf("Stem dir = %q; want empty for a bare name", dir)
	}
	if got := len(stem + ".tif"); got != maxComponent {
		t.Errorf("len(stem+ext) = %d; want %d", got, maxComponent)
	}
}

func TestStemSplitsTheLeadingDirectories(t *testing.T) {
	dir, stem := Stem("Photos/Trip/Jaws 2", 0)
	if dir != "Photos/Trip/" || stem != "Jaws 2" {
		t.Errorf("Stem = %q, %q; want %q, %q", dir, stem, "Photos/Trip/", "Jaws 2")
	}
	if dir, stem := Stem(`C:\DOS\README`, 0); dir != `C:\DOS\` || stem != "README" {
		t.Errorf("Stem = %q, %q; want %q, %q", dir, stem, `C:\DOS\`, "README")
	}
}

// The sink sanitizes the whole path again on its way out, so a stem that was
// not already a fixed point would be cut a second time and lose the room it
// just reserved.
func TestStemIsStableUnderComponent(t *testing.T) {
	for _, name := range []string{
		strings.Repeat("a", 300), "AUX", "Name .", "Fish 2", "a/b", strings.Repeat("é", 200),
	} {
		_, stem := Stem(name, 4)
		if got := Component(stem); got != stem {
			t.Errorf("Component(Stem(%q, 4)) = %q; want %q unchanged", name, got, stem)
		}
	}
}

// Trailing dots and spaces go before the extension lands, or the result is a
// name Windows silently renames after all.
func TestStemTrimsBeforeTheExtensionIsAdded(t *testing.T) {
	if _, stem := Stem("Name .", 4); stem+".tif" != "Name.tif" {
		t.Errorf("got %q; want %q", stem+".tif", "Name.tif")
	}
}

// The "_" a reserved device name is prefixed with counts against the same
// bound, reservation included.
func TestStemKeepsAReservedNameInBounds(t *testing.T) {
	if _, stem := Stem("AUX", 4); stem != "_AUX" {
		t.Errorf("Stem(AUX, 4) = %q; want _AUX", stem)
	}
	_, stem := Stem(strings.Repeat("AUX", 200), 4)
	if got := len(stem + ".tif"); got > maxComponent {
		t.Errorf("len(stem+ext) = %d; want at most %d", got, maxComponent)
	}
}
