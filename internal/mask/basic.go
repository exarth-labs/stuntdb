package mask

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/exarth-labs/stuntdb/internal/value"
)

// builtins lists the built-in transformers.
func builtins() []*Transformer {
	return []*Transformer{
		keepTransformer(), nullTransformer(), constantTransformer(), defaultTransformer(),
		firstNameTransformer(), lastNameTransformer(), fullNameTransformer(),
		usernameTransformer(), emailTransformer(), companyTransformer(),
		phoneTransformer(), addressTransformer(),
		uuidTransformer(), fpeTransformer(), hashTransformer(), randomTokenTransformer(),
		passwordHashTransformer(), redactTransformer(),
		noiseTransformer(), dateShiftTransformer(), synthesizeTextTransformer(),
	}
}

// Type sets that several transformers accept.
var (
	allTypes = []value.TypeKind{
		value.TypeBool, value.TypeInt, value.TypeFloat, value.TypeDecimal, value.TypeString,
		value.TypeText, value.TypeBinary, value.TypeDate, value.TypeTime, value.TypeTimestamp,
		value.TypeInterval, value.TypeUUID, value.TypeJSON, value.TypeEnum, value.TypeArray,
		value.TypeOpaque,
	}
	textTypes = []value.TypeKind{value.TypeString, value.TypeText}
)

// fn is a Func made of its parts.
type fn struct {
	guarantee Guarantee
	seeded    bool
	mask      func(c *Context, in value.Value) (value.Value, error)

	// fit checks that a column can hold every output: its length, NOT NULL
	// or default. It never changes the outputs. Its errors explain what to
	// change. nil means every column of an accepted type can.
	fit func(col Column) error

	// omit marks default: the column is left out of the copy, so that the
	// target fills it with its default.
	omit bool
}

func (f *fn) Guarantee() Guarantee { return f.guarantee }
func (f *fn) Seeded() bool         { return f.seeded }
func (f *fn) Mask(c *Context, in value.Value) (value.Value, error) {
	return f.mask(c, in)
}

// Omits reports whether a masking function leaves its column out of the
// copy, for the target database to fill with the column's default.
func Omits(f Func) bool {
	c, ok := f.(*checked)
	return ok && c.omit
}

// maxLen returns the most characters a text column holds, or bytes a binary
// one, or 0 for no limit.
func maxLen(col Column) int {
	switch col.Type.Kind {
	case value.TypeString, value.TypeBinary:
		return col.Type.MaxLen
	default:
		return 0
	}
}

// unit names what a column's length counts.
func unit(col Column) string {
	if col.Type.Kind == value.TypeBinary {
		return "bytes"
	}
	return "characters"
}

// fitLength returns the fit check of a function whose outputs have at most
// longest characters. advice says what to change when the column holds
// only room characters.
func fitLength(name string, longest int, advice func(col Column, room int) string) func(Column) error {
	return func(col Column) error {
		room := maxLen(col)
		if room == 0 || longest <= room {
			return nil
		}
		return fmt.Errorf("%s writes up to %d %s, and the column holds %d%s", name, longest, unit(col), room, advice(col, room))
	}
}

// maxLengthLimit is the largest max_length accepted, far beyond any output.
const maxLengthLimit = 1_000_000

// readMaxLength reads the option max_length of a transformer whose outputs
// need at least least characters. 0 means it is not set: no limit.
func readMaxLength(o *Options, least int) int { return o.Int("max_length", 0, least, maxLengthLimit) }

// maxLengthAdvice is the advice of a transformer that takes max_length and
// needs at least least characters.
func maxLengthAdvice(least int) func(Column, int) string {
	return func(_ Column, room int) string {
		if room >= least {
			return fmt.Sprintf("; set max_length: %d", room)
		}
		return fmt.Sprintf(", fewer than the %d it needs at least", least)
	}
}

// bounded returns the longest output of a transformer whose outputs have
// at most natural characters, under a max_length of n (0: none).
func bounded(natural, n int) int {
	if n > 0 {
		return min(natural, n)
	}
	return natural
}

// asText returns a masked text value in the target's kind: text for text
// columns, bytes for binary ones.
func asText(t target, s string) value.Value {
	if t.kind == value.TypeBinary {
		return value.Bytes([]byte(s))
	}
	return value.String(s)
}

func keepTransformer() *Transformer {
	return &Transformer{
		Name:    "keep",
		Summary: "Copies the value unchanged.",
		Types:   allTypes,
		alike:   anyClass,
		build: func(target, *Options) (*fn, error) {
			return &fn{guarantee: Bijective, mask: func(_ *Context, in value.Value) (value.Value, error) { return in, nil }}, nil
		},
	}
}

func nullTransformer() *Transformer {
	return &Transformer{
		Name:    "null",
		Summary: "Replaces every value with NULL.",
		Types:   allTypes,
		alike:   anyClass,
		build: func(target, *Options) (*fn, error) {
			return &fn{
				mask: func(*Context, value.Value) (value.Value, error) { return value.Null(), nil },
				fit: func(col Column) error {
					if !col.Nullable {
						return errors.New("null cannot go in a NOT NULL column")
					}
					return nil
				},
			}, nil
		},
	}
}

func defaultTransformer() *Transformer {
	return &Transformer{
		Name:    "default",
		Summary: "Leaves the column out, so that the target database fills it with the column's default.",
		Types:   allTypes,
		build: func(target, *Options) (*fn, error) {
			return &fn{
				omit: true,
				mask: func(*Context, value.Value) (value.Value, error) {
					return value.Value{}, errors.New("default leaves the column out of the copy, so it masks no value")
				},
				fit: func(col Column) error {
					if !col.HasDefault {
						return errors.New("default needs a column with a default in the database; use null or constant")
					}
					return nil
				},
			}, nil
		},
	}
}

func constantTransformer() *Transformer {
	return &Transformer{
		Name:    "constant",
		Summary: "Replaces every value with one fixed value, given by the option value.",
		Types: []value.TypeKind{
			value.TypeBool, value.TypeInt, value.TypeFloat, value.TypeDecimal, value.TypeString, value.TypeText,
			value.TypeEnum, value.TypeJSON, value.TypeUUID, value.TypeDate, value.TypeTime, value.TypeTimestamp,
		},
		Options: []string{"value"},
		build: func(t target, o *Options) (*fn, error) {
			text, ok := o.Text("value")
			if !ok {
				return nil, errors.New("constant needs the option value")
			}
			v, err := parseConstant(t.kind, text)
			if err != nil {
				return nil, fmt.Errorf("option value: %w", err)
			}
			// Compared as the value it gives, so that 5 and 05 are one
			// integer, as 5.0 and 5.00 are one decimal.
			o.resolve("value", string(appendCanonical(nil, v)))
			return &fn{
				mask: func(*Context, value.Value) (value.Value, error) { return v, nil },
				fit: func(col Column) error {
					if err := col.Type.Check(v); err != nil {
						return fmt.Errorf("option value does not fit the column: %w", err)
					}
					if col.EmptyStringIsNull && !col.Nullable && v.Kind() == value.KindString && v.Text() == "" {
						return errors.New("option value is an empty string, which this database stores as NULL, and the column is NOT NULL")
					}
					return nil
				},
			}, nil
		},
	}
}

// parseConstant reads the constant transformer's value for a column kind.
// Whether it fits the column's type is checked when the column is known.
// Its errors never repeat the text.
func parseConstant(k value.TypeKind, s string) (value.Value, error) {
	switch k {
	case value.TypeBool:
		switch s {
		case "true":
			return value.Bool(true), nil
		case "false":
			return value.Bool(false), nil
		}
		return value.Value{}, errors.New("must be true or false")
	case value.TypeInt:
		if i, err := strconv.ParseInt(s, 10, 64); err == nil {
			return value.Int(i), nil
		}
		if u, err := strconv.ParseUint(s, 10, 64); err == nil {
			return value.Uint(u), nil
		}
		return value.Value{}, errors.New("must be a whole number")
	case value.TypeFloat:
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return value.Value{}, errors.New("must be a number")
		}
		return value.Float(f), nil
	case value.TypeDecimal:
		v, err := value.Decimal(s)
		if err != nil {
			return value.Value{}, errors.New("must be a plain decimal number, such as 12.50")
		}
		return v, nil
	case value.TypeJSON:
		if !json.Valid([]byte(s)) {
			return value.Value{}, errors.New("must be valid JSON")
		}
		return value.String(s), nil
	case value.TypeUUID:
		v, err := value.ParseUUID(s)
		if err != nil {
			return value.Value{}, errors.New("must be a UUID")
		}
		return v, nil
	case value.TypeDate:
		v, err := value.ParseISO(value.KindDate, s, 'T')
		if err != nil {
			return value.Value{}, errors.New("must be a date, such as 2026-01-31")
		}
		return v, nil
	case value.TypeTime:
		v, err := value.ParseISO(value.KindTime, s, 'T')
		if err != nil {
			return value.Value{}, errors.New("must be a time of day, such as 13:45:00")
		}
		return v, nil
	case value.TypeTimestamp:
		v, err := value.ParseISO(value.KindTimestamp, s, 'T')
		if err != nil {
			if v, err = value.ParseISO(value.KindTimestamp, s, ' '); err != nil {
				return value.Value{}, errors.New("must be a timestamp, such as 2026-01-31T13:45:00, with an offset such as Z or +05:00 for a column with a time zone")
			}
		}
		return v, nil
	default: // text, enum
		return value.String(s), nil
	}
}
