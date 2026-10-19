package value

import (
	"math"
	"testing"
	"time"
)

var (
	boolT     = Type{Kind: TypeBool}
	int8T     = Type{Kind: TypeInt, Bits: 8}
	int64T    = Type{Kind: TypeInt, Bits: 64}
	uint8T    = Type{Kind: TypeInt, Bits: 8, Unsigned: true}
	uint64T   = Type{Kind: TypeInt, Bits: 64, Unsigned: true}
	float32T  = Type{Kind: TypeFloat, Bits: 32}
	float64T  = Type{Kind: TypeFloat, Bits: 64}
	decimalT  = Type{Kind: TypeDecimal}
	string3T  = Type{Kind: TypeString, MaxLen: 3}
	textT     = Type{Kind: TypeText}
	binaryT   = Type{Kind: TypeBinary}
	dateT     = Type{Kind: TypeDate}
	timeT     = Type{Kind: TypeTime, Precision: 9}
	timeTZT   = Type{Kind: TypeTime, Precision: 9, TZ: true}
	tsT       = Type{Kind: TypeTimestamp, Precision: 9}
	tsTZT     = Type{Kind: TypeTimestamp, Precision: 9, TZ: true}
	intervalT = Type{Kind: TypeInterval}
	uuidT     = Type{Kind: TypeUUID}
	jsonT     = Type{Kind: TypeJSON}
	enumT     = Type{Kind: TypeEnum, Values: []string{"active", "banned"}}
	arrayT    = Type{Kind: TypeArray, Elem: &int64T}
	nestedT   = Type{Kind: TypeArray, Elem: &arrayT}
	opaqueT   = Type{Kind: TypeOpaque, Native: "geometry"}
)

func mustDecimal(t *testing.T, s string) Value {
	t.Helper()
	v, err := Decimal(s)
	if err != nil {
		t.Fatalf("Decimal(%q): %v", s, err)
	}
	return v
}

func mustDate(t *testing.T, year int, month time.Month, day int) Value {
	t.Helper()
	v, err := Date(year, month, day)
	if err != nil {
		t.Fatalf("Date(%d, %d, %d): %v", year, month, day, err)
	}
	return v
}

func mustTime(t *testing.T, d time.Duration) Value {
	t.Helper()
	v, err := TimeOfDay(d)
	if err != nil {
		t.Fatalf("TimeOfDay(%v): %v", d, err)
	}
	return v
}

func mustTimeZone(t *testing.T, d time.Duration, offset int) Value {
	t.Helper()
	v, err := TimeOfDayWithZone(d, offset)
	if err != nil {
		t.Fatalf("TimeOfDayWithZone(%v, %d): %v", d, offset, err)
	}
	return v
}

func mustUUID(t *testing.T, s string) Value {
	t.Helper()
	v, err := ParseUUID(s)
	if err != nil {
		t.Fatalf("ParseUUID(%q): %v", s, err)
	}
	return v
}

func TestJSONRoundTrip(t *testing.T) {
	const (
		h = time.Hour
		m = time.Minute
	)
	tests := []struct {
		name string
		typ  Type
		val  Value
		want string
	}{
		{"null", textT, Null(), `null`},
		{"true", boolT, Bool(true), `true`},
		{"false", boolT, Bool(false), `false`},
		{"int64 min", int64T, Int(math.MinInt64), `-9223372036854775808`},
		{"int64 max", int64T, Int(math.MaxInt64), `9223372036854775807`},
		{"uint64 max", uint64T, Uint(math.MaxUint64), `18446744073709551615`},
		{"float", float64T, Float(1.5), `1.5`},
		{"float negative zero", float64T, Float(math.Copysign(0, -1)), `-0`},
		{"float smallest", float64T, Float(math.SmallestNonzeroFloat64), `5e-324`},
		{"float largest", float64T, Float(math.MaxFloat64), `1.7976931348623157e+308`},
		{"float NaN", float64T, Float(math.NaN()), `{"$":"nan"}`},
		{"float +Inf", float64T, Float(math.Inf(1)), `{"$":"+inf"}`},
		{"float -Inf", float64T, Float(math.Inf(-1)), `{"$":"-inf"}`},
		{
			"huge decimal stays exact", decimalT,
			mustDecimal(t, "-123456789012345678901234567890.123456789012345678901234567890"),
			`"-123456789012345678901234567890.123456789012345678901234567890"`,
		},
		{"decimal keeps trailing zeros", decimalT, mustDecimal(t, "1.2300"), `"1.2300"`},
		{"decimal NaN", decimalT, DecimalNaN(), `{"$":"nan"}`},
		{"decimal -infinity", decimalT, DecimalInfinity(true), `{"$":"-inf"}`},
		{"empty string is not NULL", textT, String(""), `""`},
		{"unicode", textT, String("Zoë 😀 ✓"), `"Zoë 😀 ✓"`},
		{"quotes and control characters", textT, String("a\"b\\c\n\x00"), `"a\"b\\c\u000a\u0000"`},
		{"invalid UTF-8 kept byte for byte", textT, String("\xff\xfeabc"), `{"$":"raw","v":"//5hYmM="}`},
		{"enum", enumT, String("active"), `"active"`},
		{"json text kept exactly", jsonT, String(`{"b":1, "a":[2]}`), `"{\"b\":1, \"a\":[2]}"`},
		{"binary", binaryT, Bytes([]byte{0, 1, 255}), `"AAH/"`},
		{"empty binary is not NULL", binaryT, Bytes(nil), `""`},
		{"opaque", opaqueT, Bytes([]byte{1, 2}), `"AQI="`},
		{"uuid", uuidT, mustUUID(t, "123E4567-E89B-12D3-A456-426614174000"), `"123e4567-e89b-12d3-a456-426614174000"`},
		{"date", dateT, mustDate(t, 2026, 10, 19), `"2026-10-19"`},
		{"date year 1", dateT, mustDate(t, 1, 1, 1), `"0001-01-01"`},
		{"date 44 BC", dateT, mustDate(t, -43, 3, 15), `"-0043-03-15"`},
		{"date after year 9999", dateT, mustDate(t, 12026, 1, 1), `"+12026-01-01"`},
		{"date leap day", dateT, mustDate(t, 2024, 2, 29), `"2024-02-29"`},
		{"date +infinity", dateT, DateInfinity(false), `{"$":"+inf"}`},
		{"MySQL zero date", dateT, MySQLZeroDate("0000-00-00"), `{"$":"zero","v":"0000-00-00"}`},
		{"MySQL partial zero date", dateT, MySQLZeroDate("2026-00-15"), `{"$":"zero","v":"2026-00-15"}`},
		{"midnight", timeT, mustTime(t, 0), `"00:00:00"`},
		{"24:00", timeT, mustTime(t, 24*h), `"24:00:00"`},
		{"time with nanoseconds", timeT, mustTime(t, 13*h+45*m+123456789), `"13:45:00.123456789"`},
		{"time with leading-zero fraction", timeT, mustTime(t, 5*time.Millisecond), `"00:00:00.005"`},
		{"time with zone", timeTZT, mustTimeZone(t, 13*h+45*m, 5*3600+30*60), `"13:45:00+05:30"`},
		{"time with a negative zone in seconds", timeTZT, mustTimeZone(t, h, -(3*3600 + 30*60 + 15)), `"01:00:00-03:30:15"`},
		{
			"timestamp without time zone", tsT,
			Timestamp(time.Date(2026, 10, 19, 13, 45, 0, 123456000, time.UTC)),
			`"2026-10-19T13:45:00.123456"`,
		},
		{
			"wall-clock timestamp ignores the location", tsT,
			Timestamp(time.Date(2026, 10, 19, 13, 45, 0, 0, time.FixedZone("", 3600))),
			`"2026-10-19T13:45:00"`,
		},
		{
			"instant", tsTZT,
			TimestampTZ(time.Date(2026, 10, 19, 13, 45, 0, 0, time.FixedZone("", 5*3600))),
			`"2026-10-19T08:45:00Z"`,
		},
		{
			"instant with its original offset", tsTZT,
			TimestampTZWithZone(time.Date(2026, 10, 19, 13, 45, 0, 0, time.FixedZone("", 5*3600+45*60))),
			`"2026-10-19T13:45:00+05:45"`,
		},
		{
			"far-future timestamp", tsT,
			Timestamp(time.Date(294276, 12, 31, 23, 59, 59, 999999999, time.UTC)),
			`"+294276-12-31T23:59:59.999999999"`,
		},
		{"timestamp -infinity", tsT, TimestampInfinity(true, false), `{"$":"-inf"}`},
		{"time-zone-aware +infinity", tsTZT, TimestampInfinity(false, true), `{"$":"+inf","tz":true}`},
		{
			"MySQL zero date-time", tsT,
			MySQLZeroTimestamp("0000-00-00 00:00:00", false),
			`{"$":"zero","v":"0000-00-00 00:00:00"}`,
		},
		{"interval with mixed signs", intervalT, Interval(1, -2, 3000), `{"months":1,"days":-2,"nanos":3000}`},
		{"array with NULL", arrayT, Array([]Value{Int(1), Null(), Int(3)}), `[1,null,3]`},
		{"empty array", arrayT, Array(nil), `[]`},
		{"nested array", nestedT, Array([]Value{Array([]Value{Int(1)}), Array(nil)}), `[[1],[]]`},

		// Values that don't match their column type keep their kind, as
		// happens with SQLite's loose typing.
		{"text in an integer column", int64T, String("abc"), `{"$":"string","v":"abc"}`},
		{"real number in an integer column", int64T, Float(2.5), `{"$":"float","v":2.5}`},
		{"signed value in an unsigned column", uint64T, Int(-1), `{"$":"int","v":-1}`},
		{"decimal NaN in a text column", textT, DecimalNaN(), `{"$":"decimal","v":{"$":"nan"}}`},
		{
			"array in an opaque column tags every element", opaqueT,
			Array([]Value{Int(1), String("x"), Null()}),
			`{"$":"array","v":[{"$":"int","v":1},{"$":"string","v":"x"},null]}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := AppendJSON(nil, tt.typ, tt.val)
			if err != nil {
				t.Fatalf("AppendJSON: %v", err)
			}
			if string(got) != tt.want {
				t.Errorf("AppendJSON =\n  %s\nwant\n  %s", got, tt.want)
			}
			back, err := DecodeJSON(tt.typ, got)
			if err != nil {
				t.Fatalf("DecodeJSON(%s): %v", got, err)
			}
			if !back.Equal(tt.val) {
				t.Errorf("round trip through %s changed the value", got)
			}
		})
	}
}

func TestDecodeJSONRejects(t *testing.T) {
	tests := []struct {
		name string
		typ  Type
		data string
	}{
		{"int written as text", int64T, `"5"`},
		{"int out of range", int64T, `9223372036854775808`},
		{"int with a plus sign", int64T, `+5`},
		{"negative unsigned", uint64T, `-1`},
		{"float spelled Infinity", float64T, `Infinity`},
		{"bool as a number", boolT, `1`},
		{"decimal with an exponent", decimalT, `"1e5"`},
		{"decimal as a number", decimalT, `1.5`},
		{"impossible date", dateT, `"2026-02-30"`},
		{"date with a time", dateT, `"2026-10-19T00:00:00"`},
		{"five-digit year without a sign", dateT, `"12026-01-01"`},
		{"time past 24:00", timeT, `"24:00:01"`},
		{"time with ten fraction digits", timeT, `"12:00:00.1234567890"`},
		{"timestamp at 24:00", tsT, `"2026-10-19T24:00:00"`},
		{"timestamp with a bad zone", tsTZT, `"2026-10-19T12:00:00+5"`},
		{"bad uuid", uuidT, `"not-a-uuid"`},
		{"bad base64", binaryT, `"%%%"`},
		{"unknown special", float64T, `{"$":"huge"}`},
		{"NaN date", dateT, `{"$":"nan"}`},
		{"zero date without its text", dateT, `{"$":"zero"}`},
		{"interval with an unknown field", intervalT, `{"months":1,"days":0,"nanos":0,"years":2}`},
		{"interval missing a part", intervalT, `{"months":1}`},
		{"untagged element in a tagged array", opaqueT, `{"$":"array","v":[1]}`},
		{"array element of the wrong type", arrayT, `[1,"x"]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if v, err := DecodeJSON(tt.typ, []byte(tt.data)); err == nil {
				t.Errorf("DecodeJSON(%s) = %v, want an error", tt.data, v)
			}
		})
	}
}
