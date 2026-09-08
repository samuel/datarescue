// Command qemucd runs the CD-ROM tests inside a QEMU virtual machine.
//
// The ioctl layer in cdrom and the rip loop in cmd/disctool can only be exercised
// against a kernel holding a real device node, which no unit test can provide.
// This driver builds the disc image, attaches it to a virtual machine as an
// ATAPI drive, and runs each package's tests inside the guest through qemu-test.
//
// It is deliberately not part of "make ci": it needs qemu and qemu-test
// installed, downloads a guest kernel on first use, and boots a virtual machine
// per scenario.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"

	"samuelks.com/datarescue/internal/testiso"
)

// badLBA is the sector the damaged-disc scenarios make unreadable. It holds the
// root directory record, so it is not all zeros and a zero fill is detectable.
const badLBA = 18

// The guest reads these to find the device and, in the damaged scenarios, the
// sector the host broke. Without them the tests skip.
const (
	deviceEnv = "DATARESCUE_CDROM_DEVICE"
	badLBAEnv = "DATARESCUE_CDROM_BAD_LBA"
	// devicePath is where the guest kernel puts the first SCSI CD-ROM. The
	// guest has no udev, so there is no /dev/cdrom symlink to use instead.
	devicePath = "/dev/sr0"
)

// modules the guest must load to see an ATAPI drive. Their dependencies
// (cdrom, scsi_mod, libata) resolve automatically.
var modules = []string{"sr_mod", "ata_piix"}

type scenario struct {
	name string
	pkg  string
	// damaged attaches the disc through qemu's blkdebug driver, which fails
	// reads of one sector. It is the only way to put a real drive error in
	// front of the retry and bad-sector paths.
	damaged bool
}

var scenarios = []scenario{
	{name: "cdrom", pkg: "./cdrom"},
	{name: "disctool", pkg: "./cmd/disctool"},
	{name: "cdrom-damaged", pkg: "./cdrom", damaged: true},
	{name: "disctool-damaged", pkg: "./cmd/disctool", damaged: true},
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "qemucd:", err)
		os.Exit(1)
	}
}

func run() error {
	qemuTest := flag.String("qemu-test", "qemu-test", "path to the qemu-test binary")
	arch := flag.String("arch", "amd64", "guest architecture")
	only := flag.String("run", "", "run only the scenarios matching this regular expression")
	verbose := flag.Bool("v", false, "pass -v and -test.v through to qemu-test")
	keep := flag.Bool("keep", false, "pass -keep through to qemu-test, keeping its temp dir")
	flag.Parse()

	var filter *regexp.Regexp
	if *only != "" {
		re, err := regexp.Compile(*only)
		if err != nil {
			return fmt.Errorf("bad -run pattern: %w", err)
		}
		filter = re
	}

	// The raw 2352-byte read path exists only in qemu's ATAPI emulation, and
	// the non-x86 machines have no IDE bus to attach an ATAPI drive to.
	if *arch != "amd64" && *arch != "386" {
		return fmt.Errorf("arch %q has no IDE bus; the raw sector reads need an ATAPI drive", *arch)
	}

	dir, err := os.MkdirTemp("", "qemucd")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir) //nolint:errcheck // a leftover temp dir is not worth failing the run over

	isoPath := filepath.Join(dir, "test.iso")
	if err := testiso.WriteFile(isoPath); err != nil {
		return err
	}
	configPath := filepath.Join(dir, "blkdebug.conf")
	if err := os.WriteFile(configPath, []byte(blkdebugConfig(badLBA)), 0o600); err != nil {
		return fmt.Errorf("write blkdebug config: %w", err)
	}

	var ran int
	for _, sc := range scenarios {
		if filter != nil && !filter.MatchString(sc.name) {
			continue
		}
		ran++
		fmt.Printf("== %s (%s)\n", sc.name, sc.pkg)
		args := scenarioArgs(sc, *arch, isoPath, configPath, *verbose, *keep)
		//nolint:gosec // running the tool named by -qemu-test with the scenario's flags is this command's whole job
		cmd := exec.Command(*qemuTest, args...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("scenario %s: %w", sc.name, err)
		}
	}
	if ran == 0 {
		return errors.New("no scenarios matched -run")
	}
	fmt.Printf("== %d scenario(s) passed\n", ran)
	return nil
}

func scenarioArgs(sc scenario, arch, isoPath, configPath string, verbose, keep bool) []string {
	args := []string{"-arch", arch}
	for _, m := range modules {
		args = append(args, "-module", m)
	}
	args = append(args, "-env", deviceEnv+"="+devicePath)
	if sc.damaged {
		args = append(args, "-env", badLBAEnv+"="+strconv.Itoa(badLBA))
		// blkdebug cannot be reached through -cdrom, so the drive and the
		// device it attaches to are specified explicitly. bus=ide.1 is the
		// second channel of the machine's IDE controller, where a CD normally
		// lives.
		//
		// blkdebug goes under the raw format rather than on top of it: its
		// rules only arm when the block layer reports a read event against the
		// blkdebug node, which happens when another node is reading through it.
		args = append(args,
			"-qemu.arg", "-drive if=none,id=cd0,readonly=on,format=raw,file=blkdebug:"+
				configPath+":"+isoPath,
			"-qemu.device", "ide-cd,drive=cd0,bus=ide.1")
	} else {
		args = append(args, "-qemu.arg", "-cdrom "+isoPath)
	}
	if verbose {
		args = append(args, "-v", "-test.v")
	}
	if keep {
		args = append(args, "-keep")
	}
	return append(args, sc.pkg)
}

// blkdebugConfig makes reads of one CD sector fail with EIO. blkdebug counts in
// 512-byte units of the image it wraps, so a 2048-byte CD sector spans four of
// them and the first is the one a read of that sector starts on.
func blkdebugConfig(lba int64) string {
	return fmt.Sprintf(`[inject-error]
event = "read_aio"
sector = "%d"
errno = "5"
immediately = "off"
once = "off"
`, lba*(testiso.SectorSize/512))
}
