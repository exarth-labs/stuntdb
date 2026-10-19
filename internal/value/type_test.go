package value

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

func TestTypeValidate(t *testing.T) {
	bad := Type{Kind: TypeInt, Bits: 12}
	tests := []struct {
		name    string
		typ     Type
		wantErr bool
	}{
		{"bool", boolT, false},
		{"int8", int8T, false},
		{"uint64", uint64T, false},
		{"float32", float32T, false},
		{"unlimited decimal", decimalT, false},
		{"decimal(10,2)", Type{Kind: TypeDecimal, Precision: 10, Scale: 2}, false},
		{"decimal with negative scale", Type{Kind: TypeDecimal, Precision: 3, Scale: -2}, false},
		{"decimal with scale above precision", Type{Kind: TypeDecimal, Precision: 2, Scale: 5}, false},
		{"string", string3T, false},
		{"char(2)", Type{Kind: TypeString, MaxLen: 2, Fixed: true}, false},
		{"timestamp(9) with time zone", tsTZT, false},
		{"enum", enumT, false},
		{"empty enum", Type{Kind: TypeEnum}, false},
		{"array of arrays", nestedT, false},
		{"opaque", opaqueT, false},

		{"zero value", Type{}, true},
		{"unknown kind", Type{Kind: 99}, true},
		{"int with 12 bits", bad, true},
		{"float with 16 bits", Type{Kind: TypeFloat, Bits: 16}, true},
		{"negative decimal precision", Type{Kind: TypeDecimal, Precision: -1}, true},
		{"unlimited decimal with a scale", Type{Kind: TypeDecimal, Scale: 2}, true},
		{"negative string length", Type{Kind: TypeString, MaxLen: -1}, true},
		{"fixed string without a length", Type{Kind: TypeString, Fixed: true}, true},
		{"negative binary length", Type{Kind: TypeBinary, MaxLen: -1}, true},
		{"ten fractional digits", Type{Kind: TypeTimestamp, Precision: 10}, true},
		{"duplicate enum label", Type{Kind: TypeEnum, Values: []string{"a", "a"}}, true},
		{"array without element type", Type{Kind: TypeArray}, true},
		{"array of an invalid type", Type{Kind: TypeArray, Elem: &bad}, true},
		{"opaque without a name", Type{Kind: TypeOpaque}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.typ.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestTypeString(t *testing.T) {
	tests := []struct {
		typ  Type
		want string
	}{
		{int8T, "int8"},
		{uint64T, "uint64"},
		{float32T, "float32"},
		{decimalT, "decimal"},
		{Type{Kind: TypeDecimal, Precision: 10, Scale: 2}, "decimal(10,2)"},
		{string3T, "string(3)"},
		{Type{Kind: TypeString, MaxLen: 2, Fixed: true}, "char(2)"},
		{Type{Kind: TypeString}, "string"},
		{Type{Kind: TypeBinary, MaxLen: 16}, "binary(16)"},
		{tsTZT, "timestamp(9) with time zone"},
		{Type{Kind: TypeTime, Precision: 3}, "time(3)"},
		{enumT, "enum(2 labels)"},
		{nestedT, "array<array<int64>>"},
		{opaqueT, "opaque(geometry)"},
		{uuidT, "uuid"},
	}
	for _, tt := range tests {
		if got := tt.typ.String(); got != tt.want {
			t.Errorf("String() = %q, want %q", got, tt.want)
		}
	}
}

func TestCheck(t *testing.T) {
	dec52 := Type{Kind: TypeDecimal, Precision: 5, Scale: 2}
	dec25 := Type{Kind: TypeDecimal, Precision: 2, Scale: 5}
	dec3neg2 := Type{Kind: TypeDecimal, Precision: 3, Scale: -2}
	time3 := Type{Kind: TypeTime, Precision: 3}
	ts0TZ := Type{Kind: TypeTimestamp, Precision: 0, TZ: true}
	ts6 := Type{Kind: TypeTimestamp, Precision: 6}
	arrayInt8 := Type{Kind: TypeArray, Elem: &int8T}
	noon := 12 * time.Hour
	instant := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name string
		typ  Type
		val  Value
		fits bool
	}{
		{"NULL fits anything", int8T, Null(), true},
		{"int8 max", int8T, Int(127), true},
		{"int8 min", int8T, Int(-128), true},
		{"int8 overflow", int8T, Int(128), false},
		{"int8 underflow", int8T, Int(-129), false},
		{"uint8 max as int", uint8T, Int(255), true},
		{"uint8 max as uint", uint8T, Uint(255), true},
		{"negative in unsigned", uint8T, Int(-1), false},
		{"uint8 overflow", uint8T, Uint(256), false},
		{"uint fits int64", int64T, Uint(math.MaxInt64), true},
		{"uint too big for int64", int64T, Uint(math.MaxInt64 + 1), false},
		{"float32 max", float32T, Float(math.MaxFloat32), true},
		{"float32 infinity", float32T, Float(math.Inf(1)), true},
		{"float32 overflow", float32T, Float(1e39), false},
		{"decimal(5,2) max", dec52, mustDecimal(t, "999.99"), true},
		{"decimal(5,2) negative fraction", dec52, mustDecimal(t, "-0.5"), true},
		{"decimal(5,2) trailing zero is not a digit", dec52, mustDecimal(t, "1.230"), true},
		{"decimal(5,2) zero", dec52, mustDecimal(t, "-0.000"), true},
		{"decimal(5,2) too big", dec52, mustDecimal(t, "1000"), false},
		{"decimal(5,2) too many decimals", dec52, mustDecimal(t, "1.001"), false},
		{"decimal(5,2) NaN", dec52, DecimalNaN(), true},
		{"decimal(2,5) small", dec25, mustDecimal(t, "0.00099"), true},
		{"decimal(2,5) too big", dec25, mustDecimal(t, "0.001"), false},
		{"decimal(3,-2) rounded to hundreds", dec3neg2, mustDecimal(t, "12300"), true},
		{"decimal(3,-2) not rounded", dec3neg2, mustDecimal(t, "12345"), false},
		{"unlimited decimal", decimalT, mustDecimal(t, "123456789012345678901234567890.5"), true},
		{"string at its limit", string3T, String("abc"), true},
		{"string limit counts characters, not bytes", string3T, String("😀😀😀"), true},
		{"string too long", string3T, String("abcd"), false},
		{"enum label", enumT, String("banned"), true},
		{"unknown enum label", enumT, String("deleted"), false},
		{"valid JSON", jsonT, String(`[1,2]`), true},
		{"invalid JSON", jsonT, String(`{oops`), false},
		{"binary at its limit", Type{Kind: TypeBinary, MaxLen: 2}, Bytes([]byte{1, 2}), true},
		{"binary too long", Type{Kind: TypeBinary, MaxLen: 2}, Bytes([]byte{1, 2, 3}), false},
		{"milliseconds in time(3)", time3, mustTime(t, noon+123*time.Millisecond), true},
		{"too precise for time(3)", time3, mustTime(t, noon+1234*time.Microsecond/10), false},
		{"time with zone in a time column", time3, mustTimeZone(t, noon, 3600), false},
		{"instant in a time-zone-aware column", ts0TZ, TimestampTZ(instant), true},
		{"wall clock in a time-zone-aware column", ts0TZ, Timestamp(instant), false},
		{"instant in a wall-clock column", ts6, TimestampTZ(instant), false},
		{"too precise for timestamp(0)", ts0TZ, TimestampTZ(instant.Add(time.Millisecond)), false},
		{"MySQL zero date-time", ts6, MySQLZeroTimestamp("0000-00-00 00:00:00", false), true},
		{"wrong kind", boolT, Int(1), false},
		{"array elements fit", arrayInt8, Array([]Value{Int(1), Null()}), true},
		{"array element too big", arrayInt8, Array([]Value{Int(1), Int(999)}), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.typ.Check(tt.val)
			if tt.fits {
				if err != nil {
					t.Errorf("Check() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, ErrDoesNotFit) {
				t.Fatalf("Check() = %v, want ErrDoesNotFit", err)
			}
		})
	}
}

func TestCheckErrorsNeverContainTheValue(t *testing.T) {
	secret := "alice@example.com"
	for _, typ := range []Type{string3T, enumT, jsonT, int64T} {
		err := typ.Check(String(secret))
		if err == nil {
			t.Fatalf("%s: expected a misfit", typ)
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("%s: error %q contains the value", typ, err)
		}
	}
}
