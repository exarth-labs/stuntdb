package mask

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/exarth-labs/stuntdb/internal/value"
)

// tweak builds an FF1 tweak from a transformer's label, the domain and
// extra context, each length-prefixed, so that every (transformer, domain,
// format) has its own permutation.
func tweak(label, domain string, extra []byte) []byte {
	b := appendField(nil, []byte(label))
	b = appendField(b, []byte(domain))
	return appendField(b, extra)
}

func uuidTransformer() *Transformer {
	return &Transformer{
		Name: "uuid",
		Summary: "Another UUID, from a keyed permutation (NIST FF1) of the 122 bits that are not the version or " +
			"the variant, so that distinct UUIDs stay distinct and keep their version. Text keeps its layout.",
		Types: []value.TypeKind{value.TypeUUID, value.TypeString, value.TypeText, value.TypeBinary},
		// One UUID gets one permutation, whether it is stored natively, as
		// 16 bytes or as text: the output is written back the same way.
		alike: func(value.Type) string { return "uuid" },
		build: func(target, *Options) (*fn, error) {
			return &fn{guarantee: Bijective, mask: maskUUID, fit: func(col Column) error {
				switch room := maxLen(col); {
				case room == 0:
				case col.Type.Kind == value.TypeBinary && room < 16:
					return errors.New("uuid needs a binary column of at least 16 bytes")
				case col.Type.Kind == value.TypeString && room < 32:
					return fmt.Errorf("uuid writes UUIDs as the text had them, in 32 or 36 characters, and the column holds %d", room)
				}
				return nil
			}}, nil
		},
	}
}

// uuidLayout remembers how a UUID was written as text.
type uuidLayout struct{ hyphens, upper bool }

func maskUUID(c *Context, in value.Value) (value.Value, error) {
	if c.Keys == nil {
		return value.Value{}, errNoKeys
	}
	var u [16]byte
	var layout uuidLayout
	switch in.Kind() {
	case value.KindUUID:
		u = in.UUID()
	case value.KindBytes:
		b := in.Bytes()
		if len(b) != 16 {
			return value.Value{}, errors.New("uuid needs 16 bytes, and the value has another length")
		}
		copy(u[:], b)
	case value.KindString:
		s := in.Text()
		v, err := value.ParseUUID(s)
		if err != nil {
			return value.Value{}, errors.New("uuid cannot read the text as a UUID")
		}
		u = v.UUID()
		layout = uuidLayout{hyphens: len(s) == 36, upper: s != strings.ToLower(s)}
	default:
		return value.Value{}, errKind("uuid", in)
	}

	out, err := permuteUUID(c.Keys.ff1, c.Domain, u)
	if err != nil {
		return value.Value{}, err
	}
	switch in.Kind() {
	case value.KindUUID:
		return value.UUID(out), nil
	case value.KindBytes:
		return value.Bytes(out[:]), nil
	default:
		s := value.FormatUUID(out)
		if !layout.hyphens {
			s = strings.ReplaceAll(s, "-", "")
		}
		if layout.upper {
			s = strings.ToUpper(s)
		}
		return value.String(s), nil
	}
}

// fixedUUIDBit reports whether bit i (0 = the most significant bit of byte
// 0) of a UUID is part of its version (byte 6, high nibble) or its variant
// (byte 8, two high bits). Those six bits are kept, and the other 122 are
// permuted: a permutation within each class of UUIDs, so the whole map is a
// permutation of all 2^128 values.
func fixedUUIDBit(i int) bool {
	return (i >= 48 && i < 52) || (i >= 64 && i < 66)
}

func permuteUUID(f *FF1, domain string, u [16]byte) ([16]byte, error) {
	x := make([]uint16, 0, 122)
	for i := range 128 {
		if !fixedUUIDBit(i) {
			x = append(x, uint16(u[i/8]>>(7-i%8)&1))
		}
	}
	y, err := f.Encrypt(2, x, tweak("uuid", domain, nil))
	if err != nil {
		return u, err
	}
	out := u
	j := 0
	for i := range 128 {
		if fixedUUIDBit(i) {
			continue
		}
		mask := byte(1) << (7 - i%8)
		if y[j] == 1 {
			out[i/8] |= mask
		} else {
			out[i/8] &^= mask
		}
		j++
	}
	return out, nil
}

// fpeAlphabets are the alphabets fpe can permute. Characters outside the
// alphabet keep their place.
var fpeAlphabets = map[string]string{
	"digits": "0123456789",
	"lower":  "abcdefghijklmnopqrstuvwxyz",
	"upper":  "ABCDEFGHIJKLMNOPQRSTUVWXYZ",
	"alnum":  "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz",
}

func fpeTransformer() *Transformer {
	return &Transformer{
		Name: "fpe",
		Summary: "Format-preserving encryption (NIST FF1, AES-256): each character of the alphabet (digits, lower, " +
			"upper or alnum) becomes another, and every other character keeps its place. Distinct values stay distinct, " +
			"with the same length. Whole numbers of at least 6 digits keep their sign, their number of digits and the " +
			"integer types they fit.",
		Types:   []value.TypeKind{value.TypeString, value.TypeText, value.TypeInt, value.TypeDecimal},
		Options: []string{"alphabet"},
		build: func(t target, o *Options) (*fn, error) {
			if isNumber(t) {
				if a, ok := o.Text("alphabet"); ok && a != "digits" {
					return nil, errors.New("option alphabet: fpe keeps numbers as digits, so only digits is allowed in a number column")
				}
				return &fn{guarantee: Bijective, mask: fpeNumber, fit: func(col Column) error {
					if col.Type.Kind == value.TypeDecimal && col.Type.Scale != 0 {
						return errors.New("fpe masks whole numbers, and the column has decimal places")
					}
					return nil
				}}, nil
			}
			name := o.OneOf("alphabet", "alnum", "digits", "lower", "upper", "alnum")
			alphabet := fpeAlphabets[name]
			return &fn{guarantee: Bijective, mask: func(c *Context, in value.Value) (value.Value, error) {
				if in.Kind() != value.KindString {
					return value.Value{}, errKind("fpe", in)
				}
				s, err := fpeText(c, name, alphabet, in.Text())
				return value.String(s), err
			}}, nil
		},
	}
}

// fpeText encrypts the characters of s that are in alphabet, and keeps the
// others in place. The places and values of the others go into the tweak,
// so the map is a permutation of the strings that share them.
func fpeText(c *Context, name, alphabet, s string) (string, error) {
	if c.Keys == nil {
		return "", errNoKeys
	}
	var x []uint16
	skeleton := make([]byte, 0, len(s)+8)
	for i := range len(s) {
		if d := strings.IndexByte(alphabet, s[i]); d >= 0 {
			x = append(x, uint16(d)) //nolint:gosec // < 62
			skeleton = append(skeleton, 1)
		} else {
			skeleton = append(skeleton, 0, s[i])
		}
	}
	if !ff1Domain(len(alphabet), len(x)) {
		return "", fmt.Errorf("fpe needs at least %d characters from its alphabet (%s) in each value, and this value has fewer; use hash for short values", ff1MinLength(len(alphabet)), name)
	}
	y, err := c.Keys.ff1.Encrypt(len(alphabet), x, tweak("fpe/"+name, c.Domain, skeleton))
	if err != nil {
		return "", err
	}
	out := []byte(s)
	j := 0
	for i := range len(out) {
		if strings.IndexByte(alphabet, out[i]) >= 0 {
			out[i] = alphabet[y[j]]
			j++
		}
	}
	return string(out), nil
}

// ff1MinLength is the fewest numerals of a radix FF1 accepts.
func ff1MinLength(radix int) int {
	n := 2
	for !ff1Domain(radix, n) {
		n++
	}
	return n
}

// fpeNumber encrypts the digits of a whole number within its class (see
// numberClass): the sign, the number of digits and the integer types it
// fits are kept. FF1 permutes all digit strings of that length; cycle-
// walking (FF1 again until the result is in the class) makes it a
// permutation of the class. The walk always ends, at the latest when it
// comes back to the input, which is in its own class.
func fpeNumber(c *Context, in value.Value) (value.Value, error) {
	if c.Keys == nil {
		return value.Value{}, errNoKeys
	}
	mag, neg, ok := wholeNumber(in)
	if !ok {
		if in.Kind() == value.KindDecimal {
			return value.Value{}, errors.New("fpe cannot mask NaN or infinity")
		}
		return value.Value{}, errKind("fpe", in)
	}
	if in.Kind() == value.KindDecimal && strings.Contains(normalizeDecimal(in.DecimalText()), ".") {
		return value.Value{}, errors.New("fpe masks whole numbers, and the value has decimal places")
	}
	n := len(mag)
	if !ff1Domain(10, n) {
		return value.Value{}, fmt.Errorf("fpe needs whole numbers of at least %d digits, as FF1 needs a million possible values, and this value has fewer", ff1MinLength(10))
	}
	class := classOf(mag, neg)
	x := make([]uint16, n)
	for i := range n {
		x[i] = uint16(mag[i] - '0')
	}
	sign := []byte{0}
	if neg {
		sign[0] = 1
	}
	tw := tweak("fpe/number", c.Domain, sign)
	// The smallest class holds 7.7% of its digit strings (the 19-digit
	// numbers above the int64 range), so the walk takes about 13 rounds
	// at worst on average. The bound only guards against a bug.
	for range maxCycleWalk {
		var err error
		if x, err = c.Keys.ff1.Encrypt(10, x, tw); err != nil {
			return value.Value{}, err
		}
		if out := digitText(x); class.has(out) {
			if neg {
				out = "-" + out
			}
			return numberOfKind(in.Kind(), out)
		}
	}
	return value.Value{}, errors.New("fpe: cycle-walking found no number in the class, which its own input rules out: this is a bug")
}

// maxCycleWalk bounds fpe's cycle-walking. With at least 7.7% of the
// strings in the class, a walk this long has a probability below 10^-30000.
const maxCycleWalk = 1 << 20

// distinctHashLen is the fewest hexadecimal digits of hash that keep
// values distinct: 4 bits each.
const distinctHashLen = collisionBits / 4

func hashTransformer() *Transformer {
	return &Transformer{
		Name: "hash",
		Summary: "A keyed hash of the value: hexadecimal text (length: 1 to 64, default 32), random digits of the same " +
			"count for whole numbers, within the integer types they fit, or a UUID. Distinct values stay distinct " +
			"(collision-resistant) with at least 24 hexadecimal digits, and for UUIDs.",
		Types:   []value.TypeKind{value.TypeString, value.TypeText, value.TypeInt, value.TypeDecimal, value.TypeUUID},
		Options: []string{"length"},
		build: func(t target, o *Options) (*fn, error) {
			switch {
			case t.kind == value.TypeUUID:
				return &fn{guarantee: CollisionResistant, seeded: true, mask: func(c *Context, _ value.Value) (value.Value, error) {
					return value.UUID(keyedUUID(c.Rand)), nil
				}}, nil
			case isNumber(t):
				return &fn{seeded: true, mask: func(c *Context, in value.Value) (value.Value, error) {
					return randomDigits("hash", c.Rand, in)
				}}, nil
			}
			n := o.Int("length", 32, 1, 64)
			if t.distinct && n < distinctHashLen {
				return nil, fmt.Errorf("option length: values here must stay distinct (a unique key shares the domain), so hash needs a length of at least %d", distinctHashLen)
			}
			g := None
			if n >= distinctHashLen {
				g = CollisionResistant
			}
			return &fn{
				guarantee: g,
				seeded:    true,
				mask: func(c *Context, _ value.Value) (value.Value, error) {
					seed := c.Rand.Seed()
					return value.String(hex.EncodeToString(seed[:])[:n]), nil
				},
				fit: fitLength("hash", n, func(col Column, room int) string {
					if t.distinct && room < distinctHashLen {
						return fmt.Sprintf(", fewer than the %d that keep values distinct%s", distinctHashLen, distinctAlternatives(col, "hash"))
					}
					return fmt.Sprintf("; set length: %d", room)
				}),
			}, nil
		},
	}
}

// keyedUUID returns a version 4 UUID from the random stream: 122 keyed bits.
func keyedUUID(r *Rand) [16]byte {
	var u [16]byte
	r.Read(u[:])
	u[6] = u[6]&0x0f | 0x40
	u[8] = u[8]&0x3f | 0x80
	return u
}

// tokenBits returns how many random bits a token keeps: the ones random_token
// redraws, from the characters' classes.
func tokenBits(s string) float64 {
	var digits, letters int
	for i := range len(s) {
		switch c := s[i]; {
		case c >= '0' && c <= '9':
			digits++
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
			letters++
		}
	}
	return float64(digits)*math.Log2(10) + float64(letters)*math.Log2(26)
}

const (
	lowerLetters = "abcdefghijklmnopqrstuvwxyz"
	upperLetters = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	digitChars   = "0123456789"
	tokenChars   = digitChars + upperLetters + lowerLetters
)

func randomTokenTransformer() *Transformer {
	return &Transformer{
		Name: "random_token",
		Summary: "A new token of the same shape: each digit, lower-case and upper-case letter is redrawn, and other " +
			"characters keep their place. Binary values get random bytes of the same length; UUIDs a version 4 UUID; " +
			"JSON a JSON string; whole numbers random digits of the same count, within the integer types they fit. " +
			"Where values must stay distinct (a unique key in its domain), each token must keep at least 96 random bits.",
		Types: []value.TypeKind{
			value.TypeString, value.TypeText, value.TypeBinary, value.TypeUUID, value.TypeJSON,
			value.TypeInt, value.TypeDecimal,
		},
		alike: jsonApart, // JSON gets a new JSON string, text a token of its own shape
		build: func(t target, _ *Options) (*fn, error) {
			switch {
			case t.kind == value.TypeUUID:
				return &fn{guarantee: CollisionResistant, seeded: true, mask: func(c *Context, _ value.Value) (value.Value, error) {
					return value.UUID(keyedUUID(c.Rand)), nil
				}}, nil
			case t.kind == value.TypeJSON:
				return &fn{guarantee: CollisionResistant, seeded: true, mask: func(c *Context, _ value.Value) (value.Value, error) {
					return value.String(`"` + c.Rand.String(tokenChars, 32) + `"`), nil
				}}, nil
			case isNumber(t):
				return &fn{seeded: true, mask: func(c *Context, in value.Value) (value.Value, error) {
					return randomDigits("random_token", c.Rand, in)
				}}, nil
			}
			g := None
			if t.distinct {
				g = CollisionResistant
			}
			return &fn{guarantee: g, seeded: true, mask: func(c *Context, in value.Value) (value.Value, error) {
				return randomToken(c.Rand, in, t.distinct)
			}}, nil
		},
	}
}

var errTokenTooShort = fmt.Errorf("the token is too short to stay distinct (it needs at least %d random bits); use hash", collisionBits)

func randomToken(r *Rand, in value.Value, distinct bool) (value.Value, error) {
	switch in.Kind() {
	case value.KindBytes:
		b := in.Bytes()
		if distinct && 8*len(b) < collisionBits {
			return value.Value{}, errTokenTooShort
		}
		r.Read(b)
		return value.Bytes(b), nil
	case value.KindString:
		s := in.Text()
		if distinct && tokenBits(s) < collisionBits {
			return value.Value{}, errTokenTooShort
		}
		out := []byte(s)
		for i, c := range out {
			switch {
			case c >= '0' && c <= '9':
				out[i] = digitChars[r.IntN(10)]
			case c >= 'a' && c <= 'z':
				out[i] = lowerLetters[r.IntN(26)]
			case c >= 'A' && c <= 'Z':
				out[i] = upperLetters[r.IntN(26)]
			}
		}
		return value.String(string(out)), nil
	default:
		return value.Value{}, errKind("random_token", in)
	}
}
