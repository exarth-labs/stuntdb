package mask

import (
	"encoding/binary"
	"math/big"
	"math/bits"
	"math/rand/v2"
)

// Rand is the deterministic random stream of one masked value.
//
// It uses only the Uint64 stream of Go's ChaCha8, which follows the
// C2SP chacha8rand specification and which Go keeps stable with a golden
// test. Everything built on top, such as numbers in a range, is stuntdb's
// own code, because math/rand/v2 makes no stability promise for its Rand
// methods. Golden tests pin the results, which are part of GeneratorVersion.
//
// A Rand is reseeded for every value, so it is not safe for concurrent use.
type Rand struct {
	c    *rand.ChaCha8
	seed [32]byte
}

func newRand() *Rand { return &Rand{c: rand.NewChaCha8([32]byte{})} }

// reset starts the stream for a new seed.
func (r *Rand) reset(seed [32]byte) {
	r.seed = seed
	r.c.Seed(seed)
}

// Seed returns the seed the stream started from: 32 bytes of a keyed hash
// of the input, for transformers that embed a collision-resistant tag.
func (r *Rand) Seed() [32]byte { return r.seed }

// Uint64 returns the next 64 random bits.
func (r *Rand) Uint64() uint64 { return r.c.Uint64() }

// Uint64N returns a uniform number in [0, n). n must not be 0.
//
// It is Lemire's multiply-and-reject method ("Fast Random Integer
// Generation in an Interval", ACM TOMACS, 2019): the high 64 bits of a
// 128-bit product are uniform once the low bits fall outside a small biased
// zone, which is redrawn.
func (r *Rand) Uint64N(n uint64) uint64 {
	if n == 0 {
		panic("mask: Uint64N(0)")
	}
	hi, lo := bits.Mul64(r.Uint64(), n)
	if lo < n {
		threshold := -n % n // 2^64 mod n
		for lo < threshold {
			hi, lo = bits.Mul64(r.Uint64(), n)
		}
	}
	return hi
}

// bigN returns a uniform number in [0, n). n must be positive. Numbers that
// fit 64 bits use Uint64N. Wider ones use rejection: draw as many bits as n
// has, and draw again when the number is n or more (less than half the
// time).
func (r *Rand) bigN(n *big.Int) *big.Int {
	if n.Sign() <= 0 {
		panic("mask: bigN of a number that is not positive")
	}
	if n.IsUint64() {
		return new(big.Int).SetUint64(r.Uint64N(n.Uint64()))
	}
	bitLen := n.BitLen()
	buf := make([]byte, (bitLen+7)/8)
	z := new(big.Int)
	for {
		r.Read(buf)
		buf[0] &= byte(0xff >> (8*len(buf) - bitLen)) // keep bitLen bits
		if z.SetBytes(buf).Cmp(n) < 0 {
			return z
		}
	}
}

// IntN returns a uniform number in [0, n). n must be positive.
func (r *Rand) IntN(n int) int {
	if n <= 0 {
		panic("mask: IntN of a number that is not positive")
	}
	return int(r.Uint64N(uint64(n))) //nolint:gosec // less than n, which is an int
}

// Between returns a uniform number in [lo, hi]. lo must not exceed hi.
func (r *Rand) Between(lo, hi int) int {
	if lo > hi {
		panic("mask: Between with lo > hi")
	}
	return lo + int(r.Uint64N(uint64(hi-lo)+1)) //nolint:gosec // at most hi-lo, which fits an int
}

// Pick returns a uniform element of a non-empty list.
func Pick[T any](r *Rand, list []T) T { return list[r.IntN(len(list))] }

// Read fills p with random bytes: each Uint64, little-endian, in turn.
func (r *Rand) Read(p []byte) {
	for len(p) >= 8 {
		binary.LittleEndian.PutUint64(p, r.Uint64())
		p = p[8:]
	}
	if len(p) > 0 {
		var last [8]byte
		binary.LittleEndian.PutUint64(last[:], r.Uint64())
		copy(p, last[:])
	}
}

// String returns n characters drawn uniformly from alphabet.
func (r *Rand) String(alphabet string, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = alphabet[r.IntN(len(alphabet))]
	}
	return string(b)
}
