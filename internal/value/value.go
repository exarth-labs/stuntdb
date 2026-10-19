package value

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"slices"
	"time"
)

// Kind is the kind of data a Value holds.
type Kind uint8

const (
	KindNull      Kind = iota // SQL NULL
	KindBool                  // true or false
	KindInt                   // a signed 64-bit integer
	KindUint                  // an unsigned 64-bit integer
	KindFloat                 // a 64-bit floating-point number, including NaN and ±Inf
	KindDecimal               // an exact decimal number, kept as text
	KindString                // text, for String, Text, Enum and JSON columns
	KindBytes                 // raw bytes, for Binary and Opaque columns
	KindUUID                  // a 128-bit UUID
	KindDate                  // a calendar date
	KindTime                  // a time of day
	KindTimestamp             // a date and a time of day
	KindInterval              // a duration in months, days and nanoseconds
	KindArray                 // a list of values
)

var kindNames = [...]string{
	KindNull:      "null",
	KindBool:      "bool",
	KindInt:       "int",
	KindUint:      "uint",
	KindFloat:     "float",
	KindDecimal:   "decimal",
	KindString:    "string",
	KindBytes:     "bytes",
	KindUUID:      "uuid",
	KindDate:      "date",
	KindTime:      "time",
	KindTimestamp: "timestamp",
	KindInterval:  "interval",
	KindArray:     "array",
}

func (k Kind) String() string {
	if int(k) < len(kindNames) {
		return kindNames[k]
	}
	return "invalid"
}

// kindByName returns the non-null kind with the given name.
func kindByName(name string) (Kind, bool) {
	for k, n := range kindNames {
		if n == name && Kind(k) != KindNull {
			return Kind(k), true
		}
	}
	return 0, false
}

// Special marks values outside the normal range that some engines allow.
type Special uint8

const (
	Normal      Special = iota
	NaN                 // a decimal "not a number" (floats use math.NaN instead)
	PosInfinity         // a decimal, date or timestamp "infinity"
	NegInfinity         // a decimal, date or timestamp "-infinity"
	ZeroDate            // a MySQL zero date such as 0000-00-00, kept as its original text
)

// Value is one column value. The zero Value is NULL.
//
// Values are small and immutable; copy them freely. Use the constructors to
// make one and the accessors to read it. The accessors panic when called on
// a value of a different kind, as reflect does.
type Value struct {
	kind    Kind
	special Special
	tz      bool      // Time, Timestamp: time-zone aware
	hasZone bool      // Time, Timestamp: the original UTC offset is known
	zone    int32     // the UTC offset in seconds, when hasZone
	months  int32     // Interval
	days    int32     // Interval
	num     uint64    // Bool (0 or 1), Int, Uint, Float bits, Time and Interval nanoseconds
	str     string    // Decimal, String, Bytes, UUID (16 bytes), and the text of a zero date
	ts      time.Time // Date and Timestamp, always in UTC
	elems   []Value   // Array
}

const (
	nanosPerDay = 24 * int64(time.Hour)
	maxZone     = 24*60*60 - 1 // a UTC offset must stay under 24 hours
)

// Null returns SQL NULL.
func Null() Value { return Value{} }

// Bool returns a boolean value.
func Bool(b bool) Value {
	v := Value{kind: KindBool}
	if b {
		v.num = 1
	}
	return v
}

// Int returns a signed integer value.
func Int(i int64) Value { return Value{kind: KindInt, num: uint64(i)} }

// Uint returns an unsigned integer value.
func Uint(u uint64) Value { return Value{kind: KindUint, num: u} }

// Float returns a floating-point value. NaN and ±Inf are allowed.
func Float(f float64) Value { return Value{kind: KindFloat, num: math.Float64bits(f)} }

// Decimal returns an exact decimal value from its text: an optional sign,
// then digits with an optional decimal point, such as "-123.4500".
// Exponents are not accepted. The text is kept exactly, including trailing zeros.
func Decimal(text string) (Value, error) {
	if !validDecimal(text) {
		return Value{}, errors.New("value: not a plain decimal number")
	}
	return Value{kind: KindDecimal, str: text}, nil
}

// DecimalNaN returns a decimal "not a number", as PostgreSQL's numeric allows.
func DecimalNaN() Value { return Value{kind: KindDecimal, special: NaN} }

// DecimalInfinity returns decimal infinity, or minus infinity if negative is true.
func DecimalInfinity(negative bool) Value {
	return Value{kind: KindDecimal, special: infinity(negative)}
}

// String returns a text value. The text may contain any bytes.
func String(s string) Value { return Value{kind: KindString, str: s} }

// Bytes returns a binary value. It copies b.
func Bytes(b []byte) Value { return Value{kind: KindBytes, str: string(b)} }

// UUID returns a UUID value.
func UUID(u [16]byte) Value { return Value{kind: KindUUID, str: string(u[:])} }

// ParseUUID parses a UUID written as 32 hexadecimal digits, with or without
// the usual hyphens.
func ParseUUID(s string) (Value, error) {
	errInvalid := errors.New("value: not a valid UUID")
	digits := s
	if len(s) == 36 {
		if s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
			return Value{}, errInvalid
		}
		digits = s[0:8] + s[9:13] + s[14:18] + s[19:23] + s[24:36]
	}
	var u [16]byte
	if len(digits) != 32 {
		return Value{}, errInvalid
	}
	if _, err := hex.Decode(u[:], []byte(digits)); err != nil {
		return Value{}, errInvalid
	}
	return UUID(u), nil
}

// Date returns a calendar date. Years use astronomical numbering, so year 0
// is 1 BC and year -43 is 44 BC.
func Date(year int, month time.Month, day int) (Value, error) {
	if !validDate(year, month, day) {
		return Value{}, errors.New("value: not a valid date")
	}
	return Value{kind: KindDate, ts: time.Date(year, month, day, 0, 0, 0, 0, time.UTC)}, nil
}

// DateInfinity returns date infinity, or minus infinity if negative is true.
func DateInfinity(negative bool) Value {
	return Value{kind: KindDate, special: infinity(negative)}
}

// MySQLZeroDate returns a MySQL zero date, such as "0000-00-00" or "2026-00-15",
// keeping its original text.
func MySQLZeroDate(original string) Value {
	return Value{kind: KindDate, special: ZeroDate, str: original}
}

// TimeOfDay returns a time of day without a time zone. d must be between 0
// and 24 hours; exactly 24:00:00 is allowed, as PostgreSQL allows it.
func TimeOfDay(d time.Duration) (Value, error) {
	if d < 0 || int64(d) > nanosPerDay {
		return Value{}, errors.New("value: a time of day must be between 00:00:00 and 24:00:00")
	}
	return Value{kind: KindTime, num: uint64(d)}, nil
}

// TimeOfDayWithZone returns a time of day with a UTC offset in seconds.
func TimeOfDayWithZone(d time.Duration, offsetSeconds int) (Value, error) {
	v, err := TimeOfDay(d)
	if err != nil {
		return Value{}, err
	}
	if offsetSeconds < -maxZone || offsetSeconds > maxZone {
		return Value{}, errors.New("value: a UTC offset must be under 24 hours")
	}
	v.tz, v.hasZone, v.zone = true, true, int32(offsetSeconds)
	return v, nil
}

// Timestamp returns a date and time without a time zone: the wall-clock
// reading of t, whatever t's location is.
func Timestamp(t time.Time) Value {
	wall := time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC)
	return Value{kind: KindTimestamp, ts: wall}
}

// TimestampTZ returns a time-zone-aware timestamp: the instant t, without
// remembering t's offset. PostgreSQL's timestamptz works this way.
func TimestampTZ(t time.Time) Value {
	return Value{kind: KindTimestamp, tz: true, ts: t.UTC()}
}

// TimestampTZWithZone returns the instant t together with t's UTC offset.
// SQL Server's datetimeoffset and Oracle's TIMESTAMP WITH TIME ZONE work this way.
func TimestampTZWithZone(t time.Time) Value {
	_, offset := t.Zone()
	return Value{kind: KindTimestamp, tz: true, hasZone: true, zone: int32(offset), ts: t.UTC()}
}

// TimestampInfinity returns timestamp infinity, or minus infinity if negative
// is true. tz says whether it belongs to a time-zone-aware column.
func TimestampInfinity(negative, tz bool) Value {
	return Value{kind: KindTimestamp, special: infinity(negative), tz: tz}
}

// MySQLZeroTimestamp returns a MySQL zero date-time, such as
// "0000-00-00 00:00:00", keeping its original text.
func MySQLZeroTimestamp(original string, tz bool) Value {
	return Value{kind: KindTimestamp, special: ZeroDate, str: original, tz: tz}
}

// Interval returns a duration in months, days and nanoseconds. The parts are
// kept separately, because a month has no fixed number of days.
func Interval(months, days int32, nanos int64) Value {
	return Value{kind: KindInterval, months: months, days: days, num: uint64(nanos)}
}

// Array returns a list of values. It copies elems.
func Array(elems []Value) Value { return Value{kind: KindArray, elems: slices.Clone(elems)} }

func infinity(negative bool) Special {
	if negative {
		return NegInfinity
	}
	return PosInfinity
}

// Kind returns the kind of data v holds.
func (v Value) Kind() Kind { return v.kind }

// IsNull reports whether v is SQL NULL.
func (v Value) IsNull() bool { return v.kind == KindNull }

// Special reports whether v is a special value, such as infinity.
func (v Value) Special() Special { return v.special }

// Bool returns the value of a bool.
func (v Value) Bool() bool {
	v.mustBe(KindBool, "Bool")
	return v.num == 1
}

// Int returns the value of a signed integer.
func (v Value) Int() int64 {
	v.mustBe(KindInt, "Int")
	return int64(v.num)
}

// Uint returns the value of an unsigned integer.
func (v Value) Uint() uint64 {
	v.mustBe(KindUint, "Uint")
	return v.num
}

// Float returns the value of a float.
func (v Value) Float() float64 {
	v.mustBe(KindFloat, "Float")
	return math.Float64frombits(v.num)
}

// DecimalText returns the exact text of a decimal, or "" for a special value.
func (v Value) DecimalText() string {
	v.mustBe(KindDecimal, "DecimalText")
	return v.str
}

// Text returns the content of a string value.
func (v Value) Text() string {
	v.mustBe(KindString, "Text")
	return v.str
}

// Bytes returns a copy of the content of a bytes value.
func (v Value) Bytes() []byte {
	v.mustBe(KindBytes, "Bytes")
	return []byte(v.str)
}

// UUID returns the value of a UUID.
func (v Value) UUID() [16]byte {
	v.mustBe(KindUUID, "UUID")
	var u [16]byte
	copy(u[:], v.str)
	return u
}

// Date returns the parts of a date, or zeros for a special value.
func (v Value) Date() (year int, month time.Month, day int) {
	v.mustBe(KindDate, "Date")
	if v.special != Normal {
		return 0, 0, 0
	}
	return v.ts.Date()
}

// TimeOfDay returns a time of day as the time since midnight.
func (v Value) TimeOfDay() time.Duration {
	v.mustBe(KindTime, "TimeOfDay")
	return time.Duration(int64(v.num))
}

// Timestamp returns a timestamp. Without a time zone, it is the wall-clock
// reading in UTC. With one, it is the instant, in its original offset when
// that is known and in UTC otherwise. Special values return the zero time.
func (v Value) Timestamp() time.Time {
	v.mustBe(KindTimestamp, "Timestamp")
	if v.hasZone {
		return v.ts.In(time.FixedZone("", int(v.zone)))
	}
	return v.ts
}

// IsTZ reports whether a time or timestamp is time-zone aware.
func (v Value) IsTZ() bool {
	v.mustBeTemporal("IsTZ")
	return v.tz
}

// Zone returns the original UTC offset of a time or timestamp, in seconds,
// and whether it is known.
func (v Value) Zone() (offsetSeconds int, known bool) {
	v.mustBeTemporal("Zone")
	return int(v.zone), v.hasZone
}

// ZeroText returns the original text of a MySQL zero date, or "" otherwise.
func (v Value) ZeroText() string {
	if v.special != ZeroDate {
		return ""
	}
	return v.str
}

// Interval returns the parts of an interval.
func (v Value) Interval() (months, days int32, nanos int64) {
	v.mustBe(KindInterval, "Interval")
	return v.months, v.days, int64(v.num)
}

// Elems returns a copy of the elements of an array.
func (v Value) Elems() []Value {
	v.mustBe(KindArray, "Elems")
	return slices.Clone(v.elems)
}

// Equal reports whether v and w are exactly the same value. Unlike SQL,
// NaN equals NaN, and -0 differs from +0: equal values encode identically.
func (v Value) Equal(w Value) bool {
	if v.kind != w.kind || v.special != w.special || v.tz != w.tz || v.hasZone != w.hasZone ||
		v.zone != w.zone || v.months != w.months || v.days != w.days || v.num != w.num ||
		v.str != w.str || !v.ts.Equal(w.ts) || len(v.elems) != len(w.elems) {
		return false
	}
	for i := range v.elems {
		if !v.elems[i].Equal(w.elems[i]) {
			return false
		}
	}
	return true
}

// String describes v without revealing its contents, such as "<string value>".
func (v Value) String() string {
	if v.kind == KindNull {
		return "NULL"
	}
	return "<" + v.kind.String() + " value>"
}

// Format makes every fmt verb (%v, %d, %x, %#v, …) print only what String
// prints, so a value cannot leak into a log or error message.
func (v Value) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, v.String()) }

// LogValue makes log/slog print only what String prints.
func (v Value) LogValue() slog.Value { return slog.StringValue(v.String()) }

func (v Value) mustBe(k Kind, method string) {
	if v.kind != k {
		panic("value: " + method + " called on a " + v.kind.String() + " value")
	}
}

func (v Value) mustBeTemporal(method string) {
	if v.kind != KindTime && v.kind != KindTimestamp {
		panic("value: " + method + " called on a " + v.kind.String() + " value")
	}
}
