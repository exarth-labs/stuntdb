package value

import (
	"bytes"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"testing"
	"time"
)

func TestConstructorsReject(t *testing.T) {
	tests := []struct {
		name string
		make func() (Value, error)
	}{
		{"empty decimal", func() (Value, error) { return Decimal("") }},
		{"decimal with exponent", func() (Value, error) { return Decimal("1e5") }},
		{"decimal with two points", func() (Value, error) { return Decimal("1.2.3") }},
		{"decimal with two signs", func() (Value, error) { return Decimal("--1") }},
		{"decimal point alone", func() (Value, error) { return Decimal(".") }},
		{"decimal with letters", func() (Value, error) { return Decimal("12abc") }},
		{"February 30", func() (Value, error) { return Date(2026, 2, 30) }},
		{"month 13", func() (Value, error) { return Date(2026, 13, 1) }},
		{"day 0", func() (Value, error) { return Date(2026, 1, 0) }},
		{"negative time", func() (Value, error) { return TimeOfDay(-1) }},
		{"time past 24:00", func() (Value, error) { return TimeOfDay(24*time.Hour + 1) }},
		{"24-hour offset", func() (Value, error) { return TimeOfDayWithZone(0, 24*3600) }},
		{"short UUID", func() (Value, error) { return ParseUUID("123") }},
		{"UUID with misplaced hyphens", func() (Value, error) { return ParseUUID("123e4567e-89b-12d3-a456-426614174000") }},
		{"UUID with non-hex digits", func() (Value, error) { return ParseUUID("zzzzzzzz-e89b-12d3-a456-426614174000") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := tt.make(); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

func TestConstructorErrorsNeverContainTheInput(t *testing.T) {
	secret := "4111-1111-1111-1111"
	if _, err := Decimal(secret); err == nil || strings.Contains(err.Error(), secret) {
		t.Errorf("Decimal error = %v", err)
	}
	if _, err := ParseUUID(secret); err == nil || strings.Contains(err.Error(), secret) {
		t.Errorf("ParseUUID error = %v", err)
	}
}

func TestAccessors(t *testing.T) {
	if !Bool(true).Bool() || Bool(false).Bool() {
		t.Error("Bool")
	}
	if Int(-7).Int() != -7 || Uint(7).Uint() != 7 || Float(2.5).Float() != 2.5 {
		t.Error("numbers")
	}
	if mustDecimal(t, "1.50").DecimalText() != "1.50" || DecimalNaN().DecimalText() != "" {
		t.Error("DecimalText")
	}
	if String("hi").Text() != "hi" || !bytes.Equal(Bytes([]byte{1}).Bytes(), []byte{1}) {
		t.Error("Text or Bytes")
	}
	if y, m, d := mustDate(t, 2026, 10, 19).Date(); y != 2026 || m != 10 || d != 19 {
		t.Error("Date")
	}
	if y, _, _ := DateInfinity(false).Date(); y != 0 {
		t.Error("Date of a special value should be zero")
	}
	tz := mustTimeZone(t, time.Hour, 3600)
	if tz.TimeOfDay() != time.Hour || !tz.IsTZ() {
		t.Error("TimeOfDay or IsTZ")
	}
	if offset, known := tz.Zone(); offset != 3600 || !known {
		t.Error("Zone")
	}
	at := time.Date(2026, 10, 19, 13, 0, 0, 0, time.FixedZone("", 7200))
	if got := TimestampTZWithZone(at).Timestamp(); !got.Equal(at) || got.Format("-07:00") != "+02:00" {
		t.Errorf("Timestamp with zone = %v", got)
	}
	if got := TimestampTZ(at).Timestamp(); !got.Equal(at) || got.Location() != time.UTC {
		t.Errorf("Timestamp without zone = %v", got)
	}
	if months, days, nanos := Interval(1, 2, 3).Interval(); months != 1 || days != 2 || nanos != 3 {
		t.Error("Interval")
	}
	if MySQLZeroDate("0000-00-00").ZeroText() != "0000-00-00" || String("x").ZeroText() != "" {
		t.Error("ZeroText")
	}
	if Null().Special() != Normal || DecimalInfinity(true).Special() != NegInfinity {
		t.Error("Special")
	}
	if !Null().IsNull() || String("").IsNull() || Int(1).Kind() != KindInt || Null().Kind() != KindNull {
		t.Error("IsNull or Kind")
	}
}

func TestValuesAreImmutable(t *testing.T) {
	b := []byte{1, 2}
	v := Bytes(b)
	b[0] = 9
	if v.Bytes()[0] != 1 {
		t.Error("Bytes did not copy its input")
	}
	v.Bytes()[1] = 9
	if v.Bytes()[1] != 2 {
		t.Error("Bytes() did not return a copy")
	}
	elems := []Value{Int(1)}
	a := Array(elems)
	elems[0] = Int(2)
	if a.Elems()[0].Int() != 1 {
		t.Error("Array did not copy its input")
	}
}

func TestEqual(t *testing.T) {
	at := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		a, b  Value
		equal bool
	}{
		{"NaN equals NaN", Float(math.NaN()), Float(math.NaN()), true},
		{"-0 differs from +0", Float(math.Copysign(0, -1)), Float(0), false},
		{"int differs from uint", Int(1), Uint(1), false},
		{"empty string differs from NULL", String(""), Null(), false},
		{"empty bytes equal nil bytes", Bytes(nil), Bytes([]byte{}), true},
		{"decimal text matters", mustDecimal(t, "1.0"), mustDecimal(t, "1.00"), false},
		{"same instant, different input zones", TimestampTZ(at), TimestampTZ(at.In(time.FixedZone("", 3600))), true},
		{"same instant, different kept offsets", TimestampTZWithZone(at), TimestampTZWithZone(at.In(time.FixedZone("", 3600))), false},
		{"wall clock differs from instant", Timestamp(at), TimestampTZ(at), false},
		{"equal arrays", Array([]Value{Int(1), Null()}), Array([]Value{Int(1), Null()}), true},
		{"different arrays", Array([]Value{Int(1)}), Array([]Value{Int(2)}), false},
		{"different array lengths", Array([]Value{Int(1)}), Array(nil), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.a.Equal(tt.b); got != tt.equal {
				t.Errorf("Equal() = %v, want %v", got, tt.equal)
			}
		})
	}
}

// A value must never reveal its contents when printed or logged, so that
// real data cannot leak into logs or error messages by accident.
func TestValuesNeverPrintTheirContents(t *testing.T) {
	const secret = "4111111111111111"
	values := []Value{
		String("card " + secret),
		Bytes([]byte(secret)),
		mustDecimal(t, secret),
		Int(4111111111111111),
		Array([]Value{String(secret)}),
	}
	for _, v := range values {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d"} {
			if out := fmt.Sprintf(verb, v); strings.Contains(out, secret) {
				t.Errorf("fmt %s printed %q", verb, out)
			}
		}
		if out := fmt.Sprintf("%+v", struct{ Email Value }{v}); strings.Contains(out, secret) {
			t.Errorf("struct field printed %q", out)
		}
		var buf bytes.Buffer
		slog.New(slog.NewTextHandler(&buf, nil)).Info("row", "value", v)
		if strings.Contains(buf.String(), secret) {
			t.Errorf("slog printed %q", buf.String())
		}
	}
	if got := String("x").String(); got != "<string value>" {
		t.Errorf("String() = %q", got)
	}
	if got := Null().String(); got != "NULL" {
		t.Errorf("Null().String() = %q", got)
	}
}

func TestAccessorPanicsWithoutTheValue(t *testing.T) {
	const secret = "alice@example.com"
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected a panic")
		}
		if msg := fmt.Sprint(r); strings.Contains(msg, secret) || !strings.Contains(msg, "Int called on a string value") {
			t.Errorf("panic message = %q", msg)
		}
	}()
	_ = String(secret).Int()
}

func TestDecimalDigits(t *testing.T) {
	tests := []struct {
		in       string
		msd, lsd int
		zero     bool
	}{
		{"120.05", 2, -2, false},
		{"0.0012", -3, -4, false},
		{"1200", 3, 2, false},
		{"-7", 0, 0, false},
		{"+0.000", 0, 0, true},
		{"007.50", 0, -1, false},
	}
	for _, tt := range tests {
		msd, lsd, zero := decimalDigits(tt.in)
		if msd != tt.msd || lsd != tt.lsd || zero != tt.zero {
			t.Errorf("decimalDigits(%q) = %d, %d, %v; want %d, %d, %v", tt.in, msd, lsd, zero, tt.msd, tt.lsd, tt.zero)
		}
	}
}
