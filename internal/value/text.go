package value

import (
	"encoding/hex"
	"errors"
)

// AppendISO appends v, a date, a time of day or a timestamp, to dst as ISO
// 8601 text, in the form the JSON encoding uses: "2026-10-19",
// "13:45:00.25+05:30", "2026-10-19T13:45:00Z". sep goes between the date and
// the time of a timestamp: 'T' as ISO 8601 writes it, or ' ' as SQL usually
// does. It reports false for every other value, including infinities and
// zero dates, which have no ISO 8601 form.
func AppendISO(dst []byte, v Value, sep byte) ([]byte, bool) {
	if v.special != Normal {
		return dst, false
	}
	switch v.kind {
	case KindDate:
		return appendDate(dst, v.ts), true
	case KindTime:
		return appendTimeOfDay(dst, v), true
	case KindTimestamp:
		return appendTimestamp(dst, v, sep), true
	default:
		return dst, false
	}
}

// ParseISO parses text in the form AppendISO writes, as a value of kind k:
// KindDate, KindTime or KindTimestamp. A time or timestamp with a UTC offset,
// or a timestamp ending in "Z", is time-zone aware. The error never contains s.
func ParseISO(k Kind, s string, sep byte) (Value, error) {
	switch k {
	case KindDate:
		if year, month, day, rest, ok := parseDate(s); ok && rest == "" {
			return Date(year, month, day)
		}
		return Value{}, errors.New("value: invalid date encoding")
	case KindTime:
		return parseTimeOfDay(s)
	case KindTimestamp:
		return parseTimestamp(s, sep)
	default:
		return Value{}, errors.New("value: ISO 8601 text is only for dates, times and timestamps")
	}
}

// FormatUUID returns u in its usual form, such as
// "123e4567-e89b-12d3-a456-426614174000".
func FormatUUID(u [16]byte) string {
	var buf [36]byte
	hex.Encode(buf[0:8], u[0:4])
	hex.Encode(buf[9:13], u[4:6])
	hex.Encode(buf[14:18], u[6:8])
	hex.Encode(buf[19:23], u[8:10])
	hex.Encode(buf[24:36], u[10:16])
	buf[8], buf[13], buf[18], buf[23] = '-', '-', '-', '-'
	return string(buf[:])
}
