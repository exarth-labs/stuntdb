package mask

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"math/big"
)

// FF1MinDomain is the smallest domain FF1 may encrypt: radix^length must
// be at least one million (NIST SP 800-38G Rev. 1). Smaller domains are
// open to known attacks, so FF1 refuses them.
const FF1MinDomain = 1_000_000

const (
	ff1MaxRadix  = 1 << 16
	ff1MaxLength = 1 << 16 // far beyond any column value; the standard allows 2^32
	ff1Rounds    = 10
)

var (
	errFF1Radix  = errors.New("mask: FF1 needs a radix from 2 to 65536")
	errFF1Length = errors.New("mask: FF1 needs at least 2 numerals and a domain of at least 1,000,000 values")
	errFF1Digit  = errors.New("mask: FF1 numeral out of range for the radix")
)

// FF1 is the format-preserving encryption mode FF1 of NIST SP 800-38G
// (Rev. 1, second public draft) over AES. It encrypts a string of numerals
// in a radix into another string of the same length and radix: a keyed
// permutation, so distinct inputs always give distinct outputs. The
// arithmetic is integer only, as Rev. 1 requires. It is safe for concurrent
// use.
type FF1 struct {
	block cipher.Block
}

// NewFF1 returns FF1 with an AES block cipher. stuntdb uses AES-256.
func NewFF1(block cipher.Block) *FF1 {
	if block.BlockSize() != aes.BlockSize {
		panic("mask: FF1 needs a 128-bit block cipher")
	}
	return &FF1{block: block}
}

// Encrypt returns the encryption of the numerals x, each less than radix,
// under the tweak. x is not modified.
func (f *FF1) Encrypt(radix int, x []uint16, tweak []byte) ([]uint16, error) {
	return f.crypt(radix, x, tweak, true)
}

// Decrypt reverses Encrypt.
func (f *FF1) Decrypt(radix int, x []uint16, tweak []byte) ([]uint16, error) {
	return f.crypt(radix, x, tweak, false)
}

// ff1Domain reports whether n numerals of a radix make a domain FF1 accepts.
func ff1Domain(radix, n int) bool {
	if radix < 2 || radix > ff1MaxRadix || n < 2 || n > ff1MaxLength {
		return false
	}
	size := 1
	for range n {
		size *= radix
		if size >= FF1MinDomain {
			return true
		}
	}
	return false
}

func (f *FF1) crypt(radix int, x []uint16, tweak []byte, encrypt bool) ([]uint16, error) {
	if radix < 2 || radix > ff1MaxRadix {
		return nil, errFF1Radix
	}
	n := len(x)
	if !ff1Domain(radix, n) || len(tweak) > 1<<31 {
		return nil, errFF1Length
	}
	for _, d := range x {
		if int(d) >= radix {
			return nil, errFF1Digit
		}
	}

	// Steps 1–4: split, and size the numbers.
	u := n / 2
	v := n - u
	bigRadix := big.NewInt(int64(radix))
	modU := new(big.Int).Exp(bigRadix, big.NewInt(int64(u)), nil)
	modV := new(big.Int).Exp(bigRadix, big.NewInt(int64(v)), nil)
	b := (new(big.Int).Sub(modV, big.NewInt(1)).BitLen() + 7) / 8 // ⌈⌈v·log2(radix)⌉/8⌉, without floats
	d := 4*((b+3)/4) + 4
	t := len(tweak)

	// Step 5: the fixed block P.
	var p [16]byte
	p[0], p[1], p[2] = 1, 2, 1
	p[3], p[4], p[5] = byte(radix>>16), byte(radix>>8), byte(radix) //nolint:gosec // radix ≤ 2^16, checked above: three bytes hold it
	p[6] = 10
	p[7] = byte(u % 256)
	binary.BigEndian.PutUint32(p[8:12], uint32(n))  //nolint:gosec // n ≤ ff1MaxLength
	binary.BigEndian.PutUint32(p[12:16], uint32(t)) //nolint:gosec // t ≤ 2^31, checked above

	// Q's layout: T, zero padding, the round number, then NUM(B) in b bytes.
	pad := ((-t-b-1)%16 + 16) % 16
	q := make([]byte, t+pad+1+b)
	copy(q, tweak)
	roundAt := t + pad

	numA := num(x[:u], bigRadix)
	numB := num(x[u:], bigRadix)
	r := make([]byte, 16)
	s := make([]byte, ((d+15)/16)*16)
	y, c := new(big.Int), new(big.Int)

	for round := range ff1Rounds {
		i := round
		if !encrypt {
			i = ff1Rounds - 1 - round
		}
		// Step 6.i: Q ends with the round number and the half that is
		// kept: B when encrypting, A when decrypting.
		q[roundAt] = byte(i)
		kept := numB
		if !encrypt {
			kept = numA
		}
		kept.FillBytes(q[roundAt+1:])

		// Step 6.ii: R = PRF(P ‖ Q), a CBC-MAC with a zero IV.
		f.block.Encrypt(r, p[:])
		for off := 0; off < len(q); off += 16 {
			for j := range 16 {
				r[j] ^= q[off+j]
			}
			f.block.Encrypt(r, r)
		}

		// Step 6.iii: S is R, then the encryptions of R ⊕ [j]^16, cut to d bytes.
		copy(s, r)
		var blockJ [16]byte
		for j := 1; j*16 < d; j++ {
			copy(blockJ[:], r)
			var ctr [16]byte
			binary.BigEndian.PutUint64(ctr[8:], uint64(j))
			for k := range 16 {
				blockJ[k] ^= ctr[k]
			}
			f.block.Encrypt(s[j*16:], blockJ[:])
		}

		// Steps 6.iv–ix.
		y.SetBytes(s[:d])
		mod := modU
		if i%2 == 1 {
			mod = modV
		}
		if encrypt {
			c.Add(numA, y)
			c.Mod(c, mod)
			numA, numB = numB, new(big.Int).Set(c)
		} else {
			c.Sub(numB, y)
			c.Mod(c, mod) // big.Int.Mod is Euclidean: never negative
			numA, numB = new(big.Int).Set(c), numA
		}
	}

	out := make([]uint16, n)
	str(numA, bigRadix, out[:u])
	str(numB, bigRadix, out[u:])
	return out, nil
}

// num is NUM_radix: the number that numerals x write, most significant first.
func num(x []uint16, radix *big.Int) *big.Int {
	z := new(big.Int)
	digit := new(big.Int)
	for _, d := range x {
		z.Mul(z, radix)
		z.Add(z, digit.SetUint64(uint64(d)))
	}
	return z
}

// str is STR^m_radix: z written in len(out) numerals, most significant first.
func str(z, radix *big.Int, out []uint16) {
	z = new(big.Int).Set(z)
	rem := new(big.Int)
	for i := len(out) - 1; i >= 0; i-- {
		z.QuoRem(z, radix, rem)
		out[i] = uint16(rem.Uint64()) //nolint:gosec // less than the radix, at most 2^16
	}
}
