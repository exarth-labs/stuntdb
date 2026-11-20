package schema

import "strings"

// Equality is when a key's collation counts two texts as equal, in terms
// every engine shares. Masking needs it because a key's equal values must
// get equal fake values, and its different values different ones.
//
// The zero value is EqualBytes. An adapter must set any other kind its
// engine's collation has.
type Equality uint8

const (
	// EqualBytes: equal only when the bytes are (SQLite BINARY,
	// PostgreSQL's deterministic collations, Oracle BINARY).
	EqualBytes Equality = iota

	// EqualIgnoringTrailingSpaces: equal once trailing U+0020 spaces are
	// removed, so 'bob' = 'bob  ' (SQLite RTRIM, MySQL's PAD SPACE binary
	// collations, SQL Server's binary collations).
	EqualIgnoringTrailingSpaces

	// EqualIgnoringASCIICase: equal once A-Z are lower-cased, so
	// 'BOB' = 'bob' but 'É' ≠ 'é' (SQLite NOCASE, the only engine with it,
	// whose rule it follows exactly: see foldNoCase).
	EqualIgnoringASCIICase

	// EqualOther: a linguistic collation, whose Unicode case, accent and
	// width rules belong to the engine, or a collation stuntdb doesn't
	// know. No fold reproduces it exactly.
	EqualOther
)

// String names the kind, for the fingerprint and for messages.
func (e Equality) String() string {
	switch e {
	case EqualBytes:
		return "bytes"
	case EqualIgnoringTrailingSpaces:
		return "ignoring trailing spaces"
	case EqualIgnoringASCIICase:
		return "ignoring ASCII case"
	default:
		return "other"
	}
}

// Fold returns the one form shared by every text equal to s under e: two
// texts are equal under e exactly when their folds are identical. It
// returns false for EqualOther, which has no such form here.
func (e Equality) Fold(s string) (string, bool) {
	switch e {
	case EqualBytes:
		return s, true
	case EqualIgnoringTrailingSpaces:
		return strings.TrimRight(s, " "), true
	case EqualIgnoringASCIICase:
		return foldNoCase(s), true
	default:
		return s, false
	}
}

// foldNoCase folds as SQLite's NOCASE compares: A-Z lower-cased, up to the
// first NUL byte, where its comparison stops (sqlite3StrNICmp) and only the
// lengths are compared, so 'a\x00A' = 'a\x00B'. The bytes after a NUL
// become NULs, which keeps the length and nothing else.
func foldNoCase(s string) string {
	i := strings.IndexByte(s, 0)
	if i < 0 {
		return lowerASCII(s)
	}
	return lowerASCII(s[:i]) + strings.Repeat("\x00", len(s)-i)
}

// lowerASCII lower-cases A-Z only, and allocates only when it changes s.
func lowerASCII(s string) string {
	for i := range len(s) {
		if c := s[i]; 'A' <= c && c <= 'Z' {
			b := []byte(s)
			for j := i; j < len(b); j++ {
				if c := b[j]; 'A' <= c && c <= 'Z' {
					b[j] = c + ('a' - 'A')
				}
			}
			return string(b)
		}
	}
	return s
}

// Coarsest returns the kind that counts as equal everything a or b does,
// which the column of two unique keys is distinct under. Ignoring trailing
// spaces and ignoring ASCII case can't be combined into one of the kinds,
// so together they give EqualOther.
func Coarsest(a, b Equality) Equality {
	switch {
	case a == b || b == EqualBytes:
		return a
	case a == EqualBytes:
		return b
	default:
		return EqualOther
	}
}

// Collation is how a key compares one of its columns.
type Collation struct {
	Name     string // the engine's own name, such as "NOCASE"; empty: the column's default
	Equality Equality
}

// Collation returns how the key compares its i-th column.
func (k *Key) Collation(i int) Collation { return collationAt(k.Collations, i) }

// Collation returns how the index compares its i-th plain column.
func (ix *Index) Collation(i int) Collation { return collationAt(ix.Collations, i) }

func collationAt(c []Collation, i int) Collation {
	if i < len(c) {
		return c[i]
	}
	return Collation{}
}
