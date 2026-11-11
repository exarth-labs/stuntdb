package mask

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"hash"
	"math"
	"strconv"
	"strings"

	"github.com/exarth-labs/stuntdb/internal/value"
)

// seedLabel starts every seed's input, so that seeds can never equal any
// other HMAC the keys are used for.
const seedLabel = "stuntdb/mask/seed"

// The family tags of canonical values. A value's family, not its column
// type, decides its encoding, so that equal values compare equal however
// each engine declares them. Distinct tags keep the text "5" apart from the
// number 5. They are part of generator version 1.
const (
	famNumber    byte = 1 // int, uint and decimal: normalized decimal text
	famFloat     byte = 2
	famText      byte = 3
	famBytes     byte = 4
	famUUID      byte = 5
	famBool      byte = 6
	famDate      byte = 7
	famTime      byte = 8
	famTimestamp byte = 9
	famInterval  byte = 10
	famArray     byte = 11
	famSpecial   byte = 12 // NaN, infinities and zero dates, by kind
)

// seeder computes seeds. It reuses one HMAC state, so it is not safe for
// concurrent use; each worker has its own.
type seeder struct {
	mac   hash.Hash
	buf   []byte
	canon []byte
}

func newSeeder(k *Keys) *seeder {
	return &seeder{mac: hmac.New(sha256.New, k.seed)}
}

// seed returns the seed of v for a transformer in a domain:
//
//	HMAC-SHA256(seed key, field(label) ‖ field(version) ‖ field(domain) ‖ field(transformer) ‖ field(canonical(v)))
//
// where field(x) is x's length as 8 bytes, big-endian, then x. Fields of
// known length make the encoding prefix-free: different tuples never give
// the same bytes.
//
// The transformer's name gives each transformer its own stream. Columns
// seeded from one key (per:) then draw independently unless they use the
// same transformer: a fake first name, which the copy shows, reveals
// nothing about the number of days the same person's birth date moved.
func (s *seeder) seed(domain, transformer string, v value.Value) [32]byte {
	b := s.buf[:0]
	b = appendField(b, []byte(seedLabel))
	b = appendField(b, binary.BigEndian.AppendUint64(nil, GeneratorVersion))
	b = appendField(b, []byte(domain))
	b = appendField(b, []byte(transformer))
	s.canon = appendCanonical(s.canon[:0], v)
	b = appendField(b, s.canon)
	s.buf = b

	s.mac.Reset()
	s.mac.Write(b)
	var out [32]byte
	s.mac.Sum(out[:0])
	return out
}

func appendField(dst, field []byte) []byte {
	dst = binary.BigEndian.AppendUint64(dst, uint64(len(field)))
	return append(dst, field...)
}

// appendCanonical appends the canonical encoding of v: its family tag, then
// its contents. Variable-length contents are length-prefixed, so that the
// elements of an array cannot run into each other.
func appendCanonical(dst []byte, v value.Value) []byte {
	if v.Special() != value.Normal {
		dst = append(dst, famSpecial, byte(v.Kind()), byte(v.Special()))
		if v.Special() == value.ZeroDate {
			dst = appendField(dst, []byte(v.ZeroText()))
		}
		return dst
	}
	switch v.Kind() {
	case value.KindInt:
		return appendNumber(dst, intText(v.Int()))
	case value.KindUint:
		return appendNumber(dst, uintText(v.Uint()))
	case value.KindDecimal:
		return appendNumber(dst, normalizeDecimal(v.DecimalText()))
	case value.KindFloat:
		f := v.Float()
		switch {
		case f == 0:
			f = 0 // -0 equals +0
		case math.IsNaN(f):
			f = math.NaN()
		}
		return binary.BigEndian.AppendUint64(append(dst, famFloat), math.Float64bits(f))
	case value.KindString:
		return appendField(append(dst, famText), []byte(v.Text()))
	case value.KindBytes:
		return appendField(append(dst, famBytes), v.Bytes())
	case value.KindUUID:
		u := v.UUID()
		return append(append(dst, famUUID), u[:]...)
	case value.KindBool:
		b := byte(0)
		if v.Bool() {
			b = 1
		}
		return append(dst, famBool, b)
	case value.KindDate:
		return appendTemporal(dst, famDate, v)
	case value.KindTime:
		return appendTemporal(dst, famTime, v)
	case value.KindTimestamp:
		if v.IsTZ() {
			// An instant compares equal whatever its offset, so the offset
			// is left out: the same moment in two zones is one value.
			v = value.TimestampTZ(v.Timestamp())
		}
		return appendTemporal(dst, famTimestamp, v)
	case value.KindInterval:
		months, days, nanos := v.Interval()
		dst = append(dst, famInterval)
		dst = binary.BigEndian.AppendUint32(dst, uint32(months)) //nolint:gosec // two's complement, on purpose
		dst = binary.BigEndian.AppendUint32(dst, uint32(days))   //nolint:gosec // two's complement, on purpose
		return binary.BigEndian.AppendUint64(dst, uint64(nanos)) //nolint:gosec // two's complement, on purpose
	case value.KindArray:
		elems := v.Elems()
		dst = binary.BigEndian.AppendUint64(append(dst, famArray), uint64(len(elems)))
		for _, e := range elems {
			dst = appendCanonical(dst, e)
		}
		return dst
	default: // NULL is never seeded; an array element may be NULL
		return append(dst, 0)
	}
}

func appendNumber(dst []byte, text string) []byte {
	return appendField(append(dst, famNumber), []byte(text))
}

func appendTemporal(dst []byte, fam byte, v value.Value) []byte {
	text, _ := value.AppendISO(nil, v, 'T')
	return appendField(append(dst, fam), text)
}

func intText(i int64) string { return strconv.FormatInt(i, 10) }

func uintText(u uint64) string { return strconv.FormatUint(u, 10) }

// normalizeDecimal returns the canonical text of a plain decimal: no plus
// sign, no leading zeros in the integer part, no trailing zeros in the
// fraction, no dot without a fraction, and "0" for every zero. "-007.50"
// becomes "-7.5", and "-0.00" becomes "0".
func normalizeDecimal(s string) string {
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimLeft(s, "+-")
	whole, frac, _ := strings.Cut(s, ".")
	whole = strings.TrimLeft(whole, "0")
	frac = strings.TrimRight(frac, "0")
	if whole == "" {
		whole = "0"
	}
	out := whole
	if frac != "" {
		out += "." + frac
	}
	if neg && out != "0" {
		out = "-" + out
	}
	return out
}
