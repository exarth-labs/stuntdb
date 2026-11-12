package mask

import (
	"bytes"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/exarth-labs/stuntdb/dialect"
	"github.com/exarth-labs/stuntdb/internal/schema"
	"github.com/exarth-labs/stuntdb/internal/value"
)

// col returns a nullable column main.<table>.<name> of a type.
func col(table, name string, typ value.Type) Column {
	return Column{Ref: schema.ColumnRef{Table: schema.TableRef{Namespace: "main", Name: schema.Ident(table)}, Column: schema.Ident(name)}, Type: typ, Nullable: true}
}

// unique returns c as a column of a unique key.
func unique(c Column) Column {
	c.Unique, c.Nullable = true, false
	return c
}

// resolved runs ResolveDomains and returns the rules by column.
func resolved(t *testing.T, cols []ColumnRule) (map[string]Rule, error) {
	t.Helper()
	rules, err := ResolveDomains(cols)
	if len(rules) != len(cols) {
		t.Fatalf("ResolveDomains returned %d rules for %d columns", len(rules), len(cols))
	}
	out := map[string]Rule{}
	for i, c := range cols {
		out[c.Column.Ref.String()] = rules[i]
	}
	return out, err
}

// TestResolveDistinct checks where Distinct is set: on every value column
// of a domain that holds a unique value column, and nowhere else. A per
// column uses its key's domain by default, and the key is the primary key
// here, yet it gets plain outputs, and a hash too short to be distinct.
func TestResolveDistinct(t *testing.T) {
	usersEmail := DefaultDomain(schema.ColumnRef{Table: usersRef, Column: "email"})
	cols := []ColumnRule{
		{unique(col("users", "id", tInt64)), Rule{Use: "keep"}},
		{unique(col("users", "email", tString(100))), Rule{Use: "email"}},
		{col("users", "born", tDate), Rule{Use: "date_shift", Per: "id", Distinct: true}}, // a stale Distinct is cleared
		{col("users", "code", tText), Rule{Use: "hash", Options: map[string]any{"length": "16"}, Per: "id"}},
		{col("users", "contact", tText), Rule{Use: "email", Per: "id"}},
		{col("orders", "user_email", tText), Rule{Use: "email", Domain: usersEmail}},
		{col("audit", "actor", tString(255)), Rule{Use: "email", Domain: usersEmail}},
		{col("notes", "body", tText), Rule{Use: "synthesize_text", Distinct: true}},
		{unique(col("tags", "name", tText)), Rule{Use: "hash", Per: "", Domain: "tag"}},
		{col("posts", "tag", tText), Rule{Use: "hash", Domain: "tag"}},
	}
	rules, err := resolved(t, cols)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"main.users.id": true, "main.users.email": true, "main.orders.user_email": true, "main.audit.actor": true,
		"main.tags.name": true, "main.posts.tag": true,
	}
	for _, c := range cols {
		ref := c.Column.Ref.String()
		if rules[ref].Distinct != want[ref] {
			t.Errorf("%s: Distinct = %v, want %v", ref, rules[ref].Distinct, want[ref])
		}
		rule := rules[ref]
		rule.Distinct = c.Rule.Distinct
		if !equalRules(rule, c.Rule) {
			t.Errorf("%s: ResolveDomains changed more than Distinct", ref)
		}
	}

	// Every resolved rule configures. The per hash is shorter than the 24
	// digits a distinct hash needs, the per email takes its plain form,
	// and the foreign keys take the tagged form of the key they reference.
	guarantees := map[string]Guarantee{}
	for _, c := range cols {
		f, err := Configure(c.Column, rules[c.Column.Ref.String()])
		if err != nil {
			t.Fatalf("%s: %v", c.Column.Ref, err)
		}
		guarantees[c.Column.Ref.String()] = f.Guarantee()
	}
	if guarantees["main.users.contact"] != None || guarantees["main.orders.user_email"] != CollisionResistant || guarantees["main.audit.actor"] != CollisionResistant {
		t.Errorf("guarantees: %v", guarantees)
	}
	// Without resolution, the per hash would inherit Distinct from the key
	// and be refused for nothing.
	if _, err := Configure(cols[3].Column, Rule{Use: "hash", Options: map[string]any{"length": "16"}, Per: "id", Distinct: true}); err == nil {
		t.Error("a distinct hash of 16 digits is accepted: the test above proves nothing")
	}
}

func equalRules(a, b Rule) bool {
	return a.Use == b.Use && a.Domain == b.Domain && a.Per == b.Per && a.Distinct == b.Distinct && maps.Equal(a.Options, b.Options)
}

// TestResolveSameMasking checks that the value columns of a domain must use
// the same transformer and options, compared after defaults are filled in.
func TestResolveSameMasking(t *testing.T) {
	tDecimal20 := value.Type{Kind: value.TypeDecimal, Precision: 20, Scale: 2}
	type side struct {
		typ  value.Type
		rule Rule
	}
	r := func(use string, opts ...string) Rule {
		o := map[string]any{}
		for i := 0; i+1 < len(opts); i += 2 {
			o[opts[i]] = opts[i+1]
		}
		return Rule{Use: use, Options: o, Domain: "shared"}
	}
	cases := []struct {
		name  string
		sides []side
		want  []string // substrings of the error, none for no error
	}{
		{"a default equals its value", []side{{tText, r("hash")}, {tText, r("hash", "length", "32")}}, nil},
		{"YAML numbers equal text", []side{{tText, Rule{Use: "hash", Options: map[string]any{"length": jsonNumber("30")}, Domain: "shared"}}, {tText, r("hash", "length", "30")}}, nil},
		{"noise percent", []side{{tInt64, r("noise")}, {tInt32, r("noise", "percent", "10")}}, nil},
		{"password_hash default", []side{{tText, r("password_hash")}, {tString(60), r("password_hash", "hash", DefaultPasswordHash)}}, nil},
		{"constant by its value", []side{{tInt64, r("constant", "value", "5")}, {tInt32, r("constant", "value", "05")}, {tDecimal, r("constant", "value", "5.0")}}, nil},
		{"fpe numbers are digits", []side{{tInt64, r("fpe")}, {tDecimal0, r("fpe", "alphabet", "digits")}}, nil},
		{"phone in numbers", []side{{tInt64, r("phone")}, {tDecimal20, r("phone", "region", "US")}}, nil},
		{"an option only some columns read", []side{{tText, r("phone", "format", "digits")}, {tString(10), r("phone", "format", "digits")}}, nil},
		{"a lone column", []side{{tText, r("email", "max_length", "40")}}, nil},
		{"an unknown transformer is Configure's to report", []side{{tText, r("emial")}, {tText, r("email")}}, nil},
		{"a type it doesn't take is Configure's to report", []side{{tInt64, r("email")}, {tText, r("hash")}}, nil},
		{"bad options are Configure's to report", []side{{tText, r("hash", "length", "x")}, {tText, r("hash", "length", "30")}}, nil},
		{
			"another length",
			[]side{{tText, r("hash")}, {tText, r("hash", "length", "24")}},
			[]string{"domain shared: its columns must mask equal values alike, but they give hash different options", "length (main.t.c0 against main.t.c1)"},
		},
		{
			"max_length",
			[]side{{tText, r("email", "max_length", "40")}, {tText, r("email")}},
			[]string{"max_length (main.t.c0 against main.t.c1)"},
		},
		{
			"several options",
			[]side{{tText, r("phone", "format", "national")}, {tText, r("phone", "region", "GB")}},
			[]string{"format (main.t.c0 against main.t.c1); region (main.t.c0 against main.t.c1)"},
		},
		{
			"groups that agree",
			[]side{{tText, r("hash", "length", "24")}, {tText, r("hash")}, {tText, r("hash", "length", "24")}},
			[]string{"length (main.t.c0, main.t.c2 against main.t.c1)"},
		},
		{
			"different transformers",
			[]side{{tText, r("email")}, {tText, r("hash")}, {tText, r("email")}},
			[]string{"they use different transformers: email (main.t.c0, main.t.c2), hash (main.t.c1)"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var cols []ColumnRule
			for i, s := range c.sides {
				cols = append(cols, ColumnRule{col("t", fmt.Sprint("c", i), s.typ), s.rule})
			}
			_, err := resolved(t, cols)
			if len(c.want) == 0 {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("no error")
			}
			for _, w := range c.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q\nlacks %q", err, w)
				}
			}
		})
	}
}

// TestResolveTypes checks that a domain mixing column types is refused
// exactly when its transformer masks them differently, and that the
// message names the transformers that would fit.
func TestResolveTypes(t *testing.T) {
	tDecimal20 := value.Type{Kind: value.TypeDecimal, Precision: 20, Scale: 2}
	cases := []struct {
		use    string
		a, b   value.Type
		unique bool
		want   string // a substring of the error, or "" for none
	}{
		{"fpe", tInt64, tText, false, "fpe masks equal values differently in int64 (main.t.a) and text (main.t.b), so they can't share a domain; no transformer masks them alike"},
		{"hash", tUUID, tText, true, "hash masks equal values differently in text (main.t.b) and uuid (main.t.a), so they can't share a domain; these transformers mask them alike: uuid"},
		{"hash", tInt32, tDecimal0, false, ""},
		{"uuid", tUUID, tText, true, ""},
		{"uuid", tBinary, tString(36), true, ""},
		{"keep", tInt64, tText, true, ""},
		{"null", tDate, tJSON, false, ""},
		{"noise", tInt32, tInt64, false, ""},
		{"noise", tUint64, tInt32, false, ""},
		{"noise", tDecimal, tDecimal20, false, ""},
		{"noise", tInt64, tDecimal, false, "noise masks equal values differently in decimal(12,2) (main.t.b) and int64 (main.t.a)"},
		{"noise", tDecimal, tDecimal0, false, "noise masks equal values differently"},
		{"noise", tInt64, tDecimal0, false, "so they can't share a domain; these transformers mask them alike: fpe, hash, phone, random_token"},
		{"noise", tInt64, tDecimal0, true, "so they can't share a domain; these transformers mask them alike: fpe"},
		{"random_token", tText, tJSON, false, "random_token masks equal values differently in json (main.t.b) and text (main.t.a)"},
		{"redact", tString(20), tText, false, ""},
		{"redact", tText, tJSON, false, "redact masks equal values differently"},
		{"date_shift", tDate, tTimestamp, false, "date_shift masks equal values differently"},
		{"date_shift", tTimestamp, tTimeTZ, false, "date_shift masks equal values differently"},
		{"date_shift", tDate, tText, false, "date_shift masks equal values differently"},
		{"default", tText, tText, false, "domain shared: default gives each column a value of its own, not one from the input, so equal values can't stay equal in main.t.a, main.t.b"},
		{"phone", tInt64, tDecimal20, false, ""},
		{"phone", tInt64, tText, false, "phone masks equal values differently"},
		{"email", tString(100), tText, true, ""},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("%s %s %s", c.use, c.a, c.b), func(t *testing.T) {
			a := col("t", "a", c.a)
			if c.unique {
				a = unique(a)
			}
			_, err := resolved(t, []ColumnRule{
				{a, Rule{Use: c.use, Domain: "shared"}},
				{col("t", "b", c.b), Rule{Use: c.use, Domain: "shared"}},
			})
			switch {
			case c.want == "" && err != nil:
				t.Errorf("unexpected error: %v", err)
			case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
				t.Errorf("error = %v\nwant one containing %q", err, c.want)
			}
		})
	}
}

// TestResolveTypesFollowDeclarations checks, for every transformer and
// every pair of column kinds it takes, that a domain is refused exactly
// when the transformer doesn't mask them alike. The two rules are the same
// and have no options, so a refusal can only come from the column types,
// or for default, from its outputs that never come from the value.
func TestResolveTypesFollowDeclarations(t *testing.T) {
	for _, tr := range Catalog() {
		for _, ka := range tr.Types {
			for _, kb := range tr.Types {
				a, b := sampleType(ka), sampleType(kb)
				_, err := ResolveDomains([]ColumnRule{
					{col("t", "a", a), Rule{Use: tr.Name, Domain: "shared"}},
					{col("t", "b", b), Rule{Use: tr.Name, Domain: "shared"}},
				})
				refused := err != nil
				if refused == tr.MasksAlike(a, b) {
					t.Errorf("%s with %s and %s: refused = %v, but MasksAlike = %v (%v)", tr.Name, a, b, refused, tr.MasksAlike(a, b), err)
				}
			}
		}
	}
}

// TestAlikeTypesMaskAlike proves the declarations: for every transformer
// and every pair of column types it declares alike, values that a database
// holds as equal in those columns get equal outputs, by their canonical
// encoding, or as one UUID for the uuid transformer's native, binary and
// text forms.
func TestAlikeTypesMaskAlike(t *testing.T) {
	u, _ := value.ParseUUID("123e4567-e89b-42d3-a456-426614174000")
	uu := u.UUID()
	date, _ := value.Date(1990, 3, 15)
	tod, _ := value.TimeOfDay(13 * time.Hour)
	ts := time.Date(2026, 1, 31, 13, 45, 0, 0, time.UTC)
	texts := []string{"123456", "123e4567-e89b-42d3-a456-426614174000", "123E4567E89B42D3A456426614174000", `"Order-123456"`, "1990-03-15", "Jane Doe"}
	strs := func(ss ...string) []value.Value {
		out := make([]value.Value, len(ss))
		for i, s := range ss {
			out[i] = value.String(s)
		}
		return out
	}
	dec := func(ss ...string) []value.Value {
		out := make([]value.Value, len(ss))
		for i, s := range ss {
			out[i] = mustDecimal(t, s)
		}
		return out
	}
	samples := []struct {
		typ  value.Type
		vals []value.Value
	}{
		{value.Type{Kind: value.TypeBool}, []value.Value{value.Bool(true)}},
		{tInt32, []value.Value{value.Int(123456), value.Int(-7654321), value.Int(5)}},
		{tInt64, []value.Value{value.Int(123456), value.Int(-7654321), value.Int(5), value.Int(1234567890123)}},
		{tUint64, []value.Value{value.Uint(123456), value.Uint(5)}},
		{tFloat, []value.Value{value.Float(1234.5)}},
		{tDecimal0, dec("123456", "-7654321", "5")},
		{tDecimal, dec("123456.00", "-7654321.00", "5.00")},
		{value.Type{Kind: value.TypeDecimal, Precision: 20, Scale: 2}, dec("123456.00", "5.00")},
		{tString(100), strs(texts...)},
		{tText, strs(texts...)},
		{value.Type{Kind: value.TypeEnum, Values: texts}, strs(texts...)},
		{tJSON, strs("123456", `"Order-123456"`, `{"a":1}`)},
		{tBinary, []value.Value{value.Bytes(uu[:]), value.Bytes([]byte("Order-123456 x"))}},
		{value.Type{Kind: value.TypeOpaque, Native: "point"}, []value.Value{value.Bytes(uu[:])}},
		{tDate, []value.Value{date}},
		{value.Type{Kind: value.TypeTime, Precision: 6}, []value.Value{tod}},
		{tTimestamp, []value.Value{value.Timestamp(ts)}},
		{tTimeTZ, []value.Value{value.TimestampTZ(ts)}},
		{value.Type{Kind: value.TypeInterval}, []value.Value{value.Interval(1, 2, 3)}},
		{tUUID, []value.Value{u}},
		{value.Type{Kind: value.TypeArray, Elem: &tText}, []value.Value{value.Array(strs("a"))}},
	}
	optionSets := map[string][]map[string]any{
		"constant": {{"value": "123456"}},
		"address":  {nil, {"part": "postcode"}},
		"hash":     {nil, {"length": "24"}},
	}
	compared := map[string]int{}
	needed := map[string]bool{}
	for _, tr := range Catalog() {
		sets := optionSets[tr.Name]
		if sets == nil {
			sets = []map[string]any{nil}
		}
		for _, opts := range sets {
			for _, distinct := range []bool{false, true} {
				rule := Rule{Use: tr.Name, Options: opts, Distinct: distinct}
				maskers := make([]*masker, len(samples))
				for i, s := range samples {
					if _, err := Configure(column(s.typ), rule); err == nil {
						maskers[i] = newMaskerRule(t, column(s.typ), rule)
					}
				}
				for i, a := range samples {
					for j, b := range samples {
						if i >= j || maskers[i] == nil || maskers[j] == nil || !tr.MasksAlike(a.typ, b.typ) {
							continue
						}
						pair := fmt.Sprintf("%s: %s and %s", tr.Name, a.typ, b.typ)
						if tr.Name != "keep" && tr.Name != "null" && tr.Name != "default" {
							needed[pair] = true
						}
						for _, x := range a.vals {
							for _, y := range b.vals {
								if !dbEqual(x, y) {
									continue
								}
								ox, ex := maskers[i].mask(x)
								oy, ey := maskers[j].mask(y)
								switch {
								case ex != nil && ey != nil:
									continue
								case (ex == nil) != (ey == nil):
									t.Errorf("%s %v distinct %v: equal %s values mask in one column and fail in the other (%v; %v)", pair, opts, distinct, x.Kind(), ex, ey)
								case !dbEqual(ox, oy):
									t.Errorf("%s %v distinct %v: equal %s values get different outputs", pair, opts, distinct, x.Kind())
								}
								compared[pair]++
							}
						}
					}
				}
			}
		}
	}
	for _, pair := range slices.Sorted(maps.Keys(needed)) {
		if compared[pair] == 0 {
			t.Errorf("%s: declared alike, but no equal values were compared; add samples", pair)
		}
	}
	if len(compared) < 150 {
		t.Errorf("only %d pairs of types were compared", len(compared))
	}
}

// dbEqual reports whether a database holds two values as equal: the same
// canonical encoding (so the integer 5 equals the decimal 5.00), or, for
// values of different kinds, one UUID stored natively, as 16 bytes or as
// text.
func dbEqual(a, b value.Value) bool {
	if a.IsNull() || b.IsNull() {
		return a.IsNull() && b.IsNull()
	}
	if bytes.Equal(appendCanonical(nil, a), appendCanonical(nil, b)) {
		return true
	}
	if a.Kind() == b.Kind() {
		return false
	}
	ua, okA := uuidOf(a)
	ub, okB := uuidOf(b)
	return okA && okB && ua == ub
}

func uuidOf(v value.Value) ([16]byte, bool) {
	switch v.Kind() {
	case value.KindUUID:
		return v.UUID(), true
	case value.KindBytes:
		var u [16]byte
		return u, copy(u[:], v.Bytes()) == 16 && len(v.Bytes()) == 16
	case value.KindString:
		p, err := value.ParseUUID(v.Text())
		if err != nil {
			return [16]byte{}, false
		}
		return p.UUID(), true
	default:
		return [16]byte{}, false
	}
}

// TestUnlikeTypesDiffer shows that every split a transformer declares is
// needed: equal values in the two columns really get different outputs,
// which would break a join between them.
func TestUnlikeTypesDiffer(t *testing.T) {
	date, _ := value.Date(1990, 3, 15)
	ts := time.Date(2026, 1, 31, 13, 45, 0, 0, time.UTC)
	cases := []struct {
		use    string
		ta, tb value.Type
		a, b   value.Value
	}{
		{"fpe", tInt64, tText, value.Int(123456), value.String("123456")},
		{"hash", tInt64, tText, value.Int(123456), value.String("123456")},
		{"noise", tInt64, tDecimal, value.Int(123456), mustDecimal(t, "123456.00")},
		{"noise", tDecimal0, tDecimal, mustDecimal(t, "123456"), mustDecimal(t, "123456.00")},
		{"random_token", tText, tJSON, value.String(`"Order-123456"`), value.String(`"Order-123456"`)},
		{"redact", tText, tJSON, value.String(`"x"`), value.String(`"x"`)},
		{"date_shift", tDate, tText, date, value.String("1990-03-15")},
		{"date_shift", tTimestamp, tTimeTZ, value.Timestamp(ts), value.TimestampTZ(ts)},
	}
	for _, c := range cases {
		tr, _ := Lookup(c.use)
		if tr.MasksAlike(c.ta, c.tb) {
			t.Errorf("%s declares %s and %s alike", c.use, c.ta, c.tb)
		}
		oa := newMasker(t, column(c.ta), c.use, nil).must(c.a)
		ob := newMasker(t, column(c.tb), c.use, nil).must(c.b)
		if asComparable(oa) == asComparable(ob) {
			t.Errorf("%s gives %s and %s the same output: the split is not needed", c.use, c.ta, c.tb)
		}
	}
}

// asComparable writes a value as a database would compare it with a value
// of another type: numbers and text as their text, dates and timestamps by
// their wall-clock text.
func asComparable(v value.Value) string {
	switch v.Kind() {
	case value.KindInt:
		return intText(v.Int())
	case value.KindDecimal:
		return normalizeDecimal(v.DecimalText())
	case value.KindString:
		return v.Text()
	case value.KindTimestamp:
		return v.Timestamp().UTC().Format(time.DateTime)
	default:
		text, _ := value.AppendISO(nil, v, 'T')
		return string(text)
	}
}

// TestResolvePerGroups checks the per groups: the columns seeded from one
// key with one transformer share their options, except the entity options
// of their transformer, and never get Distinct.
func TestResolvePerGroups(t *testing.T) {
	people := func(change func(map[string]Rule)) []ColumnRule {
		rules := map[string]Rule{
			"id":         {Use: "keep"},
			"born":       {Use: "date_shift", Per: "id"},
			"hired":      {Use: "date_shift", Per: "id"},
			"left":       {Use: "date_shift", Per: "id"},
			"address":    {Use: "address", Per: "id"},
			"street":     {Use: "address", Per: "id", Options: map[string]any{"part": "street", "max_length": "30"}},
			"zip":        {Use: "address", Per: "id", Options: map[string]any{"part": "postcode"}},
			"phone":      {Use: "phone", Per: "id", Options: map[string]any{"format": "national"}},
			"phone_num":  {Use: "phone", Per: "id"},
			"first_name": {Use: "first_name", Per: "id"},
		}
		if change != nil {
			change(rules)
		}
		types := map[string]value.Type{
			"id": tInt64, "born": tDate, "hired": tTimestamp, "left": tText, "address": tText, "street": tString(30),
			"zip": tInt32, "phone": tString(12), "phone_num": tInt64, "first_name": tText,
		}
		var cols []ColumnRule
		for _, name := range slices.Sorted(maps.Keys(rules)) {
			c := col("people", name, types[name])
			if name == "id" {
				c = unique(c)
			}
			cols = append(cols, ColumnRule{c, rules[name]})
		}
		return cols
	}

	rules, err := resolved(t, people(nil))
	if err != nil {
		t.Fatalf("one entity's parts are refused: %v", err)
	}
	for ref, r := range rules {
		if r.Distinct != (ref == "main.people.id") {
			t.Errorf("%s: Distinct = %v", ref, r.Distinct)
		}
	}

	mistakes := []struct {
		name   string
		change func(map[string]Rule)
		want   string
	}{
		{
			"dates moved by different amounts",
			func(r map[string]Rule) {
				r["born"] = Rule{Use: "date_shift", Per: "id", Options: map[string]any{"max_days": "30"}}
			},
			"domain main.people.id: the columns seeded per main.people.id with date_shift describe one entity, so they must use the same options, " +
				"but these differ (an option left out counts as its default): max_days (main.people.born against main.people.hired, main.people.left)",
		},
		{
			"phones from different regions",
			func(r map[string]Rule) {
				r["phone_num"] = Rule{Use: "phone", Per: "id", Options: map[string]any{"region": "GB"}}
			},
			"with phone describe one entity, so they must use the same options, but these differ (an option left out counts as its default): region (main.people.phone against main.people.phone_num)",
		},
	}
	for _, m := range mistakes {
		t.Run(m.name, func(t *testing.T) {
			_, err := resolved(t, people(m.change))
			if err == nil || !strings.Contains(err.Error(), m.want) {
				t.Errorf("error = %v\nwant one containing %q", err, m.want)
			}
		})
	}

	// An explicit domain separates a column on purpose.
	_, err = resolved(t, people(func(r map[string]Rule) {
		r["born"] = Rule{Use: "date_shift", Per: "id", Options: map[string]any{"max_days": "30"}, Domain: "birthdays"}
	}))
	if err != nil {
		t.Errorf("a column with a domain of its own is refused: %v", err)
	}
}

// TestResolveReportsEverything checks that every problem comes at once, in
// a stable order, and that no message shows an option's value: a constant
// or a password hash is configuration that may be private.
func TestResolveReportsEverything(t *testing.T) {
	const canaryValue, canaryHash = "canary-value-7f3a", "canary-hash-9b1c"
	cols := []ColumnRule{
		{col("t", "a", tText), Rule{Use: "constant", Options: map[string]any{"value": canaryValue}, Domain: "d1"}},
		{col("t", "b", tText), Rule{Use: "constant", Options: map[string]any{"value": "other"}, Domain: "d1"}},
		{col("t", "c", tText), Rule{Use: "password_hash", Options: map[string]any{"hash": canaryHash}, Domain: "d2"}},
		{col("t", "d", tText), Rule{Use: "password_hash", Domain: "d2"}},
		{col("t", "e", tInt64), Rule{Use: "fpe", Domain: "d0"}},
		{col("t", "f", tText), Rule{Use: "fpe", Domain: "d0"}},
		{col("t", "a", tText), Rule{Use: "keep"}},
		{col("u", "x", tDate), Rule{Use: "date_shift", Per: "id"}},
		{col("u", "y", tDate), Rule{Use: "date_shift", Per: "id", Options: map[string]any{"max_days": "7"}}},
	}
	_, err := ResolveDomains(cols)
	if err == nil {
		t.Fatal("no error")
	}
	lines := strings.Split(err.Error(), "\n")
	prefixes := []string{"domain d0: fpe", "domain d1: ", "domain d2: ", "domain main.u.id: ", "main.t.a has more than one rule"}
	if len(lines) != len(prefixes) {
		t.Fatalf("%d problems, want %d:\n%v", len(lines), len(prefixes), err)
	}
	for i, p := range prefixes {
		if !strings.HasPrefix(lines[i], p) {
			t.Errorf("problem %d = %q, want it to start with %q", i, lines[i], p)
		}
	}
	if strings.Contains(err.Error(), canaryValue) || strings.Contains(err.Error(), canaryHash) {
		t.Error("an option's value is in the error")
	}
	for range 20 {
		_, again := ResolveDomains(cols)
		if again.Error() != err.Error() {
			t.Fatal("the problems come in another order")
		}
	}
}

// TestResolvedForeignKeyMatches runs a copy end to end: the rules of a key
// and its foreign key, resolved, give the foreign key its parent's fake
// value, and the per dates of the key's table keep their gap.
func TestResolvedForeignKeyMatches(t *testing.T) {
	users, orders := shop()
	usersCols := Describe(usersRef, users, dialect.Capabilities{})
	ordersCols := Describe(ordersRef, orders, dialect.Capabilities{})
	userRules := usersRules()
	orderRules := map[schema.Ident]Rule{
		"id": {Use: "keep"},
		// As a foreign key follows its root: the same rule and domain.
		"user_email": {Use: "email", Domain: DefaultDomain(schema.ColumnRef{Table: usersRef, Column: "email"})},
	}
	var cols []ColumnRule
	for _, c := range append(usersCols, ordersCols...) {
		rules := userRules
		if c.Ref.Table == ordersRef {
			rules = orderRules
		}
		if r, ok := rules[c.Ref.Column]; ok {
			cols = append(cols, ColumnRule{c, r})
		}
	}
	rules, err := ResolveDomains(cols)
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range cols {
		if c.Column.Ref.Table == usersRef {
			userRules[c.Column.Ref.Column] = rules[i]
		} else {
			orderRules[c.Column.Ref.Column] = rules[i]
		}
	}
	if !orderRules["user_email"].Distinct || userRules["born"].Distinct {
		t.Fatal("Distinct is not set as the domains need")
	}
	ut, err := NewTable(usersRef, users, userRules, dialect.Capabilities{})
	if err != nil {
		t.Fatal(err)
	}
	ot, err := NewTable(ordersRef, orders, orderRules, dialect.Capabilities{})
	if err != nil {
		t.Fatal(err)
	}
	keys := testKeys(t)
	um, om := ut.Bind(keys), ot.Bind(keys)
	born, _ := value.Date(1990, 3, 15)
	joined, _ := value.Date(2020, 1, 1)
	const email = "canary.person@private.test"
	maskedUser, maskedOrder := make([]value.Value, 4), make([]value.Value, 2)
	if err := um.Mask(maskedUser, []value.Value{value.Int(7), value.String(email), born, joined}); err != nil {
		t.Fatal(err)
	}
	if err := om.Mask(maskedOrder, []value.Value{value.Int(1), value.String(email)}); err != nil {
		t.Fatal(err)
	}
	if !maskedOrder[1].Equal(maskedUser[1]) {
		t.Error("the foreign key's fake email differs from its parent's")
	}
	if daysBetween(born, joined) != daysBetween(maskedUser[2], maskedUser[3]) {
		t.Error("the per dates moved by different amounts")
	}
}

// TestKindFamily checks that kindFamily, which the domain rules read,
// gives the family tag that appendCanonical writes, for every column kind.
func TestKindFamily(t *testing.T) {
	for _, k := range allTypes {
		for _, v := range sampleValues(t, k) {
			if got := appendCanonical(nil, v)[0]; got != kindFamily(v.Kind()) {
				t.Errorf("%s value: the seed's tag is %d, kindFamily says %d", v.Kind(), got, kindFamily(v.Kind()))
			}
		}
		if typeFamily(k) == 0 {
			t.Errorf("%s has no family", k)
		}
	}
	if kindFamily(value.KindNull) != 0 || typeFamily(value.TypeInvalid) != 0 {
		t.Error("NULL has a family")
	}
}

// TestUsesDomain proves every transformer's declaration: masking the same
// values in two domains changes some output exactly when the transformer
// says it uses the domain.
func TestUsesDomain(t *testing.T) {
	for _, tr := range Catalog() {
		changed, masked := false, false
		for _, kind := range tr.Types {
			col := column(sampleType(kind))
			col.HasDefault = true
			rule := Rule{Use: tr.Name, Options: sampleOptions(tr.Name, kind)}
			if _, err := Configure(col, rule); err != nil {
				continue
			}
			a, b := newMaskerRule(t, col, rule), newMaskerRule(t, col, rule)
			a.ctx.Domain, b.ctx.Domain = "domain.a", "domain.b"
			for _, in := range sampleValues(t, kind) {
				oa, errA := a.mask(in)
				ob, errB := b.mask(in)
				if errA != nil || errB != nil {
					continue
				}
				masked = true
				if !dbEqual(oa, ob) {
					changed = true
				}
			}
		}
		if !masked && tr.Name != "default" { // default masks no value
			t.Errorf("%s: no sample was masked", tr.Name)
		}
		if changed != tr.UsesDomain() && tr.Name != "default" {
			t.Errorf("%s: outputs change with the domain: %v, but UsesDomain = %v", tr.Name, changed, tr.UsesDomain())
		}
	}
	if d, _ := Lookup("default"); d.UsesDomain() {
		t.Error("default leaves the column to the database, whatever the domain")
	}
}

func TestOptionScope(t *testing.T) {
	scope := func(c Column, r Rule) OptionScope { return ColumnRule{c, r}.Scope("max_length") }
	a, b := col("t", "a", tText), col("u", "b", tText)
	tests := []struct {
		name string
		x, y OptionScope
		same bool
	}{
		{"one domain", scope(a, Rule{Use: "email", Domain: "d"}), scope(b, Rule{Use: "email", Domain: "d"}), true},
		{"two domains", scope(a, Rule{Use: "email"}), scope(b, Rule{Use: "email"}), false},
		{"a per group", scope(a, Rule{Use: "first_name", Per: "id"}), scope(col("t", "c", tText), Rule{Use: "first_name", Per: "id"}), true},
		{"two per keys", scope(a, Rule{Use: "first_name", Domain: "d", Per: "id"}), scope(col("t", "c", tText), Rule{Use: "first_name", Domain: "d", Per: "owner"}), false},
		{"value and per columns", scope(a, Rule{Use: "first_name", Domain: "d"}), scope(b, Rule{Use: "first_name", Domain: "d", Per: "id"}), false},
		{"an entity option", scope(a, Rule{Use: "address", Per: "id"}), scope(col("t", "c", tText), Rule{Use: "address", Per: "id"}), false},
	}
	for _, tt := range tests {
		if (tt.x == tt.y) != tt.same {
			t.Errorf("%s: same scope = %v, want %v", tt.name, tt.x == tt.y, tt.same)
		}
	}
	if scope(a, Rule{Use: "email"}) == (ColumnRule{a, Rule{Use: "email"}}).Scope("length") {
		t.Error("two options share a scope")
	}
}
