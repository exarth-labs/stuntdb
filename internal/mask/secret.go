package mask

import (
	"errors"

	"github.com/exarth-labs/stuntdb/internal/value"
)

// DefaultPasswordHash is the bcrypt hash (cost 10) of the password
// "stuntdb". By default, password_hash gives every account this hash, so
// that developers can sign in to any account of a copy with one known
// password. A test checks that it verifies.
const DefaultPasswordHash = "$2a$10$4IF1izCsGING5OoJjHAnAOL96NR48xKDjtGsq7et0YD776DCEQa/i" //nolint:gosec // a published hash of the public development password

func passwordHashTransformer() *Transformer {
	return &Transformer{
		Name: "password_hash",
		Summary: "One password hash for every row, so that every account of the copy signs in with one known password. " +
			"The default is the bcrypt hash of \"stuntdb\"; set hash: to your framework's hash of another password.",
		Types:   []value.TypeKind{value.TypeString, value.TypeText, value.TypeBinary},
		Options: []string{"hash"},
		build: func(t target, o *Options) (*fn, error) {
			hash, ok := o.Text("hash")
			if !ok {
				hash = DefaultPasswordHash
			}
			if hash == "" {
				return nil, errors.New("option hash: must not be empty")
			}
			if !printableASCII(hash) {
				return nil, errors.New("option hash: must be printable ASCII, as password hashes are")
			}
			out := asText(t, hash)
			return &fn{
				mask: func(*Context, value.Value) (value.Value, error) { return out, nil },
				fit: fitLength("password_hash", len(hash), func(Column, int) string {
					return "; set hash: to a hash that fits"
				}),
			}, nil
		},
	}
}

// redaction is what redact writes.
const redaction = "REDACTED"

func redactTransformer() *Transformer {
	return &Transformer{
		Name: "redact",
		Summary: "Replaces every value with the text REDACTED (cut to max_length when it is set), the JSON string " +
			"\"REDACTED\" in JSON columns, or those bytes in binary columns.",
		Types:   []value.TypeKind{value.TypeString, value.TypeText, value.TypeJSON, value.TypeBinary},
		Options: []string{"max_length"},
		build: func(t target, o *Options) (*fn, error) {
			if t.kind == value.TypeJSON {
				o.refuse("max_length", "a JSON column always gets the JSON string \"REDACTED\"")
				out := value.String(`"` + redaction + `"`)
				return &fn{mask: func(*Context, value.Value) (value.Value, error) { return out, nil }}, nil
			}
			text := redaction[:bounded(len(redaction), readMaxLength(o, 1))]
			out := asText(t, text)
			return &fn{
				mask: func(*Context, value.Value) (value.Value, error) { return out, nil },
				fit:  fitLength("redact", len(text), maxLengthAdvice(1)),
			}, nil
		},
	}
}
