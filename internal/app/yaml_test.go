package app

import (
	"strings"
	"testing"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

func TestYAMLScalar(t *testing.T) {
	tests := []struct{ in, want string }{
		{"email", "email"},
		{"main.users", "main.users"},
		{"a: b", "'a: b'"},
		{"#x", "'#x'"},
		{"null", `"null"`},
		{"123", `"123"`},
		{"", `""`},
		{"<<", `"<<"`},
		{"two\nlines", `"two\nlines"`},
		{"café", "café"},
		{"bad\xffbyte", "\"bad�byte\""},
		{"bad\xff\xfe\xfdbytes", "\"bad�bytes\""}, // one replacement per run of invalid bytes
	}
	for _, tt := range tests {
		if got := yamlScalar(tt.in); got != tt.want {
			t.Errorf("yamlScalar(%q) = %s, want %s", tt.in, got, tt.want)
		}
	}
	if !shortKey(strings.Repeat("é", maxShortKey)) || shortKey(strings.Repeat("z", maxShortKey+1)) {
		t.Error("shortKey must count characters, up to maxShortKey")
	}
}

// FuzzYAMLScalar checks that any text, written by yamlScalar, reads back
// as the same text from one line: as a value, and as a key, in the short
// form when it fits and in the explicit form otherwise.
func FuzzYAMLScalar(f *testing.F) {
	for _, s := range []string{"email", "a: b", "#x", "null", "", "<<", "two\nlines", "- x", "[x]", "{x}", "!x", "&x", "*x", "|", ">", "'", `"`, " x ", "\t", "~", strings.Repeat("z", 2000)} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if !utf8.ValidString(s) {
			return // YAML cannot hold it; yamlScalar documents the loss
		}
		text := yamlScalar(s)
		if strings.Contains(text, "\n") {
			t.Fatalf("yamlScalar(%q) = %q spans lines", s, text)
		}
		doc := "? " + text + "\n: " + text + "\n"
		if shortKey(text) {
			doc = text + ": " + text + "\n"
		}
		var m map[string]string
		if err := yaml.Unmarshal([]byte(doc), &m); err != nil {
			t.Fatalf("yamlScalar(%q) = %s, which does not read back: %v", s, text, err)
		}
		if v, ok := m[s]; !ok || v != s || len(m) != 1 {
			t.Fatalf("yamlScalar(%q) = %s reads back as %q", s, text, m)
		}
	})
}
