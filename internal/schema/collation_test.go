package schema

import "testing"

func TestFold(t *testing.T) {
	tests := []struct {
		eq       Equality
		in, want string
	}{
		{EqualBytes, "Bob  ", "Bob  "},
		{EqualIgnoringTrailingSpaces, "bob  ", "bob"},
		{EqualIgnoringTrailingSpaces, "  bob", "  bob"},
		{EqualIgnoringTrailingSpaces, "b o b ", "b o b"},
		{EqualIgnoringTrailingSpaces, "bob\t", "bob\t"}, // a tab is not a space
		{EqualIgnoringTrailingSpaces, "bob ", "bob "},   // nor a no-break space
		{EqualIgnoringTrailingSpaces, "   ", ""},
		{EqualIgnoringTrailingSpaces, "", ""},
		{EqualIgnoringASCIICase, "BOB@X.COM", "bob@x.com"},
		{EqualIgnoringASCIICase, "already lower", "already lower"},
		{EqualIgnoringASCIICase, "AZaz@[`{", "azaz@[`{"}, // only A-Z, not the letters' neighbors
		{EqualIgnoringASCIICase, "ÉCOLE", "École"},       // ASCII letters only, as SQLite's NOCASE
		{EqualIgnoringASCIICase, "Bob  ", "bob  "},
		{EqualIgnoringASCIICase, "", ""},
		{EqualIgnoringASCIICase, "Ab\x00Cd", "ab\x00\x00\x00"}, // NOCASE stops at a NUL, then compares lengths
		{EqualIgnoringASCIICase, "\x00", "\x00"},
		{EqualIgnoringTrailingSpaces, "a\x00 ", "a\x00"},
	}
	for _, tt := range tests {
		got, ok := tt.eq.Fold(tt.in)
		if !ok || got != tt.want {
			t.Errorf("%s: Fold(%q) = %q, %v; want %q, true", tt.eq, tt.in, got, ok, tt.want)
		}
	}
	if _, ok := EqualOther.Fold("Bob"); ok {
		t.Error("EqualOther folds")
	}
}

// TestFoldKeepsAnUnchangedString checks that lower-casing allocates only
// when it changes a text: masking folds every key value.
func TestFoldKeepsAnUnchangedString(t *testing.T) {
	in := "already lower@example.com"
	if n := testing.AllocsPerRun(100, func() { _, _ = EqualIgnoringASCIICase.Fold(in) }); n != 0 {
		t.Errorf("folding an unchanged text allocates %v times", n)
	}
}

func TestCoarsest(t *testing.T) {
	b, sp, ci, o := EqualBytes, EqualIgnoringTrailingSpaces, EqualIgnoringASCIICase, EqualOther
	tests := []struct{ a, b, want Equality }{
		{b, b, b},
		{b, sp, sp},
		{sp, b, sp},
		{b, ci, ci},
		{ci, b, ci},
		{sp, sp, sp},
		{ci, ci, ci},
		{sp, ci, o},
		{ci, sp, o},
		{o, b, o},
		{b, o, o},
		{o, ci, o},
		{sp, o, o},
		{o, o, o},
	}
	for _, tt := range tests {
		if got := Coarsest(tt.a, tt.b); got != tt.want {
			t.Errorf("Coarsest(%s, %s) = %s, want %s", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestEqualityString(t *testing.T) {
	for eq, want := range map[Equality]string{
		EqualBytes: "bytes", EqualIgnoringTrailingSpaces: "ignoring trailing spaces",
		EqualIgnoringASCIICase: "ignoring ASCII case", EqualOther: "other", 9: "other",
	} {
		if got := eq.String(); got != want {
			t.Errorf("Equality(%d).String() = %q, want %q", eq, got, want)
		}
	}
}

func TestKeyCollation(t *testing.T) {
	nocase := Collation{Name: "NOCASE", Equality: EqualIgnoringASCIICase}
	k := &Key{Columns: []Ident{"a", "b"}, Collations: []Collation{{Name: "BINARY"}, nocase}}
	if k.Collation(1) != nocase || k.Collation(0).Equality != EqualBytes {
		t.Errorf("Collation = %v, %v", k.Collation(0), k.Collation(1))
	}
	if (&Key{Columns: []Ident{"a"}}).Collation(0) != (Collation{}) {
		t.Error("a key without collations doesn't compare bytes")
	}
	ix := &Index{Columns: []Ident{"a"}, Collations: []Collation{nocase}}
	if ix.Collation(0) != nocase || (&Index{}).Collation(3) != (Collation{}) {
		t.Error("Index.Collation")
	}
}
