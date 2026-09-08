package mactype

import (
	"strings"
	"testing"
)

func TestExtensionResolvesAKnownType(t *testing.T) {
	for _, tc := range []struct {
		typ, creator, want string
	}{
		{"TIFF", "8BIM", ".tif"},
		{"8BPS", "8BIM", ".psd"},
		{"TEXT", "ttxt", ".txt"},
		{"MooV", "TVOD", ".mov"},
	} {
		ext, ok := Extension(tc.typ, tc.creator)
		if !ok || ext != tc.want {
			t.Errorf("Extension(%q, %q) = %q, %v; want %q, true", tc.typ, tc.creator, ext, ok, tc.want)
		}
	}
}

// The catalog stores 'PDF ' padded; OSType.String() returns "PDF". A lookup
// has to work with both forms.
func TestExtensionIgnoresThePadding(t *testing.T) {
	for _, code := range []string{"PDF", "PDF ", "PDF\x00", "ZIP ", "XLS"} {
		if _, ok := Extension(code, ""); !ok {
			t.Errorf("Extension(%q, %q) not found", code, "")
		}
	}
}

// An unknown code produces nothing rather than a guess, because an extension
// is a claim about what a file holds.
func TestExtensionRefusesWhatItDoesNotKnow(t *testing.T) {
	for _, code := range []string{"", "    ", "Xyz!", "????", "APPL", "STAK"} {
		if ext, ok := Extension(code, "8BIM"); ok {
			t.Errorf("Extension(%q, _) = %q, true; want no answer", code, ext)
		}
	}
}

func TestCreatorQualifiedRowsWinAndDoNotLeak(t *testing.T) {
	if ext, _ := Extension("TEXT", "ART5"); ext != ".ai" {
		t.Errorf("Extension(TEXT, ART5) = %q; want .ai", ext)
	}
	// The same type with any other creator falls through to the plain row: the
	// creator table holds exceptions, not a second lookup for every file.
	if ext, _ := Extension("TEXT", "MSWD"); ext != ".txt" {
		t.Errorf("Extension(TEXT, MSWD) = %q; want .txt", ext)
	}
}

// A key written "PDF" where "PDF " was meant cannot be seen at the call site,
// since the lookup trims either way. This test catches it.
func TestEveryKeyIsFourBytes(t *testing.T) {
	for code := range byType {
		if len(code) != 4 {
			t.Errorf("byType key %q is %d bytes; a type code is four", code, len(code))
		}
	}
	for pair := range byCreator {
		for _, code := range pair {
			if len(code) != 4 {
				t.Errorf("byCreator key %q is %d bytes; a type code is four", code, len(code))
			}
		}
	}
}

// Two keys that trim alike collapse to one row in the index, silently dropping
// whichever the map iterated first.
func TestNoTwoKeysTrimAlike(t *testing.T) {
	if len(typeIndex) != len(byType) {
		t.Errorf("index holds %d rows for %d keys; two of them trim alike", len(typeIndex), len(byType))
	}
	if len(qualifiedIndex) != len(byCreator) {
		t.Errorf("qualified index holds %d rows for %d keys; two of them trim alike",
			len(qualifiedIndex), len(byCreator))
	}
}

func TestEveryExtensionIsBareAndLowercase(t *testing.T) {
	check := func(ext string) {
		switch {
		case !strings.HasPrefix(ext, "."):
			t.Errorf("extension %q has no leading dot; every caller would have to add one", ext)
		case ext != strings.ToLower(ext):
			t.Errorf("extension %q is not lowercase; a renderer would have to normalize it", ext)
		case len(ext) > 5:
			t.Errorf("extension %q is longer than any classic one", ext)
		}
	}
	for _, ext := range byType {
		check(ext)
	}
	for _, ext := range byCreator {
		check(ext)
	}
}

// A creator says which application owned a file, not what the file holds, so a
// row for one describes a file that cannot exist. These all appear as the
// creator code of real files in this archive or in the references, which is
// why they look like they belong.
func TestCreatorCodesAreNotRows(t *testing.T) {
	for _, code := range []string{"8BIM", "WILD", "DMGR", "XCEL", "MACS", "ttxt", "MSWD", "CARO", "dCpy"} {
		if ext, ok := Extension(code, ""); ok {
			t.Errorf("%q has a row (%q), but it is a creator code and no file is of that type", code, ext)
		}
	}
}

// An extension promises that something can open the file's data fork. These
// keep their content in the resource fork or mean nothing outside the Mac that
// wrote them, so giving them an extension is a false claim rather than a
// missing answer.
func TestTypesWithNoHonestExtensionHaveNone(t *testing.T) {
	for _, code := range []string{
		"APPL", "FFIL", "ffil", "LWFN", "sfnt", "STAK", "INIT", "cdev",
		"PREF", "dfil", "rsrc", "FNDR", "BTFL", "DTFL", "zsys", "ZSYS", "DRVR", "????",
	} {
		if ext, ok := Extension(code, ""); ok {
			t.Errorf("%q was given %q; its content is not in the data fork, so the extension is a false claim", code, ext)
		}
	}
}
