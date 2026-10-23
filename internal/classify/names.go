package classify

import (
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// words splits a name into lowercase words without accents. Punctuation
// separates words, and so does a change of case: customer_email,
// CustomerEmail, customer-email and CUSTOMER_EMAIL all give [customer email],
// and IPAddress gives [ip address]. Digits stay with the letters before them,
// so that ipv4 and address2 are single words.
func words(name string) []string {
	// Bytes that are not UTF-8 are not letters, so they separate words.
	rs := []rune(stripAccents(strings.ToValidUTF8(name, " ")))
	var out []string
	start := -1
	flush := func(end int) {
		if start >= 0 {
			out = append(out, strings.ReplaceAll(strings.ToLower(string(rs[start:end])), "ß", "ss"))
			start = -1
		}
	}
	for i, r := range rs {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && !unicode.IsMark(r) {
			flush(i)
			continue
		}
		if start >= 0 && caseBreak(rs, start, i) {
			flush(i)
		}
		if start < 0 {
			start = i
		}
	}
	flush(len(rs))
	return out
}

// caseBreak reports whether a new word starts at rs[i], in the word that
// starts at rs[start].
func caseBreak(rs []rune, start, i int) bool {
	prev, cur := rs[i-1], rs[i]
	if !isUpper(cur) {
		return false
	}
	switch {
	case unicode.IsDigit(prev):
		// sha256Hash, but not 2FA
		return strings.ContainsFunc(string(rs[start:i]), unicode.IsLetter)
	case !isUpper(prev):
		return unicode.IsLetter(prev) // customerEmail
	case i+1 < len(rs) && unicode.IsLower(rs[i+1]):
		// IPAddress is IP Address, but UserIDs is User IDs
		plural := rs[i+1] == 's' && (i+2 == len(rs) || !unicode.IsLower(rs[i+2]))
		return !plural
	}
	return false
}

// isUpper reports whether r is an uppercase letter that has a lowercase
// form. Letters without one, such as ϒ, count as having no case, so that
// lowercased words never split again.
func isUpper(r rune) bool { return unicode.ToLower(r) != r }

// stripAccents removes accents: prénom becomes prenom. Decomposing é gives
// e followed by an accent mark, which is dropped.
func stripAccents(s string) string {
	marks := func(r rune) rune {
		if unicode.Is(unicode.Mn, r) {
			return -1
		}
		return r
	}
	return norm.NFC.String(strings.Map(marks, norm.NFD.String(s)))
}

// trimNumbers drops the words at the end that are only digits, so that
// address_line_2 matches "address line". It keeps at least one word.
func trimNumbers(ws []string) []string {
	for len(ws) > 1 && strings.TrimFunc(ws[len(ws)-1], unicode.IsDigit) == "" {
		ws = ws[:len(ws)-1]
	}
	return ws
}

// wordMatches reports whether a word of a name matches a word of a rule:
// the same word, its plural, or the word followed by digits (phone2).
func wordMatches(word, rule string) bool {
	for _, w := range [2]string{word, strings.TrimRightFunc(word, unicode.IsDigit)} {
		if w == rule || w == rule+"s" || w == rule+"es" {
			return true
		}
	}
	return false
}

// endsWith reports whether a name's words end with a phrase's words.
func endsWith(name, phrase []string) bool {
	if len(phrase) == 0 || len(phrase) > len(name) {
		return false
	}
	return matchAt(name, phrase, len(name)-len(phrase))
}

// contains reports whether a name's words contain a phrase's words, anywhere.
func contains(name, phrase []string) bool {
	for at := 0; len(phrase) > 0 && at+len(phrase) <= len(name); at++ {
		if matchAt(name, phrase, at) {
			return true
		}
	}
	return false
}

func matchAt(name, phrase []string, at int) bool {
	for i, w := range phrase {
		if !wordMatches(name[at+i], w) {
			return false
		}
	}
	return true
}
