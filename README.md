# datarescue

A Go toolkit for extracting files from damaged disk images and optical media. It
combines the block-level recovery map produced by
[GNU ddrescue](https://www.gnu.org/software/ddrescue/) with filesystem parsers
for FAT12/16/32, ext2/ext3, ISO 9660 and Apple HFS to read files off partially
recovered media — and, where the medium is still in the drive, rips it first.

## How it works

1. **ddrescue** recovers as many sectors as possible from a failing disk into an image file plus a `.map` sidecar that records which byte ranges succeeded or failed.
2. **datarescue** reads the image through the map, so any access to an unrecovered sector fails immediately rather than silently returning zeros. Every block a read touches is checked, not just the one holding its first byte: a read running from a recovered range into an unrecovered one stops at the boundary and returns only the bytes that were actually recovered.
3. The partition table is parsed to locate the filesystem — an MBR's primary partitions, the logical partitions inside an extended one, an Apple partition map, or an image with no partition table at all — and the filesystem is then navigated to list, verify and extract files.
4. The format is **detected**, not declared: `fstool` probes the located volume for each filesystem it knows and reads whichever answers. An operator holding a directory of rescued images does not know which is FAT and which is HFS, and that is a question the tool can answer for itself.

Each FAT copy is read independently and sector by sector, so a bad sector costs
only the entries it holds and an unreadable copy does not make the volume
unopenable. Entries backed by an unreadable sector decode as zero, which is
indistinguishable from "free", so a cluster chain reaching one is reported as
unknown rather than silently truncated.

When the copies disagree, the tool reports the discrepancy — as data the caller
renders, not as a log line — and falls back to whichever copy holds a
structurally valid chain. A genuine disagreement — both
copies valid but different — is an error by default; `fstool -fat-policy`
overrides it with `copy0`, `copy1`, or `best-effort`, which takes the allocated
side per cluster and prefers copy 0 for the rest.

Traversal collects failures instead of stopping at them: a directory that cannot
be read, or that loops back on itself, is reported and the rest of the tree is
still walked, and a read that fails partway through a file returns the bytes
recovered before it. The same rule holds across the other parsers — an ext2
directory resynchronizes at the next block boundary, an unreadable ext2 inode
bitmap is treated as all-allocated instead of skipping the group, an HFS catalog
whose leaf chain breaks is swept node by node for the records past the break,
and a CD sector that will not read is marked and the verification continues.

## Installing

The tools need Go 1.26 or later:

```bash
go install samuelks.com/datarescue/cmd/fstool@latest
go install samuelks.com/datarescue/cmd/cuetool@latest
go install samuelks.com/datarescue/cmd/disctool@latest
go install samuelks.com/datarescue/cmd/1steptool@latest
go install samuelks.com/datarescue/cmd/qictool@latest
```

Every tool prints the version it was built from with `-version`. From a
checkout, `make tools` builds all five into `bin/`.

## Status and limitations

This is an early release. The tools are usable and the exit codes, map files
and output layout are what later releases will build on, but until 1.0 any of
them may change, and the Go packages are not yet a stable library API.

How well each format has been exercised differs:

- **FAT, ext2/ext3, ISO 9660 and HFS** are tested against committed images,
  real and generated, including deliberately damaged ones.
- **1-Step and MS Backup** were worked out from real archives that cannot be
  committed because of what they hold. The committed tests use synthetic
  archives built to the same layout; `DATARESCUE_1STEP_DIR` and
  `DATARESCUE_QIC_DIR` point the tests at real ones.
- **Optical drives** are tested in a virtual machine with an emulated drive and
  against a fake device for what the emulator cannot produce (see
  [Testing](#testing)).

What is not supported:

- Writing to any filesystem. Everything is read-only.
- ext4 extent trees, 64-bit block numbers and inline data. A volume using them
  is refused rather than misread.
- HFS+. An HFS wrapper around an HFS+ volume is refused with the offset of the
  volume inside it.
- NTFS, exFAT and GPT partition tables. A GPT disk's protective MBR yields no
  partitions; `-offset` can still point `fstool` at a FAT or ext2 volume on it.
- Compressed images. Decompress a `.gz`, `.xz`, `.bz2`, `.zst`, `.7z` or `.zip`
  first; the tools recognize one and say so.
- Reading a drive on Windows. `disctool` works on Linux and macOS only; the
  other tools run anywhere Go does.
- An ISO exported by `cuetool export` from a data track that does not start at
  disc LBA 0, or ripped by `disctool rip -format iso` from a session that does
  not, keeps the disc's absolute LBAs in its directory records while the file's
  own offsets start at 0, so those records point at the wrong place in it.

## Packages

| Package | Description |
|---------|-------------|
| `ddrescue` | Wraps a disk image + `.map` file; propagates read errors for unrecovered blocks |
| `mbr` | Parses the Master Boot Record partition table |
| `apm` | Parses the Apple partition map an Apple-formatted disk carries instead |
| `fat` | FAT12/FAT16/FAT32 filesystem parser; supports VFAT long filenames |
| `extfs` | ext2/ext3 filesystem parser; read-only, recovery-focused with superblock fallback and partial-read tolerance |
| `hfs` | Apple HFS filesystem parser: both forks, Finder type and creator, and the spare master directory block |
| `img86f` | 86Box `.86f` floppy image format reader; decodes MFM tracks (FM, M2FM and GCR are rejected) |
| `cuebin` | CUE/BIN disc image parsing, extraction, and structural verification |
| `iso` | ISO 9660 reader (Joliet and Rock Ridge) and image writer |
| `cdrom` | CD/DVD device access on Linux and macOS: table of contents, cooked and raw sector reads |
| `diskimage` | Locates the filesystem inside an image, from a path or an `io.ReaderAt`: explicit offset, named partition, auto-detection, or 86F geometry |
| `onestep` | Iomega 1-Step Backup archive reader: multi-disk sets, raw deflate chunks, per-chunk checksum verification where the writer recorded one |
| `qic` | Windows 9x MS Backup (`*.qic`) archive reader: QIC-122 decompression, per-segment random access, catalog-less recovery by scanning |

## Extracting files

Every filesystem tool takes the same shape: a subcommand, then the image.

```bash
fstool ls disk.img                       # list the whole tree
fstool ls -l mac.img                     # HFS: what each file is, from its type code
fstool cat disk.img README.TXT           # one file to stdout
fstool extract -o ./out disk.img         # the whole volume
fstool extract -o ./out disk.img A.TXT   # named files only
```

Names are taken from the damaged medium, so every one is sanitized before it is
written: path separators, `..`, absolute prefixes, NUL and control characters
and Windows reserved device names cannot place a file outside the output
directory. Existing files are never overwritten without `-f`.

Extraction is best-effort by design. A file whose data is only partly readable
is written with the bytes that were recovered and reported, rather than skipped;
`fstool extract -contiguous` additionally reads a file whose cluster chain is
broken as a straight run of clusters from its first, which recovers files on a
volume whose FAT is damaged but whose data is intact.

### Carrying the damage forward

Every tool that reads an image reads it through the image's ddrescue map:
`fstool`, `cuetool`, `1steptool` and `qictool`. `-map` gives the map's path;
without it, a map beside the image is picked up automatically, and `-no-map`
reads the bytes as they are. For `cuetool` the map belongs to
the `.bin`, not the `.cue`, which is a text file nothing was rescued from; for
`1steptool`, where every argument is a disk, `-map` takes a single image and a
set finds each disk's map beside it. Both common map file names are tried,
preferring the one that includes the image's full name — for `input.img`, first
`input.img.map` and then `input.map` — so either way the map is found without
`-map`.

For the tools that read a filesystem or a disc — `fstool` and `cuetool` —
`info` then says how much of the image was never recovered. `fstool verify`
goes further and lists the files the lost ranges affect without reading them,
by comparing where each file is stored with what the map says came back; `fstool ls` marks the
same entries. The two archive tools have no volume
geometry to measure a map against, so for them the damage shows when something
reads the bytes: `verify` and `extract` report which entries a bad range affected.
`ls -l` marks an entry with `!` when the archive's *own* structure says it is
unreachable — a 1-Step file on a disk that was not supplied, a QIC file over a
segment the index could not place — which is a different question from what a
rescue map says, and neither `ls` nor `qictool segments` consults one.

Extraction goes one step further. A file over an unrecovered range is written at
its full length, with zeros standing in for the bytes that were never read and a
`<name>.map` beside it saying which of its byte ranges those are (inside an
archive, as a member of its own). Nothing is ever zero-filled without that map:
the data and the map are written by one call, so a file cannot come back at full
size with invisible holes — the failure this toolkit exists to prevent. Where the
map cannot be written the file is taken back out of the output directory, since a
missing file is the better half of that trade; inside an archive a member cannot
be removed, so the report lists it and the run fails.

Every map these tools write is a real ddrescue mapfile, header and all, so
`ddrescue -m`, `ddrescuelog` and this toolkit's own `-map` all read it. The
header matters more than it looks: GNU ddrescue insists the current-position
line carry a single status character where a block line carries a size, and
rejects the whole file otherwise.

Reading through a map also makes damage that used to pass unnoticed visible, and
the parsers collect it rather than stopping at it. An ISO 9660 directory sector
that will not read costs the entries it holds and the walk carries on with the
rest of the tree; a lost volume descriptor is reported as never recovered rather
than as "not an ISO", which are quite different problems. An ext2 directory does
the same: its entries never span a block, so the blocks that did read give up
their entries and only the damaged block's are lost — one bad sector under
`/usr` costs the names in that sector and not the subtree beneath it. A FAT
directory is read sector by sector for the same reason, so a bad sector costs
the sixteen entries it held rather than the rest of the directory and every
cluster of it that read perfectly well. `cuetool verify` does
the same for a disc: an audio sector that will not read is counted rather than
ending the run, and a sector the map already calls bad is not reported a second
time as a structural fault of the BIN — nothing was read there, so it has no
sync pattern to be wrong.

`cat` is the exception in every tool: it writes to stdout and has nowhere to put
a map, so it stops at the damage rather than handing over zeros nothing records.
It still prints everything up to it, the readable head of the failing block
included.

On ext2 the surprising loss is an *indirect* block. A file over 12 blocks keeps
the addresses of the rest of its data in pointer blocks, so one of those going
bad costs every block it addressed — the data itself is still on the disk, but
nothing says where. `fstool verify` reports it as a lost pointer block, with the count
of file blocks the pointer block took with it, rather than reporting a file that
simply stops early. How many blocks were lost is exact rather than a guess,
which is what makes the hole fillable at all.

`cuetool export` carries the damage into the artifacts it writes. A track is
exported at its full length with the never-recovered ranges zeroed, and a
`track01.iso.map` beside it saying which of the exported file's own bytes those
are — translated out of the disc's offsets, which are not the file's. The whole
chain then holds:

```bash
disctool rip /dev/sr0 disc              # disc.bin + disc.bin.map
cuetool export -o ./out disc.cue        # track01.iso + track01.iso.map
fstool ls ./out/track01.iso            # marks the files the bad sector cost
fstool extract -o ./files ./out/track01.iso
```

`disctool`'s `-map` means the opposite of every other tool's: it *writes* a map
beside the output rather than reading one. It reads a device and never an image,
so there is nothing for it to read a map of, and the two flags cannot appear in
the same command — but the two are next to each other in the chain above, so the
spelling is worth knowing.

A disc that exported in full has all its tracks whatever the medium lost — the
artifact plus its map is the correct output for a damaged rip — and the exit
code says the disc was short rather than the run. A source with no map produces
none, since an all-recovered map would claim a recovery that was never measured.

Keeping a file's own offsets is what makes a container recoverable. A backup
archive rescued out of a disk image arrives at its own tool with the map beside
it, and `1steptool` and `qictool` pick up a sidecar the same way, so the
damage is reported against the entries inside the archive instead of being
decoded as data — and then carried one container further, into the files
restored out of it:

```bash
fstool extract -o ./out disk.img BACKUP.QIC   # writes BACKUP.QIC and BACKUP.QIC.map
qictool verify ./out/BACKUP.QIC                # lists the files the bad sector hit
qictool extract -o ./files ./out/BACKUP.QIC    # NOISE.BIN, and NOISE.BIN.map beside it
```

A hole is only ever filled where its length is arithmetic rather than a guess,
which is what each format gives:

- a QIC segment records the count of uncompressed bytes before it, so what a
  lost one cost is the distance to where the next segment resumes;
- a 1-Step chunk records its original size, so a chunk that will not decompress
  — or one on a disk of the set that was never supplied — is a hole of exactly
  that size, and a backup missing a disk restores at full length with the gaps
  marked rather than stopping at the first file that needs it;
- an ext2 pointer block addresses a known run of file blocks, so losing it
  costs that run and no more.

Where the length is not known the read still stops: a QIC segment with no later
header to bound it, or a file whose position in the archive — rather than its
contents — was lost, are reported as unrecoverable instead of being filled with
zeros that would claim more than anyone knows.

Each file's recorded modification time is restored where the medium kept one,
which `-times=false` disables. An ISO 9660 entry has none: the reader does not
parse the directory record's date and Rock Ridge `TF` is not among the tags it
handles.

### Exit codes

An exit code answers two separate questions, which is why there are four of
them: what the tool found on the medium, and whether the run itself worked.

| | |
|---|---|
| `0` | clean — the command did what was asked and found nothing wrong |
| `1` | degraded — the medium was damaged and the command handled it as designed: holes zero-filled with a `.map` recording them, a file that stopped part way, one the medium gave up nothing of, a checksum that did not match, a directory that would not read, sectors that would not come back |
| `2` | usage — the command line was wrong |
| `3` | failed — the run could not do what was asked: an image that would not open, an output or a `.map` that would not write, an output disk that filled up, an archive left truncated, a selection that matched nothing |

So `code == 0` means a faithful copy, `code <= 1` means the artifacts are
usable, and anything higher means they are not to be trusted. Every tool uses
the same four, so a script can treat them alike:

```bash
qictool extract -o ./out backup.qic
case $? in
  0) echo "clean" ;;
  1) echo "recovered what was there; read the .map files" ;;
  *) echo "did not finish" ; exit 1 ;;
esac
```

The one thing worth knowing is which commands put a finding in the status.
A command asked to *judge* the medium answers in its exit code: `verify`
reports 1 for a volume with issues, and `extract` and `cat` report 1 for damage
they met while reading. A command asked to *describe* it answers in its output
and exits 0: `info` prints how much was never recovered, `ls` marks the entries
that touch it, and `qictool segments` lists the damaged segments — none of that is a
verdict. Where a listing does exit 1, it is because it tried to read something
and could not, such as an ISO 9660 directory sector.

A flag combination that cannot work is a usage error however late it is
noticed. Some can only be judged once the image is in hand — a partition number
against the table it is not in, `-geometry` against an image type that has none
— which is past the point where the flag parser could have caught it.

### Into an archive instead

`-format zip` or `-format tgz` writes one archive rather than a tree of loose
files, which is usually what a recovery wants: it moves off the machine as a
single file. `-o` then sets the archive's path, and `-o -` sends it to stdout, so a
restore can be piped somewhere else without landing on this disk at all.
`tar.gz` is accepted as a spelling of `tgz`.

```bash
fstool extract -format zip -o recovered.zip disk.img
fstool extract -format tgz -o recovered.tgz disk.img SUBDIR
1steptool extract -format tgz -o - disk1.1-Step | ssh host 'tar xzf - -C /restore'
```

Every archive carries a `datarescue-report.txt` member holding what the run
reported: which files stopped partway, which failed a checksum, which
directories would not read.

A member records the length that was actually recovered, never the length the
image declared. Two names that sanitize to one member are refused rather than
written twice, since an archive member cannot be replaced in place the way a
file can. Empty directories are not represented, matching extraction to a
directory. `-f` applies to the archive file itself.

## Tools

| Command | Description |
|---------|-------------|
| `cmd/fstool` | Inspects and extracts from a filesystem in a disk image, detecting which one it is: `info`, `ls`, `cat`, `extract`, `verify`, `orphans` |
| `cmd/cuetool` | Verifies and exports tracks from CUE/BIN images |
| `cmd/disctool` | Rips a CD or DVD to an ISO or CUE/BIN image, recording bad sectors in a ddrescue map: `info`, `rip` |
| `cmd/1steptool` | Inspects and restores Iomega 1-Step Backup archives: `info`, `ls`, `cat`, `extract`, `verify`, `catalog`, `dbf` |
| `cmd/qictool` | Inspects and restores Windows 9x MS Backup archives: `info`, `ls`, `cat`, `extract`, `verify`, `segments`, `decompress`, `catalog` |

### fstool

Reads the filesystem in a disk image, whichever one it is. The format is
detected rather than declared: an operator holding a directory of rescued
images should not have to know what each one holds before they can look.

```bash
fstool info disk.img                       # what it is, where it is, what the map says
fstool ls -lr disk.img                     # everything, with dates and sizes
fstool cat disk.img README.TXT             # one file to stdout
fstool extract -o ./out disk.img           # the whole volume
fstool extract -o ./out disk.img A.TXT     # named files only
fstool verify disk.img                     # read every file through and report
fstool orphans -v disk.img                 # FAT: chains no directory entry points to
fstool ls -partition 2 disk.img            # a partition rather than offset 0
fstool ls -fs hfs -offset 5632 mac.img     # say what and where, when it cannot tell
fstool ls -geometry 18:2:512 floppy.86f    # an 86F image with no detectable geometry
```

```
fstool <info|ls|cat|extract|verify|orphans> [flags] <image-file> [path-in-image ...]
  -fs string          filesystem to read rather than probing for one: iso|hfs|ext|fat
  -offset int         byte offset of the filesystem within the image (-1 auto-detects)
  -partition int      partition to use (1-based, 0 auto-detects)
  -geometry string    SPT:HEADS:BPS for an .86f image, which carries no geometry
  -map string         ddrescue .map for the image, which otherwise uses the sidecar beside it
  -no-map             ignore that sidecar and read the image as it is
  -l, -g              ls: long format, group by directory
  -o, -f, -format     extract: where the files go, and whether to overwrite
  -forks string       extract: what to do with a resource fork (HFS)
  -ext                extract: append an extension the Finder type code implies (HFS)
  -v                  print each file as it is handled

 fat only:
  -fat-policy string  which copy to trust when they disagree: strict, copy0, copy1, best-effort
  -contiguous         extract: read a file with a broken chain as a straight run of clusters
  -deleted            ls: include deleted entries
 iso only:
  -sector-size int    bytes per sector: 2048 (cooked) or 2352 (raw); 0 probes both
 hfs only:
  -tz string          the zone the disk's clock was set to, for its timestamps
```

Detection tries ISO 9660, then HFS, then ext2, then FAT, at offset 0 and then
at each partition the image's table lists. The order is by how specific a
format's own evidence is, and FAT is last because it accepts the loosest thing
— a boot sector with a plausible parameter block. A hybrid disc carrying both
an ISO 9660 and an HFS volume therefore reports as ISO; `-fs hfs` reaches the
other half.

A flag that belongs to another format is refused as a mistake on the command
line, however late it is noticed, and so is `orphans` against a volume that
does not track allocation outside its directory tree. Setting a format's flag
does not imply `-fs`: `-fat-policy copy0` works on an image that was probed.

On a damaged image the probe answers three ways rather than two. "This is not
FAT" and "the boot sector was never recovered" are different findings, and the
second is the one a rescue map exists for — reporting it as the first sends an
operator looking for the wrong problem.

#### FAT

A FAT volume carries two copies of the allocation table, and on a damaged one
they rarely agree. `-fat-policy strict` cross-checks them and refuses a chain
they genuinely disagree about; `copy0` and `copy1` always use the first or
second copy, and `best-effort` merges them, taking the non-free side per
cluster. Each copy is read sector by sector, so a bad sector costs the entries
it holds rather than the copy, and a chain reaching one of those entries is reported unknown rather
than silently ended: they decode as zero, which is indistinguishable from
"free".

`orphans` lists the chains allocated in the FAT that no directory entry points to,
which is where a deleted or unlinked file's data still is; `-v` lists the
contents of any orphan that parses as a directory.

#### ext2 and ext3

Optimized for recovery rather than for mounting: nothing is replayed from the
journal, and every size and index taken off the disk is bounded before it is
used. A superblock or group descriptor table that will not read falls back to
the backup copies, which sit at the sparse-super groups — 1, 3, 5, 7, 9, 25, 27
and so on. A backup is accepted only when its own fields agree with the
geometry that located it, so stale data left at a wrong-block-size offset is not
mistaken for a superblock; `info` says which copy was used.

The loss worth knowing about on ext2 is an *indirect* block. A file over twelve
blocks keeps the addresses of the rest of its data in pointer blocks, so one of
those going bad costs every block it addressed — the data is still on the disk,
but nothing says where. How many blocks that is, though, is exact rather than a
guess, which is what makes the hole fillable: `extract` writes the file at its
full length with that run zeroed and a `<name>.map` beside it.

A directory whose data stops coming back still yields the entries that arrived.
Entries never span a block in ext2, so the blocks that read give up whole
records and only the damaged block's names are lost — one bad sector under
`/usr` costs the names in that sector, not the subtree beneath it.

#### ISO 9660

Joliet and Rock Ridge names are applied where the disc carries them, falling
back to 8.3 names where it does not; a plain name with no extension loses the
separator the format stores with it, so `README.;1` is listed as `README`.
Each entry's date is the Rock Ridge modification time where there is one and
the directory record's recording date otherwise, and `extract -times` applies
it. Rock Ridge permissions appear in `ls -l`, and a Rock Ridge device node, pipe
or socket is listed but skipped by `extract`. A date that is out of range is
reported as damage and left unset. A file stored in several extents (how ISO
9660 Level 3 holds files over 4 GiB) is listed and extracted as one file; one
whose chain of records ends early is extracted with the extents found and
reported as partial. A tree mkisofs relocated to stay within eight levels is
shown where it belongs rather than under `rr_moved`. Associated files, which
share a name with the file they belong to, are skipped and counted. A path named
on the command line is found by reading only the directories along it, so one
file can be recovered without walking the whole disc. `info` adds the publisher, data preparer and
application identifiers and the volume's creation, modification, expiration
and effective dates when the disc records them. An image ripped raw holds 2352-byte
sectors — a sync pattern, an address and an ECC area wrapped around the 2048
bytes of file data — and one ripped cooked holds only the 2048. The probe reads
the volume descriptor at both sizes and takes whichever answers, so
`-sector-size` is only for an image it cannot choose between. A raw image
reports more damage than a cooked one of the same disc: a sector's identity
cannot be checked when its header was never recovered, so a hole anywhere in
the 2352 bytes fails the sector, not just one in the payload.

What `ls` reports as lost is counted in each file's own bytes rather than the
image's, since a raw sector is 2352 bytes on the medium and only 2048 of the
file's, and a loss quoted in medium bytes beside a size quoted in file bytes
invites the wrong subtraction. Rock Ridge symlinks are listed but never
recreated: an image should not be able to place a link the operator did not
choose.

#### Apple HFS

Reads "Mac OS Standard" volumes, in a raw image or in an MBR or Apple
partition map. An HFS wrapper around an HFS+ volume is refused with the offset
and length of the volume inside it, rather than opened as the almost-empty
volume it looks like from outside — "this disk is empty" is the worst wrong
answer a recovery tool can give.

A volume's length is the partition's and not the image's, which is what locates
the spare copy of the master directory block: it sits two logical blocks from
the volume's own end, and nothing inside the volume says where that is. An
allocation area that reaches past that end is clamped to what the volume holds
and reported, rather than sending fork reads into whatever follows it; geometry
with no room left at all is refused.

A Mac file has two forks, and on an HFS volume much of the content is in the
**resource fork** rather than the data fork: applications, fonts, system
suitcases and older documents keep everything there and leave the data fork
empty, so a tool that reads only the data fork loses most of the disk.
`extract` writes the data fork as the file and the resource fork as a `._Name`
AppleDouble companion, which is what macOS itself writes onto a volume that
cannot hold two forks — so the pair reunites when it is copied back to a Mac
volume. The companion carries the file's Finder type and creator codes and its
dates.

```
-forks appledouble   the data fork, plus a ._Name companion holding the rest (default)
-forks separate      the data fork, plus a plain Name.rsrc beside it
-forks data          the data fork only, reporting what that dropped
-forks none          the data fork only, silently
```

Each is written through the same path as any other file, so a damaged resource
fork is zero-filled at its full length with a `._Name.map` recording the holes
— at the offsets they occupy in the companion, which is a header further in
than the fork's own.

A classic Mac file carries no extension. The catalog records what it holds in a
four-character Finder **type** code, and what wrote it in a **creator** code.
`ls -l` prints the pair as the volume records them (e.g. `TIFF/8BIM`,
`8BPS/8BIM`). By default `extract` writes the names as the volume spells them
with no extension attached. `-ext` appends one for known types, so `Photo` is
written as `Photo.tif`.

```
-rw-rw-rw-  12850498  1996-05-21 19:08:45  TIFF/8BIM  Photo
-rw-rw-rw-  18716397  1996-04-30 20:49:16  8BPS/8BIM  Things
```

An unknown code gets no extension. Neither does a file whose content is not in
the data fork: an application, a font suitcase, a HyperCard stack, the desktop
database.

The extension is only ever appended, so the volume's own name is always a
prefix of what was written. The `._Name` companion records the volume's name in
full. Selection still uses the name the volume gives the file (`fstool extract
disk.img "Photo"`, not `Photo.tif`), and so does every message about the
medium.

When two files collide once extended, the run fails rather than renaming one:
these tools do not invent a name nothing on the medium supports. With `-f` the
second file overwrites the first, as it does for any two names that sanitize
alike.

A file's name can contain `/`, which is an ordinary character on HFS where the
separator is `:`. The two swap roles on the way out, the same substitution
macOS makes, so a file called `10/20/99 Budget` stays one file rather than
becoming three directories.

HFS records timestamps as local wall-clock seconds and no zone, so they are
read as UTC: deterministic, and stated rather than guessed. If you know the
time zone a disk was written in, `-tz` sets it.

### disctool

Reads a CD or DVD in the drive: `info` reports what is on it, `rip` copies it
to an ISO or CUE/BIN image, choosing the format from the table of contents
unless told otherwise. A sector that will not read is zero-filled and recorded
as bad in the `.map`, which is written always and even if the rip dies partway
through: a partial image with no map cannot be interpreted, and the zeros a rip
puts in cannot be told from data without one. `-no-map` turns it off.

```bash
disctool info /dev/disk8                           # what is in the drive
disctool rip /dev/sr0 disc.iso                     # data disc
disctool rip -format cuebin /dev/sr0 album         # audio or mixed-mode disc
disctool rip /dev/disk8 dvd.iso                    # DVD; no extra flag needed
```

```
disctool rip [flags] <device> <output>
  -format string     output format: auto, iso, cuebin (default "auto")
  -session uint      session to rip for ISO output (0 = last data session)
  -no-map            do not write the ddrescue .map beside the output
  -retries int       per-sector attempts before marking a sector bad, on top of
                     the few the device makes of its own (default 3)
  -cooked-fallback   on a CD, read a sector the low-level command could not through a
                     plain block read instead
  -f                 overwrite an existing output image
  -verbose           log each bad sector
```

`info` reports what kind of medium is in the drive as well as the tracks, since that decides how the disc
is read and an operator should learn it before committing to a rip.

#### CDs, DVDs, and what `-cooked-fallback` is for

The command the tool reads CD sectors with — SCSI `READ CD` — is CD-only. It
cannot address a DVD's sectors at all, and rejects every one of them
identically rather than failing on the ones that are actually damaged.

That is not a gap to work around, it is the shape of the two formats. The
2352-byte raw sector, with its sync pattern, header and EDC/ECC, is a CD
structure; a DVD sector is 2048 bytes of user data whose error correction is
arranged differently and handled entirely inside the drive. DVDs are read with
`READ(10)`, which is the plain block path — the one `mount` and `dd` use. So
for a DVD those 2048-byte reads are not a fallback at all, they are the
medium's native and only readable form, and ripping one needs no flag.

`-cooked-fallback` is for the other case: a **CD** whose low-level read fails.
There the block path may still recover the sector, but it forfeits the raw
sector's own EDC, which `cuetool verify` could otherwise check independently.
That is a real trade, so the tool asks first. Sectors recovered that way are
written like any other and additionally recorded in `<output>.cooked.map`, so
they stay distinguishable from low-level-verified ones after the fact. That
sidecar is written only for CD media — on a DVD there is no low-level path to
contrast with, so it would duplicate the main `.map` byte for byte.

`-format cuebin` needs raw 2352-byte sectors, which have no plain-block
equivalent, so it refuses a DVD outright.

### cuetool

A combined tool for working with CUE/BIN disc images.

#### cuetool verify

Verifies the structural integrity of a CD rip given a `.cue` file, its `.bin`, and an optional ddrescue `.map`:

```
cuetool verify [flags] <file.cue>
  -map string   ddrescue .map for the .bin, which otherwise uses the sidecar beside it
  -no-map       ignore that sidecar and read the image as it is
  -full         read and verify every file's data (slow)
  -v            verbose per-sector output
  -json         output JSON report
```

`-no-map` is worth having here rather than only on the tools that extract: the
EDC cross-reference exists to catch a *stale* map, and once it has, the next
question is what the bytes say without it.

Checks performed:

- **BIN size alignment** — file size must be a whole number of sectors
- **Sync pattern** — bytes 0–11 of each raw sector must equal `00 FF×10 00`
- **MSF sequence** — the embedded MSF address must increment by one frame per sector
- **EDC** — CRC-32/CD checksum over bytes 0–2063 compared against bytes 2064–2067 (MODE1 only)
- **ddrescue overlay** — cross-references each sector against the map; flags any mismatch between EDC failures and map status. Sync and MSF are checked only where the map says the bytes were recovered
- **Filesystem traversal** — detects ISO 9660 or Apple HFS/APM and maps each file's extent to its sector range; reports affected files
- **Audio analysis** — for audio tracks: duration, bad-sector coverage, silence runs, repeated-sector detection, and how many sectors would not read at all (which are neither hashed nor counted as silence)

Exit codes are the shared ones above: `0` clean, `1` for anything the disc
lost — bad sectors, a broken sync pattern, a catalog that will not parse — and
`3` when the run could not get that far. The report says which of those it was
in words, printing `Result: CLEAN`, `DEGRADED` or `FAILED`; the code used to
say `FAILED` with `2`, which a wrapper reads as "you typed the command wrong".

#### cuetool export

Exports tracks from a CUE/BIN image to ISO and WAV files:

```
cuetool export [flags] <file.cue>
  -o string      output directory (default: same as .cue file)
  -tracks string comma-separated track numbers to export (default: all)
  -swap-audio    byte-swap audio samples (for big-endian source images)
  -map string    ddrescue .map for the .bin, which otherwise uses the sidecar beside it
  -no-map        ignore that sidecar and read the image as it is
  -f             overwrite existing output files
```

`PREGAP` and `POSTGAP` in a cue sheet are silence the writer generates and are
not bytes in the `.bin`; a gap that *is* in the file is spelled `INDEX 00`, and
only that moves a track boundary. A cue sheet with a track mode these tools do not
know is refused rather than guessed at, since guessing 2352 for a `MODE2/2336`
sheet drifts 16 bytes per sector and reports nothing.

Each track is written with a `.map` of its own where the source had one, so an
exported ISO or WAV stays interpretable: see "Carrying the damage forward"
above. Note that for a data track not starting at disc LBA 0, the exported ISO's
internal directory LBAs are the disc's absolute ones while the file's own
offsets are track-relative.
### 1steptool

Restores files from Iomega 1-Step Backup archives, the `*.1-Step` files the
software bundled with Zip and Jaz drives wrote on Windows 9x.

A backup's data runs from the first disk of the set to the last and the catalog
is written at the end of the last one, so every subcommand takes all the disks
at once. They may be given in any order and are sorted by the disk number in
each header; the one carrying the catalog must be among them. Because the
arguments are the disks, paths inside the backup are selected with `-path`
rather than as trailing arguments.

Each disk is filled to its end and a file's data simply continues on the next
one, so a single file is often spread over two disks. A disk that is missing or
was only partly recovered costs the files stored on it and nothing else:
`info` says how much of each disk is there, `ls -l` marks the files that cannot
be read with `!`, and `verify` and `extract` list them.

```bash
1steptool info    "Backup Job 7, Disk 1, 16-10-27 19.36.39.1-Step"
1steptool ls -l   disk1.1-Step disk2.1-Step             # the whole set
1steptool verify  disk1.1-Step disk2.1-Step             # check, write nothing
1steptool extract -o ./out disk1.1-Step disk2.1-Step    # restore everything
1steptool extract -o ./out -path 'C:\DOS' disk1.1-Step  # one directory tree
1steptool cat -path 'C:\DOS\ATTRIB.EXE' disk1.1-Step > ATTRIB.EXE
1steptool dbf -map IOA.DBF.map IOA.DBF                  # a table rescued out of an image
```

Every subcommand accepts `-map` for a ddrescue sidecar when given a single
image; with several, each disk's map is picked up from beside it
(`disk1.1-Step.map`, or `disk1.map`). `-no-map` reads the disks as they are.

#### 1steptool verify

Decompresses every chunk of every file and checks it against the catalog,
writing nothing:

```
1steptool verify [flags] <file.1-Step ...>
  -map string    ddrescue .map for the image; only with a single image
  -no-map        ignore the sidecar and read the image as it is
  -path string   restrict to this path in the backup, or the tree under it (repeatable)
  -v             print each file as it is checked
```

A chunk can carry the one's complement of the CRC-32 of its original bytes, so
a compressed backup can be proven byte for byte. Not every writer recorded
them: a chunk stored verbatim never has one, and some backups carry none at
all. The summary says how many chunks were actually checked, so a clean result
cannot be mistaken for more proof than it is. Whether the data is all there is
checked either way, from the sizes the catalog records.

Exit codes: 0 if everything verified, 1 if a file failed or its data is on a
disk that was not supplied, 2 for a bad command line, 3 if the set could not be
opened at all.

#### 1steptool extract

```
1steptool extract [flags] <file.1-Step ...>
  -o string      output directory, or the archive file when -format is not "dir" ("-" for stdout) (default ".")
  -f             overwrite existing output
  -format string output format: "dir", "zip" or "tgz" (default "dir")
  -map string    ddrescue .map for the image; only with a single image
  -no-map        ignore the sidecar and read the image as it is
  -path string   restrict to this path in the backup, or the tree under it (repeatable)
  -times         restore each file's recorded modification time (default true)
  -v             print each file as it is written
```

The backup records DOS paths, so `C:\DOS\ATTRIB.EXE` is written to
`C_/DOS/ATTRIB.EXE`: a colon cannot be a path component on Windows, where it
would be read as a drive letter or an alternate data stream.

A chunk that will not read or decompress — or one on a disk of the set that was
never supplied — becomes zeros at the size the catalog records for it, with a
`<name>.map` beside the file saying where. A backup missing a middle disk
therefore restores at full length with the gaps marked, rather than stopping at
the first file that needs it.

A file whose data stops partway anyway is kept, along with the reason, since a
partial recovery is still evidence of what the backup held. So is one whose
checksum did not match, reported as a mismatch rather than discarded. All three
make the command exit 1 — the backup was damaged, and the files it did give up
are still written.

#### 1steptool catalog and dbf

The catalog is a container of dBase III tables. `catalog -l` lists its members
and `catalog -o <dir>` writes them out; `dbf` dumps one, whether it came from
`catalog` or from the `IOA*.DBF` configuration database 1-Step Backup kept on
the machine the backup ran from.

```
1steptool catalog [flags] <file.1-Step ...>
  -o string      output directory (default ".")
  -f             overwrite existing output files
  -l             list the members instead of writing them

1steptool dbf [flags] <file.dbf>
  -map string    ddrescue .map for the table, which otherwise uses the sidecar beside it
  -no-map        ignore that sidecar and read the file as it is
  -offset int    byte offset of the table within the file
  -rows int      print at most this many rows (0 = all)
  -fields        print the field definitions only
```

`dbf` closes a loop of its own: `fstool extract` writes `IOA.DBF` and
`IOA.DBF.map` out of a damaged image, and this reads the table through that
sidecar rather than decoding the zeros as records. A record over an unrecovered
range is reported rather than returned, and the reports are capped so a table
sitting over a bad region does not print one line per lost sector.

### qictool

Restores files from Windows 9x MS Backup archives, the `*.qic` files Microsoft
Backup wrote on Windows 95, 98 and ME.

Past a head of 128-byte volume table records the file is a grid of
29696-byte segments: a data region, then the catalog. In a compressed archive
every segment carries a header giving the count of uncompressed bytes that
precede it, which is what makes the format worth recovering rather than merely
reading. Any file can be reached by expanding only the segments it occupies,
and a segment that will not decompress costs the bytes it held instead of
everything after it. On one of the damaged archives this was written against,
ten bad segments all fell inside a single 3 MB file: the other 1455 files came
back whole, including the 301 stored past the first bad segment, which a reader
that decompresses from the start would lose.

The catalog records a size and a directory tree but no pointer to any file's
data, so a data offset is the running total of every record before it and one
bad record would displace all the rest. The data region repeats each record
inline ahead of the bytes it describes, which is what lets an accumulated
offset be checked before it is trusted; where it disagrees the region is
searched for the record and everything after it is moved to match. The search
is bounded, so an archive whose every record disagrees cannot sweep the whole
region once per file: once it has run its allowed number of times, a record
that still disagrees leaves its file's position unresolved, and the file is
refused rather than read from where the catalog put it. Those inline
records also carry the path, so a backup whose catalog will not read is still
recoverable in full by scanning for them, which `-source scan` does on demand
and which happens by itself when the catalog yields nothing.

```bash
qictool info     backup.qic
qictool ls -l    backup.qic                        # size, time, and ! if damaged
qictool verify   backup.qic                        # check everything, write nothing
qictool extract -o ./out backup.qic                # restore everything
qictool extract -o ./out -path 'C:\DOS' backup.qic # one directory tree
qictool cat      backup.qic 'C:\DOS\ATTRIB.EXE' > ATTRIB.EXE
qictool segments -damaged backup.qic               # what a bad region of the medium cost
```

Every subcommand accepts `-map` for a ddrescue sidecar, defaulting to the one
beside the archive (`backup.qic.map`, or `backup.map`) and disabled with
`-no-map`, `-volume` to pick among
the volume table records a Windows 98 or ME archive writes one of per source
drive, and `-source` to choose whether the file list comes from the catalog,
from scanning the data region, or from the catalog with the scan as a fallback.

#### qictool verify

Reads every file through without writing anything, and separately reads every
segment:

```
qictool verify [flags] <file.qic>
  -map string    ddrescue .map for the archive, which otherwise uses the sidecar beside it
  -no-map        ignore that sidecar and read the archive as it is
  -path string   restrict to this path in the backup, or the tree under it (repeatable)
  -v             print each file as it is checked
```

The archive carries no checksums of any kind, so what can be proven is
structural: a segment has to expand to exactly the number of bytes the next
segment's recorded offset accounts for, and the segments have to total the size
the volume table declares. That is a real check rather than a formality —
damage to a compressed stream almost always changes its length — and it is
reported per segment, because a segment no file reads can still be damaged and
is worth knowing about before the medium is returned to a shelf.

Exit codes: 0 if every file and every segment came back whole, 1 if any file
stopped part way or any segment did not yield what its offsets account for, 2
for a bad command line, 3 if the archive could not be opened at all.

#### qictool extract

```
qictool extract [flags] <file.qic>
  -o string      output directory, or the archive file when -format is not "dir" ("-" for stdout) (default ".")
  -f             overwrite existing output
  -format string output format: "dir", "zip" or "tgz" (default "dir")
  -map string    ddrescue .map for the archive, which otherwise uses the sidecar beside it
  -no-map        ignore that sidecar and read the archive as it is
  -path string   restrict to this path in the backup, or the tree under it (repeatable)
  -times         restore each file's recorded modification time (default true)
  -v             print each file as it is written
```

The backup records DOS paths, so `C:\DOS\ATTRIB.EXE` is written to
`C_/DOS/ATTRIB.EXE`: a colon cannot be a path component on Windows, where it
would be read as a drive letter or an alternate data stream.

A file over a segment that will not expand is written at the length the catalog
declares, with zeros where the segment was and a `<name>.map` beside it saying
which of its byte ranges those are. Nothing is ever filled silently: the data
and the map are written by one call, so the zeros cannot exist without the
record of them. Where the length is not known — a segment with no later header
to bound it, or a file whose position in the archive was lost — the read stops
instead, and the file is kept at the length that actually came back.

Either way the command exits 1 — the archive was damaged, not the run — and the
file is listed in the report, which an archive carries as its
`datarescue-report.txt` member.

#### qictool segments, decompress and catalog

`segments` prints the segment map: where each segment sits in the file, where
its bytes fall in the uncompressed stream, how long it is, and whether it could
be placed. It is how an operator matches a bad region of the medium to the
files it cost. `decompress` writes the whole uncompressed data region out, the
equivalent of the reference tool's whole-archive mode, zero-filling any segment
that will not expand so that everything after it stays at the offset the archive
gives it. It writes `<out>.map` beside the output saying which bytes those are,
and it writes it whatever the run came to: zeros nothing records are the one
outcome worse than a short file. That is also why `-o -` refuses to fill — a
stream has nowhere to keep the map, the same reason `cat` latches. A run that
filled anything exits 1: the archive was damaged, not the run. `catalog` dumps
the records with the fields the format's documentation leaves unexplained.

```
qictool segments [flags] <file.qic>
  -damaged       show only the segments that could not be placed

qictool decompress [flags] <file.qic>
  -o string      write to this file, or - for stdout
  -f             overwrite an existing output file

qictool catalog [flags] <file.qic>
  -l             show the flag and attribute bits and the record's place in the data region
  -n int         show at most this many records
```

## Testing

`make ci` runs everything CI runs: format check, build, vet, cross-compile,
lint and tests. `make fuzz` runs the fuzz targets briefly.

Every parser that takes bytes off damaged media has one, since a corrupt length
field is the same input an attacker would craft: `fat`, `extfs`, `mbr`,
`img86f`, `ddrescue`, `hfs`, `iso`, `cdrom`, `onestep`, `qic` and the dBase
tables inside a 1-Step catalog. Each asserts the same thing — parse anything,
panic on nothing, and return no size, offset or cluster the medium cannot
hold — and CI runs the same smoke pass on every push.

To test real 1-Step files, point `DATARESCUE_1STEP_DIR` at a directory of
archives to verify them; the test skips when it is unset. `DATARESCUE_QIC_DIR`
does the same for `*.qic` archives, checking each one against the invariants
that pinned the format down: every segment expands to what the next segment's
offset accounts for, the segments total the size the volume table declares, the
rebuilt directory tree reproduces the parent path length every record carries,
the accumulated data offsets end exactly where the data does, and the records
the data region repeats inline agree with the catalog on every file's size and
position. It also checks that a recovered file begins with the signature its
name implies, which is the one test that trusts no implementation of the format
at all.

`make qemutest` covers what a unit test cannot reach: the CD-ROM ioctls in
`cdrom` and the rip loop in `cmd/disctool` need a real device node, so it boots a
Linux virtual machine with an emulated ATAPI drive and runs those packages'
tests inside it. It needs [`qemu-test`](https://github.com/samuel/qemu-test) and
`qemu-system-x86_64` installed and skips with a message when they are absent, so
it is not part of `make ci`. Four scenarios run: each package against a clean
disc, and again against one whose sectors qemu's `blkdebug` driver has been told
to fail.

The tests that need the drive are gated on `DATARESCUE_CDROM_DEVICE` (and
`DATARESCUE_CDROM_BAD_LBA` for the injected read errors), which `make qemutest`
sets inside the guest. Without them they skip, so `go test ./...` neither fails
on a machine with no optical drive nor touches one that has it.

An emulated drive presents a single-session, data-only disc and synthesizes the
raw sector header without error correction, so audio tracks, pregaps and
multisession discs are covered instead by the fake `cdrom.Device` in
`cmd/disctool`'s tests.
