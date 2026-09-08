package xio

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrExists reports that the output path is already present and force was not given.
var ErrExists = errors.New("output file already exists")

// Create makes a file for writing, refusing to truncate an existing one unless
// force is set. Parent directories are created as needed.
func Create(path string, force bool) (*os.File, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("creating directory %s: %w", dir, err)
		}
	}
	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if !force {
		flags = os.O_WRONLY | os.O_CREATE | os.O_EXCL
	}
	f, err := os.OpenFile(path, flags, 0o644) //nolint:gosec // the output path is the tool's argument
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("%s: %w (use -f to overwrite)", path, ErrExists)
		}
		return nil, err
	}
	return f, nil
}

// EnsureDir creates dir if needed and verifies it is a usable directory.
func EnsureDir(dir string) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("creating output directory %s: %w", dir, err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("output directory %s: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("output path %s is not a directory", dir)
	}
	return nil
}
