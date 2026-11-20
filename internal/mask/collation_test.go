package mask

import (
	"slices"
	"strings"
	"testing"

	"github.com/exarth-labs/stuntdb/dialect"
	"github.com/exarth-labs/stuntdb/internal/schema"
	"github.com/exarth-labs/stuntdb/internal/value"
)

var (
	nocase     = schema.Collation{Name: "NOCASE", Equality: schema.EqualIgnoringASCIICase}
	rtrim      = schema.Collation{Name: "RTRIM", Equality: schema.EqualIgnoringTrailingSpaces}
	collBinary = schema.Collation{Name: "BINARY"}
)

// TestDescribeEquality checks how Describe finds when a column's unique keys
// count two texts as equal: the coarsest kind among them, text only.
func TestDescribeEquality(t *testing.T) {
	text, id := value.Type{Kind: value.TypeText}, value.Type{Kind: value.TypeInt, Bits: 64}
	tbl := &schema.Table{
		Name: "t",
		Columns: []*schema.Column{
			{Name: "id", Type: id},
			{Name: "email", Type: text},
			{Name: "code", Type: value.Type{Kind: value.TypeString, MaxLen: 20}},
			{Name: "both", Type: text},
			{Name: "tag", Type: text},
			{Name: "note", Type: text},
			{Name: "n", Type: id},
		},
		PrimaryKey: &schema.Key{Columns: []schema.Ident{"id"}},
		Uniques: []*schema.Key{
			{Columns: []schema.Ident{"email"}, Collations: []schema.Collation{nocase}},
			{Columns: []schema.Ident{"code", "both"}, Collations: []schema.Collation{rtrim, nocase}},
			{Columns: []schema.Ident{"tag"}, Collations: []schema.Collation{collBinary}},
			{Columns: []schema.Ident{"n"}, Collations: []schema.Collation{nocase}},
		},
		Indexes: []*schema.Index{
			{Name: "both_rtrim", Unique: true, Columns: []schema.Ident{"both"}, Collations: []schema.Collation{rtrim}},
			{Name: "email_binary", Unique: true, Columns: []schema.Ident{"email"}, Collations: []schema.Collation{collBinary}},
			{Name: "note_nocase", Columns: []schema.Ident{"note"}, Collations: []schema.Collation{nocase}}, // not unique
		},
	}
	want := map[schema.Ident]struct {
		eq   schema.Equality
		coll string
	}{
		"id":    {schema.EqualBytes, ""},
		"email": {schema.EqualIgnoringASCIICase, "NOCASE"}, // the coarsest of NOCASE and BINARY
		"code":  {schema.EqualIgnoringTrailingSpaces, "RTRIM"},
		"both":  {schema.EqualOther, "NOCASE, RTRIM"}, // no kind ignores both
		"tag":   {schema.EqualBytes, ""},
		"note":  {schema.EqualBytes, ""}, // its index isn't unique
		"n":     {schema.EqualBytes, ""}, // not text
	}
	ref := schema.TableRef{Namespace: "main", Name: "t"}
	for _, c := range Describe(ref, tbl, dialect.Capabilities{}) {
		w := want[c.Ref.Column]
		if c.Equality != w.eq || c.Collation != w.coll {
			t.Errorf("%s: %s (%q), want %s (%q)", c.Ref.Column, c.Equality, c.Collation, w.eq, w.coll)
		}
	}
}

// keyCol returns a unique text column whose key compares as eq.
func keyCol(table, name string, eq schema.Equality, coll string) Column {
	c := unique(col(table, name, tText))
	c.Equality, c.Collation = eq, coll
	return c
}

// TestResolveEquality checks the kind ResolveDomains gives each rule: its
// domain's unique text columns', which must agree when the transformer
// masks from the value.
func TestResolveEquality(t *testing.T) {
	parent := keyCol("users", "email", schema.EqualIgnoringASCIICase, "NOCASE")
	child := col("orders", "email", tText)
	strict := keyCol("profiles", "email", schema.EqualBytes, "")
	domain := DefaultDomain(parent.Ref)
	t.Run("a foreign key takes its parent's", func(t *testing.T) {
		rules, err := resolved(t, []ColumnRule{
			{parent, Rule{Use: "hash"}},
			{child, Rule{Use: "hash", Domain: domain}},
			{col("users", "born", tDate), Rule{Use: "date_shift", Per: "email"}},
			{col("orders", "note", tText), Rule{Use: "hash"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		for ref, want := range map[string]schema.Equality{
			"main.users.email": schema.EqualIgnoringASCIICase, "main.orders.email": schema.EqualIgnoringASCIICase,
			"main.users.born": schema.EqualBytes, "main.orders.note": schema.EqualBytes,
		} {
			if got := rules[ref].Equality; got != want {
				t.Errorf("%s: %s, want %s", ref, got, want)
			}
		}
	})
	t.Run("unique columns that compare differently", func(t *testing.T) {
		rules, err := resolved(t, []ColumnRule{{parent, Rule{Use: "hash"}}, {strict, Rule{Use: "hash", Domain: domain}}})
		want := "domain main.users.email: its unique columns count different texts as equal: main.profiles.email compare bytes; " +
			"main.users.email (NOCASE) compare ignoring ASCII case; one fake value can't fit both, so give them separate domains, or keep their values"
		if err == nil || err.Error() != want {
			t.Errorf("err = %v\nwant %s", err, want)
		}
		if rules["main.users.email"].Equality != schema.EqualBytes {
			t.Error("a refused domain should not also be folded")
		}
	})
	t.Run("keep copies, whatever the keys", func(t *testing.T) {
		rules, err := resolved(t, []ColumnRule{{parent, Rule{Use: "keep"}}, {strict, Rule{Use: "keep", Domain: domain}}})
		if err != nil {
			t.Fatal(err)
		}
		if rules["main.users.email"].Equality != schema.EqualBytes {
			t.Error("keep's domain is folded")
		}
	})
	t.Run("an unknown collation", func(t *testing.T) {
		app := keyCol("users", "email", schema.EqualOther, "reverse")
		rules, err := resolved(t, []ColumnRule{{app, Rule{Use: "hash"}}, {child, Rule{Use: "hash", Domain: domain}}})
		if err != nil {
			t.Fatal(err)
		}
		if rules["main.orders.email"].Equality != schema.EqualOther {
			t.Error("the foreign key doesn't follow its parent's collation")
		}
	})
}

// TestConfigureRefusesKeysItCantFold checks that a value-derived transformer
// is refused under a collation stuntdb can't reproduce, on the key and on a
// foreign key that follows it, and that keep isn't.
func TestConfigureRefusesKeysItCantFold(t *testing.T) {
	key := keyCol("users", "email", schema.EqualOther, "reverse")
	_, err := Configure(key, Rule{Use: "hash", Distinct: true, Equality: schema.EqualOther})
	want := "its key compares text under the collation reverse, which stuntdb can't reproduce, so hash could give values the key counts as equal " +
		"different fake values, or the reverse; keep the values, or compare the key with a binary, NOCASE or RTRIM collation"
	if err == nil || err.Error() != want {
		t.Errorf("key: %v\nwant %s", err, want)
	}
	_, err = Configure(col("orders", "email", tText), Rule{Use: "hash", Distinct: true, Equality: schema.EqualOther})
	if err == nil || !strings.Contains(err.Error(), "under a collation of the kind other") {
		t.Errorf("foreign key: %v", err)
	}
	if _, err := Configure(key, Rule{Use: "keep", Equality: schema.EqualOther}); err != nil {
		t.Errorf("keep: %v", err)
	}
	// Numbers aren't text: a collation never applies to them.
	num := unique(col("users", "n", tInt64))
	num.Equality = schema.EqualOther
	if _, err := Configure(num, Rule{Use: "fpe", Distinct: true}); err != nil {
		t.Errorf("a number column: %v", err)
	}
}

// TestFPEUnderACaseInsensitiveKey checks which of fpe's alphabets work on a
// key that ignores ASCII case, whose values arrive lower-cased.
func TestFPEUnderACaseInsensitiveKey(t *testing.T) {
	key := keyCol("users", "code", schema.EqualIgnoringASCIICase, "NOCASE")
	for alphabet, want := range map[string]string{
		"":      "option alphabet: the column's key ignores ASCII case, so fpe masks each value's lower-case form, and alnum would write capital letters, so two values could differ only in case, which the key counts as equal; use lower or digits",
		"alnum": "and alnum would write capital letters",
		"upper": "and upper would find no letters to encrypt; use lower or digits",
		"lower": "", "digits": "",
	} {
		options := map[string]any{}
		if alphabet != "" {
			options["alphabet"] = alphabet
		}
		_, err := Configure(key, Rule{Use: "fpe", Options: options, Distinct: true, Equality: schema.EqualIgnoringASCIICase})
		switch {
		case want == "" && err != nil:
			t.Errorf("%q: %v", alphabet, err)
		case want != "" && (err == nil || !strings.Contains(err.Error(), want)):
			t.Errorf("%q: %v\nwant %s", alphabet, err, want)
		}
	}
	// Trailing spaces aren't letters: every alphabet works.
	trim := keyCol("users", "code", schema.EqualIgnoringTrailingSpaces, "RTRIM")
	if _, err := Configure(trim, Rule{Use: "fpe", Distinct: true, Equality: schema.EqualIgnoringTrailingSpaces}); err != nil {
		t.Errorf("RTRIM: %v", err)
	}
	// The suggestions for a refused transformer leave fpe out, as its
	// default alphabet would be refused too.
	_, err := Configure(key, Rule{Use: "full_name", Distinct: true, Equality: schema.EqualIgnoringASCIICase})
	if err == nil || strings.Contains(err.Error(), "fpe") || !strings.Contains(err.Error(), "hash") {
		t.Errorf("the alternatives: %v", err)
	}
}

// TestOutputMustBeFolded checks the masker's last check: an output not in
// the form its key compares could equal another one under the key.
func TestOutputMustBeFolded(t *testing.T) {
	const secret = "QNGg"
	for _, tt := range []struct {
		eq  schema.Equality
		out string
		ok  bool
	}{
		{schema.EqualIgnoringASCIICase, secret, false},
		{schema.EqualIgnoringASCIICase, "qngg", true},
		{schema.EqualIgnoringTrailingSpaces, "qngg ", false},
		{schema.EqualIgnoringTrailingSpaces, "QNGg", true},
		{schema.EqualBytes, secret, true},
	} {
		c := &checked{fn: &fn{guarantee: Bijective, mask: func(*Context, value.Value) (value.Value, error) {
			return value.String(tt.out), nil
		}}, col: keyCol("t", "k", tt.eq, ""), name: "fake", equality: tt.eq}
		_, err := c.Mask(&Context{}, value.String("in"))
		if (err == nil) != tt.ok {
			t.Errorf("%s, %q: %v", tt.eq, tt.out, err)
		}
		if err != nil && (strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "fake wrote a value its key, "+tt.eq.String())) {
			t.Errorf("message: %v", err)
		}
	}
}

// TestEqualKeysMaskAlike checks, for every transformer that masks a unique
// text key from its value, that texts its key counts as equal get one fake
// value, in the form the key compares, and different texts different ones;
// and that keep copies each value as it is.
func TestEqualKeysMaskAlike(t *testing.T) {
	cases := []struct {
		coll        schema.Collation
		equal, diff []string
	}{
		{
			nocase,
			[]string{"Bob.Smith-123456@Example-Holdings.COM", "bob.smith-123456@example-holdings.com", "BOB.SMITH-123456@EXAMPLE-HOLDINGS.COM"},
			[]string{"bob.smith-123457@example-holdings.com"},
		},
		{
			rtrim,
			[]string{"bob.smith-123456@example-holdings", "bob.smith-123456@example-holdings ", "bob.smith-123456@example-holdings    "},
			[]string{"bob.smith-123456@example-holdings\t", " bob.smith-123456@example-holdings"},
		},
	}
	uuids := map[schema.Ident][]string{ // uuid reads UUID text only
		"NOCASE": {"3F2504E0-4F89-11D3-9A0C-0305E82C3301", "3f2504e0-4f89-11d3-9a0c-0305e82c3301", "3f2504e0-4f89-11d3-9a0c-0305e82c3302"},
		"RTRIM":  {"3f2504e0-4f89-11d3-9a0c-0305e82c3301", "3f2504e0-4f89-11d3-9a0c-0305e82c3301  ", "3f2504e0-4f89-11d3-9a0c-0305e82c3302"},
	}
	keys := testKeys(t)
	var tested []string
	for _, tr := range Catalog() {
		if !tr.UsesDomain() || !tr.Accepts(value.TypeText) {
			continue
		}
		for _, c := range cases {
			options := map[string]any{}
			if tr.Name == "fpe" && c.coll.Equality == schema.EqualIgnoringASCIICase {
				options["alphabet"] = "lower"
			}
			m, err := keyTable(tr.Name, options, c.coll)
			if err != nil {
				continue // not a transformer for a unique text key
			}
			equal, diff := c.equal, c.diff
			if tr.Name == "uuid" {
				u := uuids[schema.Ident(c.coll.Name)]
				equal, diff = u[:2], u[2:]
			}
			outs := maskAll(t, m.Bind(keys), append(slices.Clone(equal), diff...))
			for i, out := range outs[:len(equal)] {
				if out != outs[0] {
					t.Errorf("%s under %s: %q and %q got different fake values", tr.Name, c.coll.Name, equal[0], equal[i])
				}
			}
			for i, out := range outs {
				if folded, _ := c.coll.Equality.Fold(out); folded != out {
					t.Errorf("%s under %s: the fake value of input %d isn't folded", tr.Name, c.coll.Name, i)
				}
				if i >= len(equal) && out == outs[0] {
					t.Errorf("%s under %s: different texts got one fake value", tr.Name, c.coll.Name)
				}
			}
			if c.coll == nocase {
				tested = append(tested, tr.Name)
			}
		}
	}
	if want := []string{"email", "fpe", "hash", "random_token", "username", "uuid"}; !slices.Equal(tested, want) {
		t.Errorf("tested %v, want %v", tested, want)
	}

	m, err := keyTable("keep", nil, nocase)
	if err != nil {
		t.Fatal(err)
	}
	if got := maskAll(t, m.Bind(keys), []string{"BOB", "bob"}); !slices.Equal(got, []string{"BOB", "bob"}) {
		t.Errorf("keep: %v", got)
	}
	// A BINARY key on a NOCASE column holds both 'a' and 'A': no fold.
	m, err = keyTable("hash", nil, collBinary)
	if err != nil {
		t.Fatal(err)
	}
	if got := maskAll(t, m.Bind(keys), []string{"a", "A"}); got[0] == got[1] {
		t.Error("a BINARY key's 'a' and 'A' got one fake value")
	}
}

// keyTable prepares the masking of a table whose text primary key compares
// under coll, as the copy does: Describe, ResolveDomains, NewTable.
func keyTable(use string, options map[string]any, coll schema.Collation) (*Table, error) {
	ref := schema.TableRef{Namespace: "main", Name: "keys"}
	tbl := &schema.Table{
		Name:       "keys",
		Columns:    []*schema.Column{{Name: "k", Type: tText}},
		PrimaryKey: &schema.Key{Columns: []schema.Ident{"k"}, Collations: []schema.Collation{coll}},
	}
	cols := Describe(ref, tbl, dialect.Capabilities{})
	rules, err := ResolveDomains([]ColumnRule{{cols[0], Rule{Use: use, Options: options}}})
	if err != nil {
		return nil, err
	}
	return NewTable(ref, tbl, map[schema.Ident]Rule{"k": rules[0]}, dialect.Capabilities{})
}

func maskAll(t *testing.T, m *RowMasker, in []string) []string {
	t.Helper()
	out := make([]string, len(in))
	// Rows sized from the table, as the engine sizes them.
	dst, src := make([]value.Value, len(m.t.plans)), make([]value.Value, len(m.t.plans))
	for i, s := range in {
		src[0] = value.String(s)
		if err := m.Mask(dst, src); err != nil {
			t.Fatalf("mask %d: %v", i, err)
		}
		out[i] = dst[0].Text()
	}
	return out
}
