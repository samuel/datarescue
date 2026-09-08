package fatdrv

import (
	"fmt"
	"io"
	"strings"

	"samuelks.com/datarescue/fat"
	"samuelks.com/datarescue/internal/fstool"
	"samuelks.com/datarescue/internal/humanize"
)

// Info writes what the volume says about itself.
func (v *volume) Info(w io.Writer) error {
	f := v.f
	bpb := f.BootSector.BIOSParameterBlock
	p := func(format string, args ...any) {
		fmt.Fprintf(w, format+"\n", args...) //nolint:errcheck // report output
	}

	p("Type:             %s", f.Type)
	p("OEM Name:         %s", strings.TrimSpace(string(f.BootSector.OEMName[:])))
	p("Volume Label:     %s", strings.TrimSpace(string(bpb.NoNameVolumeLabel[:])))
	p("System ID:        %s", strings.TrimSpace(string(bpb.SystemID[:])))
	p("Serial Number:    %s", formatSerial(bpb.VolumeSerialNumber))
	p("Boot Offset:      %d bytes", f.BootSectorOffset)
	p("")
	p("Sector Size:      %d bytes", f.SectorSize)
	p("Sectors/Cluster:  %d", bpb.SectorsPerCluster)
	p("Cluster Size:     %d bytes", f.ClusterSize)
	p("Total Sectors:    %d", f.TotalSectors)
	p("Total Size:       %s", humanize.Bytes(v.Bytes()))
	p("")
	p("FAT Copies:       %d", bpb.NumberOfFATs)
	p("Reserved Sectors: %d", bpb.ReservedSectors)
	p("Media Type:       %s", bpb.MediaType)
	if f.Type != fat.TypeFAT32 {
		p("Root Entries:     %d", bpb.RootEntries)
	} else {
		p("Root Cluster:     %d", f.RootCluster)
		if fi := f.FSInfo; fi != nil {
			p("")
			if fi.FreeCount == 0xFFFFFFFF {
				p("FSInfo Free Clusters: unknown")
			} else {
				p("FSInfo Free Clusters: %d (%s)", fi.FreeCount,
					humanize.Bytes(int64(fi.FreeCount)*int64(f.ClusterSize)))
			}
			if fi.NextFree == 0xFFFFFFFF {
				p("FSInfo Next Free:     unknown")
			} else {
				p("FSInfo Next Free:     cluster %d", fi.NextFree)
			}
		}
	}
	p("")
	summary, err := f.AllocationSummary()
	if err != nil {
		p("Cluster summary:  (error: %s)", err)
		return nil
	}
	p("Clusters (total): %d", summary.Total)
	p("Clusters (free):  %d (%s)", summary.Free,
		humanize.Bytes(int64(summary.Free)*int64(f.ClusterSize)))
	p("Clusters (used):  %d", summary.Reachable)
	if summary.Orphaned > 0 {
		p("Clusters (orphaned): %d (%s)", summary.Orphaned,
			humanize.Bytes(int64(summary.Orphaned)*int64(f.ClusterSize)))
	}
	if summary.Bad > 0 {
		p("Clusters (bad):   %d", summary.Bad)
	}
	return nil
}

// Verify is FAT's own structural check: the table copies, what they disagree
// about, and the directory entries that point somewhere they should not.
//
// Reading every file and attributing the map's damage to each is done by the
// shared walk, so this covers only the FAT-specific checks.
func (v *volume) Verify(rep *fstool.Report, verbose bool) {
	f := v.f

	// Problems noticed while opening (boot sector, table validation) reach the
	// report as data rather than as scraped log lines.
	for _, d := range f.Diagnostics() {
		rep.Warnf("(open) %s", d)
	}

	if f.ChainPolicy != fat.ChainPolicyStrict {
		rep.Infof("FAT copy policy: %s", f.ChainPolicy)
	}

	// Which copies were readable, and what each lost to bad sectors. An entry
	// backed by an unreadable sector decodes as zero, which is
	// indistinguishable from "free", so the count matters.
	if want := int(f.BootSector.BIOSParameterBlock.NumberOfFATs); want > len(f.Tables) {
		rep.Warnf("FAT copies: %d of %d readable (copies %v)", len(f.Tables), want, f.TableCopies)
	}
	for i, t := range f.Tables {
		if n := t.UnreadableEntries(); n > 0 {
			rep.Warnf("FAT copy %d: %d entries lost to unreadable sectors", f.TableCopies[i], n)
		}
	}

	v.verifyTableCopies(rep, verbose)

	entryIssues, err := f.CheckDirectoryEntries()
	if err != nil {
		rep.Warnf("directory entry check failed: %s", err)
	}
	for _, ei := range entryIssues {
		switch ei.IssueKind {
		case fat.IssueClusterFreed:
			rep.Warnf("%s: cluster freed — %s", ei.Path, ei.Detail)
		case fat.IssueSizeMismatch:
			rep.Warnf("%s: size mismatch — %s", ei.Path, ei.Detail)
		case fat.IssueChainCorrupt:
			rep.Errorf("%s: corrupt chain — %s", ei.Path, ei.Detail)
		}
	}
}

// verifyTableCopies compares the FAT copies. A difference where one side is
// free can be merged, and the non-zero side is the reliable one. A difference
// where both sides are allocated cannot be reconciled. The two cases mean
// different things, so they are counted separately.
func (v *volume) verifyTableCopies(rep *fstool.Report, verbose bool) {
	f := v.f
	if len(f.Tables) < 2 {
		return
	}
	if f.Tables[0].Len() != f.Tables[1].Len() {
		rep.Warnf("FAT tables have different lengths: %d vs %d", f.Tables[0].Len(), f.Tables[1].Len())
		return
	}
	conflicts, err := f.AnalyzeFATConflicts()
	switch {
	case err != nil:
		rep.Warnf("FAT conflict analysis failed: %s", err)
		return
	case len(conflicts) == 0:
		rep.Infof("FAT tables: consistent (%d entries)", f.Tables[0].Len())
		return
	}
	var mergeable, irreconcilable int
	for _, c := range conflicts {
		if c.Mergeable {
			mergeable++
		} else {
			irreconcilable++
		}
		if verbose {
			tag := "conflict"
			if c.Mergeable {
				tag = "mergeable"
			}
			rep.Warnf("FAT cluster %d: table[0]=%08X table[1]=%08X [%s]",
				c.Cluster, c.Table0, c.Table1, tag)
		}
	}
	if mergeable > 0 {
		rep.Warnf("FAT tables: %d mergeable differences (one side is free; non-zero is reliable)", mergeable)
	}
	if irreconcilable > 0 {
		rep.Warnf("FAT tables: %d irreconcilable conflicts (both sides are non-zero)", irreconcilable)
	}
}

// VerifyTail reports the cluster allocation once the walk has run, since the
// summary walks the tree again and its findings belong after the walk's.
func (v *volume) VerifyTail(rep *fstool.Report) {
	summary, err := v.f.AllocationSummary()
	for _, d := range v.f.Diagnostics() {
		rep.Warnf("(allocation) %s", d)
	}
	if err != nil {
		rep.Warnf("cluster allocation summary failed: %s", err)
		return
	}
	if summary.Orphaned > 0 {
		rep.Warnf("orphaned clusters: %d (%s) — run 'orphans' to inspect",
			summary.Orphaned, humanize.Bytes(int64(summary.Orphaned)*int64(v.f.ClusterSize)))
	}
	if summary.Total > 0 {
		rep.Printf("Clusters:    %d total, %d free, %d used, %d orphaned, %d bad",
			summary.Total, summary.Free, summary.Reachable, summary.Orphaned, summary.Bad)
	}
}
