// Package mactype maps a classic Mac OS file's Finder type and creator codes
// to the filename extension a modern system needs.
//
// A file on an HFS volume carries no extension. Its file type is stored in the
// catalog's four-character type code, and the application that wrote it in the
// creator code, so a file recovered under its own name arrives as "Photo" with
// nothing saying it holds a TIFF.
//
// The table is curated and deliberately small. A code it does not contain
// produces nothing rather than a guess. The evidence is four bytes read from a
// damaged medium, and a wrong extension is worse than none: a file named
// ".doc" that is not a Word document opens in the wrong application and looks
// like the operator's mistake.
//
// The rows are not all equally certain. Those marked attested below were
// checked against files read from media in this archive. Every other row comes
// from the references and no file here confirms it, so it is a plausible
// reading rather than a measurement. This matches the distinction the rest of
// the toolkit draws between what a medium recorded and what a tool inferred.
// A row that cannot be traced to one of the references does not belong here.
//
// A creator code is never a row. Creators appear beside the attested types
// (8BIM, DMGR, WILD) and look like they belong, but a creator says which
// application owned a file, not what the file holds. A row for one would
// describe a file that cannot exist. A test checks that they stay absent.
//
// References:
//
//	Inside Macintosh: Files, "Finder Information"
//	http://fileformats.archiveteam.org/wiki/Type_code
package mactype

import "strings"

// Extension returns the extension a type and creator code imply, leading dot
// included, and whether the table carries the pair.
//
// Trailing spaces and NULs are ignored on both. The catalog stores 'PDF ' and
// hfs.OSType.String() returns "PDF", so a lookup has to accept both.
func Extension(typ, creator string) (string, bool) {
	typ, creator = trim(typ), trim(creator)
	if typ == "" {
		return "", false
	}
	if ext, ok := qualifiedIndex[[2]string{typ, creator}]; ok {
		return ext, true
	}
	ext, ok := typeIndex[typ]
	return ext, ok
}

func trim(code string) string { return strings.TrimRight(code, " \x00") }

// byCreator holds the exceptions: type codes whose meaning depends on the
// creator. They are few and all alike: a type that describes a container
// rather than its contents. TEXT is the important one, since an Illustrator
// drawing is stored as text but is unreadable as text.
var byCreator = map[[2]string]string{
	{"TEXT", "ART5"}: ".ai", // Adobe Illustrator 5
	{"TEXT", "ARTZ"}: ".ai", // Adobe Illustrator 88
}

// byType is keyed on the type alone, which decides every other case. The
// creator is not consulted here: it says which application owned a file, not
// what the file holds. A page and a note saved by one application are both
// TEXT, and the creator cannot tell them apart.
//
// Keys are written as the four bytes the catalog holds, padding included, so
// the source matches the medium; lookups go through index below.
var byType = map[string]string{
	// Images. TIFF and 8BPS are attested: every image file on the Zip disks in
	// this archive is one or the other, written by Photoshop.
	"TIFF": ".tif",
	"8BPS": ".psd",
	"JPEG": ".jpg",
	"GIFf": ".gif",
	"PNGf": ".png",
	// A PICT file starts with 512 bytes of header before the picture, so the
	// extension describes the file, not a format every modern reader accepts.
	"PICT": ".pict",
	"EPSF": ".eps",
	"BMP ": ".bmp",
	"BMPf": ".bmp",

	// Documents.
	"TEXT": ".txt",
	"ttro": ".txt", // TeachText / SimpleText read-only
	"PDF ": ".pdf",
	"RTF ": ".rtf",
	"WDBN": ".doc", // Word 4 and 5
	"W6BN": ".doc", // Word 6
	"XLS ": ".xls",
	"CWWP": ".cwk", // ClarisWorks, one extension for all four document kinds
	"CWDB": ".cwk",
	"CWSS": ".cwk",
	"CWGR": ".cwk",

	// Sound and film.
	"MooV": ".mov",
	"AIFF": ".aif",
	"AIFC": ".aifc",
	"WAVE": ".wav",
	"Midi": ".mid",
	"MPEG": ".mpg",
	"ULAW": ".au",

	// Containers.
	"SIT!": ".sit",
	"SITD": ".sit",
	"ZIP ": ".zip",
	"TARF": ".tar",
	"Gzip": ".gz",
	"BINA": ".bin", // MacBinary

	// Applications and system files are deliberately absent, and a test checks
	// that they stay absent. An extension promises that something can open the
	// file's *data fork*. These keep their content in the resource fork or mean
	// nothing outside the Mac that wrote them. ".app" on a modern Mac is a bundle
	// directory, not a file, so it would describe something the output is not.
	// ".ttf" on a font suitcase would label a file whose data fork is empty.
	// A misleading extension is worse than none:
	//
	//	APPL FFIL ffil LWFN sfnt STAK INIT cdev PREF dfil rsrc
	//	FNDR BTFL DTFL zsys ZSYS DRVR ????
	//
	// FNDR, BTFL and DTFL are attested here: the Finder and desktop database
	// files on every Mac Zip disk in this archive. They are the clearest case:
	// giving the desktop database a ".db" extension would invite something to
	// open it as a database.
}

// The two indexes hold the tables keyed the way lookups arrive: trimmed, since
// hfs.OSType.String() strips the padding a catalog stores. The tables above
// stay padded to match the medium, and a test checks that no two padded keys
// trim to the same key.
var typeIndex = func() map[string]string {
	m := make(map[string]string, len(byType))
	for code, ext := range byType {
		m[trim(code)] = ext
	}
	return m
}()

var qualifiedIndex = func() map[[2]string]string {
	m := make(map[[2]string]string, len(byCreator))
	for pair, ext := range byCreator {
		m[[2]string{trim(pair[0]), trim(pair[1])}] = ext
	}
	return m
}()
