package redact

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/exarth-labs/stuntdb/internal/errs"
)

// canary is planted in every test message: it must never come out.
const canary = "canary-7f3a"

func TestStringRemovesValues(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			"MySQL duplicate entry",
			"Error 1062 (23000): Duplicate entry 'canary-7f3a@example.com' for key 'users.email'",
			"Error 1062 (23000): Duplicate entry '***' for key '***'",
		},
		{
			"SQL Server conversion",
			"Conversion failed when converting the varchar value 'canary-7f3a' to data type int.",
			"Conversion failed when converting the varchar value '***' to data type int.",
		},
		{
			"SQL Server truncation",
			"String or binary data would be truncated in table 'app.dbo.users', column 'email'. Truncated value: 'canary-7f3a'.",
			"String or binary data would be truncated in table '***', column '***'. Truncated value: '***'.",
		},
		{
			"a quote inside a value",
			"Duplicate entry 'O'Brien canary-7f3a' for key 'name'",
			"Duplicate entry '***' for key '***'",
		},
		{
			"a doubled quote inside a value",
			"value 'it''s canary-7f3a' is invalid",
			"value '***' is invalid",
		},
		{
			"several literals in a row",
			"('canary-7f3a','canary-7f3a', 'canary-7f3a')",
			"('***','***', '***')",
		},
		{
			"an empty literal",
			"default is '' here",
			"default is '***' here",
		},
		{
			"apostrophes in English are kept",
			"stuntdb: the target database's folder doesn't exist, and can't be made",
			"stuntdb: the target database's folder doesn't exist, and can't be made",
		},
		{
			"apostrophes in other scripts are kept",
			"l'ordre n'existe pas",
			"l'ordre n'existe pas",
		},
		{
			"identifiers in double quotes are kept",
			`column "email" of main.users cannot be NULL`,
			`column "email" of main.users cannot be NULL`,
		},
		{
			"URL credentials",
			"failed to connect to postgres://admin:canary-7f3a@db.internal:5432/app?sslmode=require",
			"failed to connect to postgres://***@db.internal:5432/app?sslmode=require",
		},
		{
			"a token in a URL",
			"GET https://canary-7f3a@api.example.com/v1 failed",
			"GET https://***@api.example.com/v1 failed",
		},
		{
			"URL without credentials is kept",
			"see https://example.com/docs and sqlite:./app.db",
			"see https://example.com/docs and sqlite:./app.db",
		},
		{
			"key=value settings",
			"host=db user=admin password=canary-7f3a dbname=app; Pwd=canary-7f3a;api_key: canary-7f3a&token=canary-7f3a",
			"host=db user=admin password=*** dbname=app; Pwd=***;api_key: ***&token=***",
		},
	}
	r := New()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := r.String(tt.in)
			if got != tt.want {
				t.Errorf("String =\n  %s\nwant\n  %s", got, tt.want)
			}
			if strings.Contains(got, canary) {
				t.Error("the canary came through")
			}
		})
	}
}

func TestKnownSecrets(t *testing.T) {
	r := New("canary-7f3a-masking-key-0123456789", "abc") // "abc" is too short to replace safely
	r.AddURL("postgres://admin:p%40ss-canary-7f3a@db.internal/app")
	r.AddURL("not a URL")

	for _, in := range []string{
		"key canary-7f3a-masking-key-0123456789 rejected",
		"auth failed for password p@ss-canary-7f3a",              // decoded, as a driver prints it
		"bad option p%40ss-canary-7f3a in the connection string", // as written
		"cannot parse postgres://admin:p%40ss-canary-7f3a@db.internal/app",
	} {
		if got := r.String(in); strings.Contains(got, canary) || strings.Contains(got, "p@ss") {
			t.Errorf("String(%q) = %q, still holds the secret", in, got)
		}
	}
	if got := r.String("abcdef"); got != "abcdef" {
		t.Errorf("a secret shorter than %d characters was replaced: %q", minSecret, got)
	}
}

func TestErrorKeepsTheChain(t *testing.T) {
	r := New("canary-7f3a-secret")
	inner := fmt.Errorf("load main.users: duplicate 'canary-7f3a': %w", context.Canceled)
	err := r.Error(errs.E(errs.Output, inner))

	if strings.Contains(err.Error(), canary) {
		t.Errorf("Error() = %q, still holds the canary", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Error("errors.Is no longer finds the cause")
	}
	if errs.KindOf(err) != errs.Output {
		t.Errorf("KindOf = %v, want OUTPUT", errs.KindOf(err))
	}
	if !IsRedacted(err) {
		t.Error("IsRedacted = false")
	}

	clean := errors.New("nothing to hide")
	if got := r.Error(clean); !errors.Is(got, clean) || IsRedacted(got) {
		t.Error("an error with nothing to remove should come back unchanged")
	}
	if r.Error(nil) != nil {
		t.Error("Error(nil) should be nil")
	}
}

func TestZeroRedactor(t *testing.T) {
	var r *Redactor
	if got := r.String("x 'canary-7f3a'"); strings.Contains(got, canary) {
		t.Errorf("a nil Redactor let a literal through: %q", got)
	}
	var zero Redactor
	zero.Add("canary-7f3a-secret")
	if got := zero.String("canary-7f3a-secret"); got != Mask {
		t.Errorf("the zero Redactor = %q, want %q", got, Mask)
	}
}

// Longer secrets go first, so a secret that contains another is replaced whole.
func TestOverlappingSecrets(t *testing.T) {
	r := New("canary", "canary-7f3a-long")
	if got := r.String("x canary-7f3a-long y"); got != "x *** y" {
		t.Errorf("String = %q, want %q", got, "x *** y")
	}
}

func TestConcurrentUse(t *testing.T) {
	r := New()
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			r.Add(fmt.Sprintf("secret-%d-canary", i))
			_ = r.String("secret-1-canary 'x'")
		})
	}
	wg.Wait()
	if got := r.String("secret-3-canary"); got != Mask {
		t.Errorf("String = %q, want %q", got, Mask)
	}
}

func FuzzString(f *testing.F) {
	for _, seed := range []string{"", "'", "''", "'a'b'", "x://@", "password=", "'O'Brien'", "don't 'x' can't", "é'ü'ö"} {
		f.Add(seed)
	}
	r := New()
	f.Fuzz(func(t *testing.T, s string) {
		// Must never panic, and must never break valid UTF-8, whatever the
		// quotes in s. (Stray quotes can pair up differently on a second
		// pass, so redacting twice may mask more; it never reveals more.)
		got := r.String(s)
		if utf8.ValidString(s) && !utf8.ValidString(got) {
			t.Errorf("String(%q) = %q, which is not valid UTF-8", s, got)
		}
	})
}
