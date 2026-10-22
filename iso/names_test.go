package iso

import (
	"io"
	"io/fs"
	"testing"

	"samuelks.com/datarescue/internal/testfixture"
)

// testdata/hdiutil-hybrid.iso.gz is the output of
//
//	hdiutil makehybrid -iso -joliet -o hybrid.iso src
//
// where src holds "Long File Name.txt" (mode 0755) and "Sub Folder/Another
// Long Name.dat". hdiutil writes SP, PX and TF in both trees but no NM, so
// Rock Ridge has attributes and no names, and the names are Joliet's.
func TestHdiutilHybridUsesJolietNames(t *testing.T) {
	t.Parallel()
	r, err := OpenReader(testfixture.Reader(t, "testdata/hdiutil-hybrid.iso.gz"))
	if err != nil {
		t.Fatal(err)
	}
	if !r.UsesJoliet() || r.Names() != "Joliet" || !r.HasRockRidge() {
		t.Errorf("UsesJoliet %v, Names %q, HasRockRidge %v; want Joliet names with Rock Ridge attributes",
			r.UsesJoliet(), r.Names(), r.HasRockRidge())
	}
	got := walkAll(t, r)
	for _, p := range []string{"/Long File Name.txt", "/Sub Folder/", "/Sub Folder/Another Long Name.dat"} {
		if _, ok := got[p]; !ok {
			t.Errorf("no %s; got %v", p, keys(got))
		}
	}
	f := got["/Long File Name.txt"]
	if !f.HasPOSIX || f.Mode != 0o755 {
		t.Errorf("Long File Name.txt: HasPOSIX %v mode %v; want the 0755 PX records", f.HasPOSIX, f.Mode)
	}
	if f.ModTime.IsZero() {
		t.Error("Long File Name.txt has no modification time")
	}
	if d := got["/Sub Folder/"]; !d.Mode.IsDir() || d.Mode.Perm() != 0o755 {
		t.Errorf("Sub Folder mode = %v, want drwxr-xr-x", d.Mode)
	}
	if data, err := io.ReadAll(r.OpenFile(f)); err != nil || string(data) != "hello\n" {
		t.Errorf("read %q, %v", data, err)
	}
	if _, err := fs.ReadFile(r.FS(), "Sub Folder/Another Long Name.dat"); err != nil {
		t.Errorf("FS ReadFile: %v", err)
	}
	if len(r.Issues()) != 0 {
		t.Errorf("issues = %q, want none", r.Issues())
	}
}

// hybridISO builds a primary tree whose root has the SP entry and a Joliet
// tree with plain records. withNM decides whether the primary tree's file
// record carries a Rock Ridge name.
func hybridISO(t *testing.T, withNM bool) *isoBuilder {
	t.Helper()
	b := newISOBuilder()
	svdLBA := b.alloc()
	b.buildSetTerminator(b.alloc())
	rootLBA, jRoot := b.alloc(), b.alloc()

	root := b.sector(rootLBA)
	off := rrRoot(root, rootLBA)
	sua := [][]byte{pxEntry(0o100644, 0, 0)}
	if withNM {
		sua = append(sua, nmEntry("rr name.txt"))
	}
	rrRecord(root, off, 0, false, "LONGNAME.TXT;1", nil, sua...)

	jr := b.sector(jRoot)
	off = dotEntries(jr, jRoot, jRoot)
	putDirRecord(jr, off, 0, 0, false, encodeUCS2("Joliet Name.txt;1"))
	b.buildJolietSVD(svdLBA, uint32(jRoot), 2048, "J")
	b.buildPVD(uint32(rootLBA), 2048, "HYBRID")
	return b
}

// Rock Ridge with no names would give 8.3 names, so Joliet is read instead.
// This Joliet tree has no Rock Ridge of its own, so the attributes are the
// defaults.
func TestRockRidgeWithoutNamesYieldsToJoliet(t *testing.T) {
	t.Parallel()
	r := openBuilt(t, hybridISO(t, false))
	got := walkAll(t, r)
	e, ok := got["/Joliet Name.txt"]
	if !ok {
		t.Fatalf("no /Joliet Name.txt; got %v", keys(got))
	}
	if e.HasPOSIX || r.HasRockRidge() || r.Names() != "Joliet" {
		t.Errorf("HasPOSIX %v, HasRockRidge %v, Names %q; want Joliet with no Rock Ridge",
			e.HasPOSIX, r.HasRockRidge(), r.Names())
	}
}

// Rock Ridge that does supply names still wins over Joliet: it carries
// attributes and symlinks that Joliet cannot.
func TestRockRidgeWithNamesWinsOverJoliet(t *testing.T) {
	t.Parallel()
	r := openBuilt(t, hybridISO(t, true))
	got := walkAll(t, r)
	e, ok := got["/rr name.txt"]
	if !ok {
		t.Fatalf("no /rr name.txt; got %v", keys(got))
	}
	if !e.HasPOSIX || r.UsesJoliet() || r.Names() != "Rock Ridge" {
		t.Errorf("HasPOSIX %v, UsesJoliet %v, Names %q; want Rock Ridge", e.HasPOSIX, r.UsesJoliet(), r.Names())
	}
}
