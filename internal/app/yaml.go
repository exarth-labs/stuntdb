package app

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

// maxShortKey is the longest key, in characters, that YAML reads in the
// short form "key: value". A longer key needs the explicit form: "? key" on
// one line, then ": value".
const maxShortKey = 1024

// yamlScalar writes text as a YAML scalar, on one line, that reads back as
// the same text, as a value or as a key: plain where YAML allows it, and
// quoted otherwise. Text that is not UTF-8 cannot be written in YAML at all;
// each run of invalid bytes becomes U+FFFD.
func yamlScalar(s string) string {
	// Plain, << is YAML's merge key, which reads as a string only as a value.
	if utf8.ValidString(s) && s != "<<" {
		if out, err := yaml.Marshal(s); err == nil {
			if text := strings.TrimSuffix(string(out), "\n"); !strings.Contains(text, "\n") {
				return text
			}
		}
	}
	// A JSON string is also a valid YAML double-quoted scalar, and is
	// always one line. HTML escaping would only make it harder to read.
	// Invalid bytes are replaced first, because Go versions differ in how
	// encoding/json writes them, and the output must not.
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(strings.ToValidUTF8(s, "�")) // encoding a string cannot fail
	return strings.TrimSuffix(b.String(), "\n")
}

// shortKey reports whether a key, as yamlScalar writes it, fits the short
// form "key: value".
func shortKey(key string) bool {
	return utf8.RuneCountInString(key) <= maxShortKey
}
