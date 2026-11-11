package mask

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/exarth-labs/stuntdb/internal/value"
)

// numberTypes are the numeric column types that phone numbers and postcodes
// can go in, as digits.
var numberTypes = []value.TypeKind{value.TypeInt, value.TypeDecimal}

// isNumber reports whether a target is a numeric column.
func isNumber(t target) bool { return t.kind == value.TypeInt || t.kind == value.TypeDecimal }

// digitsFit returns the fit check of a function that writes whole numbers
// of n digits into a numeric column: the column's type must hold every one.
func digitsFit(what string, n int) func(Column) error {
	return func(col Column) error {
		t := col.Type
		fits := true
		switch t.Kind {
		case value.TypeInt:
			limit := uint64(1)<<(t.Bits-1) - 1 // signed maximum
			if t.Unsigned {
				limit = uint64(1)<<(t.Bits-1)<<1 - 1
			}
			fits = n <= len(strconv.FormatUint(limit, 10))-1
		case value.TypeDecimal:
			fits = t.Precision == 0 || t.Precision-t.Scale >= n
		}
		if !fits {
			return fmt.Errorf("%s have %d digits, which don't always fit a %s column", what, n, t)
		}
		return nil
	}
}

// numberOf returns a whole number, written in digits, as a value for a
// numeric target: a decimal for a decimal column, an integer otherwise.
func numberOf(t target, digits string) (value.Value, error) {
	if t.kind == value.TypeDecimal {
		return value.Decimal(digits)
	}
	i, err := strconv.ParseInt(digits, 10, 64)
	return value.Int(i), err
}

// phoneFormats are the ways to write a phone number as text, longest first.
var phoneFormats = []string{"international", "national", "digits"}

// phonePlan is one region's fictional numbers.
type phonePlan struct {
	texts  func(r *Rand) [3]string // the number in each of phoneFormats
	digits func(r *Rand) string    // the number as digits, for numeric columns
	length int                     // the number of digits
	widths [3]int                  // the characters of each of phoneFormats
}

var phonePlans = map[string]phonePlan{
	// NANP: the lines 555-0100 to 555-0199 are reserved for fiction in
	// every area code.
	"US": {
		texts: func(r *Rand) [3]string {
			area, line := Pick(r, areaCodes), fmt.Sprintf("01%02d", r.IntN(100))
			return [3]string{"+1-" + area + "-555-" + line, area + "-555-" + line, area + "555" + line}
		},
		digits: func(r *Rand) string { return Pick(r, areaCodes) + "55501" + fmt.Sprintf("%02d", r.IntN(100)) },
		length: 10,
		widths: [3]int{15, 12, 10},
	},
	// Ofcom reserves the mobile numbers 07700 900000 to 07700 900999 for
	// drama. As digits, the number is written with the country code, 44,
	// because a number column would drop the leading zero.
	"GB": {
		texts: func(r *Rand) [3]string {
			line := fmt.Sprintf("%03d", r.IntN(1000))
			return [3]string{"+44 7700 900" + line, "07700 900" + line, "07700900" + line}
		},
		digits: func(r *Rand) string { return "447700900" + fmt.Sprintf("%03d", r.IntN(1000)) },
		length: 12,
		widths: [3]int{15, 12, 11},
	},
}

func phoneTransformer() *Transformer {
	return &Transformer{
		Name: "phone",
		Summary: "A phone number from a range reserved for fiction, so that no call reaches a real person: " +
			"555-0100 to 555-0199 in the US (region: US), 07700 900000 to 900999 in the UK (region: GB). " +
			"Text is written in a format: international (the default, +1-201-555-0123), national (201-555-0123) or digits (2015550123). " +
			"A number column gets the digits, with the country code for the UK.",
		Types:   append(append([]value.TypeKind{}, textTypes...), numberTypes...),
		Options: []string{"region", "format"},
		build: func(t target, o *Options) (*fn, error) {
			plan := phonePlans[o.OneOf("region", "US", "US", "GB")]
			if isNumber(t) {
				o.refuse("format", "a number column always gets the digits")
				return &fn{
					seeded: true,
					mask: func(c *Context, _ value.Value) (value.Value, error) {
						return numberOf(t, plan.digits(c.Rand))
					},
					fit: digitsFit("phone numbers", plan.length),
				}, nil
			}
			format := slices.Index(phoneFormats, o.OneOf("format", phoneFormats[0], phoneFormats...))
			return &fn{
				seeded: true,
				mask: func(c *Context, _ value.Value) (value.Value, error) {
					return value.String(plan.texts(c.Rand)[format]), nil
				},
				fit: fitLength("phone", plan.widths[format], func(_ Column, room int) string {
					var shorter []string
					for i := format + 1; i < len(phoneFormats); i++ {
						if plan.widths[i] <= room {
							shorter = append(shorter, fmt.Sprintf("%s (%d characters)", phoneFormats[i], plan.widths[i]))
						}
					}
					if len(shorter) == 0 {
						return fmt.Sprintf(", fewer than the %d of its shortest format, digits", plan.widths[len(phoneFormats)-1])
					}
					return "; set format: " + strings.Join(shorter, " or ")
				}),
			}, nil
		},
	}
}

// addressParts are the values of the address transformer's part option.
var addressParts = []string{"full", "street", "city", "state", "postcode"}

// addressLengths returns the fewest characters a part of an address needs,
// and the most it can have.
func addressLengths(part string) (least, most int) {
	street := 4 + 1 + longest(streetNames) + 1 + longest(streetSuffixes) // 9999 Name Suffix
	switch part {
	case "street":
		return shortest(streetNames), street
	case "city":
		return shortest(cities), longest(cities)
	case "state":
		return 2, 2
	case "postcode":
		return 5, 5
	default: // full: street, city, state and postcode
		return shortest(streetNames), street + 2 + longest(cities) + 2 + 2 + 1 + 5
	}
}

func addressTransformer() *Transformer {
	return &Transformer{
		Name: "address",
		Summary: "A United States address, or one part of it (part: full, street, city, state or postcode). " +
			"Columns with the same per: <key column> describe one address per key. max_length bounds the text.",
		Types:   append(append([]value.TypeKind{}, textTypes...), numberTypes...),
		Options: []string{"part", "max_length"},
		build: func(t target, o *Options) (*fn, error) {
			part := o.OneOf("part", "full", addressParts...)
			if isNumber(t) {
				o.refuse("max_length", "a number column holds the postcode's 5 digits")
				if part != "postcode" {
					return nil, errors.New("only part: postcode can go in a number column")
				}
				return &fn{
					seeded: true,
					mask: func(c *Context, _ value.Value) (value.Value, error) {
						return numberOf(t, strconv.Itoa(postcode(c.Rand)))
					},
					fit: digitsFit("postcodes", 5),
				}, nil
			}
			least, most := addressLengths(part)
			n := readMaxLength(o, least)
			return &fn{
				seeded: true,
				mask: func(c *Context, _ value.Value) (value.Value, error) {
					return value.String(addressPart(c.Rand, part, n)), nil
				},
				fit: fitLength("address", bounded(most, n), maxLengthAdvice(least)),
			}, nil
		},
	}
}

// addressPart writes one part of a fake address, fitting n characters (0:
// no limit). Every part draws every component in the same order, so that
// columns seeded from the same key (per:) describe one address.
func addressPart(r *Rand, part string, n int) string {
	number := strconv.Itoa(r.Between(1, 9999))
	name, suffix := Pick(r, streetNames), Pick(r, streetSuffixes)
	city, state, zip := Pick(r, cities), Pick(r, states), strconv.Itoa(postcode(r))
	street := number + " " + name + " " + suffix

	var s string
	var ok bool
	switch part {
	case "street":
		s, ok = firstFitting(n, street, name+" "+suffix, name)
	case "city":
		s, ok = firstFitting(n, city)
	case "state":
		return state
	case "postcode":
		return zip
	default:
		s, ok = firstFitting(n, street+", "+city+", "+state+" "+zip, street+", "+city, street, name+" "+suffix, name)
	}
	if ok {
		return s
	}
	if part == "city" {
		return Pick(r, fitting(cities, n))
	}
	return Pick(r, fitting(streetNames, n))
}

// postcode returns a five-digit ZIP code that doesn't start with zero, so
// that it keeps five digits in a number column.
func postcode(r *Rand) int { return r.Between(10000, 99999) }
