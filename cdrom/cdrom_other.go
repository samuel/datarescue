//go:build !linux && !darwin

package cdrom

func Open(_ string) (Device, error) {
	return nil, ErrUnsupportedPlatform
}
