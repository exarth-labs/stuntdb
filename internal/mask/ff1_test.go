package mask

import (
	"crypto/aes"
	"encoding/hex"
	"slices"
	"strings"
	"testing"
)

const base36 = "0123456789abcdefghijklmnopqrstuvwxyz"

func numerals(t *testing.T, s string) []uint16 {
	t.Helper()
	out := make([]uint16, len(s))
	for i := range len(s) {
		d := strings.IndexByte(base36, s[i])
		if d < 0 {
			t.Fatalf("bad numeral %q", s[i])
		}
		out[i] = uint16(d)
	}
	return out
}

func text(x []uint16) string {
	var b strings.Builder
	for _, d := range x {
		b.WriteByte(base36[d])
	}
	return b.String()
}

func newTestFF1(t *testing.T, keyHex string) *FF1 {
	t.Helper()
	key, err := hex.DecodeString(keyHex)
	if err != nil {
		t.Fatal(err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	return NewFF1(block)
}

// TestFF1NISTSamples checks the nine FF1 examples NIST publishes with SP
// 800-38G (FF1samples.pdf): AES-128, -192 and -256, radix 10 and 36, with
// and without a tweak.
func TestFF1NISTSamples(t *testing.T) {
	const (
		k128 = "2B7E151628AED2A6ABF7158809CF4F3C"
		k192 = "2B7E151628AED2A6ABF7158809CF4F3CEF4359D8D580AA4F"
		k256 = "2B7E151628AED2A6ABF7158809CF4F3CEF4359D8D580AA4F7F036D6F04FC6A94"
		t1   = "39383736353433323130"
		t2   = "3737373770717273373737"
	)
	tests := []struct {
		name, key, tweak string
		radix            int
		pt, ct           string
	}{
		{"sample 1", k128, "", 10, "0123456789", "2433477484"},
		{"sample 2", k128, t1, 10, "0123456789", "6124200773"},
		{"sample 3", k128, t2, 36, "0123456789abcdefghi", "a9tv40mll9kdu509eum"},
		{"sample 4", k192, "", 10, "0123456789", "2830668132"},
		{"sample 5", k192, t1, 10, "0123456789", "2496655549"},
		{"sample 6", k192, t2, 36, "0123456789abcdefghi", "xbj3kv35jrawxv32ysr"},
		{"sample 7", k256, "", 10, "0123456789", "6657667009"},
		{"sample 8", k256, t1, 10, "0123456789", "1001623463"},
		{"sample 9", k256, t2, 36, "0123456789abcdefghi", "xs8a0azh2avyalyzuwd"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newTestFF1(t, tt.key)
			tweak, _ := hex.DecodeString(tt.tweak)
			pt := numerals(t, tt.pt)
			ct, err := f.Encrypt(tt.radix, pt, tweak)
			if err != nil {
				t.Fatal(err)
			}
			if got := text(ct); got != tt.ct {
				t.Errorf("Encrypt = %s, want %s", got, tt.ct)
			}
			back, err := f.Decrypt(tt.radix, ct, tweak)
			if err != nil {
				t.Fatal(err)
			}
			if got := text(back); got != tt.pt {
				t.Errorf("Decrypt = %s, want %s", got, tt.pt)
			}
			if text(pt) != tt.pt {
				t.Error("Encrypt modified its input")
			}
		})
	}
}

// TestFF1IsAPermutation encrypts every value of the smallest allowed
// domain and checks that no two collide; for radix 2 and an odd length
// (unequal halves), a prefix of the domain.
func TestFF1IsAPermutation(t *testing.T) {
	f := newTestFF1(t, strings.Repeat("ab", 32))
	for _, c := range []struct{ radix, n, limit int }{{10, 6, 1_000_000}, {2, 20, 100_000}, {10, 7, 100_000}} {
		if testing.Short() && c.limit > 100_000 {
			c.limit = 100_000
		}
		size := c.limit
		seen := make(map[string]bool, size)
		x := make([]uint16, c.n)
		for i := range size {
			v := i
			for j := c.n - 1; j >= 0; j-- {
				x[j] = uint16(v % c.radix)
				v /= c.radix
			}
			ct, err := f.Encrypt(c.radix, x, []byte("tweak"))
			if err != nil {
				t.Fatal(err)
			}
			key := string(slices.Clone(toBytes(ct)))
			if seen[key] {
				t.Fatalf("radix %d, length %d: two inputs encrypt to the same output", c.radix, c.n)
			}
			seen[key] = true
		}
	}
}

func toBytes(x []uint16) []byte {
	b := make([]byte, 0, 2*len(x))
	for _, d := range x {
		b = append(b, byte(d>>8), byte(d))
	}
	return b
}

func TestFF1Refuses(t *testing.T) {
	f := newTestFF1(t, strings.Repeat("00", 32))
	tests := []struct {
		name  string
		radix int
		x     []uint16
	}{
		{"radix 1", 1, make([]uint16, 30)},
		{"radix too large", ff1MaxRadix + 1, make([]uint16, 4)},
		{"one numeral", 65536, []uint16{1}},
		{"domain under a million", 10, make([]uint16, 5)},
		{"numeral not in the radix", 10, []uint16{1, 2, 3, 4, 5, 10}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := f.Encrypt(tt.radix, tt.x, nil); err == nil {
				t.Error("Encrypt succeeded")
			}
		})
	}
	// The smallest domains that are allowed.
	for _, c := range []struct{ radix, n int }{{10, 6}, {26, 5}, {62, 4}, {2, 20}} {
		if !ff1Domain(c.radix, c.n) || ff1Domain(c.radix, c.n-1) {
			t.Errorf("radix %d: the minimum length should be %d", c.radix, c.n)
		}
	}
}

func TestFF1TweakChangesOutput(t *testing.T) {
	f := newTestFF1(t, strings.Repeat("11", 32))
	x := numerals(t, "0123456789")
	a, _ := f.Encrypt(10, x, []byte("domain a"))
	b, _ := f.Encrypt(10, x, []byte("domain b"))
	if slices.Equal(a, b) {
		t.Error("two tweaks gave the same output")
	}
}

func BenchmarkFF1Radix10(b *testing.B) {
	block, _ := aes.NewCipher(make([]byte, 32))
	f := NewFF1(block)
	x := make([]uint16, 16)
	for b.Loop() {
		_, _ = f.Encrypt(10, x, []byte("bench"))
	}
}
