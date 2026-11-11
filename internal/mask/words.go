package mask

import (
	"embed"
	"slices"
	"strings"
)

//go:embed words/*.txt
var wordFiles embed.FS

// The word lists, part of GeneratorVersion: a test pins the SHA-256 of each
// one, so that any change to a list is a deliberate version change.
var (
	firstNames      = loadWords("first_names")
	lastNames       = loadWords("last_names")
	streetNames     = loadWords("streets")
	streetSuffixes  = loadWords("street_suffixes")
	cities          = loadWords("cities")
	states          = loadWords("states")
	companySuffixes = loadWords("company_suffixes")
	loremWords      = loadWords("lorem")
	areaCodes       = loadWords("area_codes")
)

// loadWords reads a word list: one entry per line; empty lines and lines
// starting with # are left out. Entries must be printable ASCII and
// distinct, so that every generated value is valid text on every engine
// and its length in characters equals its length in bytes. The lists are
// part of the binary, so a broken one is a bug and panics at start-up.
func loadWords(name string) []string {
	data, err := wordFiles.ReadFile("words/" + name + ".txt")
	if err != nil {
		panic("mask: word list " + name + ": " + err.Error())
	}
	var out []string
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !printableASCII(line) {
			panic("mask: word list " + name + " has an entry that is not printable ASCII")
		}
		if slices.Contains(out, line) {
			panic("mask: word list " + name + " has a duplicate entry")
		}
		out = append(out, line)
	}
	if len(out) == 0 {
		panic("mask: word list " + name + " is empty")
	}
	return out
}

func printableASCII(s string) bool {
	for i := range len(s) {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// fitting returns the entries of a list no longer than n bytes; n ≤ 0 means
// no limit. The order is kept, so picks stay deterministic.
func fitting(list []string, n int) []string {
	if n <= 0 {
		return list
	}
	var out []string
	for _, w := range list {
		if len(w) <= n {
			out = append(out, w)
		}
	}
	return out
}

// shortest returns the length of the shortest entry of a list.
func shortest(list []string) int {
	n := len(list[0])
	for _, w := range list[1:] {
		n = min(n, len(w))
	}
	return n
}

// longest returns the length of the longest entry of a list.
func longest(list []string) int {
	n := 0
	for _, w := range list {
		n = max(n, len(w))
	}
	return n
}
