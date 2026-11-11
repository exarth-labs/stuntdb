package mask

import (
	"strings"
	"unicode/utf8"

	"github.com/exarth-labs/stuntdb/internal/value"
)

func synthesizeTextTransformer() *Transformer {
	return &Transformer{
		Name: "synthesize_text",
		Summary: "Filler sentences (lorem ipsum) with as many characters as the original text, so that layouts still " +
			"look real while nothing a person wrote, including instructions hidden for AI agents, reaches the copy.",
		Types: textTypes,
		build: func(target, *Options) (*fn, error) {
			return &fn{seeded: true, mask: func(c *Context, in value.Value) (value.Value, error) {
				if in.Kind() != value.KindString {
					return value.Value{}, errKind("synthesize_text", in)
				}
				return value.String(filler(c.Rand, utf8.RuneCountInString(in.Text()))), nil
			}}, nil
		},
	}
}

// filler writes n characters of sentences made of lorem ipsum words. Each
// sentence starts with a capital and ends with a full stop. The text is
// ASCII, so it has n bytes: never more than the original, whose n
// characters took at least n bytes, so it fits wherever the original did.
func filler(r *Rand, n int) string {
	var b strings.Builder
	b.Grow(n + 16)
	for b.Len() < n {
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		words := r.Between(4, 12)
		for i := range words {
			w := Pick(r, loremWords)
			if i == 0 {
				w = strings.ToUpper(w[:1]) + w[1:]
			} else {
				b.WriteByte(' ')
			}
			b.WriteString(w)
		}
		b.WriteByte('.')
	}
	s := b.String()[:n]
	if strings.HasSuffix(s, " ") {
		s = s[:n-1] + "."
	}
	return s
}
