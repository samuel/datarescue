package xio

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCreateRefusesExisting(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "image.iso")
	if err := os.WriteFile(path, []byte("a previous rip"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Create(path, false); !errors.Is(err, ErrExists) {
		t.Fatalf("Create over an existing file = %v; want ErrExists", err)
	}
	// The refusal must leave the original untouched.
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "a previous rip" {
		t.Errorf("the existing file was modified: %q", got)
	}

	f, err := Create(path, true)
	if err != nil {
		t.Fatalf("Create with force = %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(path)
	if len(got) != 0 {
		t.Errorf("force did not truncate: %q", got)
	}
}

func TestCreateMakesParents(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "a", "b", "c.txt")
	f, err := Create(path, false)
	if err != nil {
		t.Fatalf("Create = %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("file not created: %v", err)
	}
}

func TestEnsureDir(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "out", "nested")
	if err := EnsureDir(dir); err != nil {
		t.Fatalf("EnsureDir = %v", err)
	}
	if err := EnsureDir(dir); err != nil {
		t.Errorf("EnsureDir on an existing directory = %v", err)
	}

	// A path that exists as a file is not a usable output directory.
	file := filepath.Join(t.TempDir(), "notadir")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EnsureDir(file); err == nil {
		t.Error("EnsureDir on a regular file = nil; want an error")
	}
}

type closeErrWriter struct{ err error }

func (c closeErrWriter) Close() error { return c.err }

func TestCloseAfter(t *testing.T) {
	t.Parallel()
	writeErr := errors.New("write failed")
	closeErr := errors.New("close failed")

	// A write failure wins: it explains what went wrong first.
	if got := CloseAfter(closeErrWriter{closeErr}, writeErr); !errors.Is(got, writeErr) {
		t.Errorf("CloseAfter(closeErr, writeErr) = %v; want the write error", got)
	}
	if got := CloseAfter(closeErrWriter{closeErr}, nil); !errors.Is(got, closeErr) {
		t.Errorf("CloseAfter(closeErr, nil) = %v; want the close error", got)
	}
	if got := CloseAfter(closeErrWriter{nil}, nil); got != nil {
		t.Errorf("CloseAfter(nil, nil) = %v; want nil", got)
	}
}
