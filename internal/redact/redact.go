// Package redact removes secrets and row values from error messages and
// logs, before anything reaches the screen, a log file or a JSON report.
//
// It is a safety net, not the first line of defense: adapters already
// return errors without row values, and the conformance suite checks it.
// A Redactor removes three things:
//
//   - known secrets, given to it exactly: connection URLs, passwords, the
//     masking key;
//   - credentials in any URL ("postgres://user:***@host") and in
//     key=value settings ("password=***");
//   - SQL string literals ('***'), the standard SQL form of a text value,
//     which is how most database messages quote the values they complain
//     about.
//
// Identifiers in double quotes, such as table and column names, are kept:
// they describe structure, not data, and are what makes a message useful.
package redact

import (
	"errors"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// quote opens and closes a SQL string literal.
const quote = '\''

// Mask replaces whatever is removed.
const Mask = "***"

// minSecret is the shortest secret replaced wherever it appears. Shorter
// strings would match inside ordinary words and make messages unreadable.
const minSecret = 4

var (
	// scheme://user:password@ and scheme://token@ in any URL.
	urlCredentials = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^/\s@'"]*@`)

	// password=…, pwd=…, secret=…, token=…, key=… settings, as in
	// connection strings and query parameters, up to a separator.
	settingSecrets = regexp.MustCompile(`(?i)\b(password|passwd|pwd|secret|token|api_?key|access_?key)(\s*[=:]\s*)([^\s;&,'"]+)`)
)

// Redactor removes secrets and row values from text. It is safe for
// concurrent use. The zero value is ready to use, and knows no secrets.
type Redactor struct {
	mu      sync.RWMutex
	secrets []string // longest first, so that a secret containing another is removed whole
}

// New returns a Redactor that knows the given secrets.
func New(secrets ...string) *Redactor {
	r := &Redactor{}
	r.Add(secrets...)
	return r
}

// Add registers secrets to remove wherever they appear. Secrets shorter than
// four characters are ignored.
func (r *Redactor) Add(secrets ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range secrets {
		if len(s) >= minSecret && !slices.Contains(r.secrets, s) {
			r.secrets = append(r.secrets, s)
		}
	}
	slices.SortFunc(r.secrets, func(a, b string) int { return len(b) - len(a) })
}

// AddURL registers a connection URL as a secret, together with its password
// both as written and decoded, because drivers may print either.
func (r *Redactor) AddURL(rawURL string) {
	secrets := []string{rawURL}
	if u, err := url.Parse(rawURL); err == nil && u.User != nil {
		if p, ok := u.User.Password(); ok {
			secrets = append(secrets, p, url.PathEscape(p), url.QueryEscape(p))
		}
	}
	r.Add(secrets...)
}

// String returns s with secrets, credentials and SQL string literals
// replaced by Mask.
func (r *Redactor) String(s string) string {
	if r != nil {
		r.mu.RLock()
		for _, secret := range r.secrets {
			s = strings.ReplaceAll(s, secret, Mask)
		}
		r.mu.RUnlock()
	}
	s = urlCredentials.ReplaceAllString(s, "${1}"+Mask+"@")
	s = settingSecrets.ReplaceAllString(s, "${1}${2}"+Mask)
	return maskLiterals(s)
}

// maskLiterals replaces the contents of SQL string literals: text between
// single quotes, with ” standing for a quote inside. A quote right after a
// letter or digit, as in "don't", opens no literal, and one right before a
// letter or digit, as in 'O'Brien', closes none.
func maskLiterals(s string) string {
	var b strings.Builder
	last := 0 // s[last:] is not copied yet
	for i := 0; i < len(s); i++ {
		if s[i] != quote || wordBefore(s, i) {
			continue
		}
		end := literalEnd(s, i+1)
		if end < 0 {
			continue
		}
		b.WriteString(s[last : i+1])
		b.WriteString(Mask)
		last, i = end, end
	}
	if last == 0 {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

// literalEnd returns the index of the quote that closes a literal whose
// contents start at start, or -1 if there is none.
func literalEnd(s string, start int) int {
	for j := start; j < len(s); j++ {
		switch {
		case s[j] != quote:
		case j+1 < len(s) && s[j+1] == quote:
			j++ // '' is a quote inside the literal
		case wordAt(s, j+1):
			// an apostrophe inside the value, as in 'O'Brien'
		default:
			return j
		}
	}
	return -1
}

// wordBefore reports whether the character just before byte i of s is a
// letter or digit; wordAt, whether the one starting at byte i is.
func wordBefore(s string, i int) bool {
	r, size := utf8.DecodeLastRuneInString(s[:i])
	return size > 0 && isWord(r)
}

func wordAt(s string, i int) bool {
	r, size := utf8.DecodeRuneInString(s[i:])
	return size > 0 && isWord(r)
}

func isWord(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

// Error returns err with its message redacted. The result keeps err's chain,
// so errors.Is, errors.As and errs.KindOf work as before; only its Error
// text changes. It returns nil for nil, and err itself when there is nothing
// to remove.
func (r *Redactor) Error(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	clean := r.String(msg)
	if clean == msg {
		return err
	}
	return &redacted{msg: clean, err: err}
}

type redacted struct {
	msg string
	err error
}

func (e *redacted) Error() string { return e.msg }
func (e *redacted) Unwrap() error { return e.err }

// IsRedacted reports whether err was returned by Redactor.Error with
// something removed.
func IsRedacted(err error) bool {
	_, ok := errors.AsType[*redacted](err)
	return ok
}
