//go:build darwin || linux

package cdrom

import (
	"errors"
	"time"

	"golang.org/x/sys/unix"
)

// retryable reports whether another attempt at err could plausibly succeed.
// Errors that describe the request rather than the medium — a rejected ioctl,
// a closed or inaccessible device — fail identically every time.
func retryable(err error) bool {
	switch {
	case errors.Is(err, unix.EINVAL), errors.Is(err, unix.ENOTTY),
		errors.Is(err, unix.EBADF), errors.Is(err, unix.EPERM),
		errors.Is(err, unix.EACCES), errors.Is(err, unix.ENXIO),
		errors.Is(err, unix.EOPNOTSUPP):
		return false
	}
	return true
}

// retryWait pauses before attempt n (zero-based). Retrying a marginal sector
// immediately gives the drive no chance to reseek or settle.
func retryWait(n int) {
	if n > 0 {
		time.Sleep(retryDelay)
	}
}
