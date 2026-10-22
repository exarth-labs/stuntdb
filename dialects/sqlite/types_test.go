package sqlite

import (
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/exarth-labs/stuntdb/internal/value"
)

func TestColumnType(t *testing.T) {
	tests := []struct {
		declared string
		want     value.Type
	}{
		// INTEGER affinity: anything containing INT, as SQLite decides it.
		{"INTEGER", value.Type{Kind: value.TypeInt, Bits: 64}},
		{"bigint", value.Type{Kind: value.TypeInt, Bits: 64}},
		{"TINYINT(1)", value.Type{Kind: value.TypeInt, Bits: 64}},
		{"FLOATING POINT", value.Type{Kind: value.TypeInt, Bits: 64}}, // contains "INT"
		// TEXT affinity.
		{"TEXT", value.Type{Kind: value.TypeText}},
		{"VARCHAR(255)", value.Type{Kind: value.TypeString, MaxLen: 255}},
		{"character varying(10)", value.Type{Kind: value.TypeString, MaxLen: 10}},
		{"CHAR(2)", value.Type{Kind: value.TypeString, MaxLen: 2}},
		{"NCHAR", value.Type{Kind: value.TypeText}},
		{"CLOB", value.Type{Kind: value.TypeText}},
		// BLOB affinity.
		{"BLOB", value.Type{Kind: value.TypeBinary}},
		// REAL affinity.
		{"REAL", value.Type{Kind: value.TypeFloat, Bits: 64}},
		{"DOUBLE PRECISION", value.Type{Kind: value.TypeFloat, Bits: 64}},
		{"float", value.Type{Kind: value.TypeFloat, Bits: 64}},
		// NUMERIC affinity, with the names it recognizes.
		{"BOOLEAN", value.Type{Kind: value.TypeBool}},
		{"DECIMAL(12, 2)", value.Type{Kind: value.TypeDecimal, Precision: 12, Scale: 2}},
		{"NUMERIC", value.Type{Kind: value.TypeDecimal}},
		{"numeric(10)", value.Type{Kind: value.TypeDecimal, Precision: 10}},
		{"DATE", value.Type{Kind: value.TypeDate}},
		{"DATETIME", value.Type{Kind: value.TypeTimestamp, Precision: 9}},
		{"TIMESTAMP", value.Type{Kind: value.TypeTimestamp, Precision: 9}},
		{"TIMESTAMPTZ", value.Type{Kind: value.TypeTimestamp, Precision: 9, TZ: true}},
		{"timestamp with time zone", value.Type{Kind: value.TypeTimestamp, Precision: 9, TZ: true}},
		{"TIME", value.Type{Kind: value.TypeTime, Precision: 9}},
		{"TIMETZ", value.Type{Kind: value.TypeTime, Precision: 9, TZ: true}},
		{"JSON", value.Type{Kind: value.TypeJSON}},
		{"JSONB", value.Type{Kind: value.TypeBinary}},
		{"uuid", value.Type{Kind: value.TypeUUID}},
		// No type, ANY, or unknown names: whatever was stored, most often text.
		{"", value.Type{Kind: value.TypeText}},
		{"ANY", value.Type{Kind: value.TypeText}},
		{"STRING", value.Type{Kind: value.TypeText}},
		{"MONEY", value.Type{Kind: value.TypeText}},
	}
	for _, tt := range tests {
		got := columnType(tt.declared)
		if got.String() != tt.want.String() || got.Kind != tt.want.Kind {
			t.Errorf("columnType(%q) = %s, want %s", tt.declared, got, tt.want)
		}
		if err := got.Validate(); err != nil {
			t.Errorf("columnType(%q) is invalid: %v", tt.declared, err)
		}
	}
}

// Every type the canonical DDL writes maps back to the same kind.
func TestSQLTypeRoundTrip(t *testing.T) {
	for _, typ := range []value.Type{
		{Kind: value.TypeBool},
		{Kind: value.TypeInt, Bits: 32},
		{Kind: value.TypeFloat, Bits: 32},
		{Kind: value.TypeDecimal, Precision: 10, Scale: 2},
		{Kind: value.TypeDecimal},
		{Kind: value.TypeString, MaxLen: 20},
		{Kind: value.TypeString, MaxLen: 3, Fixed: true},
		{Kind: value.TypeText},
		{Kind: value.TypeBinary},
		{Kind: value.TypeDate},
		{Kind: value.TypeTime, Precision: 9},
		{Kind: value.TypeTime, Precision: 9, TZ: true},
		{Kind: value.TypeTimestamp, Precision: 9},
		{Kind: value.TypeTimestamp, Precision: 9, TZ: true},
		{Kind: value.TypeUUID},
		{Kind: value.TypeJSON},
	} {
		declared, err := sqlType(typ)
		if err != nil {
			t.Fatalf("sqlType(%s): %v", typ, err)
		}
		back := columnType(declared)
		if back.Kind != typ.Kind || back.TZ != typ.TZ || back.Precision != typ.Precision && typ.Kind == value.TypeDecimal {
			t.Errorf("%s → %q → %s", typ, declared, back)
		}
	}
	for _, typ := range []value.Type{{Kind: value.TypeInterval}, {Kind: value.TypeArray, Elem: &value.Type{Kind: value.TypeText}}, {Kind: value.TypeOpaque, Native: "geometry"}} {
		if _, err := sqlType(typ); err == nil {
			t.Errorf("sqlType(%s) succeeded; SQLite can't store it yet", typ)
		}
	}
}

// fromStorage followed by toStorage gives back exactly what SQLite stored,
// whatever the column type.
func TestStorageRoundTrip(t *testing.T) {
	types := []value.Type{
		columnType("INTEGER"), columnType("TEXT"), columnType("BLOB"), columnType("REAL"),
		columnType("BOOLEAN"), columnType("DECIMAL(10,2)"), columnType("DATE"), columnType("TIME"),
		columnType("TIMESTAMP"), columnType("TIMESTAMPTZ"), columnType("UUID"), columnType(""),
	}
	stored := []any{
		nil, int64(0), int64(1), int64(2), int64(math.MinInt64), int64(math.MaxInt64),
		0.1, 12.5, -1e-300, 1e300, math.Inf(1), math.Inf(-1), 1e20,
		"", "text", "2026-10-19", "2026-10-19 13:45:00", "2026-10-19 13:45:00.5Z",
		"2026-10-19 13:45:00+05:30", "2026-10-19T13:45:00", "2026-10-19 13:45:00.100", "13:45:00",
		"123e4567-e89b-12d3-a456-426614174000", "123E4567-E89B-12D3-A456-426614174000",
		"a\x00b", "\xff\xfe",
		[]byte{},
		[]byte{0, 1, 2},
	}
	for _, typ := range types {
		for i, raw := range stored {
			v, err := fromStorage(raw, typ)
			if err != nil {
				t.Errorf("%s, stored value %d: fromStorage: %v", typ, i, err)
				continue
			}
			back, err := toStorage(v)
			if err != nil {
				t.Errorf("%s, stored value %d: toStorage: %v", typ, i, err)
				continue
			}
			if !sameStored(raw, back) {
				t.Errorf("%s, stored value %d (%T) came back different (%T)", typ, i, raw, back)
			}
		}
	}
}

func sameStored(a, b any) bool {
	switch x := a.(type) {
	case []byte:
		y, ok := b.([]byte)
		return ok && string(x) == string(y)
	case float64:
		y, ok := b.(float64)
		return ok && (x == y || math.IsNaN(x) && math.IsNaN(y))
	default:
		return a == b
	}
}

func TestFromStorageConverts(t *testing.T) {
	tests := []struct {
		declared string
		raw      any
		want     value.Kind
	}{
		{"BOOLEAN", int64(1), value.KindBool},
		{"BOOLEAN", int64(2), value.KindInt},
		{"DECIMAL", int64(5), value.KindDecimal},
		{"DECIMAL", 12.5, value.KindDecimal},
		{"DECIMAL", "n/a", value.KindString},
		{"DATE", "2026-10-19", value.KindDate},
		{"DATE", "19/10/2026", value.KindString},
		{"DATETIME", "2026-10-19 13:45:00", value.KindTimestamp},
		{"DATETIME", "2026-10-19T13:45:00", value.KindString},     // writes back with a space
		{"DATETIME", "2026-10-19 13:45:00.100", value.KindString}, // writes back without trailing zeros
		{"DATETIME", "2026-10-19 13:45:00Z", value.KindString},    // a time zone in a column without one
		{"TIMESTAMPTZ", "2026-10-19 13:45:00Z", value.KindTimestamp},
		{"TIME", "24:00:00", value.KindTime},
		{"UUID", "123e4567-e89b-12d3-a456-426614174000", value.KindUUID},
		{"UUID", "123E4567-E89B-12D3-A456-426614174000", value.KindString},
		{"INTEGER", "abc", value.KindString},
		{"TEXT", int64(5), value.KindInt},
		{"BLOB", []byte{1}, value.KindBytes},
	}
	for _, tt := range tests {
		v, err := fromStorage(tt.raw, columnType(tt.declared))
		if err != nil {
			t.Errorf("fromStorage(%T in %s): %v", tt.raw, tt.declared, err)
			continue
		}
		if v.Kind() != tt.want {
			t.Errorf("fromStorage(%T in %s) is a %s value, want %s", tt.raw, tt.declared, v.Kind(), tt.want)
		}
	}
}

func TestToStorageRefusesLoss(t *testing.T) {
	const canary = "do-not-print-7f3a"
	for _, v := range []value.Value{
		value.Float(math.NaN()),
		value.DecimalNaN(),
		value.Uint(math.MaxUint64),
		value.Interval(1, 2, 3),
		value.Array([]value.Value{value.String(canary)}),
	} {
		_, err := toStorage(v)
		if !errors.Is(err, errCannotStore) {
			t.Errorf("toStorage(%s value) = %v, want errCannotStore", v.Kind(), err)
			continue
		}
		if strings.Contains(err.Error(), canary) {
			t.Errorf("the error contains the value: %q", err)
		}
	}
}
