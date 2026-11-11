package mask

import (
	"cmp"
	"math/big"
	"strconv"
	"strings"

	"github.com/exarth-labs/stuntdb/internal/value"
)

// Whole numbers keep their place among the standard integer types. A
// masked whole number fits every integer type its input fits (int8 to
// uint64), whatever column it is in: that depends only on the value, so
// the two ends of a foreign key get the same output even when one is an
// int4 and the other an int8.

// bound is the largest magnitude of a standard integer type, and the
// number after it, as digits.
type bound struct{ max, next string }

func bounds(maxima ...string) []bound {
	out := make([]bound, len(maxima))
	for i, m := range maxima {
		n, ok := new(big.Int).SetString(m, 10)
		if !ok {
			panic("mask: bad integer bound " + m)
		}
		out[i] = bound{max: m, next: n.Add(n, big.NewInt(1)).String()}
	}
	return out
}

var (
	// positiveBounds are the largest values of int8, uint8, int16, uint16,
	// int32, uint32, int64 and uint64.
	positiveBounds = bounds("127", "255", "32767", "65535", "2147483647", "4294967295",
		"9223372036854775807", "18446744073709551615")

	// negativeBounds are the largest magnitudes of negative int8, int16,
	// int32 and int64 values.
	negativeBounds = bounds("128", "32768", "2147483648", "9223372036854775808")
)

func boundsFor(neg bool) []bound {
	if neg {
		return negativeBounds
	}
	return positiveBounds
}

// compareMagnitudes compares two magnitudes written as digits without
// leading zeros.
func compareMagnitudes(a, b string) int {
	if len(a) != len(b) {
		return cmp.Compare(len(a), len(b))
	}
	return strings.Compare(a, b)
}

// wholeNumber returns the magnitude of an integer, or of a decimal's whole
// part, as digits without leading zeros, and whether it is negative. Zero
// is never negative.
func wholeNumber(in value.Value) (mag string, neg, ok bool) {
	var s string
	switch in.Kind() {
	case value.KindInt:
		s = intText(in.Int())
	case value.KindUint:
		s = uintText(in.Uint())
	case value.KindDecimal:
		if in.Special() != value.Normal {
			return "", false, false
		}
		s, _, _ = strings.Cut(normalizeDecimal(in.DecimalText()), ".")
	default:
		return "", false, false
	}
	mag, neg = strings.CutPrefix(s, "-")
	return mag, neg && mag != "0", true
}

// numberClass is the set of magnitudes a whole number is masked within:
// those with its count of digits that lie between the same two standard
// bounds. lo and hi have exactly that count of digits, so digit strings of
// that length compare with them as text. The classes of one sign and one
// count of digits don't overlap, so a walk that stays in its class keeps a
// permutation a permutation.
type numberClass struct{ lo, hi string }

// classOf returns the class of a magnitude of the given sign. The class of
// zero is the digits 1 to 9.
func classOf(mag string, neg bool) numberClass {
	n := len(mag)
	c := numberClass{lo: "1" + strings.Repeat("0", n-1), hi: strings.Repeat("9", n)}
	for _, b := range boundsFor(neg) {
		if compareMagnitudes(mag, b.max) <= 0 {
			if len(b.max) == n {
				c.hi = b.max
			}
			return c
		}
		if len(b.next) == n {
			c.lo = b.next
		}
	}
	return c
}

// has reports whether digits, of the class's length, lie in the class.
func (c numberClass) has(digits string) bool { return digits >= c.lo && digits <= c.hi }

// limitOf returns the largest magnitude of the smallest standard integer
// type that holds a magnitude of the given sign, or "" when none does.
func limitOf(mag string, neg bool) string {
	for _, b := range boundsFor(neg) {
		if compareMagnitudes(mag, b.max) <= 0 {
			return b.max
		}
	}
	return ""
}

// keyedDigits returns a uniform random magnitude of a class.
func keyedDigits(r *Rand, c numberClass) string {
	lo, _ := new(big.Int).SetString(c.lo, 10)
	hi, _ := new(big.Int).SetString(c.hi, 10)
	span := hi.Sub(hi, lo)
	span.Add(span, big.NewInt(1))
	return lo.Add(lo, r.bigN(span)).String()
}

// randomDigits replaces a whole number with a random one of its class, with
// the same sign, as a value of the same kind. A decimal's fraction is
// dropped.
func randomDigits(name string, r *Rand, in value.Value) (value.Value, error) {
	mag, neg, ok := wholeNumber(in)
	if !ok {
		return value.Value{}, errKind(name, in)
	}
	digits := keyedDigits(r, classOf(mag, neg))
	if neg {
		digits = "-" + digits
	}
	return numberOfKind(in.Kind(), digits)
}

// numberOfKind returns a whole number, written in digits, as a value of
// kind k: KindInt, KindUint or KindDecimal.
func numberOfKind(k value.Kind, digits string) (value.Value, error) {
	switch k {
	case value.KindInt:
		i, err := strconv.ParseInt(digits, 10, 64)
		return value.Int(i), err
	case value.KindUint:
		u, err := strconv.ParseUint(digits, 10, 64)
		return value.Uint(u), err
	default:
		return value.Decimal(digits)
	}
}

func digitText(d []uint16) string {
	b := make([]byte, len(d))
	for i, x := range d {
		b[i] = byte('0' + x) //nolint:gosec // a decimal digit, below 10
	}
	return string(b)
}
