package classify

import (
	"slices"
	"strings"
	"testing"
)

func TestWords(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{"email", "email"},
		{"customer_email", "customer email"},
		{"CustomerEmail", "customer email"},
		{"customerEmail", "customer email"},
		{"CUSTOMER_EMAIL", "customer email"},
		{"customer-email", "customer email"},
		{"customer email", "customer email"},
		{"  __customer..email__ ", "customer email"},
		{"IPAddress", "ip address"},
		{"EMail", "e mail"},
		{"UserID", "user id"},
		{"UserIDs", "user ids"},
		{"URLs", "urls"},
		{"address2", "address2"},
		{"address_line_2", "address line 2"},
		{"Address2Line", "address2 line"},
		{"sha256Hash", "sha256 hash"},
		{"ipv4", "ipv4"},
		{"2FA_secret", "2fa secret"},
		{"prénom", "prenom"},
		{"Prénom", "prenom"},
		{"STRASSE", "strasse"},
		{"Straße", "strasse"},
		{"correo_electrónico", "correo electronico"},
		{"émail", "email"}, // an accent written as a separate mark
		{"メール", "メール"},
		{"", ""},
		{"___", ""},
	}
	for _, tt := range tests {
		if got := strings.Join(words(tt.name), " "); got != tt.want {
			t.Errorf("words(%q) = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestTrimNumbers(t *testing.T) {
	tests := []struct{ in, want string }{
		{"address line 2", "address line"},
		{"phone 1 2", "phone"},
		{"phone2", "phone2"},
		{"2", "2"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := strings.Join(trimNumbers(strings.Fields(tt.in)), " "); got != tt.want {
			t.Errorf("trimNumbers(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestPhraseMatching(t *testing.T) {
	tests := []struct {
		name, phrase  string
		ends, inWords bool
	}{
		{"customer email", "email", true, true},
		{"email", "email", true, true},
		{"emails", "email", true, true},
		{"addresses", "address", true, true},
		{"phone2", "phone", true, true},
		{"phones2", "phone", true, true},
		{"cvv2", "cvv", true, true},
		{"ipv4", "ipv4", true, true},
		{"ipv4", "ip", false, false},
		{"email subject", "email", false, true},
		{"voicemail", "mail", false, false},
		{"hotel", "tel", false, false},
		{"email address", "email address", true, true},
		{"address", "email address", false, false},
		{"tbl users archive", "user", false, true},
		{"", "email", false, false},
		{"email", "", false, false},
	}
	for _, tt := range tests {
		name, phrase := strings.Fields(tt.name), strings.Fields(tt.phrase)
		if got := endsWith(name, phrase); got != tt.ends {
			t.Errorf("endsWith(%q, %q) = %t", tt.name, tt.phrase, got)
		}
		if got := contains(name, phrase); got != tt.inWords {
			t.Errorf("contains(%q, %q) = %t", tt.name, tt.phrase, got)
		}
	}
}

// FuzzWords checks that words never panics, never returns an empty word,
// and is stable: splitting its own output again changes nothing. Rule packs
// rely on that: a phrase is valid only if words() writes it back unchanged.
func FuzzWords(f *testing.F) {
	for _, s := range []string{"CustomerEmail", "IPAddress", "UserIDs", "2FA", "Straße", "émail", "İstanbul", "ǅemal", "メール"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		ws := words(s)
		if slices.Contains(ws, "") {
			t.Fatalf("words(%q) has an empty word", s)
		}
		joined := strings.Join(ws, " ")
		if again := strings.Join(words(joined), " "); again != joined {
			t.Fatalf("words is not stable: %q gives %q, then %q", s, joined, again)
		}
	})
}
