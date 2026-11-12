package mask

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/exarth-labs/stuntdb/internal/schema"
	"github.com/exarth-labs/stuntdb/internal/value"
)

// Column types for the tests.
var (
	tText      = value.Type{Kind: value.TypeText}
	tInt64     = value.Type{Kind: value.TypeInt, Bits: 64}
	tInt32     = value.Type{Kind: value.TypeInt, Bits: 32}
	tUint64    = value.Type{Kind: value.TypeInt, Bits: 64, Unsigned: true}
	tFloat     = value.Type{Kind: value.TypeFloat, Bits: 64}
	tDecimal   = value.Type{Kind: value.TypeDecimal, Precision: 12, Scale: 2}
	tDecimal0  = value.Type{Kind: value.TypeDecimal, Precision: 20}
	tDate      = value.Type{Kind: value.TypeDate}
	tTimestamp = value.Type{Kind: value.TypeTimestamp, Precision: 6}
	tTimeTZ    = value.Type{Kind: value.TypeTimestamp, Precision: 6, TZ: true}
	tUUID      = value.Type{Kind: value.TypeUUID}
	tJSON      = value.Type{Kind: value.TypeJSON}
	tBinary    = value.Type{Kind: value.TypeBinary}
)

func tString(n int) value.Type { return value.Type{Kind: value.TypeString, MaxLen: n} }

func column(t value.Type) Column {
	return Column{Ref: schema.ColumnRef{Table: schema.TableRef{Namespace: "main", Name: "people"}, Column: "c"}, Type: t, Nullable: true}
}

func uniqueColumn(t value.Type) Column {
	c := column(t)
	c.Unique, c.Nullable = true, false
	return c
}

// masker masks single values with one column's function, the way a
// RowMasker does.
type masker struct {
	t      testing.TB
	use    string
	f      Func
	seeder *seeder
	rand   *Rand
	ctx    Context
}

func newMasker(t testing.TB, col Column, use string, opts map[string]any) *masker {
	t.Helper()
	return newMaskerRule(t, col, Rule{Use: use, Options: opts})
}

func newMaskerRule(t testing.TB, col Column, rule Rule) *masker {
	t.Helper()
	f, err := Configure(col, rule)
	if err != nil {
		t.Fatalf("%s on %s: %v", rule.Use, col.Type, err)
	}
	k := testKeys(t)
	return &masker{t: t, use: rule.Use, f: f, seeder: newSeeder(k), rand: newRand(), ctx: Context{Keys: k, Domain: "test.domain"}}
}

func (m *masker) mask(in value.Value) (value.Value, error) {
	m.ctx.Rand = nil
	if m.f.Seeded() {
		m.rand.reset(m.seeder.seed(m.ctx.Domain, m.use, in))
		m.ctx.Rand = m.rand
	}
	return m.f.Mask(&m.ctx, in)
}

func (m *masker) must(in value.Value) value.Value {
	m.t.Helper()
	out, err := m.mask(in)
	if err != nil {
		m.t.Fatalf("mask: %v", err)
	}
	return out
}

// show writes a test value for a golden file. Test inputs are made up, and
// outputs are fake, so printing them is safe here (and only here).
func show(t value.Type, v value.Value) string {
	if t.Kind == value.TypeBinary {
		if v.Kind() == value.KindBytes {
			return fmt.Sprintf("bytes %q", v.Bytes())
		}
	}
	b, err := value.AppendJSON(nil, t, v)
	if err != nil {
		return "unprintable " + v.Kind().String()
	}
	return string(b)
}

// TestTransformersGolden pins the output of every transformer for a fixed
// key: part of GeneratorVersion.
func TestTransformersGolden(t *testing.T) {
	date, _ := value.Date(1990, 3, 15)
	ts := value.Timestamp(time.Date(2026, 10, 19, 13, 45, 0, 500000000, time.UTC))
	tsz := value.TimestampTZWithZone(time.Date(2026, 10, 19, 13, 45, 0, 0, time.FixedZone("", 5*3600)))
	u, _ := value.ParseUUID("123e4567-e89b-42d3-a456-426614174000")
	cases := []struct {
		use  string
		col  Column
		opts map[string]any
		in   []value.Value
	}{
		{"keep", column(tText), nil, []value.Value{value.String("unchanged")}},
		{"null", column(tText), nil, []value.Value{value.String("gone")}},
		{"constant", column(tDecimal), map[string]any{"value": "12.50"}, []value.Value{mustDecimal(t, "99.99")}},
		{"first_name", column(tText), nil, []value.Value{value.String("Jane"), value.String("John")}},
		{"last_name", column(tString(6)), map[string]any{"max_length": "6"}, []value.Value{value.String("Doe")}},
		{"full_name", column(tText), nil, []value.Value{value.String("Jane Doe")}},
		{"full_name", column(tString(10)), map[string]any{"max_length": "10"}, []value.Value{value.String("Jane Doe")}},
		{"username", column(tText), nil, []value.Value{value.String("jdoe")}},
		{"username", uniqueColumn(tString(40)), map[string]any{"max_length": "40"}, []value.Value{value.String("jdoe")}},
		{"email", column(tText), nil, []value.Value{value.String("jane@doe.test"), value.String("JANE@DOE.TEST")}},
		{"email", uniqueColumn(tText), nil, []value.Value{value.String("jane@doe.test")}},
		{"email", uniqueColumn(tString(40)), map[string]any{"max_length": "40"}, []value.Value{value.String("jane@doe.test")}},
		{"company", column(tText), nil, []value.Value{value.String("Acme")}},
		{"phone", column(tText), nil, []value.Value{value.String("+1 555 123 4567")}},
		{"phone", column(tString(10)), map[string]any{"format": "digits"}, []value.Value{value.String("+1 555 123 4567")}},
		{"phone", column(tString(12)), map[string]any{"region": "GB", "format": "national"}, []value.Value{value.String("020 7946 0000")}},
		{"phone", column(tInt64), nil, []value.Value{value.Int(5551234567)}},
		{"phone", column(tInt64), map[string]any{"region": "GB"}, []value.Value{value.Int(442079460000)}},
		{"address", column(tText), nil, []value.Value{value.String("1 Real Street")}},
		{"address", column(tText), map[string]any{"part": "street"}, []value.Value{value.String("1 Real Street")}},
		{"address", column(tText), map[string]any{"part": "city"}, []value.Value{value.String("Paris")}},
		{"address", column(tString(2)), map[string]any{"part": "state"}, []value.Value{value.String("CA")}},
		{"address", column(tText), map[string]any{"part": "postcode"}, []value.Value{value.String("75001")}},
		{"address", column(tInt32), map[string]any{"part": "postcode"}, []value.Value{value.Int(75001)}},
		{"uuid", column(tUUID), nil, []value.Value{u}},
		{"uuid", column(tText), nil, []value.Value{value.String("123E4567E89B42D3A456426614174000")}},
		{"fpe", column(tText), nil, []value.Value{value.String("ABC-123-xyz")}},
		{"fpe", column(tText), map[string]any{"alphabet": "digits"}, []value.Value{value.String("4111-1111-1111-1111")}},
		{"fpe", column(tInt64), nil, []value.Value{value.Int(1234567), value.Int(-9876543)}},
		{"hash", column(tText), nil, []value.Value{value.String("secret")}},
		{"hash", column(tText), map[string]any{"length": json.Number("8")}, []value.Value{value.String("secret")}},
		{"hash", column(tInt64), nil, []value.Value{value.Int(123456789)}},
		{"hash", column(tDecimal0), nil, []value.Value{mustDecimal(t, "12345678901234567890")}}, // 9·10^19 choices: over 64 bits
		{"hash", column(tUUID), nil, []value.Value{u}},
		{"random_token", column(tText), nil, []value.Value{value.String("tok_demo_AbC123xyz")}},
		{"random_token", column(tJSON), nil, []value.Value{value.String(`{"t":1}`)}},
		{"random_token", column(tBinary), nil, []value.Value{value.Bytes([]byte("0123456789abcdef"))}},
		{"password_hash", column(tText), nil, []value.Value{value.String("$2b$12$real")}},
		{"redact", column(tText), nil, []value.Value{value.String("private")}},
		{"redact", column(tString(5)), map[string]any{"max_length": "5"}, []value.Value{value.String("private")}},
		{"redact", column(tJSON), nil, []value.Value{value.String(`{"a":1}`)}},
		{"noise", column(tInt64), nil, []value.Value{value.Int(1000), value.Int(-1000)}},
		{"noise", column(tDecimal), map[string]any{"percent": json.Number("50")}, []value.Value{mustDecimal(t, "1234.50")}},
		{"noise", column(tFloat), nil, []value.Value{value.Float(98.6)}},
		{"date_shift", column(tDate), nil, []value.Value{date}},
		{"date_shift", column(tTimestamp), map[string]any{"max_days": json.Number("30")}, []value.Value{ts}},
		{"date_shift", column(tTimeTZ), nil, []value.Value{tsz}},
		{"date_shift", column(tText), nil, []value.Value{value.String("1990-03-15"), value.String("2026-10-19 13:45:00")}},
		{"synthesize_text", column(tText), nil, []value.Value{value.String("Call me on my cell, it's urgent!")}},
	}
	var b strings.Builder
	for _, c := range cases {
		m := newMasker(t, c.col, c.use, c.opts)
		opts := ""
		for _, k := range slices.Sorted(maps.Keys(c.opts)) {
			opts += fmt.Sprintf(" %s=%v", k, c.opts[k])
		}
		unique := ""
		if c.col.Unique {
			unique = " unique"
		}
		for _, in := range c.in {
			fmt.Fprintf(&b, "%s%s [%s%s] %s -> %s\n", c.use, opts, c.col.Type, unique, show(c.col.Type, in), show(c.col.Type, m.must(in)))
		}
	}
	golden(t, "transformers", b.String())
}

// TestCatalog checks every transformer's description, and that every
// transformer is deterministic and keeps NULL out of its inputs.
func TestCatalog(t *testing.T) {
	names := map[string]bool{}
	for _, tr := range Catalog() {
		names[tr.Name] = true
		if tr.Summary == "" || len(tr.Types) == 0 || tr.build == nil {
			t.Errorf("%s is incomplete", tr.Name)
		}
		if !regexp.MustCompile(`^[a-z][a-z0-9_]*$`).MatchString(tr.Name) {
			t.Errorf("%s is not a valid transformer name for the configuration schema", tr.Name)
		}
	}
	if len(names) != 21 {
		t.Errorf("the catalog has %d transformers, want 21", len(names))
	}
	if _, ok := Lookup("email"); !ok {
		t.Error("Lookup(email) found nothing")
	}
	if got := Suggest("emial"); got != "email" {
		t.Errorf("Suggest(emial) = %q", got)
	}
	if got := Suggest("zzzzzzzz"); got != "" {
		t.Errorf("Suggest(zzzzzzzz) = %q", got)
	}
	if got := fmt.Sprint(None, CollisionResistant, Bijective); got != "none collision-resistant bijective" {
		t.Errorf("the guarantees print as %q", got)
	}
}

func TestDeterministic(t *testing.T) {
	for _, use := range []string{"first_name", "email", "phone", "address", "hash", "fpe", "uuid", "random_token", "noise", "synthesize_text"} {
		tr, _ := Lookup(use)
		col := column(tText)
		in := value.String("Some Input Text 12345678")
		switch {
		case use == "uuid":
			in = value.String("123e4567-e89b-42d3-a456-426614174000")
		case tr.Accepts(value.TypeText):
		case tr.Accepts(value.TypeInt):
			col, in = column(tInt64), value.Int(12345678)
		}
		a := newMasker(t, col, use, nil)
		b := newMasker(t, col, use, nil)
		if x, y := a.must(in), b.must(in); !x.Equal(y) {
			t.Errorf("%s gives two outputs for one input", use)
		}
		b.ctx.Domain = "another.domain"
		if use != "phone" && use != "address" && a.must(in).Equal(b.must(in)) {
			t.Errorf("%s gives the same output in two domains", use)
		}
	}
}

var (
	reservedEmail = regexp.MustCompile(`^[a-z0-9.]+@example\.(com|net|org)$`)
	fictionalUS   = regexp.MustCompile(`^(\+1-)?\d{3}-?555-?01\d\d$`)
	fictionalGB   = regexp.MustCompile(`^(\+44 7700 900|07700 ?900)\d{3}$`)
)

// TestRealWorldSafety checks the promises that keep a copy from reaching
// real people: reserved email domains and fictional phone ranges.
func TestRealWorldSafety(t *testing.T) {
	emails := newMasker(t, column(tText), "email", nil)
	short := newMasker(t, column(tString(20)), "email", map[string]any{"max_length": "20"})
	distinct := newMaskerRule(t, column(tText), Rule{Use: "email", Distinct: true})
	var us, gb []*masker
	for _, format := range phoneFormats {
		us = append(us, newMasker(t, column(tText), "phone", map[string]any{"format": format}))
		gb = append(gb, newMasker(t, column(tText), "phone", map[string]any{"region": "GB", "format": format}))
	}
	usNum := newMasker(t, column(tInt64), "phone", nil)
	gbNum := newMasker(t, column(tInt64), "phone", map[string]any{"region": "GB"})
	for i := range 2000 {
		in := value.String(strconv.Itoa(i))
		for _, m := range []*masker{emails, short, distinct} {
			if s := m.must(in).Text(); !reservedEmail.MatchString(s) || len(s) > 20 && m == short {
				t.Fatalf("email %q", s)
			}
		}
		for _, m := range us {
			if s := m.must(in).Text(); !fictionalUS.MatchString(s) {
				t.Fatalf("US phone %q", s)
			}
		}
		for _, m := range gb {
			if s := m.must(in).Text(); !fictionalGB.MatchString(s) {
				t.Fatalf("GB phone %q", s)
			}
		}
		if n := usNum.must(value.Int(int64(i))).Int(); n%10000/100 != 1 || n/10000%1000 != 555 {
			t.Fatalf("US phone as a number %d", n)
		}
		if n := gbNum.must(value.Int(int64(i))).Int(); n/1000 != 447700900 {
			t.Fatalf("GB phone as a number %d", n)
		}
	}
}

// TestUniqueFormsStayDistinct masks many distinct inputs with every
// transformer that a unique column accepts, and checks no two collide.
func TestUniqueFormsStayDistinct(t *testing.T) {
	n := 50_000
	if testing.Short() {
		n = 5_000
	}
	cases := []struct {
		use  string
		col  Column
		in   func(i int) value.Value
		opts map[string]any
	}{
		{"email", uniqueColumn(tString(40)), func(i int) value.Value { return value.String(fmt.Sprintf("user%d@x.test", i)) }, map[string]any{"max_length": "40"}},
		{"email", uniqueColumn(tText), func(i int) value.Value { return value.String(fmt.Sprintf("user%d@x.test", i)) }, nil},
		{"username", uniqueColumn(tString(30)), func(i int) value.Value { return value.String(fmt.Sprint("u", i)) }, map[string]any{"max_length": "30"}},
		{"hash", uniqueColumn(tString(24)), func(i int) value.Value { return value.String(fmt.Sprint(i)) }, map[string]any{"length": "24"}},
		{"fpe", uniqueColumn(tText), func(i int) value.Value { return value.String(fmt.Sprintf("AB%06d", i)) }, map[string]any{"alphabet": "digits"}},
		{"fpe", uniqueColumn(tInt64), func(i int) value.Value { return value.Int(int64(1_000_000 + i)) }, nil},
		{"uuid", uniqueColumn(tUUID), func(i int) value.Value {
			var u [16]byte
			u[0], u[1], u[2] = byte(i), byte(i>>8), byte(i>>16)
			return value.UUID(u)
		}, nil},
		{"random_token", uniqueColumn(tText), func(i int) value.Value { return value.String(fmt.Sprintf("tok_%026d", i)) }, nil},
	}
	for _, c := range cases {
		t.Run(c.use+"/"+c.col.Type.String(), func(t *testing.T) {
			m := newMasker(t, c.col, c.use, c.opts)
			if m.f.Guarantee() == None {
				t.Fatalf("%s reports no guarantee on a unique column", c.use)
			}
			seen := make(map[string]int, n)
			for i := range n {
				key := show(c.col.Type, m.must(c.in(i)))
				if j, dup := seen[key]; dup {
					t.Fatalf("inputs %d and %d collide", j, i)
				}
				seen[key] = i
			}
		})
	}
}

func TestFPEInvertible(t *testing.T) {
	m := newMasker(t, column(tText), "fpe", nil)
	in := "Order-2026/AbcDef-0042"
	out := m.must(value.String(in)).Text()
	if len(out) != len(in) || out == in {
		t.Fatalf("fpe gave %q", out)
	}
	for i := range len(in) {
		inAlnum := strings.IndexByte(fpeAlphabets["alnum"], in[i]) >= 0
		if inAlnum != (strings.IndexByte(fpeAlphabets["alnum"], out[i]) >= 0) || !inAlnum && in[i] != out[i] {
			t.Fatalf("fpe moved a separator: %q -> %q", in, out)
		}
	}
	// Decrypting with the same tweak gives the input back: a permutation.
	var x []uint16
	skeleton := []byte{}
	for i := range len(out) {
		if d := strings.IndexByte(fpeAlphabets["alnum"], out[i]); d >= 0 {
			x = append(x, uint16(d))
			skeleton = append(skeleton, 1)
		} else {
			skeleton = append(skeleton, 0, out[i])
		}
	}
	y, err := m.ctx.Keys.ff1.Decrypt(62, x, tweak("fpe/alnum", m.ctx.Domain, skeleton))
	if err != nil {
		t.Fatal(err)
	}
	back := []byte(out)
	j := 0
	for i := range len(back) {
		if strings.IndexByte(fpeAlphabets["alnum"], back[i]) >= 0 {
			back[i] = fpeAlphabets["alnum"][y[j]]
			j++
		}
	}
	if string(back) != in {
		t.Errorf("decrypting gives %q, want %q", back, in)
	}
}

func TestFPENumbers(t *testing.T) {
	m := newMasker(t, column(tInt32), "fpe", nil)
	for _, in := range []int64{1_000_000, 2_147_483_647, -2_147_483_648, 9_999_999, -1_234_567} {
		out := m.must(value.Int(in)).Int()
		if len(strconv.FormatInt(out, 10)) != len(strconv.FormatInt(in, 10)) || (out < 0) != (in < 0) {
			t.Errorf("fpe(%d) = %d: sign or digits changed", in, out)
		}
		if out < math.MinInt32 || out > math.MaxInt32 {
			t.Errorf("fpe(%d) = %d: out of the int32 range", in, out)
		}
	}
	big := newMasker(t, column(value.Type{Kind: value.TypeDecimal}), "fpe", nil)
	if out := big.must(mustDecimal(t, "123456789012345678901234567890")).DecimalText(); len(out) != 30 || out[0] == '0' {
		t.Errorf("fpe on a 30-digit decimal = %s", out)
	}
	u := newMasker(t, column(tUint64), "fpe", nil)
	if out := u.must(value.Uint(math.MaxUint64)).Uint(); out < 1e19 {
		t.Errorf("fpe kept 20 digits badly: %d", out)
	}
	for _, in := range []value.Value{value.Int(12345), mustDecimal(t, "1234567.5"), value.String("x")} {
		if _, err := newMasker(t, column(tDecimal0), "fpe", nil).mask(in); err == nil {
			t.Errorf("fpe accepted a %s value it should refuse", in.Kind())
		}
	}
	if _, err := newMasker(t, column(tText), "fpe", map[string]any{"alphabet": "lower"}).mask(value.String("ab-cd")); err == nil ||
		!strings.Contains(err.Error(), "at least 5") || strings.Contains(err.Error(), "ab-cd") {
		t.Errorf("a short value: %v", err)
	}
	// A value too large for its column's type, as a database with dynamic
	// types can hold one, gives an output that is too large too; the error
	// says the source value is the cause, and never shows it.
	int16Col := newMasker(t, column(value.Type{Kind: value.TypeInt, Bits: 16}), "fpe", nil)
	for _, in := range []int64{123456, -123456} {
		if _, err := int16Col.mask(value.Int(in)); err == nil ||
			!strings.Contains(err.Error(), "fit the column's type (int16)") || strings.Contains(err.Error(), "123456") {
			t.Errorf("fpe on %d in an int16 column: %v", in, err)
		}
	}
}

// TestNumberClasses checks the classes that keep a masked whole number
// within the integer types its input fits, at every boundary.
func TestNumberClasses(t *testing.T) {
	tests := []struct {
		mag    string
		neg    bool
		lo, hi string
	}{
		{"0", false, "1", "9"},
		{"5", true, "1", "9"},
		{"127", false, "100", "127"},
		{"128", false, "128", "255"},
		{"256", false, "256", "999"},
		{"128", true, "100", "128"},
		{"129", true, "129", "999"},
		{"999999", false, "100000", "999999"},
		{"2147483647", false, "1000000000", "2147483647"},
		{"2147483648", false, "2147483648", "4294967295"},
		{"4294967296", false, "4294967296", "9999999999"},
		{"2147483648", true, "1000000000", "2147483648"},
		{"2147483649", true, "2147483649", "9999999999"},
		{"9223372036854775807", false, "1000000000000000000", "9223372036854775807"},
		{"9223372036854775808", false, "9223372036854775808", "9999999999999999999"},
		{"9223372036854775809", true, "9223372036854775809", "9999999999999999999"},
		{"18446744073709551615", false, "10000000000000000000", "18446744073709551615"},
		{"18446744073709551616", false, "18446744073709551616", "99999999999999999999"},
		{"123456789012345678901234567890", false, "100000000000000000000000000000", "999999999999999999999999999999"},
	}
	for _, tt := range tests {
		if got := classOf(tt.mag, tt.neg); got != (numberClass{tt.lo, tt.hi}) {
			t.Errorf("classOf(%s, negative %v) = %v, want [%s, %s]", tt.mag, tt.neg, got, tt.lo, tt.hi)
		}
	}
	// fpe, hash and random_token keep a number in its class, whatever the
	// column: a uint32 value stays above the int32 range, and an int32
	// value stays within it.
	for _, use := range []string{"fpe", "hash", "random_token"} {
		m := newMasker(t, column(tInt64), use, nil)
		for i := range int64(300) {
			for _, in := range []int64{2_147_483_648 + i*7_000_001, 1_000_000_000 + i*3_000_001, -2_147_483_648 + i*5_000_003} {
				mag, neg, _ := wholeNumber(value.Int(in))
				out, _, _ := wholeNumber(m.must(value.Int(in)))
				if c := classOf(mag, neg); !c.has(out) || len(out) != len(mag) {
					t.Fatalf("%s(%d) left the class [%s, %s]", use, in, c.lo, c.hi)
				}
			}
		}
	}
}

func TestUUIDKeepsVersion(t *testing.T) {
	m := newMasker(t, column(tUUID), "uuid", nil)
	for i := range 200 {
		var u [16]byte
		u[15], u[6], u[8] = byte(i), 0x40|byte(i)&0x0f, 0x80|byte(i)&0x3f
		out := m.must(value.UUID(u)).UUID()
		if out[6]>>4 != 4 || out[8]>>6 != 2 || out == u {
			t.Fatalf("uuid changed the version or variant, or nothing: %x -> %x", u, out)
		}
	}
	text := newMasker(t, column(tText), "uuid", nil)
	if s := text.must(value.String("123e4567-e89b-42d3-a456-426614174000")).Text(); len(s) != 36 || s != strings.ToLower(s) {
		t.Errorf("uuid changed the layout: %s", s)
	}
	bin := newMasker(t, column(tBinary), "uuid", nil)
	if _, err := bin.mask(value.Bytes([]byte{1, 2})); err == nil {
		t.Error("uuid accepted 2 bytes")
	}
	if _, err := text.mask(value.String("not a uuid")); err == nil || strings.Contains(err.Error(), "not a uuid") {
		t.Errorf("a bad UUID: %v", err)
	}
}

// TestNoiseBounds checks noise's promises: the change is at most percent
// of the value (plus half a unit of rounding), it surely changes a number
// once the smallest change, 1/10000 of it, exceeds half a unit, and zero
// stays zero.
func TestNoiseBounds(t *testing.T) {
	ints := newMasker(t, column(tInt64), "noise", map[string]any{"percent": "20"})
	for i := int64(-3_000_000); i <= 3_000_000; i += 4999 {
		out := ints.must(value.Int(i)).Int()
		if d := math.Abs(float64(out - i)); d > 0.2*math.Abs(float64(i))+0.5 {
			t.Fatalf("noise(%d) = %d: more than 20%%", i, out)
		}
		if (i > 5000 || i < -5000) && out == i {
			t.Fatalf("noise(%d) left it unchanged, though every change rounds to at least one", i)
		}
	}
	// A float that fits a float32 stays within its range, whatever the
	// column: here a float32 column, whose output check would refuse more.
	f32 := newMasker(t, column(value.Type{Kind: value.TypeFloat, Bits: 32}), "noise", nil)
	for x, i := math.MaxFloat32, 0; i < 20; x, i = math.Nextafter(x, 0), i+1 {
		f32.must(value.Float(x))
		f32.must(value.Float(-x))
	}
	// As the Summary says, rounding keeps small numbers: at 20%, ±2 change
	// by at most 0.4, which rounds back, and zero has nothing to scale.
	for _, i := range []int64{0, 1, -1, 2, -2} {
		if out := ints.must(value.Int(i)).Int(); out != i {
			t.Errorf("noise(%d) = %d: a change below half a unit must round away", i, out)
		}
	}
	small := newMasker(t, column(value.Type{Kind: value.TypeInt, Bits: 8}), "noise", map[string]any{"percent": "100"})
	for i := range 128 {
		if out := small.must(value.Int(int64(i))).Int(); out < -128 || out > 127 {
			t.Fatalf("noise left int8: %d", out)
		}
	}
	dec := newMasker(t, column(value.Type{Kind: value.TypeDecimal, Precision: 4, Scale: 2}), "noise", map[string]any{"percent": "100"})
	for _, in := range []string{"99.99", "-99.99", "0.01", "12.5"} {
		out := dec.must(mustDecimal(t, in)).DecimalText()
		_, frac, _ := strings.Cut(out, ".")
		_, inFrac, _ := strings.Cut(in, ".")
		if len(frac) != len(inFrac) {
			t.Errorf("noise(%s) = %s: decimal places changed", in, out)
		}
	}
	if out := dec.must(value.DecimalNaN()); out.Special() != value.NaN {
		t.Error("noise changed NaN")
	}
	if out := newMasker(t, column(tUint64), "noise", nil).must(value.Uint(math.MaxUint64)).Uint(); out == 0 {
		t.Error("noise wrapped an unsigned number")
	}
}

func TestDateShift(t *testing.T) {
	m := newMasker(t, column(tTimestamp), "date_shift", map[string]any{"max_days": "10"})
	base := time.Date(2026, 10, 19, 13, 45, 30, 123000000, time.UTC)
	for i := range 500 {
		in := base.Add(time.Duration(i) * time.Hour)
		out := m.must(value.Timestamp(in)).Timestamp()
		days := out.Sub(in).Hours() / 24
		if math.Abs(days) < 1 || math.Abs(days) > 10 || days != math.Trunc(days) {
			t.Fatalf("shift of %v days", days)
		}
	}
	edge, _ := value.Date(9999, 12, 31)
	if y, _, _ := newMasker(t, column(tDate), "date_shift", nil).must(edge).Date(); y > 9999 {
		t.Error("date_shift left the year 9999")
	}
	text := newMasker(t, column(tText), "date_shift", nil)
	for _, in := range []string{"2026-10-19", "2026-10-19T13:45:00Z", "2026-10-19 13:45:00"} {
		out := text.must(value.String(in)).Text()
		if len(out) != len(in) || (strings.Contains(in, " ") != strings.Contains(out, " ")) {
			t.Errorf("date_shift(%q) = %q: the form changed", in, out)
		}
	}
	if _, err := text.mask(value.String("19/10/2026")); err == nil || strings.Contains(err.Error(), "2026") {
		t.Errorf("unreadable date text: %v", err)
	}
	inf := value.TimestampInfinity(false, false)
	if out := m.must(inf); out.Special() != value.PosInfinity {
		t.Error("date_shift changed infinity")
	}
}

func TestSynthesizeTextLength(t *testing.T) {
	m := newMasker(t, column(tText), "synthesize_text", nil)
	for _, in := range []string{"", "a", "ab ", "Grüße aus Köln — ✓", strings.Repeat("long text ", 500)} {
		out := m.must(value.String(in)).Text()
		if utf8.RuneCountInString(out) != utf8.RuneCountInString(in) || len(out) > len(in) || !printableASCII(out) {
			t.Errorf("synthesize_text: %d characters in, %q out", utf8.RuneCountInString(in), out)
		}
		if len(out) > 0 && (out[0] == ' ' || out[len(out)-1] == ' ') {
			t.Errorf("synthesize_text has a leading or trailing space: %q", out)
		}
	}
}

func TestRandomToken(t *testing.T) {
	m := newMasker(t, column(tText), "random_token", nil)
	in := "tok_demo_AbC123xyz"
	out := m.must(value.String(in)).Text()
	shape := func(s string) string {
		return regexp.MustCompile(`[a-z]`).ReplaceAllString(regexp.MustCompile(`[A-Z]`).ReplaceAllString(regexp.MustCompile(`\d`).ReplaceAllString(s, "9"), "A"), "a")
	}
	if shape(out) != shape(in) || out == in {
		t.Errorf("random_token(%q) = %q", in, out)
	}
	u := newMasker(t, uniqueColumn(tText), "random_token", nil)
	if _, err := u.mask(value.String("abc123")); err == nil {
		t.Error("a short token in a unique column is accepted")
	}
	ub := newMasker(t, uniqueColumn(tBinary), "random_token", nil)
	if _, err := ub.mask(value.Bytes(make([]byte, 11))); err == nil {
		t.Error("11 random bytes in a unique column are accepted")
	}
	j := newMasker(t, column(tJSON), "random_token", nil)
	if !json.Valid([]byte(j.must(value.String("{}")).Text())) {
		t.Error("random_token wrote invalid JSON")
	}
	num := newMasker(t, column(value.Type{Kind: value.TypeInt, Bits: 16}), "random_token", nil)
	for i := range 300 {
		if out := num.must(value.Int(int64(10000 + i))).Int(); out < 10000 || out > 32767 {
			t.Fatalf("random_token on an int16: %d", out)
		}
	}
}

func TestPasswordHash(t *testing.T) {
	if got := newMasker(t, column(tText), "password_hash", nil).must(value.String("x")).Text(); got != DefaultPasswordHash {
		t.Errorf("default hash %q", got)
	}
	own := "pbkdf2_sha256$600000$salt$hash="
	if got := newMasker(t, column(tBinary), "password_hash", map[string]any{"hash": own}).must(value.Bytes([]byte("x"))); string(got.Bytes()) != own {
		t.Error("password_hash on a binary column")
	}
}

// TestConfigureMistakes checks that mistakes are refused with a message that
// explains them.
func TestConfigureMistakes(t *testing.T) {
	notNull := column(tText)
	notNull.Nullable = false
	oracle := notNull
	oracle.EmptyStringIsNull = true
	withDefault := column(tText)
	withDefault.HasDefault = true
	nullableUnique := uniqueColumn(tText)
	nullableUnique.Nullable = true
	tests := []struct {
		name string
		col  Column
		rule Rule
		want string
	}{
		{"unknown", column(tText), Rule{Use: "emial"}, "did you mean email"},
		{"unknown, no suggestion", column(tText), Rule{Use: "zzzzzz"}, "there is no transformer zzzzzz"},
		{"wrong type", column(tDate), Rule{Use: "email"}, "email cannot mask a date column"},
		{"unknown option", column(tText), Rule{Use: "email", Options: map[string]any{"domain2": "x"}}, "email takes no option domain2"},
		{"unknown option listed", column(tText), Rule{Use: "phone", Options: map[string]any{"country": "x"}}, "its options are region"},
		{"bad choice", column(tText), Rule{Use: "phone", Options: map[string]any{"region": "FR"}}, "must be one of: US, GB"},
		{"bad number", column(tInt64), Rule{Use: "noise", Options: map[string]any{"percent": "0"}}, "from 1 to 100"},
		{"list option", column(tInt64), Rule{Use: "noise", Options: map[string]any{"percent": []any{"1"}}}, "single value"},
		{"empty option", column(tInt64), Rule{Use: "noise", Options: map[string]any{"percent": nil}}, "has no value"},
		{"null in NOT NULL", notNull, Rule{Use: "null"}, "NOT NULL"},
		{"name on unique", uniqueColumn(tText), Rule{Use: "first_name"}, "first_name cannot promise that; these can: email, fpe, hash, random_token, username"},
		{"token on a unique number", uniqueColumn(tInt64), Rule{Use: "random_token"}, "random_token cannot promise that; these can: fpe"},
		{"nothing fits a unique date", uniqueColumn(tDate), Rule{Use: "date_shift"}, "in a date column, no transformer can"},
		{"null on unique", nullableUnique, Rule{Use: "null"}, "null cannot promise that"}, // SQL Server allows one NULL per unique key
		{"short hash on unique", uniqueColumn(tText), Rule{Use: "hash", Options: map[string]any{"length": "16"}}, "option length: values here must stay distinct (a unique key shares the domain), so hash needs a length of at least 24"},
		{"short hash in a distinct domain", column(tText), Rule{Use: "hash", Options: map[string]any{"length": "16"}, Distinct: true}, "hash needs a length of at least 24"},
		{"hash in a short unique column", uniqueColumn(tString(20)), Rule{Use: "hash"}, "hash writes up to 32 characters, and the column holds 20, fewer than the 24 that keep values distinct; these fit: fpe"},
		{"per on unique", uniqueColumn(tText), Rule{Use: "hash", Per: "id"}, "a column in a unique key can't use per"},
		{"per without a seed", column(tText), Rule{Use: "redact", Per: "id"}, "redact takes no seed, so per has no effect"},
		{"hash longer than column", column(tString(10)), Rule{Use: "hash", Options: map[string]any{"length": "20"}}, "hash writes up to 20 characters, and the column holds 10; set length: 10"},
		{"name longer than column", column(tString(5)), Rule{Use: "first_name"}, "and the column holds 5; set max_length: 5"},
		{"max_length below every name", column(tText), Rule{Use: "first_name", Options: map[string]any{"max_length": "1"}}, "option max_length: must be a whole number from"},
		{"email in 10", column(tString(10)), Rule{Use: "email"}, "and the column holds 10, fewer than the 15 it needs at least"},
		{"unique email in 30", uniqueColumn(tString(30)), Rule{Use: "email"}, "and the column holds 30, fewer than the 32 it needs at least; these fit: fpe, random_token"},
		{"phone too long", column(tString(12)), Rule{Use: "phone"}, "phone writes up to 15 characters, and the column holds 12; set format: national (12 characters) or digits (10 characters)"},
		{"phone shorter than every format", column(tString(9)), Rule{Use: "phone"}, "fewer than the 10 of its shortest format, digits; these fit: first_name, fpe, last_name, random_token, redact, synthesize_text"},
		{"phone format in a number column", column(tInt64), Rule{Use: "phone", Options: map[string]any{"format": "national"}}, "option format: a number column always gets the digits"},
		{"phone in int32", column(tInt32), Rule{Use: "phone"}, "don't always fit"},
		{"street in a number", column(tInt64), Rule{Use: "address", Options: map[string]any{"part": "street"}}, "only part: postcode"},
		{"postcode max_length in a number", column(tInt64), Rule{Use: "address", Options: map[string]any{"part": "postcode", "max_length": "5"}}, "option max_length: a number column holds the postcode's 5 digits"},
		{"state in one character", column(tString(1)), Rule{Use: "address", Options: map[string]any{"part": "state"}}, "the column holds 1, fewer than the 2 it needs at least"},
		{"redact longer than column", column(tString(5)), Rule{Use: "redact"}, "redact writes up to 8 characters, and the column holds 5; set max_length: 5"},
		{"redact max_length in JSON", column(tJSON), Rule{Use: "redact", Options: map[string]any{"max_length": "5"}}, "option max_length: a JSON column always gets"},
		{"redact in short binary", column(value.Type{Kind: value.TypeBinary, MaxLen: 5}), Rule{Use: "redact"}, "redact writes up to 8 bytes, and the column holds 5; set max_length: 5"},
		{"uuid in short text", column(tString(20)), Rule{Use: "uuid"}, "in 32 or 36 characters, and the column holds 20"},
		{"constant missing", column(tText), Rule{Use: "constant"}, "needs the option value"},
		{"constant bad", column(tInt64), Rule{Use: "constant", Options: map[string]any{"value": "x"}}, "whole number"},
		{"constant too long", column(tString(2)), Rule{Use: "constant", Options: map[string]any{"value": "abc"}}, "does not fit"},
		{"constant empty on Oracle", oracle, Rule{Use: "constant", Options: map[string]any{"value": ""}}, "stores as NULL"},
		{"default without one", column(tText), Rule{Use: "default"}, "needs a column with a default"},
		{"fpe letters in a number", column(tInt64), Rule{Use: "fpe", Options: map[string]any{"alphabet": "alnum"}}, "keeps numbers as digits"},
		{"fpe on decimal places", column(tDecimal), Rule{Use: "fpe"}, "whole numbers"},
		{"uuid in 8 bytes", column(value.Type{Kind: value.TypeBinary, MaxLen: 8}), Rule{Use: "uuid"}, "16 bytes"},
		{"password hash too long", column(tString(10)), Rule{Use: "password_hash"}, "password_hash writes up to 60 characters, and the column holds 10; set hash: to a hash that fits"},
		{"password hash empty", column(tText), Rule{Use: "password_hash", Options: map[string]any{"hash": ""}}, "must not be empty"},
		{"fpe in 3 characters", column(tString(3)), Rule{Use: "fpe"}, "fpe needs at least 4 characters from its alphabet (alnum) in each value, and the column holds 3"},
		{"fpe digits in 5 characters", column(tString(5)), Rule{Use: "fpe", Options: map[string]any{"alphabet": "digits"}}, "fpe needs at least 6 characters from its alphabet (digits)"},
		{"distinct token in 20 characters", uniqueColumn(tString(20)), Rule{Use: "random_token"}, "each token needs at least 96 random bits: at least 21 characters, and the column holds 20"},
		{"distinct token in 11 bytes", uniqueColumn(value.Type{Kind: value.TypeBinary, MaxLen: 11}), Rule{Use: "random_token"}, "at least 12 bytes, and the column holds 11"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Configure(tt.col, tt.rule)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want one containing %q", err, tt.want)
			}
		})
	}
	if _, err := Configure(withDefault, Rule{Use: "default"}); err != nil {
		t.Errorf("default on a column with a default: %v", err)
	}
	// The boundaries of the refusals above.
	for _, ok := range []struct {
		col  Column
		rule Rule
	}{
		{column(tString(4)), Rule{Use: "fpe"}},
		{uniqueColumn(tString(21)), Rule{Use: "random_token"}},
		{uniqueColumn(value.Type{Kind: value.TypeBinary, MaxLen: 12}), Rule{Use: "random_token"}},
		{column(tString(5)), Rule{Use: "random_token"}}, // not distinct: any length
	} {
		if _, err := Configure(ok.col, ok.rule); err != nil {
			t.Errorf("%s in %s: %v", ok.rule.Use, ok.col.Type, err)
		}
	}
}

// TestTooShortError checks the refusal of a column too short for a rule:
// the option that bounds the outputs and its value that fits, so that a
// caller can give one value for every column of a domain, or no option
// when none can make the outputs fit.
func TestTooShortError(t *testing.T) {
	tests := []struct {
		col    Column
		rule   Rule
		option string
		fits   int
	}{
		{column(tString(5)), Rule{Use: "first_name"}, "max_length", 5},
		{column(tString(20)), Rule{Use: "email"}, "max_length", 20},
		{column(tString(10)), Rule{Use: "hash", Options: map[string]any{"length": "20"}}, "length", 10},
		{column(tString(5)), Rule{Use: "redact"}, "max_length", 5},
		{column(tString(10)), Rule{Use: "email"}, "", 0},                                            // shorter than any email
		{uniqueColumn(tString(30)), Rule{Use: "email"}, "", 0},                                      // shorter than a tagged email
		{uniqueColumn(tString(20)), Rule{Use: "hash"}, "", 0},                                       // shorter than a distinct hash
		{column(tString(12)), Rule{Use: "phone"}, "", 0},                                            // a format fixes it, not a length
		{column(tString(10)), Rule{Use: "password_hash"}, "", 0},                                    // another hash fixes it
		{column(tString(1)), Rule{Use: "address", Options: map[string]any{"part": "state"}}, "", 0}, // shorter than a state
	}
	for _, tt := range tests {
		_, err := Configure(tt.col, tt.rule)
		var short *TooShortError
		if !errors.As(err, &short) {
			t.Errorf("%s in %s: error %v is not a TooShortError", tt.rule.Use, tt.col.Type, err)
			continue
		}
		if short.Option != tt.option || short.Fits != tt.fits {
			t.Errorf("%s in %s: Option %q, Fits %d; want %q, %d", tt.rule.Use, tt.col.Type, short.Option, short.Fits, tt.option, tt.fits)
		}
		if tt.option != "" && !strings.HasSuffix(short.Error(), fmt.Sprintf("; set %s: %d", tt.option, tt.fits)) {
			t.Errorf("%s in %s: the message %q doesn't give the value", tt.rule.Use, tt.col.Type, short)
		}
	}
	// When nothing fits a column, the message says so.
	_, err := Configure(column(tString(1)), Rule{Use: "address", Options: map[string]any{"part": "state"}})
	if err == nil || !strings.HasSuffix(err.Error(), "; these fit: random_token, synthesize_text") {
		t.Errorf("alternatives in one character: %v", err)
	}
	_, err = Configure(uniqueColumn(tString(1)), Rule{Use: "email"})
	if err == nil || !strings.HasSuffix(err.Error(), "; no other transformer fits the column") {
		t.Errorf("no alternative: %v", err)
	}
}

// TestConstantTextIsLiteral checks that a text column takes constant's value
// exactly as written, however much it looks like another type.
func TestConstantTextIsLiteral(t *testing.T) {
	for _, s := range []string{"", "true", "1", "1.5", "12.50", "x", `{"a":1}`, "123e4567-e89b-42d3-a456-426614174000", "2026-01-31", "13:45:00", "2026-01-31 13:45:00"} {
		for _, typ := range []value.Type{tText, tString(40)} {
			got := newMasker(t, column(typ), "constant", map[string]any{"value": s}).must(value.String("anything"))
			if !got.Equal(value.String(s)) {
				t.Errorf("constant %q on %s gave another value", s, typ)
			}
		}
	}
}

// TestConstantTypes configures constant for every type it accepts.
func TestConstantTypes(t *testing.T) {
	for _, c := range []struct {
		t value.Type
		v string
	}{
		{value.Type{Kind: value.TypeBool}, "true"},
		{value.Type{Kind: value.TypeBool}, "false"},
		{tInt64, "-7"},
		{tUint64, "7"},
		{tFloat, "1.5"},
		{tDecimal, "12.50"},
		{tText, ""},
		{value.Type{Kind: value.TypeEnum, Values: []string{"a", "b"}}, "b"},
		{tJSON, `{"a":1}`},
		{tUUID, "123e4567-e89b-42d3-a456-426614174000"},
		{tDate, "2026-01-31"},
		{value.Type{Kind: value.TypeTime}, "13:45:00"},
		{tTimestamp, "2026-01-31 13:45:00"},
		{tTimeTZ, "2026-01-31T13:45:00Z"},
	} {
		if _, err := Configure(column(c.t), Rule{Use: "constant", Options: map[string]any{"value": c.v}}); err != nil {
			t.Errorf("constant %q on %s: %v", c.v, c.t, err)
		}
	}
	for _, c := range []struct {
		t value.Type
		v string
	}{
		{value.Type{Kind: value.TypeBool}, "yes"},
		{tUint64, "-1"},
		{tFloat, "x"},
		{tDecimal, "1e3"},
		{tJSON, "{"},
		{tUUID, "x"},
		{tDate, "x"},
		{value.Type{Kind: value.TypeTime}, "x"},
		{tTimestamp, "x"},
		{value.Type{Kind: value.TypeEnum, Values: []string{"a"}}, "c"},
		{tTimeTZ, "2026-01-31 13:45:00"},
	} {
		if _, err := Configure(column(c.t), Rule{Use: "constant", Options: map[string]any{"value": c.v}}); err == nil {
			t.Errorf("constant %q on %s is accepted", c.v, c.t)
		}
	}
}

// TestOutputChecks checks the guard every masking function runs behind.
func TestOutputChecks(t *testing.T) {
	notNull := column(tText)
	notNull.Nullable = false
	f := &checked{fn: &fn{mask: func(*Context, value.Value) (value.Value, error) { return value.Null(), nil }}, col: notNull, name: "x"}
	if _, err := f.Mask(&Context{}, value.String("a")); err == nil {
		t.Error("NULL into a NOT NULL column passed")
	}
	oracle := notNull
	oracle.EmptyStringIsNull = true
	f = &checked{fn: &fn{mask: func(*Context, value.Value) (value.Value, error) { return value.String(""), nil }}, col: oracle, name: "x"}
	if _, err := f.Mask(&Context{}, value.String("a")); err == nil {
		t.Error("an empty string into a NOT NULL Oracle column passed")
	}
	// Text in an INTEGER column (SQLite) passes through keep, but a
	// transformer may not introduce it.
	keep := newMasker(t, column(tInt64), "keep", nil)
	if _, err := keep.mask(value.String("abc")); err != nil {
		t.Errorf("keep on text in an integer column: %v", err)
	}
	f = &checked{fn: &fn{mask: func(*Context, value.Value) (value.Value, error) { return value.String("x"), nil }}, col: column(tInt64), name: "x"}
	if _, err := f.Mask(&Context{}, value.Int(1)); err == nil {
		t.Error("a transformer turned an integer into text")
	}
	f = &checked{fn: &fn{mask: func(*Context, value.Value) (value.Value, error) { return value.Int(1 << 40), nil }}, col: column(tInt32), name: "x"}
	if _, err := f.Mask(&Context{}, value.Int(1)); err == nil {
		t.Error("an out-of-range integer passed")
	}
}

// TestKindErrors checks that a value of a kind a transformer can't mask is
// an error that names the kind, not the value.
func TestKindErrors(t *testing.T) {
	for _, c := range []struct {
		use string
		col Column
	}{
		{"noise", column(tInt64)},
		{"date_shift", column(tDate)},
		{"synthesize_text", column(tText)},
		{"fpe", column(tText)},
		{"uuid", column(tUUID)},
		{"random_token", column(tText)},
		{"hash", column(tInt64)},
	} {
		_, err := newMasker(t, c.col, c.use, nil).mask(value.Bool(true))
		if err == nil || !strings.Contains(err.Error(), "bool value") {
			t.Errorf("%s on a bool: %v", c.use, err)
		}
	}
}

func TestNoKeys(t *testing.T) {
	for _, use := range []string{"uuid", "fpe"} {
		f, err := Configure(column(tText), Rule{Use: use})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Mask(&Context{}, value.String("123e4567-e89b-42d3-a456-426614174000")); err == nil {
			t.Errorf("%s ran without keys", use)
		}
	}
}

func TestTag(t *testing.T) {
	var seed [32]byte
	if got := tag(seed); got != strings.Repeat("0", tagLen) {
		t.Errorf("tag of zeros = %q", got)
	}
	for i := range seed {
		seed[i] = 0xff
	}
	if got := tag(seed); got != strings.Repeat("z", tagLen) {
		t.Errorf("tag of ones = %q", got)
	}
	if 5*tagLen < collisionBits {
		t.Error("a tag carries fewer bits than CollisionResistant promises")
	}
}

func TestPhonePlans(t *testing.T) {
	r := seededRand(5)
	for region, p := range phonePlans {
		for i, text := range p.texts(r) {
			if len(text) != p.widths[i] {
				t.Errorf("%s: the %s format has %d characters, the plan says %d", region, phoneFormats[i], len(text), p.widths[i])
			}
			if i > 0 && p.widths[i] > p.widths[i-1] {
				t.Errorf("%s: the formats are not longest first", region)
			}
		}
		if len(p.digits(r)) != p.length {
			t.Errorf("%s: the digits have another length than the plan says", region)
		}
	}
}

// TestOutputsIgnoreColumn checks the rule that keeps foreign keys intact: a
// value's output depends only on the rule, the domain and the value, never
// on the column it is in. Each case masks the same inputs in columns that
// differ in type, length, nullability and uniqueness, and every column
// must give the same outputs (compared by their canonical encoding, so the
// integer 5 equals the decimal 5).
func TestOutputsIgnoreColumn(t *testing.T) {
	tInt16 := value.Type{Kind: value.TypeInt, Bits: 16}
	date, _ := value.Date(1990, 3, 15)
	u, _ := value.ParseUUID("123e4567-e89b-42d3-a456-426614174000")
	texts := []value.Value{value.String("Jane Doe"), value.String("jdoe@example.test"), value.String("Order-123456 x")}
	cases := []struct {
		rule   Rule
		in     []value.Value
		cols   []value.Type
		unique bool // also try unique columns; the rule must then be Distinct
	}{
		{Rule{Use: "first_name"}, texts, []value.Type{tText, tString(255), tString(40)}, false},
		{Rule{Use: "first_name", Options: map[string]any{"max_length": "6"}}, texts, []value.Type{tText, tString(6), tString(9)}, false},
		{Rule{Use: "last_name"}, texts, []value.Type{tText, tString(40)}, false},
		{Rule{Use: "full_name", Options: map[string]any{"max_length": "12"}}, texts, []value.Type{tText, tString(12), tString(50)}, false},
		{Rule{Use: "company"}, texts, []value.Type{tText, tString(60)}, false},
		{Rule{Use: "username"}, texts, []value.Type{tText, tString(60)}, false},
		{Rule{Use: "username", Distinct: true, Options: map[string]any{"max_length": "30"}}, texts, []value.Type{tText, tString(30), tString(80)}, true},
		{Rule{Use: "email"}, texts, []value.Type{tText, tString(100)}, false},
		{Rule{Use: "email", Distinct: true}, texts, []value.Type{tText, tString(100), tString(255)}, true},
		{Rule{Use: "email", Distinct: true, Options: map[string]any{"max_length": "40"}}, texts, []value.Type{tText, tString(40), tString(64)}, true},
		{Rule{Use: "phone"}, texts, []value.Type{tText, tString(15), tString(40)}, false},
		{Rule{Use: "phone", Options: map[string]any{"region": "GB", "format": "national"}}, texts, []value.Type{tText, tString(12)}, false},
		{Rule{Use: "phone"}, []value.Value{value.Int(5551234567)}, []value.Type{tInt64, tUint64, tDecimal0}, false},
		{Rule{Use: "address"}, texts, []value.Type{tText, tString(100)}, false},
		{Rule{Use: "address", Options: map[string]any{"max_length": "20"}}, texts, []value.Type{tText, tString(20)}, false},
		{Rule{Use: "address", Options: map[string]any{"part": "postcode"}}, []value.Value{value.Int(75001)}, []value.Type{tInt32, tInt64, tDecimal0}, false},
		{Rule{Use: "uuid", Distinct: true}, []value.Value{value.String("123e4567-e89b-42d3-a456-426614174000")}, []value.Type{tText, tString(36)}, true},
		{Rule{Use: "uuid", Distinct: true}, []value.Value{u}, []value.Type{tUUID}, true},
		{Rule{Use: "fpe", Distinct: true}, texts, []value.Type{tText, tString(40)}, true},
		{Rule{Use: "fpe", Distinct: true}, []value.Value{value.Int(1234567), value.Int(-7654321), value.Int(2147483647)}, []value.Type{tInt32, tInt64, tDecimal0}, true},
		{Rule{Use: "hash", Distinct: true}, texts, []value.Type{tText, tString(32), tString(64)}, true},
		{Rule{Use: "hash"}, []value.Value{value.Int(12345), value.Int(-300), value.Int(32767)}, []value.Type{tInt16, tInt32, tInt64, tDecimal0}, false},
		{Rule{Use: "random_token", Distinct: true}, []value.Value{value.String("tok_demo_AbCdEfGhIjKlMnOpQrStUvWx12")}, []value.Type{tText, tString(40)}, true},
		{Rule{Use: "random_token"}, []value.Value{value.Int(12345), value.Int(-300)}, []value.Type{tInt16, tInt32, tInt64}, false},
		{Rule{Use: "password_hash"}, texts, []value.Type{tText, tString(60), tString(255)}, false},
		{Rule{Use: "redact"}, texts, []value.Type{tText, tString(8), tString(20)}, false},
		{Rule{Use: "redact", Options: map[string]any{"max_length": "5"}}, texts, []value.Type{tText, tString(5)}, false},
		{Rule{Use: "noise"}, []value.Value{value.Int(1000), value.Int(-120), value.Int(32760)}, []value.Type{tInt16, tInt32, tInt64}, false},
		{Rule{Use: "noise"}, []value.Value{mustDecimal(t, "99.95"), mustDecimal(t, "-1234.50")}, []value.Type{tDecimal, {Kind: value.TypeDecimal, Precision: 20, Scale: 4}, {Kind: value.TypeDecimal}}, false},
		{Rule{Use: "noise"}, []value.Value{value.Float(98.6), value.Float(3e38)}, []value.Type{tFloat, {Kind: value.TypeFloat, Bits: 32}}, false},
		{Rule{Use: "date_shift"}, []value.Value{date}, []value.Type{tDate}, false},
		{Rule{Use: "date_shift"}, []value.Value{value.String("1990-03-15")}, []value.Type{tText, tString(10)}, false},
		{Rule{Use: "synthesize_text"}, texts, []value.Type{tText, tString(20)}, false},
		{Rule{Use: "constant", Options: map[string]any{"value": "x"}}, texts, []value.Type{tText, tString(1)}, false},
	}
	for _, c := range cases {
		var want []string
		for _, typ := range c.cols {
			variants := []Column{column(typ), column(typ)}
			variants[1].Nullable, variants[1].HasDefault, variants[1].EmptyStringIsNull = false, true, true
			if c.unique {
				variants = append(variants, uniqueColumn(typ))
			}
			for _, col := range variants {
				m := newMaskerRule(t, col, c.rule)
				var got []string
				for _, in := range c.in {
					got = append(got, string(appendCanonical(nil, m.must(in))))
				}
				if want == nil {
					want = got
				} else if !slices.Equal(got, want) {
					t.Errorf("%s %v: a %s column (nullable %v, unique %v) gives other outputs", c.rule.Use, c.rule.Options, typ, col.Nullable, col.Unique)
				}
			}
		}
	}
}
