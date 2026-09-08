package iso

import (
	"errors"
	"fmt"
	"time"
)

// recordTimeSize is the length of the 7-byte date in a directory record and
// in a Rock Ridge TF entry's short form.
const recordTimeSize = 7

// volumeTimeSize is the length of the 17-byte date in a volume descriptor and
// in a Rock Ridge TF entry's long form.
const volumeTimeSize = 17

var errBadTime = errors.New("date out of range")

// zone returns the location for a GMT offset in 15-minute units, which ISO
// 9660 bounds to -48 (GMT-12) through 52 (GMT+13).
func zone(units int8) (*time.Location, error) {
	if units < -48 || units > 52 {
		return nil, fmt.Errorf("%w: GMT offset %d", errBadTime, units)
	}
	if units == 0 {
		return time.UTC, nil
	}
	return time.FixedZone("", int(units)*15*60), nil
}

// parseRecordTime decodes the 7-byte form: years since 1900, month, day,
// hour, minute, second and GMT offset. All zeros means no date was recorded
// and returns the zero time. A field out of range is damage and returns an
// error with the zero time.
func parseRecordTime(b []byte) (time.Time, error) {
	if len(b) < recordTimeSize {
		return time.Time{}, fmt.Errorf("%w: %d bytes", errBadTime, len(b))
	}
	b = b[:recordTimeSize]
	if allBytes(b, 0) {
		return time.Time{}, nil
	}
	loc, err := zone(int8(b[6])) //nolint:gosec // the offset is a signed byte on disc
	if err != nil {
		return time.Time{}, err
	}
	return buildTime(1900+int(b[0]), int(b[1]), int(b[2]), int(b[3]), int(b[4]), int(b[5]), 0, loc)
}

// parseVolumeTime decodes the 17-byte form: "YYYYMMDDHHMMSScc" in ASCII
// digits, then a GMT offset. Discs mark an unset date with either all zero
// bytes or all '0' digits, and both return the zero time. Anything else that
// does not decode is damage.
func parseVolumeTime(b []byte) (time.Time, error) {
	if len(b) < volumeTimeSize {
		return time.Time{}, fmt.Errorf("%w: %d bytes", errBadTime, len(b))
	}
	b = b[:volumeTimeSize]
	if allBytes(b, 0) || allBytes(b[:16], '0') {
		return time.Time{}, nil
	}
	var n [16]int
	for i, c := range b[:16] {
		if c < '0' || c > '9' {
			return time.Time{}, fmt.Errorf("%w: %q is not a date", errBadTime, b[:16])
		}
		n[i] = int(c - '0')
	}
	num := func(i, width int) int {
		v := 0
		for _, d := range n[i : i+width] {
			v = v*10 + d
		}
		return v
	}
	loc, err := zone(int8(b[16])) //nolint:gosec // the offset is a signed byte on disc
	if err != nil {
		return time.Time{}, err
	}
	year := num(0, 4)
	if year == 0 {
		return time.Time{}, fmt.Errorf("%w: year 0", errBadTime)
	}
	return buildTime(year, num(4, 2), num(6, 2), num(8, 2), num(10, 2), num(12, 2), num(14, 2), loc)
}

// buildTime checks every field before calling time.Date, which would
// otherwise normalize a damaged month 13 into January of the next year.
func buildTime(year, month, day, hour, minute, sec, centi int, loc *time.Location) (time.Time, error) {
	if month < 1 || month > 12 || day < 1 || hour > 23 || minute > 59 || sec > 59 || centi > 99 {
		return time.Time{}, fmt.Errorf("%w: %04d-%02d-%02d %02d:%02d:%02d", errBadTime, year, month, day, hour, minute, sec)
	}
	// Day 0 of the next month is the last day of this one.
	if last := time.Date(year, time.Month(month)+1, 0, 0, 0, 0, 0, time.UTC).Day(); day > last {
		return time.Time{}, fmt.Errorf("%w: %04d-%02d-%02d", errBadTime, year, month, day)
	}
	return time.Date(year, time.Month(month), day, hour, minute, sec, centi*int(10*time.Millisecond), loc), nil
}

func allBytes(b []byte, c byte) bool {
	for _, x := range b {
		if x != c {
			return false
		}
	}
	return true
}
