package mask

import (
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/exarth-labs/stuntdb/internal/value"
)

// The fuzz tests mask any value with every transformer that takes its
// column, under options and columns chosen from the fuzz input, and check
// what every masking must do (checkMasking). Inputs they find stay in
// testdata/fuzz as regression cases, which every go test runs.

// valueRefusals are the refusals that depend on the value, not on the
// column: a transformer that reads one text format, a value too short for
// a keyed permutation or a distinct token, or a fraction for fpe in a
// decimal column without a scale. Any other error for a value that fits
// its column is a bug: a fit check promised more than the transformer keeps.
var valueRefusals = []string{
	"date_shift cannot read the text as an ISO 8601 date or timestamp",
	"uuid cannot read the text as a UUID",
	"uuid needs 16 bytes, and the value has another length",
	"and this value has fewer", // fpe: FF1 needs a million possible values
	"fpe masks whole numbers, and the value has decimal places",
	errTokenTooShort.Error(),
}

// leakMarker surrounds text inputs. No error may show it: an error that
// formats the value, or its start or end, shows it.
const leakMarker = "Qz7mK"

// checkMasking masks in under rule, the way a RowMasker does, and checks:
//  1. nothing panics (the fuzzer reports a panic as a failure)
//  2. one value gives one output, also from a masker that masked another
//     value before, and twice in a row
//  3. a value that fits the column is masked, unless the refusal is in
//     valueRefusals
//  4. no error shows the value: leaked reports whether a message does, for
//     in and for marked, a variant of in that carries a marker
//
// A rule Configure refuses is skipped: refusing it is Configure's job.
func checkMasking(t *testing.T, k *Keys, col Column, rule Rule, in, marked value.Value, leaked func(string) bool) {
	t.Helper()
	f, err := Configure(col, rule)
	if err != nil || Omits(f) {
		return
	}
	newMasker := func() *masker {
		return &masker{t: t, use: rule.Use, f: f, seeder: newSeeder(k), rand: newRand(), ctx: Context{Keys: k, Domain: rule.DomainFor(col.Ref)}}
	}
	out, err := newMasker().mask(in)
	used := newMasker()
	_, markedErr := used.mask(marked)
	again, againErr := used.mask(in)
	// Twice in a row too: state that flips between calls can't hide.
	third, thirdErr := used.mask(in)

	what := rule.Use + " in a column of type " + col.Type.String()
	for _, e := range []error{err, markedErr} {
		if e != nil && leaked(e.Error()) {
			t.Fatalf("%s: an error shows the value (the input is saved by the fuzzer)", what)
		}
	}
	switch {
	case (err == nil) != (againErr == nil) || (err == nil) != (thirdErr == nil):
		t.Fatalf("%s: one value is masked once and refused once: %v / %v / %v", what, err, againErr, thirdErr)
	case err == nil && (!out.Equal(again) || !out.Equal(third)):
		t.Fatalf("%s: one value gives two outputs", what)
	case err != nil && fits(col, in) && !isValueRefusal(err):
		t.Fatalf("%s: a value that fits the column is refused: %v", what, err)
	}
}

// fits reports whether a database could hold in in col: its natural kind,
// within the type's limits. An engine that stores an empty string as NULL
// never holds an empty string.
func fits(col Column, in value.Value) bool {
	if col.EmptyStringIsNull && in.Kind() == value.KindString && in.Text() == "" {
		return false
	}
	return natural(col.Type, in.Kind()) && col.Type.Check(in) == nil
}

func isValueRefusal(err error) bool {
	for _, r := range valueRefusals {
		if strings.Contains(err.Error(), r) {
			return true
		}
	}
	return false
}

// fuzzColumn is a column with the flags of the fuzz input: bit 0 nullable,
// 1 unique, 2 an engine that stores an empty string as NULL.
func fuzzColumn(typ value.Type, flags uint8) Column {
	c := column(typ)
	c.Nullable = flags&1 != 0
	c.Unique = flags&2 != 0
	c.EmptyStringIsNull = flags&4 != 0
	return c
}

// fuzzOptions sets some of a transformer's options: bit i of set sets its
// i-th option, to a value chosen by n or to text. Some values are invalid;
// Configure then refuses the rule, and the input is skipped.
func fuzzOptions(tr *Transformer, set uint8, n uint16, text string) map[string]any {
	opts := map[string]any{}
	pick := func(choices ...string) string { return choices[int(n)%len(choices)] }
	for i, name := range tr.Options {
		if set&(1<<i) == 0 {
			continue
		}
		switch name {
		case "max_length", "length", "max_days", "percent":
			opts[name] = strconv.Itoa(int(n))
		case "format":
			opts[name] = pick("international", "national", "digits")
		case "region":
			opts[name] = pick("US", "GB")
		case "part":
			opts[name] = pick("full", "street", "city", "state", "postcode")
		case "alphabet":
			opts[name] = pick("alnum", "digits", "lower", "upper")
		default: // constant's value, password_hash's hash
			opts[name] = text
		}
	}
	return opts
}

// eachRule calls check with a rule for every transformer that takes col.
// Bit 3 of flags marks the domain distinct.
func eachRule(col Column, flags, set uint8, n uint16, text string, check func(Rule)) {
	for _, tr := range Catalog() {
		if tr.Accepts(col.Type.Kind) {
			check(Rule{Use: tr.Name, Options: fuzzOptions(tr, set, n, text), Distinct: flags&8 != 0})
		}
	}
}

// FuzzMaskText masks any text, JSON, bytes or UUID.
func FuzzMaskText(f *testing.F) {
	for _, s := range []string{
		"", "a", "Ada Lovelace", "ada@example.com", "+1 202 555 0143", "2026-01-31", "2026-01-31T13:45:00Z",
		"0b8e5c1e-6b8f-4c3a-9d2e-1f0a2b3c4d5e", `{"a":1}`, "tok_demo_0123456789abcdef", "\x00", "\xff\xfe",
		"Zoë Ångström", strings.Repeat("x", 300),
	} {
		f.Add(s, uint8(0), uint16(0), uint8(1), uint8(0), uint16(0), "")
		f.Add(s, uint8(0), uint16(8), uint8(0b1011), uint8(1), uint16(12), "fixed")
	}
	k := testKeys(f)
	f.Fuzz(func(t *testing.T, s string, kind uint8, maxLen uint16, flags, set uint8, n uint16, text string) {
		var typ value.Type
		var in, marked value.Value
		leaked := func(msg string) bool { return strings.Contains(msg, leakMarker) }
		switch kind % 5 {
		case 0:
			typ = value.Type{Kind: value.TypeString, MaxLen: int(maxLen), Fixed: flags&16 != 0}
			in, marked = value.String(s), value.String(leakMarker+s+leakMarker)
		case 1:
			typ = value.Type{Kind: value.TypeText}
			in, marked = value.String(s), value.String(leakMarker+s+leakMarker)
		case 2:
			typ = value.Type{Kind: value.TypeJSON}
			in, marked = value.String(s), value.String(`"`+leakMarker+s+leakMarker+`"`)
		case 3:
			typ = value.Type{Kind: value.TypeBinary, MaxLen: int(maxLen)}
			in, marked = value.Bytes([]byte(s)), value.Bytes([]byte(leakMarker+s+leakMarker))
		default:
			typ = value.Type{Kind: value.TypeUUID}
			var u [16]byte
			copy(u[:], s)
			in, marked = value.UUID(u), value.UUID(u)
			leaked = func(msg string) bool { return strings.Contains(msg, value.FormatUUID(u)) }
		}
		col := fuzzColumn(typ, flags)
		eachRule(col, flags, set, n, text, func(r Rule) { checkMasking(t, k, col, r, in, marked, leaked) })
	})
}

// FuzzMaskNumber masks any integer, decimal or float, and text in a number
// column, which SQLite allows.
func FuzzMaskNumber(f *testing.F) {
	for _, c := range []struct {
		i int64
		u uint64
		d string
	}{
		{0, 0, "0"},
		{123456, 123456, "1234.56"},
		{-1, math.MaxUint64, "-0.001"},
		{math.MinInt64, 1 << 63, "99999999999999999999.99"},
		{math.MaxInt64, 255, "1e5"},
	} {
		for kind := range uint8(3) {
			f.Add(c.i, c.u, float64(c.i), c.d, kind, uint8(3), uint8(0), int8(0), uint8(1), uint8(0), uint16(0))
		}
	}
	f.Add(int64(0), uint64(0), math.Inf(-1), "x", uint8(2), uint8(1), uint8(0), int8(0), uint8(0b1000001), uint8(1), uint16(5))
	f.Add(int64(0), uint64(0), math.NaN(), "NaN", uint8(1), uint8(0), uint8(10), int8(2), uint8(0b100001), uint8(1), uint16(5))
	// A scale above the precision: decimal(2,4) holds less than 0.01.
	for _, d := range []string{"0.0099", "0.0098", "0.0095", "-0.0099"} {
		f.Add(int64(0), uint64(0), float64(0), d, uint8(1), uint8(0), uint8(2), int8(4), uint8(0), uint8(1), uint16(50))
	}
	k := testKeys(f)
	f.Fuzz(func(t *testing.T, i int64, u uint64, fl float64, d string, kind, bits, precision uint8, scale int8, flags, set uint8, n uint16) {
		var typ value.Type
		var in value.Value
		switch kind % 3 {
		case 0:
			typ = value.Type{Kind: value.TypeInt, Bits: 8 << (bits % 4), Unsigned: flags&16 != 0}
			in = value.Int(i)
			if flags&32 != 0 {
				in = value.Uint(u)
			}
		case 1:
			typ = value.Type{Kind: value.TypeDecimal, Precision: int(precision % 50), Scale: int(scale % 20)}
			switch v, err := value.Decimal(d); {
			case flags&32 != 0:
				in = value.DecimalInfinity(i < 0)
				if i == 0 {
					in = value.DecimalNaN()
				}
			case err == nil:
				in = v
			default:
				in, _ = value.Decimal(strconv.FormatInt(i, 10))
			}
		default:
			typ = value.Type{Kind: value.TypeFloat, Bits: 32 << (bits % 2)}
			in = value.Float(fl)
		}
		if flags&64 != 0 {
			in = value.String(d)
		}
		if typ.Validate() != nil {
			return
		}
		if beyondScale(typ, in) {
			return // no database returns it
		}
		text := numberText(in)
		leaked := func(msg string) bool { return len(text) >= 6 && strings.Contains(msg, text) }
		col := fuzzColumn(typ, flags)
		eachRule(col, flags, set, n, d, func(r Rule) { checkMasking(t, k, col, r, in, in, leaked) })
	})
}

// beyondScale reports whether a decimal has more decimal places than its
// column's scale, such as 5.0 in decimal(19,0). Check accepts it (it is
// the number 5), but databases return a column's values at its scale, and
// noise keeps a value's own decimal places.
func beyondScale(typ value.Type, in value.Value) bool {
	if typ.Kind != value.TypeDecimal || typ.Precision == 0 || in.Kind() != value.KindDecimal || in.Special() != value.Normal {
		return false
	}
	_, frac, _ := strings.Cut(in.DecimalText(), ".")
	return len(frac) > max(typ.Scale, 0)
}

// numberText is how an error would show a number, if it showed one.
func numberText(v value.Value) string {
	switch v.Kind() {
	case value.KindInt:
		return strconv.FormatInt(v.Int(), 10)
	case value.KindUint:
		return strconv.FormatUint(v.Uint(), 10)
	case value.KindFloat:
		return strconv.FormatFloat(v.Float(), 'g', -1, 64)
	case value.KindDecimal:
		return v.DecimalText()
	}
	return v.Text()
}

// FuzzMaskTemporal masks any date or timestamp, naive or zoned, within and
// beyond the years 1 to 9999 (PostgreSQL holds 4713 BC to 5874897 AD), and
// the same instants written as ISO 8601 text.
func FuzzMaskTemporal(f *testing.F) {
	for _, sec := range []int64{
		0, 1_700_000_000,
		time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC).Unix(),
		time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC).Unix(),
		time.Date(9999, 12, 20, 22, 0, 0, 0, time.UTC).Unix(),
		time.Date(-4713, 11, 24, 0, 0, 0, 0, time.UTC).Unix(),
		time.Date(20000, 6, 1, 0, 0, 0, 0, time.UTC).Unix(),
	} {
		for kind := range uint8(4) {
			f.Add(sec, uint32(0), int32(0), kind, uint8(6), uint8(1), uint8(1), uint16(36500), "")
			f.Add(sec, uint32(123456789), int32(5*60+30), kind, uint8(9), uint8(0b10001), uint8(0), uint16(0), "")
			f.Add(sec, uint32(0), int32(-14*60), kind, uint8(0), uint8(0b10001), uint8(1), uint16(1), "")
		}
	}
	f.Add(int64(0), uint32(0), int32(0), uint8(2), uint8(0), uint8(1), uint8(0), uint16(0), "31/01/2026")
	k := testKeys(f)
	f.Fuzz(func(t *testing.T, sec int64, nsec uint32, offsetMinutes int32, kind, precision, flags, set uint8, n uint16, s string) {
		zone := time.FixedZone("", int(offsetMinutes%(24*60))*60)
		at := time.Unix(sec, int64(nsec%1_000_000_000)).In(zone)
		var typ value.Type
		var in value.Value
		switch kind % 4 {
		case 0:
			typ = value.Type{Kind: value.TypeDate}
			v, err := value.Date(at.Year(), at.Month(), at.Day())
			if err != nil {
				return
			}
			in = v
		case 1:
			typ = value.Type{Kind: value.TypeTimestamp, Precision: int(precision % 10), TZ: flags&16 != 0}
			switch {
			case !typ.TZ:
				in = value.Timestamp(at)
			case flags&32 != 0:
				in = value.TimestampTZ(at)
			default:
				in = value.TimestampTZWithZone(at)
			}
		default:
			typ = value.Type{Kind: value.TypeText}
			if kind%4 == 2 && s != "" {
				in = value.String(s)
				break
			}
			iso, ok := value.AppendISO(nil, value.TimestampTZWithZone(at), "T "[flags>>5&1])
			if !ok {
				return
			}
			in = value.String(string(iso))
		}
		if typ.Validate() != nil {
			return
		}
		marked, leaked := in, func(msg string) bool { return strings.Contains(msg, at.Format("2006-01-02")) }
		if in.Kind() == value.KindString {
			marked = value.String(leakMarker + in.Text() + leakMarker)
			leaked = func(msg string) bool { return strings.Contains(msg, leakMarker) }
		}
		col := fuzzColumn(typ, flags)
		eachRule(col, flags, set, n, s, func(r Rule) { checkMasking(t, k, col, r, in, marked, leaked) })
	})
}
