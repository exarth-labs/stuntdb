package value

import (
	"errors"
	"strconv"
	"time"
)

func validDate(year int, month time.Month, day int) bool {
	if month < time.January || month > time.December || day < 1 {
		return false
	}
	t := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
	return t.Year() == year && t.Month() == month && t.Day() == day
}

// appendDate appends t's date in ISO 8601 form. Years outside 0–9999 get a
// sign and at least four digits: "-0043-03-15", "+12026-01-01".
func appendDate(dst []byte, t time.Time) []byte {
	year, month, day := t.Date()
	switch {
	case year < 0:
		dst = append(dst, '-')
		year = -year
	case year > 9999:
		dst = append(dst, '+')
	}
	digits := strconv.Itoa(year)
	for i := len(digits); i < 4; i++ {
		dst = append(dst, '0')
	}
	dst = append(dst, digits...)
	dst = append(dst, '-')
	dst = append2(dst, int(month))
	dst = append(dst, '-')
	return append2(dst, day)
}

// appendClock appends a time of day, given in nanoseconds since midnight, as
// "HH:MM:SS" plus a fraction without trailing zeros.
func appendClock(dst []byte, nanos int64) []byte {
	hours := nanos / int64(time.Hour)
	nanos -= hours * int64(time.Hour)
	minutes := nanos / int64(time.Minute)
	nanos -= minutes * int64(time.Minute)
	seconds := nanos / int64(time.Second)
	nanos -= seconds * int64(time.Second)

	dst = append2(dst, int(hours))
	dst = append(dst, ':')
	dst = append2(dst, int(minutes))
	dst = append(dst, ':')
	dst = append2(dst, int(seconds))
	if nanos > 0 {
		frac := strconv.FormatInt(nanos+1e9, 10)[1:] // nine digits, keeping leading zeros
		for frac[len(frac)-1] == '0' {
			frac = frac[:len(frac)-1]
		}
		dst = append(dst, '.')
		dst = append(dst, frac...)
	}
	return dst
}

// appendZone appends a UTC offset as "+HH:MM", or "+HH:MM:SS" when it has seconds.
func appendZone(dst []byte, offset int) []byte {
	sign := byte('+')
	if offset < 0 {
		sign = '-'
		offset = -offset
	}
	dst = append(dst, sign)
	dst = append2(dst, offset/3600)
	dst = append(dst, ':')
	dst = append2(dst, offset/60%60)
	if s := offset % 60; s != 0 {
		dst = append(dst, ':')
		dst = append2(dst, s)
	}
	return dst
}

// appendTimeOfDay appends a time of day, with its UTC offset when it has one.
func appendTimeOfDay(dst []byte, v Value) []byte {
	dst = appendClock(dst, int64(v.num))
	if v.hasZone {
		dst = appendZone(dst, int(v.zone))
	}
	return dst
}

// appendTimestamp appends a timestamp, with sep between the date and the time.
func appendTimestamp(dst []byte, v Value, sep byte) []byte {
	t := v.Timestamp()
	dst = appendDate(dst, t)
	dst = append(dst, sep)
	hour, minute, second := t.Clock()
	nanos := int64(hour)*int64(time.Hour) + int64(minute)*int64(time.Minute) +
		int64(second)*int64(time.Second) + int64(t.Nanosecond())
	dst = appendClock(dst, nanos)
	switch {
	case v.hasZone:
		dst = appendZone(dst, int(v.zone))
	case v.tz:
		dst = append(dst, 'Z')
	}
	return dst
}

func append2(dst []byte, n int) []byte {
	return append(dst, byte('0'+n/10), byte('0'+n%10))
}

// parseDate parses the date at the start of s and returns the rest of s.
func parseDate(s string) (year int, month time.Month, day int, rest string, ok bool) {
	i, negative := 0, false
	if s != "" && (s[0] == '+' || s[0] == '-') {
		negative, i = s[0] == '-', 1
	}
	j := i
	for j < len(s) && isDigit(s[j]) {
		j++
	}
	if n := j - i; n < 4 || (i == 0 && n != 4) {
		return 0, 0, 0, "", false
	}
	year, err := strconv.Atoi(s[i:j])
	if err != nil {
		return 0, 0, 0, "", false
	}
	if negative {
		year = -year
	}
	if len(s) < j+6 || s[j] != '-' || s[j+3] != '-' {
		return 0, 0, 0, "", false
	}
	m, ok1 := atoi2(s[j+1 : j+3])
	d, ok2 := atoi2(s[j+4 : j+6])
	if !ok1 || !ok2 || !validDate(year, time.Month(m), d) {
		return 0, 0, 0, "", false
	}
	return year, time.Month(m), d, s[j+6:], true
}

// parseClock parses "HH:MM:SS" with an optional fraction of up to nine
// digits at the start of s, and returns nanoseconds since midnight and the
// rest of s. 24:00:00 is the only time allowed past 23:59:59.
func parseClock(s string) (nanos int64, rest string, ok bool) {
	if len(s) < 8 || s[2] != ':' || s[5] != ':' {
		return 0, "", false
	}
	h, ok1 := atoi2(s[0:2])
	m, ok2 := atoi2(s[3:5])
	sec, ok3 := atoi2(s[6:8])
	if !ok1 || !ok2 || !ok3 || h > 24 || m > 59 || sec > 59 {
		return 0, "", false
	}
	nanos = int64(h)*int64(time.Hour) + int64(m)*int64(time.Minute) + int64(sec)*int64(time.Second)
	rest = s[8:]
	if rest != "" && rest[0] == '.' {
		j := 1
		for j < len(rest) && isDigit(rest[j]) {
			j++
		}
		digits := rest[1:j]
		if digits == "" || len(digits) > 9 {
			return 0, "", false
		}
		frac, _ := strconv.Atoi(digits)
		for range 9 - len(digits) {
			frac *= 10
		}
		nanos += int64(frac)
		rest = rest[j:]
	}
	if nanos > nanosPerDay {
		return 0, "", false
	}
	return nanos, rest, true
}

// parseZone parses a whole UTC offset, "+HH:MM" or "+HH:MM:SS".
func parseZone(s string) (offset int, ok bool) {
	if (len(s) != 6 && len(s) != 9) || (s[0] != '+' && s[0] != '-') || s[3] != ':' {
		return 0, false
	}
	h, ok1 := atoi2(s[1:3])
	m, ok2 := atoi2(s[4:6])
	sec, ok3 := 0, true
	if len(s) == 9 {
		if s[6] != ':' {
			return 0, false
		}
		sec, ok3 = atoi2(s[7:9])
	}
	if !ok1 || !ok2 || !ok3 || m > 59 || sec > 59 {
		return 0, false
	}
	offset = h*3600 + m*60 + sec
	if s[0] == '-' {
		offset = -offset
	}
	return offset, offset >= -maxZone && offset <= maxZone
}

func parseTimeOfDay(s string) (Value, error) {
	nanos, rest, ok := parseClock(s)
	if !ok {
		return Value{}, errors.New("value: invalid time encoding")
	}
	if rest == "" {
		return TimeOfDay(time.Duration(nanos))
	}
	offset, ok := parseZone(rest)
	if !ok {
		return Value{}, errors.New("value: invalid time encoding")
	}
	return TimeOfDayWithZone(time.Duration(nanos), offset)
}

// parseTimestamp parses a timestamp with sep between the date and the time.
func parseTimestamp(s string, sep byte) (Value, error) {
	errInvalid := errors.New("value: invalid timestamp encoding")
	year, month, day, rest, ok := parseDate(s)
	if !ok || rest == "" || rest[0] != sep {
		return Value{}, errInvalid
	}
	nanos, rest, ok := parseClock(rest[1:])
	if !ok || nanos == nanosPerDay {
		return Value{}, errInvalid
	}
	switch rest {
	case "":
		return Timestamp(time.Date(year, month, day, 0, 0, 0, int(nanos), time.UTC)), nil
	case "Z":
		return TimestampTZ(time.Date(year, month, day, 0, 0, 0, int(nanos), time.UTC)), nil
	}
	offset, ok := parseZone(rest)
	if !ok {
		return Value{}, errInvalid
	}
	return TimestampTZWithZone(time.Date(year, month, day, 0, 0, 0, int(nanos), time.FixedZone("", offset))), nil
}

func atoi2(s string) (int, bool) {
	if len(s) != 2 || !isDigit(s[0]) || !isDigit(s[1]) {
		return 0, false
	}
	return int(s[0]-'0')*10 + int(s[1]-'0'), true
}
