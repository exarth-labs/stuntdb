package mask

import (
	"fmt"
	"math"
	"math/big"
	"strings"
	"testing"
)

func seededRand(seed byte) *Rand {
	r := newRand()
	var s [32]byte
	for i := range s {
		s[i] = seed + byte(i)
	}
	r.reset(s)
	return r
}

// TestRandGolden pins the stream and stuntdb's range reduction: a Go
// release that changed ChaCha8, or a change to Uint64N, would show here.
func TestRandGolden(t *testing.T) {
	r := seededRand(1)
	var b strings.Builder
	for range 4 {
		fmt.Fprintf(&b, "uint64 %d\n", r.Uint64())
	}
	for _, n := range []uint64{1, 2, 7, 10, 1000, 1 << 40, math.MaxUint64} {
		fmt.Fprintf(&b, "uint64n(%d) %d\n", n, r.Uint64N(n))
	}
	fmt.Fprintf(&b, "between(-5,5) %d\n", r.Between(-5, 5))
	fmt.Fprintf(&b, "string %s\n", r.String("abcdef", 12))
	p := make([]byte, 13)
	r.Read(p)
	fmt.Fprintf(&b, "read %x\n", p)
	// Ranges where draws are redrawn: a quarter of the time for 3·2^62
	// (Uint64N's rejection), and about half the time just above 2^100
	// (bigN's).
	for range 8 {
		fmt.Fprintf(&b, "uint64n(3<<62) %d\n", r.Uint64N(3<<62))
	}
	for _, n := range []*big.Int{threeTimes2To64(), twoTo100Plus1()} {
		for range 4 {
			fmt.Fprintf(&b, "bign(%s) %s\n", n, r.bigN(n))
		}
	}
	golden(t, "rand", b.String())
}

func threeTimes2To64() *big.Int { return new(big.Int).Lsh(big.NewInt(3), 64) }

func twoTo100Plus1() *big.Int {
	return new(big.Int).Add(new(big.Int).Lsh(big.NewInt(1), 100), big.NewInt(1))
}

// TestUint64NRejects checks Lemire's rejection step where it matters most.
// For n = 3·2^62, the plain multiply-and-shift maps four draws onto three
// numbers, so multiples of 3 would come up half the time. The rejection
// step redraws a quarter of the draws, and makes them come up a third of
// the time, as they should.
func TestUint64NRejects(t *testing.T) {
	r := seededRand(7)
	const n, draws = 3 << 62, 30_000
	multiples := 0
	for range draws {
		x := r.Uint64N(n)
		if x >= n {
			t.Fatalf("Uint64N(%d) = %d", uint64(n), x)
		}
		if x%3 == 0 {
			multiples++
		}
	}
	// Expected 10,000 with a standard deviation of 82; without rejection, 15,000.
	if multiples < 9_500 || multiples > 10_500 {
		t.Errorf("multiples of 3: %d of %d draws, want about a third", multiples, draws)
	}
}

// TestBigN checks draws from ranges wider than 64 bits: always below n,
// and uniform. For n = 3·2^64, a quarter of the draws are redrawn; without
// that, a quarter of the results would be n or more.
func TestBigN(t *testing.T) {
	r := seededRand(11)
	n := threeTimes2To64()
	third := new(big.Int).Lsh(big.NewInt(1), 64)
	const draws = 30_000
	var thirds [3]int
	for range draws {
		z := r.bigN(n)
		if z.Sign() < 0 || z.Cmp(n) >= 0 {
			t.Fatalf("bigN(%s) = %s", n, z)
		}
		thirds[new(big.Int).Div(z, third).Int64()]++
	}
	for i, c := range thirds {
		if c < draws/3*95/100 || c > draws/3*105/100 {
			t.Errorf("third %d drawn %d times of %d", i, c, draws)
		}
	}
	m := twoTo100Plus1()
	for range 1000 {
		if z := r.bigN(m); z.Sign() < 0 || z.Cmp(m) >= 0 {
			t.Fatalf("bigN(%s) = %s", m, z)
		}
	}
	if got := r.bigN(big.NewInt(1)); got.Sign() != 0 {
		t.Errorf("bigN(1) = %s", got)
	}
}

func TestRandReset(t *testing.T) {
	r := seededRand(9)
	first := r.Uint64()
	r.Uint64()
	r.reset(seededRand(9).Seed())
	if r.Uint64() != first {
		t.Error("reset does not restart the stream")
	}
}

// TestUint64NIsUniform draws from a range that is not a power of two and
// checks every result is in range and every bucket is hit about equally.
func TestUint64NIsUniform(t *testing.T) {
	r := seededRand(3)
	const n, draws = 6, 60_000
	var counts [n]int
	for range draws {
		x := r.Uint64N(n)
		if x >= n {
			t.Fatalf("Uint64N(%d) = %d", n, x)
		}
		counts[x]++
	}
	for i, c := range counts {
		if c < draws/n*9/10 || c > draws/n*11/10 {
			t.Errorf("bucket %d drawn %d times of %d", i, c, draws)
		}
	}
	if got := r.Between(7, 7); got != 7 {
		t.Errorf("Between(7, 7) = %d", got)
	}
}

func TestRandPanics(t *testing.T) {
	for name, f := range map[string]func(r *Rand){
		"Uint64N(0)":   func(r *Rand) { r.Uint64N(0) },
		"IntN(0)":      func(r *Rand) { r.IntN(0) },
		"Between(2,1)": func(r *Rand) { r.Between(2, 1) },
		"bigN(0)":      func(r *Rand) { r.bigN(new(big.Int)) },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("no panic")
				}
			}()
			f(seededRand(0))
		})
	}
}
