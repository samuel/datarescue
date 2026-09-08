package onestep

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"samuelks.com/datarescue/ddrescue"
	"samuelks.com/datarescue/internal/dbf"
)

// Source identifies one disk image of a backup set and, optionally, the ddrescue
// map recording which of its sectors were recovered.
type Source struct {
	Path string
	// Map is the .map sidecar. When empty, the one beside Path is used if there
	// is one.
	Map string
	// NoMap reads the disk as it is, even with a sidecar beside it.
	NoMap bool
	// Reader supplies the image's bytes directly, for a caller that already
	// holds them. Size must then be set too, Path is only a label for
	// messages, and closing the reader stays the caller's business.
	Reader io.ReaderAt
	Size   int64
}

// Disk describes one disk of the set as it was found.
type Disk struct {
	Path    string
	MapPath string
	Header  Header
	Size    int64
	// DataLength is how many bytes of file data the image holds: up to the
	// catalog on the disk carrying it, up to the end of the file otherwise.
	DataLength int64
	// ExpectedDataLength is how many bytes of data the catalog's chunks put on
	// this disk, or 0 when none were placed here. A DataLength short of it
	// means the image is truncated; a longer one means it has slack after its
	// data.
	ExpectedDataLength int64
	// FirstChunk and LastChunk are the serials of the first and last chunk
	// placed on this disk, or 0 when none were.
	FirstChunk int
	LastChunk  int
}

// diskFile is one open disk of the set.
type diskFile struct {
	source     Source
	mapPath    string
	header     Header
	size       int64
	dataLength int64
	r          io.ReaderAt
	closer     io.Closer

	// expected, firstChunk and lastChunk are filled in once the chunks have
	// been placed; see Disk for what they mean.
	expected   int64
	firstChunk int
	lastChunk  int
}

// Archive is an opened backup set.
type Archive struct {
	diags   diagnostics
	disks   []*diskFile
	members []Member
	// catDisk is the disk the catalog was read from; its members live there.
	catDisk *diskFile
	cat     *catalog
}

// Open opens a backup set. The paths may be given in any order and are sorted
// by the disk number in each header; a <path>.map sidecar is picked up when it
// is present. The disk carrying the catalog must be among them, since without
// it there is no record of what the data is.
func Open(paths ...string) (*Archive, error) {
	srcs := make([]Source, 0, len(paths))
	for _, p := range paths {
		srcs = append(srcs, Source{Path: p})
	}
	return OpenSources(srcs)
}

// OpenSources is Open with explicit ddrescue map files.
func OpenSources(srcs []Source) (*Archive, error) {
	if len(srcs) == 0 {
		return nil, errors.New("onestep: no backup files given")
	}
	a := &Archive{}
	for _, src := range srcs {
		d, err := a.openDisk(src)
		if err != nil {
			return nil, a.failed(err)
		}
		a.disks = append(a.disks, d)
	}
	sort.SliceStable(a.disks, func(i, j int) bool {
		return a.disks[i].header.Disk < a.disks[j].header.Disk
	})
	a.checkSet()

	catDisk := -1
	for i, d := range a.disks {
		if d.header.CatalogOffset == 0 {
			continue
		}
		if catDisk >= 0 {
			a.diags.add(DiagDiskMismatch, "%s and %s both carry a catalog; using %s",
				a.disks[catDisk].source.Path, d.source.Path, a.disks[catDisk].source.Path)
			continue
		}
		catDisk = i
	}
	if catDisk < 0 {
		return nil, a.failed(errors.New("onestep: none of the given files carries a catalog; the last disk of the set is needed"))
	}

	a.catDisk = a.disks[catDisk]
	if err := a.readCatalogFrom(a.catDisk); err != nil {
		return nil, a.failed(err)
	}
	a.cat.buildPaths(a.diags.add)
	a.placeChunks()
	a.attachChunks()
	if a.cat.job.HasPassword {
		a.diags.add(DiagPasswordProtected, "the job records a password; stored data may not be readable")
	}
	return a, nil
}

// failed closes the set and returns err with everything noticed on the way
// appended, since a caller that never receives an Archive cannot ask it for
// diagnostics.
func (a *Archive) failed(err error) error {
	a.Close() //nolint:errcheck // best-effort cleanup on the error path
	diags := a.diags.drain()
	if len(diags) == 0 {
		return err
	}
	msgs := make([]string, 0, len(diags))
	for _, d := range diags {
		msgs = append(msgs, d.Message)
	}
	return fmt.Errorf("%w\n  %s", err, strings.Join(msgs, "\n  "))
}

func (a *Archive) openDisk(src Source) (*diskFile, error) {
	d := &diskFile{source: src, mapPath: src.Map}
	switch {
	case src.Reader != nil:
		if src.Map != "" {
			return nil, errors.New("onestep: a supplied reader cannot also take a ddrescue map")
		}
		d.r, d.size = src.Reader, src.Size
	default:
		ds, err := ddrescue.OpenSource(src.Path, ddrescue.Options{Map: src.Map, NoMap: src.NoMap})
		if err != nil {
			return nil, fmt.Errorf("onestep: %w", err)
		}
		d.mapPath, d.size = ds.MapPath, ds.Size
		d.r, d.closer = ds.Checked(), ds
	}

	hdr := make([]byte, HeaderSize)
	if _, err := d.r.ReadAt(hdr, 0); err != nil {
		d.close() //nolint:errcheck // best-effort cleanup on the error path
		return nil, fmt.Errorf("onestep: %s: read header: %w", src.Path, err)
	}
	h, err := parseHeader(hdr)
	if err != nil {
		d.close() //nolint:errcheck // best-effort cleanup on the error path
		return nil, fmt.Errorf("%s: %w", src.Path, err)
	}
	d.header = h
	d.dataLength = d.size - HeaderSize
	switch {
	case d.header.CatalogOffset == 0:
		// A disk before the last carries no catalog, so all of it is data.
	case d.header.CatalogOffset < HeaderSize || d.header.CatalogOffset >= d.size:
		// The header points past the end, so the image is truncated or its
		// offset is corrupt. Treat the catalog as lost rather than refusing:
		// another disk of the set may carry a readable one.
		a.diags.add(DiagDirectoryUnreadable,
			"%s: the catalog should start at %d but the file is %d bytes; treating it as absent",
			src.Path, d.header.CatalogOffset, d.size)
		d.header.CatalogOffset = 0
	default:
		d.dataLength = d.header.CatalogOffset - HeaderSize
	}
	return d, nil
}

// close releases a disk this package opened. A reader the caller supplied is
// left alone.
func (d *diskFile) close() error {
	if d.closer == nil {
		return nil
	}
	return d.closer.Close()
}

// checkSet reports disks that do not belong together and numbers that are
// missing. It reports rather than refuses, so listing a set with a lost disk
// still works.
func (a *Archive) checkSet() {
	first := a.disks[0]
	for _, d := range a.disks[1:] {
		if !d.header.Created.Equal(first.header.Created) || d.header.Job != first.header.Job {
			a.diags.add(DiagDiskMismatch, "%s is job %d of %s, not job %d of %s",
				d.source.Path, d.header.Job, d.header.Created, first.header.Job, first.header.Created)
		}
	}
	for i, d := range a.disks {
		if i > 0 && d.header.Disk == a.disks[i-1].header.Disk {
			a.diags.add(DiagDiskMismatch, "%s and %s are both disk %d",
				a.disks[i-1].source.Path, d.source.Path, d.header.Disk)
		}
		if i == 0 && d.header.Disk != 1 {
			a.diags.add(DiagDiskMissing, "the set starts at disk %d, so disk 1 is missing", d.header.Disk)
		}
	}
	for i := 1; i < len(a.disks); i++ {
		for n := a.disks[i-1].header.Disk + 1; n < a.disks[i].header.Disk; n++ {
			a.diags.add(DiagDiskMissing, "disk %d of the set is missing", n)
		}
	}
}

// readCatalogFrom locates the catalog tables on d and decodes them. The member
// directory lists each table, so nothing has to be searched for; when the
// directory itself cannot be read, the tables are scanned for instead.
func (a *Archive) readCatalogFrom(d *diskFile) error {
	tables := make(map[tableID]*dbf.Table, numTables)
	members, err := readContainer(d.r, d.header.CatalogOffset, d.size)
	switch {
	case err != nil:
		a.diags.add(DiagDirectoryUnreadable, "catalog directory at %d unreadable, scanning for tables instead: %s",
			d.header.CatalogOffset, err)
		if err := a.scanTables(d, tables); err != nil {
			return err
		}
	default:
		a.members = members
		for _, m := range members {
			id, ok := memberTableID(m.Name)
			if !ok {
				continue
			}
			t, err := dbf.Open(d.r, m.Offset, m.Size)
			if err != nil {
				a.diags.add(DiagTableDamaged, "catalog member %s is not readable as a table: %s", m.Name, err)
				continue
			}
			if want := tableSpecs[id].fields; len(t.Fields) != len(want) || !t.HasFields(want...) {
				a.diags.add(DiagTableDamaged, "catalog member %s does not hold the expected %s fields", m.Name, id)
				continue
			}
			tables[id] = t
		}
		if len(tables) < int(numTables) {
			a.diags.add(DiagDirectoryScan, "%d of %d catalog tables came from the directory, scanning for the rest",
				len(tables), int(numTables))
			if err := a.scanTables(d, tables); err != nil {
				return err
			}
		}
	}

	cat, err := readCatalog(tables, a.diags.add)
	if err != nil {
		return err
	}
	a.cat = cat
	return nil
}

// maxSkippedWindows bounds the windows an unreadable catalog region is reported
// for. A catalog that was never recovered would report the same thing once per
// window; a tally after them gives the total.
const maxSkippedWindows = 10

// scanTables searches the catalog region for dBase tables, for a backup whose
// member directory is damaged. Each table is identified by its own field
// names, so a missing one does not shift the others, and only tables not
// already found are taken.
func (a *Archive) scanTables(d *diskFile, tables map[tableID]*dbf.Table) error {
	const (
		window = 1 << 16
		// maxParses bounds the work on an image whose catalog offset is itself
		// corrupt, where the sweep would otherwise run to the end of a disk.
		maxParses = 1 << 14
	)
	start := max(d.header.CatalogOffset, HeaderSize)
	buf := make([]byte, window)
	var parses, found, skipped int
	for off := start; off < d.size && len(tables) < int(numTables); {
		n, err := d.r.ReadAt(buf, off)
		if n == 0 {
			if err == nil {
				break
			}
			if off == start {
				return fmt.Errorf("onestep: scanning for catalog tables at %d: %w", off, err)
			}
			// A bad region inside the catalog is what this sweep exists for, so
			// it is recorded and stepped over rather than ending the search:
			// the tables past it are still there to be found.
			if skipped < maxSkippedWindows {
				a.diags.add(DiagDirectoryScan, "catalog region unreadable at %d, skipping ahead: %s", off, err)
			}
			skipped++
			off += window
			continue
		}
		// Sweep the window for a table. The cheap in-buffer test comes first:
		// a full parse costs several reads and almost no position is a table.
		advance := int64(n)
		var stepped bool
		for i := 0; i+dbfHeaderSize <= n; i++ {
			if !dbf.PlausibleHeader(buf[i:]) {
				continue
			}
			if parses >= maxParses {
				a.diags.add(DiagDirectoryScan, "gave up scanning for catalog tables after %d candidates", parses)
				return a.scanResult(tables, found, skipped)
			}
			parses++
			at := off + int64(i)
			// The candidate's declared count is clamped only against the
			// region it is opened with. With the whole rest of the image as
			// that region, a false-positive header's dead count could inflate
			// its Size() and jump the cursor past every real table after it,
			// so the region stops at the next table already claimed. Anything
			// between that table and this candidate can only be damage.
			region := d.size - at
			for _, claimed := range tables {
				if next := claimed.Start() - at; next > 0 && next < region {
					region = next
				}
			}
			t, err := dbf.Open(d.r, at, region)
			if err != nil {
				continue
			}
			// Step over the whole table either way, so its own records cannot
			// look like headers and a table already found is not re-examined.
			advance = at + t.Size() - off
			stepped = true
			if id, ok := identifyTable(t); ok {
				if _, taken := tables[id]; !taken {
					tables[id] = t
					found++
					a.diags.add(DiagDirectoryScan, "found %s by scanning, at %d", id, at)
				}
			}
			// Never go backwards over the position just examined.
			advance = max(advance, int64(i)+1)
			break
		}
		// Keep enough overlap that a header straddling the window end is still
		// seen whole on the next pass. A table stepped over is exempt: it was
		// examined whole, and pulling the position back into it would sweep its
		// records again on every pass.
		if !stepped && advance >= int64(n) && int64(n) == window {
			advance = window - dbfHeaderSize + 1
		}
		off += max(advance, 1)
	}
	return a.scanResult(tables, found, skipped)
}

func (a *Archive) scanResult(tables map[tableID]*dbf.Table, found, skipped int) error {
	if skipped > maxSkippedWindows {
		a.diags.add(DiagDirectoryScan, "and %d more unreadable window(s) in the catalog region", skipped-maxSkippedWindows)
	}
	if found == 0 && len(tables) == 0 {
		return errors.New("onestep: no catalog tables found")
	}
	return nil
}

// placeChunks works out which disk of the set holds each chunk's bytes.
//
// A chunk's recorded offset is a position in its own disk's file, not in the
// backup as a whole: the offsets restart at HeaderSize on every disk, and the
// last chunk on a disk ends exactly where its data does. The session table
// maps chunk serials to disks directly. Where it does not cover a chunk, the
// offset restart marks the disk boundary instead, since within one disk the
// offsets only advance.
//
// So the image sizes do not decide placement; they serve as a check: a disk
// shorter than the chunks the catalog puts on it is truncated, and a longer
// one has slack after its data.
func (a *Archive) placeChunks() {
	order := make([]*Chunk, len(a.cat.chunks))
	for i := range a.cat.chunks {
		order[i] = &a.cat.chunks[i]
	}
	sort.Slice(order, func(i, j int) bool { return order[i].Serial < order[j].Serial })

	scan := scanDisks(order)
	sessionOf := a.sessionDisks()
	var lastPair [2]int
	for i, c := range order {
		c.Disk = scan[i]
		d, ok := sessionOf[c.Serial]
		if !ok {
			continue
		}
		c.Disk = d
		// One disagreement covers every chunk it shifts, so report the pair
		// rather than each chunk carrying it.
		if pair := [2]int{scan[i], d}; d != scan[i] && pair != lastPair {
			lastPair = pair
			a.diags.add(DiagSessionMismatch,
				"the offset chain puts chunk %d on disk %d, the session table on disk %d; using disk %d",
				c.Serial, scan[i], d, d)
		}
	}
	a.checkDisks()
}

// scanDisks numbers the chunks by disk from the offset chain alone, for a
// backup whose session table is missing or does not cover every chunk. Within
// a disk the offsets only advance, so one that goes backwards starts the next
// disk. The chunks must be in serial order.
func scanDisks(order []*Chunk) []int {
	out := make([]int, len(order))
	var disk int
	var end int64
	for i, c := range order {
		// An empty chunk consumes nothing, so the comparison is against where
		// the previous chunk ended rather than where it began.
		if disk == 0 || c.Offset < end {
			disk++
		}
		out[i] = disk
		end = c.Offset + c.CompSize
	}
	return out
}

// sessionDisks maps a chunk serial to the disk the session table puts it on.
// Ranges that overlap or run backwards are reported and dropped, since a
// damaged row that swallowed the whole set would otherwise place every chunk
// on one disk.
func (a *Archive) sessionDisks() map[int]int {
	// A range comes off the medium and is untrusted, so it is clipped to the
	// serials the chunk table actually holds: a row damaged into
	// 1..999999999999 would otherwise be expanded one entry at a time, and
	// every entry past the last chunk maps to nothing anyway.
	var lastSerial int
	for i := range a.cat.chunks {
		lastSerial = max(lastSerial, a.cat.chunks[i].Serial)
	}
	out := make(map[int]int)
	for _, s := range a.cat.sessions {
		if s.Disk <= 0 || s.FirstChunk <= 0 || s.LastChunk < s.FirstChunk {
			a.diags.add(DiagSessionMismatch, "the session table puts chunks %d-%d on disk %d, which is not a range",
				s.FirstChunk, s.LastChunk, s.Disk)
			continue
		}
		if s.LastChunk > lastSerial {
			a.diags.add(DiagSessionMismatch, "the session table puts chunks %d-%d on disk %d, past chunk %d, the last the catalog holds",
				s.FirstChunk, s.LastChunk, s.Disk, lastSerial)
		}
		for n := s.FirstChunk; n <= min(s.LastChunk, lastSerial); n++ {
			if other, taken := out[n]; taken {
				a.diags.add(DiagSessionMismatch, "the session table puts chunk %d on both disk %d and disk %d; using disk %d",
					n, other, s.Disk, other)
				continue
			}
			out[n] = s.Disk
		}
	}
	return out
}

// checkDisks resolves each chunk to a supplied image and measures the images
// against what the catalog puts on them.
func (a *Archive) checkDisks() {
	index := make(map[int]int, len(a.disks))
	for i, d := range a.disks {
		index[d.header.Disk] = i
	}

	byDisk := make(map[int][]*Chunk)
	numbers := make([]int, 0, len(a.disks))
	for i := range a.cat.chunks {
		c := &a.cat.chunks[i]
		c.diskIdx = -1
		if _, seen := byDisk[c.Disk]; !seen {
			numbers = append(numbers, c.Disk)
		}
		byDisk[c.Disk] = append(byDisk[c.Disk], c)
	}
	sort.Ints(numbers)

	for _, n := range numbers {
		chunks := byDisk[n]
		sort.Slice(chunks, func(i, j int) bool { return chunks[i].Serial < chunks[j].Serial })

		// The chunks of one disk tile it from the end of its header, so a
		// chunk that does not start where the previous one ended is a break.
		want := int64(HeaderSize)
		for _, c := range chunks {
			if c.Offset != want {
				a.diags.add(DiagOffsetChainBroken, "disk %d: chunk %d starts at %d, %d expected", n, c.Serial, c.Offset, want)
			}
			want = max(want, c.Offset+c.CompSize)
		}

		idx, ok := index[n]
		if !ok {
			continue
		}
		d := a.disks[idx]
		d.expected = want - HeaderSize
		d.firstChunk, d.lastChunk = chunks[0].Serial, chunks[len(chunks)-1].Serial
		a.measureDisk(d, n)
		a.resolveChunks(chunks, idx, d)
	}
}

// measureDisk compares an image against the data the catalog puts on it.
func (a *Archive) measureDisk(d *diskFile, number int) {
	switch {
	case d.dataLength < d.expected:
		a.diags.add(DiagDiskTruncated,
			"%s: the catalog puts %d bytes of data on disk %d but the image holds %d, so %d bytes are missing from its end",
			d.source.Path, d.expected, number, d.dataLength, d.expected-d.dataLength)
	case d.dataLength > d.expected:
		a.diags.add(DiagSessionMismatch,
			"%s: the catalog puts %d bytes of data on disk %d but the image holds %d, so it has slack after its data",
			d.source.Path, d.expected, number, d.dataLength)
	}
}

// resolveChunks points each chunk at the image holding it. A chunk the image
// does not reach is left unplaced rather than read from whatever happens to
// lie at its offset.
func (a *Archive) resolveChunks(chunks []*Chunk, idx int, d *diskFile) {
	dataEnd := HeaderSize + d.dataLength
	for _, c := range chunks {
		switch {
		case c.Offset < HeaderSize:
			a.diags.add(DiagChunkUnplaced, "chunk %d starts at %d, inside the header of %s",
				c.Serial, c.Offset, d.source.Path)
		case c.Offset >= dataEnd:
			// Past the end of what the image holds, which measureDisk has
			// already reported for the disk as a whole.
		default:
			c.diskIdx = idx
		}
	}
}

// attachChunks groups the chunk table by file and checks each group against
// the file it describes. Grouping is by the chunk's own file serial rather than
// by position in the table, so a damaged row costs one file instead of
// shifting every file after it.
func (a *Archive) attachChunks() {
	byFile := make(map[int][]Chunk, len(a.cat.files))
	for i := range a.cat.chunks {
		c := &a.cat.chunks[i]
		byFile[c.File] = append(byFile[c.File], *c)
	}

	known := make(map[int]bool, len(a.cat.files))
	for i := range a.cat.files {
		f := &a.cat.files[i]
		known[f.Serial] = true
		chunks := byFile[f.Serial]
		sort.Slice(chunks, func(i, j int) bool { return chunks[i].Sequence < chunks[j].Sequence })
		f.Chunks = chunks

		switch {
		case len(chunks) == 0:
			a.diags.add(DiagChunkCountMismatch, "%s: no stored data recorded for it", f.Path)
			continue
		case !chunks[len(chunks)-1].Last:
			a.diags.add(DiagChunkCountMismatch, "%s: the last of its %d chunks is not marked final", f.Path, len(chunks))
		}
		var total int64
		var unplaced int
		for _, c := range chunks {
			total += c.OrigSize
			if c.diskIdx < 0 && !c.Empty() {
				unplaced++
			}
			if c.Level != LevelStored && c.Level != LevelDeflate && c.Level != LevelEmpty {
				a.diags.add(DiagUnknownCompression, "%s: chunk %d uses unknown compression level %d", f.Path, c.Serial, c.Level)
			}
		}
		if total != f.Size {
			a.diags.add(DiagChunkCountMismatch, "%s: chunks hold %d bytes, the catalog says %d", f.Path, total, f.Size)
		}
		if unplaced > 0 {
			a.diags.add(DiagChunkUnplaced, "%s: %d of %d chunks are not on the disks supplied", f.Path, unplaced, len(chunks))
		}
	}
	for serial := range byFile {
		if !known[serial] {
			a.diags.add(DiagChunkCountMismatch, "chunks reference file %d, which the catalog does not list", serial)
		}
	}
}

// Job returns the backup job record.
func (a *Archive) Job() Job { return a.cat.job }

// Files returns every file the catalog lists, in catalog order.
func (a *Archive) Files() []File { return a.cat.files }

// Drives returns the source drives the backup read from.
func (a *Archive) Drives() []DiskInfo { return a.cat.disks }

// Sessions returns the mapping of chunks to media disks.
func (a *Archive) Sessions() []Session { return a.cat.sessions }

// Selections returns the paths the operator chose when starting the backup.
func (a *Archive) Selections() []Selection { return a.cat.selections }

// Members returns the catalog container's members, which include the FoxPro
// index files the restore does not need.
func (a *Archive) Members() []Member { return a.members }

// OpenMember streams a catalog member's bytes, so the tables can be written
// out and read with other tools.
func (a *Archive) OpenMember(m Member) io.Reader {
	return io.NewSectionReader(a.catDisk.r, m.Offset, m.Size)
}

// Disks returns the disks of the set as they were found.
func (a *Archive) Disks() []Disk {
	out := make([]Disk, 0, len(a.disks))
	for _, d := range a.disks {
		out = append(out, Disk{
			Path:               d.source.Path,
			MapPath:            d.mapPath,
			Header:             d.header,
			Size:               d.size,
			DataLength:         d.dataLength,
			ExpectedDataLength: d.expected,
			FirstChunk:         d.firstChunk,
			LastChunk:          d.lastChunk,
		})
	}
	return out
}

// Find returns the file at path, matched without regard to case and accepting
// either separator, since a path typed on a shell will not use backslashes.
func (a *Archive) Find(path string) (*File, bool) {
	want := normalizePath(path)
	for i := range a.cat.files {
		if normalizePath(a.cat.files[i].Path) == want {
			return &a.cat.files[i], true
		}
	}
	return nil, false
}

func normalizePath(p string) string {
	return strings.ToUpper(strings.Trim(strings.ReplaceAll(p, "/", dosSeparator), dosSeparator))
}

// Diagnostics returns the problems noticed so far and clears the list, so a
// caller can attribute them to the operation that produced them.
func (a *Archive) Diagnostics() []Diagnostic { return a.diags.drain() }

// Close releases every disk of the set.
func (a *Archive) Close() error {
	var err error
	for _, d := range a.disks {
		if cerr := d.close(); cerr != nil && err == nil {
			err = cerr
		}
	}
	return err
}
