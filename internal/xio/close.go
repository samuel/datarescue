// Package xio holds small I/O helpers shared across the datarescue packages.
package xio

import "io"

// CloseAfter closes c and returns err, or the close error when err is nil. It
// is for the common write-then-close sequence where the close error matters
// only if the write succeeded.
func CloseAfter(c io.Closer, err error) error {
	cerr := c.Close()
	if err != nil {
		return err
	}
	return cerr
}
