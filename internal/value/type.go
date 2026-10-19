package value

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"unicode/utf8"
)

// TypeKind is a canonical column type.
type TypeKind uint8

const (
	TypeInvalid   TypeKind = iota // the zero value; never valid
	TypeBool                      // true or false
	TypeInt                       // an integer: Bits and Unsigned
	TypeFloat                     // floating point: Bits
	TypeDecimal                   // an exact decimal: Precision and Scale
	TypeString                    // bounded text: MaxLen and Fixed
	TypeText                      // unbounded text
	TypeBinary                    // bytes: MaxLen
	TypeDate                      // a calendar date
	TypeTime                      // a time of day: Precision and TZ
	TypeTimestamp                 // a date and a time of day: Precision and TZ
	TypeInterval                  // a duration
	TypeUUID                      // a 128-bit UUID
	TypeJSON                      // JSON text
	TypeEnum                      // one of a fixed set of labels: Values
	TypeArray                     // a list of Elem
	TypeOpaque                    // an engine-specific type, copied as bytes between the same engine: Native
)

var typeKindNames = [...]string{
	TypeInvalid:   "invalid",
	TypeBool:      "bool",
	TypeInt:       "int",
	TypeFloat:     "float",
	TypeDecimal:   "decimal",
	TypeString:    "string",
	TypeText:      "text",
	TypeBinary:    "binary",
	TypeDate:      "date",
	TypeTime:      "time",
	TypeTimestamp: "timestamp",
	TypeInterval:  "interval",
	TypeUUID:      "uuid",
	TypeJSON:      "json",
	TypeEnum:      "enum",
	TypeArray:     "array",
	TypeOpaque:    "opaque",
}

func (k TypeKind) String() string {
	if int(k) < len(typeKindNames) {
		return typeKindNames[k]
	}
	return "invalid"
}

// Type is a column's canonical type. Only the fields that apply to Kind are used.
type Type struct {
	Kind TypeKind

	Bits     int  // Int: 8, 16, 32 or 64. Float: 32 or 64.
	Unsigned bool // Int: no negative values.

	// Decimal: the total number of digits, and the digits after the decimal
	// point. Precision 0 means unlimited. Scale may be negative or larger than
	// Precision, as PostgreSQL allows.
	// Time, Timestamp: Precision is the number of fractional-second digits, 0–9.
	Precision int
	Scale     int

	MaxLen int  // String: characters. Binary: bytes. 0 means no limit.
	Fixed  bool // String: fixed width, like CHAR(n).

	TZ bool // Time, Timestamp: time-zone aware.

	Values []string // Enum: the allowed labels, in order.
	Elem   *Type    // Array: the element type.
	Native string   // Opaque: the engine's own type name.
}

// Validate reports whether t is a well-formed type.
func (t Type) Validate() error {
	switch t.Kind {
	case TypeBool, TypeText, TypeDate, TypeInterval, TypeUUID, TypeJSON:
		return nil
	case TypeInt:
		switch t.Bits {
		case 8, 16, 32, 64:
			return nil
		}
		return fmt.Errorf("an int type needs 8, 16, 32 or 64 bits, not %d", t.Bits)
	case TypeFloat:
		if t.Bits == 32 || t.Bits == 64 {
			return nil
		}
		return fmt.Errorf("a float type needs 32 or 64 bits, not %d", t.Bits)
	case TypeDecimal:
		if t.Precision < 0 {
			return errors.New("a decimal type cannot have a negative precision")
		}
		if t.Precision == 0 && t.Scale != 0 {
			return errors.New("an unlimited decimal type cannot have a scale")
		}
		return nil
	case TypeString:
		if t.MaxLen < 0 {
			return errors.New("a string type cannot have a negative length")
		}
		if t.Fixed && t.MaxLen == 0 {
			return errors.New("a fixed-width string type needs a length")
		}
		return nil
	case TypeBinary:
		if t.MaxLen < 0 {
			return errors.New("a binary type cannot have a negative length")
		}
		return nil
	case TypeTime, TypeTimestamp:
		if t.Precision < 0 || t.Precision > 9 {
			return fmt.Errorf("a %s type needs 0 to 9 fractional-second digits, not %d", t.Kind, t.Precision)
		}
		return nil
	case TypeEnum:
		seen := make(map[string]bool, len(t.Values))
		for _, label := range t.Values {
			if seen[label] {
				return fmt.Errorf("enum label %q appears twice", label)
			}
			seen[label] = true
		}
		return nil
	case TypeArray:
		if t.Elem == nil {
			return errors.New("an array type needs an element type")
		}
		if err := t.Elem.Validate(); err != nil {
			return fmt.Errorf("array element: %w", err)
		}
		return nil
	case TypeOpaque:
		if t.Native == "" {
			return errors.New("an opaque type needs its native type name")
		}
		return nil
	default:
		return fmt.Errorf("unknown type kind %d", t.Kind)
	}
}

// String returns a short description of the type, such as "decimal(10,2)".
func (t Type) String() string {
	switch t.Kind {
	case TypeInt:
		if t.Unsigned {
			return "uint" + strconv.Itoa(t.Bits)
		}
		return "int" + strconv.Itoa(t.Bits)
	case TypeFloat:
		return "float" + strconv.Itoa(t.Bits)
	case TypeDecimal:
		if t.Precision == 0 {
			return "decimal"
		}
		return fmt.Sprintf("decimal(%d,%d)", t.Precision, t.Scale)
	case TypeString:
		switch {
		case t.Fixed:
			return fmt.Sprintf("char(%d)", t.MaxLen)
		case t.MaxLen > 0:
			return fmt.Sprintf("string(%d)", t.MaxLen)
		}
		return "string"
	case TypeBinary:
		if t.MaxLen > 0 {
			return fmt.Sprintf("binary(%d)", t.MaxLen)
		}
		return "binary"
	case TypeTime, TypeTimestamp:
		s := fmt.Sprintf("%s(%d)", t.Kind, t.Precision)
		if t.TZ {
			s += " with time zone"
		}
		return s
	case TypeEnum:
		return fmt.Sprintf("enum(%d labels)", len(t.Values))
	case TypeArray:
		if t.Elem == nil {
			return "array<?>"
		}
		return "array<" + t.Elem.String() + ">"
	case TypeOpaque:
		return "opaque(" + t.Native + ")"
	default:
		return t.Kind.String()
	}
}

// naturalKind is the kind of value a column of type t normally holds.
func naturalKind(t Type) Kind {
	switch t.Kind {
	case TypeBool:
		return KindBool
	case TypeInt:
		if t.Unsigned {
			return KindUint
		}
		return KindInt
	case TypeFloat:
		return KindFloat
	case TypeDecimal:
		return KindDecimal
	case TypeString, TypeText, TypeEnum, TypeJSON:
		return KindString
	case TypeBinary, TypeOpaque:
		return KindBytes
	case TypeDate:
		return KindDate
	case TypeTime:
		return KindTime
	case TypeTimestamp:
		return KindTimestamp
	case TypeInterval:
		return KindInterval
	case TypeUUID:
		return KindUUID
	case TypeArray:
		return KindArray
	default:
		return KindNull // TypeInvalid holds nothing but NULL
	}
}

// ErrDoesNotFit is returned by Check when a value cannot be stored in a
// column of the given type without losing information.
var ErrDoesNotFit = errors.New("value does not fit the column type")

func misfit(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrDoesNotFit, fmt.Sprintf(format, args...))
}

// Check reports whether v can be stored in a column of type t without
// losing information. NULL fits every type: whether a column allows NULL is
// a property of the column, not of its type. The error never contains the value.
func (t Type) Check(v Value) error {
	if v.kind == KindNull {
		return nil
	}
	if t.Kind == TypeInt && (v.kind == KindInt || v.kind == KindUint) {
		return t.checkInt(v)
	}
	if v.kind != naturalKind(t) {
		return misfit("a %s value cannot go in a %s column", v.kind, t)
	}
	switch t.Kind {
	case TypeFloat:
		f := v.Float()
		if t.Bits == 32 && !math.IsInf(f, 0) && !math.IsNaN(f) && math.Abs(f) > math.MaxFloat32 {
			return misfit("the number is too large for %s", t)
		}
	case TypeDecimal:
		return t.checkDecimal(v)
	case TypeString:
		if n := utf8.RuneCountInString(v.str); t.MaxLen > 0 && n > t.MaxLen {
			return misfit("the text has %d characters, the column allows %d", n, t.MaxLen)
		}
	case TypeEnum:
		if !slices.Contains(t.Values, v.str) {
			return misfit("the text is not one of the enum's %d labels", len(t.Values))
		}
	case TypeJSON:
		if !json.Valid([]byte(v.str)) {
			return misfit("the text is not valid JSON")
		}
	case TypeBinary:
		if t.MaxLen > 0 && len(v.str) > t.MaxLen {
			return misfit("the value has %d bytes, the column allows %d", len(v.str), t.MaxLen)
		}
	case TypeTime, TypeTimestamp:
		return t.checkTemporal(v)
	case TypeArray:
		if t.Elem == nil {
			return misfit("the array type has no element type")
		}
		for i, e := range v.elems {
			if err := t.Elem.Check(e); err != nil {
				return fmt.Errorf("array element %d: %w", i, err)
			}
		}
	}
	return nil
}

func (t Type) checkInt(v Value) error {
	bits := uint(t.Bits)
	if v.kind == KindInt {
		i := int64(v.num)
		switch {
		case t.Unsigned && i < 0:
			return misfit("a negative number cannot go in a %s column", t)
		case t.Unsigned && bits < 64 && uint64(i) > uint64(1)<<bits-1:
			return misfit("the number is out of range for %s", t)
		case !t.Unsigned && bits < 64 && (i < -(int64(1)<<(bits-1)) || i > int64(1)<<(bits-1)-1):
			return misfit("the number is out of range for %s", t)
		}
		return nil
	}
	u := v.num
	if t.Unsigned {
		if bits < 64 && u > uint64(1)<<bits-1 {
			return misfit("the number is out of range for %s", t)
		}
		return nil
	}
	if u > uint64(1)<<(bits-1)-1 {
		return misfit("the number is out of range for %s", t)
	}
	return nil
}

func (t Type) checkDecimal(v Value) error {
	if v.special != Normal || t.Precision == 0 {
		return nil
	}
	msd, lsd, zero := decimalDigits(v.str)
	if zero {
		return nil
	}
	if lsd < -t.Scale {
		return misfit("the number has more decimal places than %s allows", t)
	}
	if msd >= t.Precision-t.Scale {
		return misfit("the number is too large for %s", t)
	}
	return nil
}

func (t Type) checkTemporal(v Value) error {
	if v.tz != t.TZ {
		if v.tz {
			return misfit("a value with a time zone cannot go in a %s column", t)
		}
		return misfit("a value without a time zone cannot go in a %s column", t)
	}
	if v.special != Normal {
		return nil
	}
	nanos := int64(v.num) % int64(1e9)
	if v.kind == KindTimestamp {
		nanos = int64(v.ts.Nanosecond())
	}
	if unit := int64(math.Pow10(9 - t.Precision)); nanos%unit != 0 {
		return misfit("the value has more fractional-second digits than %s allows", t)
	}
	return nil
}
