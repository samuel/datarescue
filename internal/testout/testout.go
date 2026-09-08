// Package testout captures what a command wrote to stdout, so each tool's
// tests can assert on a listing or a report without carrying their own copy of
// the pipe plumbing.
package testout

import (
	"bytes"
	"io"
	"os"
	"testing"
)

// CaptureStdout runs fn with os.Stdout redirected and returns what it wrote.
// The commands here print through the package-level os.Stdout, so this replaces
// it for the duration; tests using it cannot run in parallel with each other.
func CaptureStdout(t testing.TB, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()
	fn()
	w.Close() //nolint:errcheck // closing the write end is what ends the copy
	os.Stdout = orig
	out := <-done
	r.Close() //nolint:errcheck // the read end is drained
	return out
}
