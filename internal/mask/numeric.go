package mask

import (
	"errors"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/exarth-labs/stuntdb/internal/value"
)

// basisPoints is the unit of noise: 1/10000.
const basisPoints = 10000

func noiseTransformer() *Transformer {
	return &Transformer{
		Name: "noise",
		Summary: "Changes a number by a keyed random amount of up to percent (1 to 100, default 10) of its value, up or down. " +
			"Integers and decimals are rounded to their own precision, so zero, and a number too small for the change to reach " +
			"one unit, stay the same. Results fit wherever the input fits: an integer stays within the integer types it fits, " +
			"a decimal keeps its decimal places and its number of whole digits, computed exactly.",
		Types:   []value.TypeKind{value.TypeInt, value.TypeFloat, value.TypeDecimal},
		Options: []string{"percent"},
		alike:   noiseClass,
		build: func(_ target, o *Options) (*fn, error) {
			percent := o.Int("percent", 10, 1, 100)
			return &fn{seeded: true, mask: func(c *Context, in value.Value) (value.Value, error) {
				// A factor of 1 ± k/10000, with k from 1 to percent·100. The
				// factor is never 1, but rounding can give back the input: the
				// result surely differs only when |in|·k/10000 > ½ unit.
				k := int64(c.Rand.Between(1, percent*100))
				if c.Rand.IntN(2) == 0 {
					k = -k
				}
				return noisy(in, big.NewRat(basisPoints+k, basisPoints))
			}}, nil
		},
	}
}

// noiseClass is noise's class of a column type. noise keeps an integer
// within the integer types it fits, and a decimal within its whole digits
// and decimal places, so equal values come out alike only in columns of
// one kind, and for decimals, of one scale: 500 noised by 1.23% is 506 as
// an integer, 506.15 in a decimal(12,2) column.
func noiseClass(typ value.Type) string {
	if typ.Kind == value.TypeDecimal {
		return "decimal scale " + strconv.Itoa(typ.Scale)
	}
	return typ.Kind.String()
}

// noisy multiplies a number by factor, which is from 0 to 2, and keeps the
// result where the input fits, from the value alone (see the noise Summary).
func noisy(in value.Value, factor *big.Rat) (value.Value, error) {
	switch in.Kind() {
	case value.KindFloat:
		f := in.Float()
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return in, nil
		}
		ff, _ := factor.Float64()
		out := f * ff
		if math.Abs(f) <= math.MaxFloat32 { // it fits a float32, so the result must too
			out = math.Max(-math.MaxFloat32, math.Min(math.MaxFloat32, out))
		}
		return value.Float(out), nil
	case value.KindInt, value.KindUint:
		mag, neg, _ := wholeNumber(in)
		x, _ := new(big.Int).SetString(mag, 10)
		if neg {
			x.Neg(x)
		}
		n := roundHalfAway(new(big.Rat).Mul(new(big.Rat).SetInt(x), factor), 0)
		// The factor is not negative, so the sign is kept or the result is
		// zero; the magnitude stays within the smallest type that holds the
		// input.
		if lim, ok := new(big.Int).SetString(limitOf(mag, neg), 10); ok && new(big.Int).Abs(n).Cmp(lim) > 0 {
			n = lim.Mul(lim, big.NewInt(int64(n.Sign())))
		}
		return numberOfKind(in.Kind(), n.String())
	case value.KindDecimal:
		if in.Special() != value.Normal {
			return in, nil
		}
		text := in.DecimalText()
		_, frac, _ := strings.Cut(text, ".")
		scale := len(frac)
		r, ok := new(big.Rat).SetString(text)
		if !ok {
			return value.Value{}, errors.New("noise cannot read the decimal")
		}
		scaled := roundHalfAway(r.Mul(r, factor), scale)
		// The largest magnitude with the input's whole digits, written with
		// its decimal places: the input fit its column's precision, so
		// this does too.
		whole, _, _ := wholeNumber(in)
		digits := len(whole)
		if whole == "0" {
			digits = 0
		}
		limit := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(digits+scale)), nil)
		limit.Sub(limit, big.NewInt(1))
		if new(big.Int).Abs(scaled).Cmp(limit) > 0 {
			scaled = limit.Mul(limit, big.NewInt(int64(scaled.Sign())))
		}
		return value.Decimal(decimalText(scaled, scale))
	default:
		return value.Value{}, errKind("noise", in)
	}
}

// roundHalfAway returns r·10^scale rounded to the nearest whole number,
// halves away from zero.
func roundHalfAway(r *big.Rat, scale int) *big.Int {
	x := new(big.Rat).Mul(r, new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale)), nil)))
	num, den := new(big.Int).Abs(x.Num()), x.Denom()
	q, m := new(big.Int).QuoRem(num, den, new(big.Int))
	if m.Lsh(m, 1).Cmp(den) >= 0 {
		q.Add(q, big.NewInt(1))
	}
	if x.Sign() < 0 {
		q.Neg(q)
	}
	return q
}

// decimalText writes n·10^-scale with exactly scale decimal places.
func decimalText(n *big.Int, scale int) string {
	s := new(big.Int).Abs(n).String()
	if scale > 0 {
		if len(s) <= scale {
			s = strings.Repeat("0", scale-len(s)+1) + s
		}
		s = s[:len(s)-scale] + "." + s[len(s)-scale:]
	}
	if n.Sign() < 0 {
		s = "-" + s
	}
	return s
}

// The dates every engine can store. A shift that would leave this range
// goes the other way instead.
var (
	minDate = time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC)
	maxDate = time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC)
)

func dateShiftTransformer() *Transformer {
	return &Transformer{
		Name:      "date_shift",
		readsText: true, // ISO 8601 dates in text columns
		Summary: "Moves a date or timestamp by a keyed number of days, from 1 to max_days (1 to 36500, default 365), " +
			"earlier or later. The time of day and time zone are kept. Text in ISO 8601 form is shifted too. " +
			"With per: <key column>, the columns with the same max_days move all of one key's dates by the same " +
			"number of days, so their order and the gaps between them are kept.",
		Types:   []value.TypeKind{value.TypeDate, value.TypeTimestamp, value.TypeString, value.TypeText},
		Options: []string{"max_days"},
		build: func(_ target, o *Options) (*fn, error) {
			maxDays := o.Int("max_days", 365, 1, 36500)
			return &fn{seeded: true, mask: func(c *Context, in value.Value) (value.Value, error) {
				days := c.Rand.Between(1, maxDays)
				if c.Rand.IntN(2) == 0 {
					days = -days
				}
				return shiftDate(in, days)
			}}, nil
		},
	}
}

func shiftDate(in value.Value, days int) (value.Value, error) {
	if in.Special() != value.Normal {
		return in, nil // infinities and zero dates have no place in time
	}
	switch in.Kind() {
	case value.KindDate:
		y, m, d := in.Date()
		t := shift(time.Date(y, m, d, 0, 0, 0, 0, time.UTC), days)
		return value.Date(t.Year(), t.Month(), t.Day())
	case value.KindTimestamp:
		t := shift(in.Timestamp(), days)
		_, hasZone := in.Zone()
		switch {
		case !in.IsTZ():
			return value.Timestamp(t), nil
		case hasZone:
			return value.TimestampTZWithZone(t), nil
		default:
			return value.TimestampTZ(t), nil
		}
	case value.KindString:
		return shiftDateText(in.Text(), days)
	default:
		return value.Value{}, errKind("date_shift", in)
	}
}

// shift adds days in the calendar, so the time of day never moves, and
// goes the other way when the result would leave the years 1 to 9999.
func shift(t time.Time, days int) time.Time {
	out := t.AddDate(0, 0, days)
	if out.Before(minDate) || out.After(maxDate) {
		out = t.AddDate(0, 0, -days)
	}
	return out
}

// shiftDateText shifts a date or timestamp written as ISO 8601 text, and
// writes it back the same way.
func shiftDateText(s string, days int) (value.Value, error) {
	if v, err := value.ParseISO(value.KindDate, s, 'T'); err == nil {
		out, err := shiftDate(v, days)
		if err != nil {
			return value.Value{}, err
		}
		text, _ := value.AppendISO(nil, out, 'T')
		return value.String(string(text)), nil
	}
	for _, sep := range []byte{'T', ' '} {
		if v, err := value.ParseISO(value.KindTimestamp, s, sep); err == nil {
			out, err := shiftDate(v, days)
			if err != nil {
				return value.Value{}, err
			}
			text, _ := value.AppendISO(nil, out, sep)
			return value.String(string(text)), nil
		}
	}
	return value.Value{}, errors.New("date_shift cannot read the text as an ISO 8601 date or timestamp")
}
