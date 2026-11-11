package mask

import (
	"strconv"
	"strings"

	"github.com/exarth-labs/stuntdb/internal/value"
)

// tagLen is the length of a collision-resistant tag: 20 characters of 5
// bits, 100 bits of the keyed seed, at least collisionBits.
const tagLen = 20

// tagAlphabet is Crockford's base 32 in lower case: no i, l, o or u, so a
// tag can't be misread or spell words.
const tagAlphabet = "0123456789abcdefghjkmnpqrstvwxyz"

// tag returns the first 100 bits of a seed as 20 base-32 characters. The
// seed is a keyed hash of the input, so two distinct inputs share a tag
// with probability 2^-100.
func tag(seed [32]byte) string {
	var b [tagLen]byte
	acc, bits, j := uint32(0), 0, 0
	for i := 0; j < tagLen; i++ {
		acc = acc<<8 | uint32(seed[i])
		bits += 8
		for bits >= 5 && j < tagLen {
			bits -= 5
			b[j] = tagAlphabet[(acc>>bits)&31]
			j++
		}
	}
	return string(b[:])
}

// emailDomains are the domains RFC 2606 reserves for examples, so that a
// fake address can never reach a real person.
var emailDomains = []string{"example.com", "example.net", "example.org"}

// emailAt is the length of "@" and a reserved domain: all three have the
// same length.
var emailAt = 1 + len(emailDomains[0])

// firstFitting returns the first candidate that fits n characters (n ≤ 0:
// no limit), and whether one did.
func firstFitting(n int, candidates ...string) (string, bool) {
	for _, c := range candidates {
		if n <= 0 || len(c) <= n {
			return c, true
		}
	}
	return "", false
}

// The transformers of this file write text from word lists. Their option
// max_length bounds the text, for a column that is shorter than their
// longest output: it is an option, not the column's length, so that every
// column of a domain gets the same values.

// nameTransformer picks one entry of a word list.
func nameTransformer(name, summary string, list []string) *Transformer {
	return &Transformer{
		Name:    name,
		Summary: summary,
		Types:   textTypes,
		Options: []string{"max_length"},
		build: func(_ target, o *Options) (*fn, error) {
			least := shortest(list)
			choices := fitting(list, readMaxLength(o, least))
			return &fn{
				seeded: true,
				mask: func(c *Context, _ value.Value) (value.Value, error) {
					return value.String(Pick(c.Rand, choices)), nil
				},
				fit: fitLength(name, longest(choices), maxLengthAdvice(least)),
			}, nil
		},
	}
}

func firstNameTransformer() *Transformer {
	return nameTransformer("first_name", "A given name. max_length bounds it.", firstNames)
}

func lastNameTransformer() *Transformer {
	return nameTransformer("last_name", "A family name. max_length bounds it.", lastNames)
}

func fullNameTransformer() *Transformer {
	return &Transformer{
		Name:    "full_name",
		Summary: "A given name and a family name. max_length bounds it.",
		Types:   textTypes,
		Options: []string{"max_length"},
		build: func(_ target, o *Options) (*fn, error) {
			least := shortest(firstNames) + 1 + shortest(lastNames)
			n := readMaxLength(o, least)
			return &fn{
				seeded: true,
				mask: func(c *Context, _ value.Value) (value.Value, error) {
					first := Pick(c.Rand, firstNames)
					last := Pick(c.Rand, lastNames)
					if s, ok := firstFitting(n, first+" "+last); ok {
						return value.String(s), nil
					}
					// Too long: choose again among the names that fit together.
					first = Pick(c.Rand, fitting(firstNames, n-1-shortest(lastNames)))
					last = Pick(c.Rand, fitting(lastNames, n-1-len(first)))
					return value.String(first + " " + last), nil
				},
				fit: fitLength("full_name", bounded(longest(firstNames)+1+longest(lastNames), n), maxLengthAdvice(least)),
			}, nil
		},
	}
}

func companyTransformer() *Transformer {
	return &Transformer{
		Name:    "company",
		Summary: "A company name, such as Nakamura Logistics. max_length bounds it.",
		Types:   textTypes,
		Options: []string{"max_length"},
		build: func(_ target, o *Options) (*fn, error) {
			least := shortest(lastNames)
			n := readMaxLength(o, least)
			return &fn{
				seeded: true,
				mask: func(c *Context, _ value.Value) (value.Value, error) {
					name := Pick(c.Rand, lastNames)
					suffix := Pick(c.Rand, companySuffixes)
					if s, ok := firstFitting(n, name+" "+suffix, name); ok {
						return value.String(s), nil
					}
					return value.String(Pick(c.Rand, fitting(lastNames, n))), nil
				},
				fit: fitLength("company", bounded(longest(lastNames)+1+longest(companySuffixes), n), maxLengthAdvice(least)),
			}, nil
		},
	}
}

// personal is what username and email build on: a name, and in a domain
// whose values must stay distinct, a collision-resistant tag.
type personal struct {
	distinct bool
}

// lengths returns the fewest characters a name of p needs, and the most it
// can have.
func (p personal) lengths() (least, most int) {
	name := longest(firstNames) + 1 + longest(lastNames)
	if p.distinct {
		return tagLen, name + 1 + tagLen
	}
	return shortest(firstNames), name + 2
}

// local returns the part of a username or an email address before any
// suffix, in lower case, fitting room characters (0: no limit): first.last,
// then shorter forms. With a tag, the tag always comes last.
func (p personal) local(c *Context, room int) string {
	first := strings.ToLower(Pick(c.Rand, firstNames))
	last := strings.ToLower(Pick(c.Rand, lastNames))
	if p.distinct {
		t := tag(c.Rand.Seed())
		s, _ := firstFitting(room, first+"."+last+"."+t, first+"."+t, t)
		return s
	}
	digits := strconv.Itoa(c.Rand.Between(10, 99))
	if s, ok := firstFitting(room, first+"."+last+digits, first+"."+last, first+digits, first); ok {
		return s
	}
	return strings.ToLower(Pick(c.Rand, fitting(firstNames, room)))
}

func (p personal) guarantee() Guarantee {
	if p.distinct {
		return CollisionResistant
	}
	return None
}

func usernameTransformer() *Transformer {
	return &Transformer{
		Name: "username",
		Summary: "A user name such as olivia.chen42. Where values must stay distinct (a unique key in its domain), it ends in " +
			"a 20-character keyed tag. max_length bounds it.",
		Types:   textTypes,
		Options: []string{"max_length"},
		build: func(t target, o *Options) (*fn, error) {
			p := personal{distinct: t.distinct}
			least, most := p.lengths()
			n := readMaxLength(o, least)
			return &fn{
				guarantee: p.guarantee(),
				seeded:    true,
				mask: func(c *Context, _ value.Value) (value.Value, error) {
					return value.String(p.local(c, n)), nil
				},
				fit: fitLength("username", bounded(most, n), maxLengthAdvice(least)),
			}, nil
		},
	}
}

func emailTransformer() *Transformer {
	return &Transformer{
		Name: "email",
		Summary: "An email address at example.com, example.net or example.org, domains reserved for examples, " +
			"so that no message can reach a real person. Where values must stay distinct (a unique key in its domain), " +
			"the address carries a 20-character keyed tag. max_length bounds it.",
		Types:   textTypes,
		Options: []string{"max_length"},
		build: func(t target, o *Options) (*fn, error) {
			p := personal{distinct: t.distinct}
			least, most := p.lengths()
			n := readMaxLength(o, least+emailAt)
			room := 0
			if n > 0 {
				room = n - emailAt
			}
			return &fn{
				guarantee: p.guarantee(),
				seeded:    true,
				mask: func(c *Context, _ value.Value) (value.Value, error) {
					domain := Pick(c.Rand, emailDomains)
					return value.String(p.local(c, room) + "@" + domain), nil
				},
				fit: fitLength("email", bounded(most+emailAt, n), maxLengthAdvice(least+emailAt)),
			}, nil
		},
	}
}
