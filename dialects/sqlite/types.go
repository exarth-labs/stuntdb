package sqlite

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/exarth-labs/stuntdb/internal/value"
)

// columnType maps a column's declared type to a canonical type. It follows
// SQLite's rules for column affinity (https://sqlite.org/datatype3.html,
// section 3.1), and within each affinity recognizes the common type names.
//
// SQLite enforces none of these types (except in STRICT tables), so a column
// can hold values of other kinds. They still copy exactly, because values
// keep their stored kind (see fromStorage).
func columnType(declared string) value.Type {
	upper := strings.ToUpper(strings.TrimSpace(declared))
	name, args := splitType(upper)
	switch {
	case strings.Contains(upper, "INT"):
		return value.Type{Kind: value.TypeInt, Bits: 64}
	case strings.Contains(upper, "CHAR"), strings.Contains(upper, "CLOB"), strings.Contains(upper, "TEXT"):
		if strings.Contains(name, "CHAR") && len(args) > 0 && args[0] > 0 {
			// SQLite never pads CHAR(n), so it is not a fixed-width type here.
			return value.Type{Kind: value.TypeString, MaxLen: args[0]}
		}
		return value.Type{Kind: value.TypeText}
	case strings.Contains(upper, "BLOB"):
		return value.Type{Kind: value.TypeBinary}
	case strings.Contains(upper, "REAL"), strings.Contains(upper, "FLOA"), strings.Contains(upper, "DOUB"): //nolint:misspell // SQLite's rule matches these letters
		return value.Type{Kind: value.TypeFloat, Bits: 64}
	}

	// The rest have NUMERIC affinity, or no type at all.
	tz := strings.HasSuffix(name, "TZ") || strings.Contains(upper, "WITH TIME ZONE")
	switch strings.TrimSuffix(name, "TZ") {
	case "BOOL", "BOOLEAN":
		return value.Type{Kind: value.TypeBool}
	case "DECIMAL", "NUMERIC", "NUMBER":
		t := value.Type{Kind: value.TypeDecimal}
		if len(args) > 0 && args[0] > 0 {
			t.Precision = args[0]
			if len(args) > 1 {
				t.Scale = args[1]
			}
		}
		return t
	case "DATE":
		return value.Type{Kind: value.TypeDate}
	case "DATETIME", "TIMESTAMP":
		return value.Type{Kind: value.TypeTimestamp, Precision: 9, TZ: tz}
	case "TIME":
		return value.Type{Kind: value.TypeTime, Precision: 9, TZ: tz}
	case "JSON":
		return value.Type{Kind: value.TypeJSON}
	case "JSONB":
		return value.Type{Kind: value.TypeBinary}
	case "UUID", "GUID", "UNIQUEIDENTIFIER":
		return value.Type{Kind: value.TypeUUID}
	}
	// No type, ANY, or an unknown name such as STRING: the column holds
	// whatever was put in it, which is most often text.
	return value.Type{Kind: value.TypeText}
}

// splitType splits an upper-case declared type such as "DECIMAL(10, 2)" or
// "TIMESTAMP WITH TIME ZONE" into its first word and its numeric arguments.
func splitType(upper string) (name string, args []int) {
	name, rest, _ := strings.Cut(upper, "(")
	if i := strings.IndexByte(name, ' '); i >= 0 {
		name = name[:i]
	}
	name = strings.TrimSpace(name)
	inner, _, ok := strings.Cut(rest, ")")
	if !ok {
		return name, nil
	}
	for part := range strings.SplitSeq(inner, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil {
			return name, nil
		}
		args = append(args, n)
	}
	return name, args
}

// sqlType is the declared type for a canonical type, for tables built from
// the canonical model rather than replayed from a SQLite source. columnType
// maps each one back to the same kind.
func sqlType(t value.Type) (string, error) {
	switch t.Kind {
	case value.TypeBool:
		return "BOOLEAN", nil
	case value.TypeInt:
		return "INTEGER", nil
	case value.TypeFloat:
		return "REAL", nil
	case value.TypeDecimal:
		if t.Precision > 0 {
			return fmt.Sprintf("NUMERIC(%d,%d)", t.Precision, t.Scale), nil
		}
		return "NUMERIC", nil
	case value.TypeString:
		switch {
		case t.MaxLen > 0 && t.Fixed:
			return fmt.Sprintf("CHAR(%d)", t.MaxLen), nil
		case t.MaxLen > 0:
			return fmt.Sprintf("VARCHAR(%d)", t.MaxLen), nil
		}
		return "TEXT", nil
	case value.TypeText, value.TypeEnum:
		return "TEXT", nil
	case value.TypeBinary:
		return "BLOB", nil
	case value.TypeDate:
		return "DATE", nil
	case value.TypeTime:
		if t.TZ {
			return "TIMETZ", nil
		}
		return "TIME", nil
	case value.TypeTimestamp:
		if t.TZ {
			return "TIMESTAMPTZ", nil
		}
		return "TIMESTAMP", nil
	case value.TypeUUID:
		return "UUID", nil
	case value.TypeJSON:
		return "JSON", nil
	default:
		return "", fmt.Errorf("sqlite: a %s column cannot be created yet", t)
	}
}

// fromStorage converts a value as the driver returns it (nil, int64, float64,
// string or []byte) into a canonical value for a column of type t.
//
// A value keeps its stored kind, except where converting it to the column's
// natural kind is exactly reversible by toStorage, so that writing it back
// stores the same thing:
//
//   - INTEGER 0 or 1 in a BOOLEAN column becomes a bool;
//   - INTEGER or REAL in a DECIMAL column becomes a decimal, from the
//     shortest text that gives back the same number;
//   - TEXT in a DATE, TIME or TIMESTAMP column becomes a date, time or
//     timestamp, if AppendISO writes it back exactly ("2026-10-19 13:45:00");
//   - TEXT in a UUID column becomes a UUID, if it is in lower-case standard form.
func fromStorage(raw any, t value.Type) (value.Value, error) {
	switch x := raw.(type) {
	case nil:
		return value.Null(), nil
	case int64:
		switch {
		case t.Kind == value.TypeBool && (x == 0 || x == 1):
			return value.Bool(x == 1), nil
		case t.Kind == value.TypeDecimal:
			return value.Decimal(strconv.FormatInt(x, 10))
		}
		return value.Int(x), nil
	case float64:
		if t.Kind == value.TypeDecimal {
			if math.IsInf(x, 0) {
				return value.DecimalInfinity(x < 0), nil
			}
			if !math.IsNaN(x) {
				return value.Decimal(strconv.FormatFloat(x, 'f', -1, 64))
			}
		}
		return value.Float(x), nil
	case string:
		switch t.Kind {
		case value.TypeDate, value.TypeTime, value.TypeTimestamp:
			if v, ok := temporalFromText(x, t); ok {
				return v, nil
			}
		case value.TypeUUID:
			if v, err := value.ParseUUID(x); err == nil && value.FormatUUID(v.UUID()) == x {
				return v, nil
			}
		}
		return value.String(x), nil
	case []byte:
		return value.Bytes(x), nil
	default:
		return value.Value{}, fmt.Errorf("sqlite: the driver returned an unexpected %T", raw)
	}
}

func temporalFromText(s string, t value.Type) (value.Value, bool) {
	kind := value.KindTimestamp
	switch t.Kind {
	case value.TypeDate:
		kind = value.KindDate
	case value.TypeTime:
		kind = value.KindTime
	}
	v, err := value.ParseISO(kind, s, ' ')
	if err != nil {
		return value.Value{}, false
	}
	if kind != value.KindDate && v.IsTZ() != t.TZ {
		return value.Value{}, false
	}
	back, ok := value.AppendISO(nil, v, ' ')
	return v, ok && string(back) == s
}

// errCannotStore is wrapped by toStorage errors.
var errCannotStore = errors.New("SQLite cannot store this value")

// toStorage converts a canonical value into one the driver binds as SQLite's
// INTEGER, REAL, TEXT, BLOB or NULL. It fails rather than store something
// different, such as a NaN, which SQLite would turn into NULL. The error
// names the value's kind only.
func toStorage(v value.Value) (any, error) {
	switch v.Kind() {
	case value.KindNull:
		return nil, nil
	case value.KindBool:
		if v.Bool() {
			return int64(1), nil
		}
		return int64(0), nil
	case value.KindInt:
		return v.Int(), nil
	case value.KindUint:
		if u := v.Uint(); u <= math.MaxInt64 {
			return int64(u), nil
		}
		return nil, fmt.Errorf("%w: an unsigned integer above the 64-bit signed range", errCannotStore)
	case value.KindFloat:
		if f := v.Float(); !math.IsNaN(f) {
			return f, nil
		}
		return nil, fmt.Errorf("%w: a float NaN (SQLite stores it as NULL)", errCannotStore)
	case value.KindDecimal:
		return decimalToStorage(v)
	case value.KindString:
		return v.Text(), nil
	case value.KindBytes:
		b := v.Bytes()
		if b == nil {
			b = []byte{} // an empty BLOB, not NULL
		}
		return b, nil
	case value.KindUUID:
		return value.FormatUUID(v.UUID()), nil
	case value.KindDate, value.KindTime, value.KindTimestamp:
		if text, ok := value.AppendISO(nil, v, ' '); ok {
			return string(text), nil
		}
		switch v.Special() {
		case value.PosInfinity:
			return "infinity", nil
		case value.NegInfinity:
			return "-infinity", nil
		case value.ZeroDate:
			return v.ZeroText(), nil
		}
	}
	return nil, fmt.Errorf("%w: a %s value", errCannotStore, v.Kind())
}

// decimalToStorage stores a decimal the way a NUMERIC column would: as an
// INTEGER when it is a whole number in range, and as a REAL otherwise. The
// conversion happens here, with Go's correctly rounded parser, instead of in
// SQLite.
func decimalToStorage(v value.Value) (any, error) {
	switch v.Special() {
	case value.PosInfinity:
		return math.Inf(1), nil
	case value.NegInfinity:
		return math.Inf(-1), nil
	case value.NaN:
		return nil, fmt.Errorf("%w: a decimal NaN", errCannotStore)
	}
	text := v.DecimalText()
	if i, err := strconv.ParseInt(text, 10, 64); err == nil {
		return i, nil
	}
	if f, err := strconv.ParseFloat(text, 64); err == nil {
		return f, nil
	}
	// Too large for a REAL: store the text, which is what SQLite keeps when
	// it cannot convert.
	return text, nil
}
