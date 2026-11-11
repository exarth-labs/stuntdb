package mask

import (
	"bytes"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/exarth-labs/stuntdb/internal/value"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// testKey is the masking key of every test. Golden files pin what it gives.
const testKey = "stuntdb test key, never used for real data"

func testKeys(t testing.TB) *Keys {
	t.Helper()
	k, err := NewKeys([]byte(testKey))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// golden compares got with testdata/<name>.golden, or rewrites the file
// with -update.
func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run the test with -update to create it)", err)
	}
	if got != strings.ReplaceAll(string(want), "\r\n", "\n") {
		t.Errorf("output differs from %s; a change to masked values needs a new GeneratorVersion.\ngot:\n%s", path, got)
	}
}

func mustDecimal(t testing.TB, s string) value.Value {
	t.Helper()
	v, err := value.Decimal(s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestNewKeys(t *testing.T) {
	if _, err := NewKeys(make([]byte, MinKeyLength-1)); err == nil {
		t.Error("a short key is accepted")
	}
	a, b := testKeys(t), testKeys(t)
	if a.Fingerprint() != b.Fingerprint() || !strings.HasPrefix(a.Fingerprint(), "hkdf-sha256:") {
		t.Errorf("fingerprint = %q, %q", a.Fingerprint(), b.Fingerprint())
	}
	other, _ := NewKeys([]byte(testKey + "!"))
	if other.Fingerprint() == a.Fingerprint() {
		t.Error("two keys have the same fingerprint")
	}
	// The subkeys are separate: none equals another, and none shows the key.
	fp, _ := hex.DecodeString(strings.TrimPrefix(a.Fingerprint(), "hkdf-sha256:"))
	if bytes.HasPrefix(a.seed, fp) || bytes.Contains(a.seed, []byte(testKey)) {
		t.Error("the seed key overlaps the fingerprint or the masking key")
	}
	golden(t, "fingerprint", a.Fingerprint()+"\n")
}

// TestKeysNeverPrint prints Keys with every fmt verb and through both slog
// handlers, as a pointer and as a value, and checks that only the
// fingerprint shows: no subkey, in raw bytes, hexadecimal or Go syntax.
func TestKeysNeverPrint(t *testing.T) {
	k := testKeys(t)
	want := "mask.Keys(" + k.Fingerprint() + ")"
	leaks := func(s string) bool {
		return strings.Contains(s, string(k.seed)) || strings.Contains(s, hex.EncodeToString(k.seed)) ||
			strings.Contains(s, fmt.Sprintf("%#v", k.seed)) || strings.Contains(s, "seed")
	}
	for _, arg := range []any{k, *k} {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d"} {
			if s := fmt.Sprintf(verb, arg); s != want {
				t.Errorf("Sprintf(%q, %T) = %q, want %q", verb, arg, s, want)
			}
		}
		for name, h := range map[string]func(*bytes.Buffer) slog.Handler{
			"text": func(b *bytes.Buffer) slog.Handler { return slog.NewTextHandler(b, nil) },
			"json": func(b *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(b, nil) },
		} {
			var b bytes.Buffer
			slog.New(h(&b)).Info("masking", "keys", arg)
			if out := b.String(); leaks(out) || !strings.Contains(out, k.Fingerprint()) {
				t.Errorf("slog %s handler with a %T writes %q", name, arg, out)
			}
		}
	}
	if leaks(want) {
		t.Fatal("the test's leak check matches the fingerprint itself")
	}
}

// TestCanonicalFamilies checks that equal values give equal seeds whatever
// their kind, as databases compare them, and that different values don't.
func TestCanonicalFamilies(t *testing.T) {
	s := newSeeder(testKeys(t))
	seed := func(v value.Value) [32]byte { return s.seed("d", "hash", v) }
	instant := time.Date(2026, 10, 19, 8, 45, 0, 0, time.UTC)

	equal := [][]value.Value{
		{value.Int(5), value.Uint(5), mustDecimal(t, "5"), mustDecimal(t, "5.00"), mustDecimal(t, "+005.0")},
		{value.Int(0), mustDecimal(t, "-0.000"), mustDecimal(t, "0")},
		{value.Int(-12), mustDecimal(t, "-12.0")},
		{value.Int(math.MinInt64), mustDecimal(t, "-9223372036854775808")},
		{value.Uint(math.MaxUint64), mustDecimal(t, "18446744073709551615")},
		{value.Float(0), value.Float(math.Copysign(0, -1))},
		{value.Float(math.NaN()), value.Float(math.Float64frombits(0x7ff8000000000001))},
		{value.TimestampTZ(instant), value.TimestampTZWithZone(instant.In(time.FixedZone("", 5*3600)))},
	}
	for i, group := range equal {
		for _, v := range group[1:] {
			if seed(v) != seed(group[0]) {
				t.Errorf("group %d: %s and %s have different seeds", i, v.Kind(), group[0].Kind())
			}
		}
	}

	distinct := []value.Value{
		value.Int(5), value.String("5"), value.Float(5), value.Bytes([]byte("5")), mustDecimal(t, "5.1"),
		value.Bool(true), value.Int(1), value.String(""), value.Bytes(nil),
		value.Timestamp(instant), value.TimestampTZ(instant),
		value.Array([]value.Value{value.String("ab"), value.String("c")}),
		value.Array([]value.Value{value.String("a"), value.String("bc")}),
		value.Array([]value.Value{value.String("abc")}),
		value.DecimalNaN(), value.DecimalInfinity(false), value.DecimalInfinity(true),
		value.MySQLZeroDate("0000-00-00"), value.MySQLZeroDate("2026-00-15"),
		value.Interval(1, 0, 0), value.Interval(0, 30, 0),
	}
	seen := map[[32]byte]int{}
	for i, v := range distinct {
		if j, dup := seen[seed(v)]; dup {
			t.Errorf("values %d and %d (%s, %s) have the same seed", j, i, distinct[j].Kind(), v.Kind())
		}
		seen[seed(v)] = i
	}
}

// TestSeedFieldsAreUnambiguous checks that moving bytes between the domain,
// the transformer and the value never gives the same seed.
func TestSeedFieldsAreUnambiguous(t *testing.T) {
	s := newSeeder(testKeys(t))
	seeds := [][32]byte{
		s.seed("users.email", "email", value.String("x")),
		s.seed("users.emai", "email", value.String("lx")),
		s.seed("users.emailx", "email", value.String("")),
		s.seed("users.email", "emailx", value.String("")),
		s.seed("users.emaile", "mail", value.String("x")),
	}
	for i := range seeds {
		for j := range i {
			if seeds[i] == seeds[j] {
				t.Errorf("seeds %d and %d are equal: the fields run into each other", j, i)
			}
		}
	}
	other, _ := NewKeys([]byte(testKey + "?"))
	if newSeeder(other).seed("users.email", "email", value.String("x")) == seeds[0] {
		t.Error("another key gives the same seed")
	}
}

// TestSeedStreamsAreSeparate checks that two transformers seeded from the
// same key in the same domain (per:) get unrelated streams. If they shared
// one, a fake first name, which the copy shows, would tell the number of
// days the same person's birth date moved: both are drawn from the first
// number of the stream.
func TestSeedStreamsAreSeparate(t *testing.T) {
	s := newSeeder(testKeys(t))
	id := value.Int(42)
	names := s.seed("main.people.id", "first_name", id)
	dates := s.seed("main.people.id", "date_shift", id)
	if names == dates {
		t.Fatal("first_name and date_shift share a stream")
	}
	if again := s.seed("main.people.id", "date_shift", id); again != dates {
		t.Error("one transformer with one key gets two streams")
	}
}

func TestNormalizeDecimal(t *testing.T) {
	tests := map[string]string{
		"0": "0", "-0": "0", "+0.000": "0", "007": "7", "7.": "7", "-7.50": "-7.5",
		".5": "0.5", "-.50": "-0.5", "1234567890123456789012345.000100": "1234567890123456789012345.0001",
	}
	for in, want := range tests {
		if got := normalizeDecimal(in); got != want {
			t.Errorf("normalizeDecimal(%q) = %q, want %q", in, got, want)
		}
	}
}

// FuzzNormalizeDecimal checks that two decimals have the same canonical text
// exactly when they are the same number.
func FuzzNormalizeDecimal(f *testing.F) {
	for _, s := range []string{"0", "-0.0", "10.50", "010.5", ".5", "1e5", "-", "1.2.3"} {
		f.Add(s, "10.5")
	}
	f.Fuzz(func(t *testing.T, a, b string) {
		va, errA := value.Decimal(a)
		vb, errB := value.Decimal(b)
		if errA != nil || errB != nil {
			return
		}
		ra, _ := new(big.Rat).SetString(va.DecimalText())
		rb, _ := new(big.Rat).SetString(vb.DecimalText())
		same := normalizeDecimal(a) == normalizeDecimal(b)
		if same != (ra.Cmp(rb) == 0) {
			t.Errorf("%q and %q: canonical texts equal = %v, numbers equal = %v", a, b, same, ra.Cmp(rb) == 0)
		}
		if _, err := value.Decimal(normalizeDecimal(a)); err != nil {
			t.Errorf("the canonical text of %q is not a decimal", a)
		}
	})
}

// TestSeedFormula recomputes a seed from the formula in the package
// documentation, with the standard library only, so that the documented
// derivation and the code can't drift apart.
func TestSeedFormula(t *testing.T) {
	field := func(b []byte) []byte {
		return append(binary.BigEndian.AppendUint64(nil, uint64(len(b))), b...)
	}
	seedKey, err := hkdf.Key(sha256.New, []byte(testKey), nil, "stuntdb/mask/v1/seed", 32)
	if err != nil {
		t.Fatal(err)
	}
	var msg []byte
	msg = append(msg, field([]byte("stuntdb/mask/seed"))...)
	msg = append(msg, field(binary.BigEndian.AppendUint64(nil, 1))...) // GeneratorVersion
	msg = append(msg, field([]byte("main.users.email"))...)
	msg = append(msg, field([]byte("email"))...)                                           // the transformer
	msg = append(msg, field(append([]byte{3}, field([]byte("person@example.com"))...))...) // family text
	mac := hmac.New(sha256.New, seedKey)
	mac.Write(msg)
	var want [32]byte
	copy(want[:], mac.Sum(nil))

	if got := newSeeder(testKeys(t)).seed("main.users.email", "email", value.String("person@example.com")); got != want {
		t.Errorf("seed = %x, the documented formula gives %x", got, want)
	}
}

// TestSeedGolden pins the seeds of a few values: the HKDF subkeys, the
// field encoding and the canonical families. A change here changes every
// masked value.
func TestSeedGolden(t *testing.T) {
	s := newSeeder(testKeys(t))
	var b strings.Builder
	for _, c := range []struct {
		domain, transformer string
		v                   value.Value
	}{
		{"main.users.email", "email", value.String("person@example.com")},
		{"main.users.id", "hash", value.Int(42)},
		{"main.users.id", "hash", mustDecimal(t, "42.000")},
		{"main.users.id", "date_shift", value.Int(42)},
		{"person.email", "email", value.String("")},
		{"main.users.photo", "random_token", value.Bytes([]byte{0, 1, 2})},
	} {
		seed := s.seed(c.domain, c.transformer, c.v)
		fmt.Fprintf(&b, "%s %s %s %x\n", c.domain, c.transformer, c.v.Kind(), seed)
	}
	golden(t, "seeds", b.String())
}
