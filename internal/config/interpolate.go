package config

import (
	"errors"
	"strings"
)

// interpolate replaces ${NAME} with the environment variable NAME, and $$
// with a single $. Any other $ stays as it is. It returns the first variable
// that is not set, which is an error only when the setting is used; err is
// a mistake in the text itself.
func interpolate(s string, lookup func(string) (string, bool)) (out, missing string, err error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '$' || i+1 == len(s) {
			b.WriteByte(s[i])
			continue
		}
		switch s[i+1] {
		case '$':
			b.WriteByte('$')
			i++
		case '{':
			end := strings.IndexByte(s[i+2:], '}')
			if end < 0 {
				return "", "", errors.New("a ${ is not closed with }")
			}
			name := s[i+2 : i+2+end]
			if !validEnvName(name) {
				return "", "", errors.New("${…} must hold an environment variable name: letters, digits and _, not starting with a digit")
			}
			if v, ok := lookup(name); ok {
				b.WriteString(v)
			} else if missing == "" {
				missing = name
			}
			i += 2 + end
		default:
			b.WriteByte('$')
		}
	}
	return b.String(), missing, nil
}

func validEnvName(name string) bool {
	if name == "" {
		return false
	}
	for i, c := range name {
		switch {
		case c == '_', c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}
