package app

import (
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/exarth-labs/stuntdb/internal/classify"
	"github.com/exarth-labs/stuntdb/internal/config"
	"github.com/exarth-labs/stuntdb/internal/errs"
	"github.com/exarth-labs/stuntdb/internal/mask"
	"github.com/exarth-labs/stuntdb/internal/schema"
	"github.com/exarth-labs/stuntdb/internal/value"
)

// checkConfig runs check --update on a database with a configuration's
// column rules, and returns the problems and the lockfile, if written.
func checkConfig(t *testing.T, db *schema.Database, columns string) (problems, lock string) {
	t.Helper()
	f := newFixture(t, db)
	f.write(t, config.DefaultPath, "version: 1\nsource:\n  url: fake:shop\n"+columns)
	res, err := f.check(t, true)
	if err != nil && errs.KindOf(err) != errs.CheckFailed {
		t.Fatalf("check: %v", err)
	}
	if res.LockfileWritten {
		lock = f.read(t, config.LockPath)
	}
	return problemText(res.Problems), lock
}

// Mistakes that only the masking engine finds, in the shop database. Each
// names the column and the rule to change, never a value.
func TestCheckMaskingMistakes(t *testing.T) {
	tests := []struct {
		name    string
		columns string
		want    []string
	}{
		{
			"a foreign key with its parent's rule",
			"columns:\n  app.customers:\n    email: email\n  app.orders:\n    billing_email: email\n",
			[]string{"stuntdb.yaml:8:5: APP.ORDERS.BILLING_EMAIL references APP.CUSTOMERS.EMAIL, so it is masked like it and can't have a rule of its own; remove this rule, and to change how both are masked, change the rule of APP.CUSTOMERS.EMAIL"},
		},
		{
			"a foreign key whose parent has no rule",
			"columns:\n  app.orders:\n    customer_id: keep\n",
			[]string{"APP.ORDERS.CUSTOMER_ID references APP.CUSTOMERS.ID, so it is masked like it and can't have a rule of its own; remove this rule, and to change how both are masked, write a rule for APP.CUSTOMERS.ID"},
		},
		{
			"an unknown transformer",
			"columns:\n  app.customers:\n    email: emial\n",
			[]string{"stuntdb.yaml:6:5: APP.CUSTOMERS.EMAIL: there is no transformer emial; did you mean email?"},
		},
		{
			"a bad option",
			"columns:\n  app.customers:\n    first_name: { use: first_name, max_length: x }\n",
			[]string{"stuntdb.yaml:6:5: APP.CUSTOMERS.FIRST_NAME: option max_length: must be a whole number from"},
		},
		{
			"a weak transformer on a unique column",
			"columns:\n  app.customers:\n    email: first_name\n",
			[]string{"stuntdb.yaml:6:5: APP.CUSTOMERS.EMAIL: the column is part of a unique key, so its masked values must stay distinct, and first_name cannot promise that; these can: email, fpe, hash, random_token, username"},
		},
		{
			"per on a column that isn't there",
			"columns:\n  app.customers:\n    first_name: { use: first_name, per: nope }\n",
			[]string{`stuntdb.yaml:6:5: APP.CUSTOMERS.FIRST_NAME: per: column "nope" is not in the table`},
		},
		{
			"per on the column itself",
			"columns:\n  app.customers:\n    first_name: { use: first_name, per: first_name }\n",
			[]string{"stuntdb.yaml:6:5: APP.CUSTOMERS.FIRST_NAME: per names the column itself"},
		},
		{
			"per on a generated column",
			"columns:\n  app.customers:\n    notes: { use: synthesize_text, per: search_key }\n",
			[]string{"stuntdb.yaml:6:5: APP.CUSTOMERS.NOTES: per names APP.CUSTOMERS.SEARCH_KEY, a generated column, which the copy never reads"},
		},
		{
			"per on a column left to its default",
			"columns:\n  app.customers:\n    tier: default\n    notes: { use: synthesize_text, per: tier }\n",
			[]string{"stuntdb.yaml:7:5: APP.CUSTOMERS.NOTES: per names APP.CUSTOMERS.TIER, which is left to its default, so the copy never reads it"},
		},
		{
			"per on a column foreign keys reference",
			"columns:\n  app.customers:\n    email: { use: email, per: id }\n",
			[]string{
				"stuntdb.yaml:6:5: APP.CUSTOMERS.EMAIL: foreign keys reference it (APP.ORDERS.BILLING_EMAIL), and their rows don't hold its per column, so it can't use per",
				"stuntdb.yaml:6:5: APP.CUSTOMERS.EMAIL: per seeds the column from another column",
			},
		},
		{
			"a domain with two transformers",
			"columns:\n  app.customers:\n    first_name: { use: first_name, domain: names }\n    notes: { use: synthesize_text, domain: names }\n",
			[]string{"domain names: its columns must mask equal values alike, but they use different transformers: first_name (APP.CUSTOMERS.FIRST_NAME), synthesize_text (APP.CUSTOMERS.NOTES)"},
		},
		{
			"default in a shared domain",
			"columns:\n  app.customers:\n    first_name: { use: default, domain: d }\n    notes: { use: default, domain: d }\n",
			[]string{"domain d: default gives each column a value of its own, not one from the input, so equal values can't stay equal in APP.CUSTOMERS.FIRST_NAME, APP.CUSTOMERS.NOTES"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			problems, lock := checkConfig(t, shop(), tt.columns)
			for _, w := range tt.want {
				if !strings.Contains(problems, w) {
					t.Errorf("the problems do not contain %q:\n%s", w, problems)
				}
			}
			if lock != "" {
				t.Error("the lockfile was written despite the mistakes")
			}
		})
	}
}

// links is a database of references: a unique code that two tables
// reference from shorter columns, a column that references two parents,
// and a one-to-one pair of foreign keys that reference each other, with a
// third column that references the pair.
func links() *schema.Database {
	str := func(n int) value.Type { return value.Type{Kind: value.TypeString, MaxLen: n} }
	fk := func(from schema.Ident, table, to schema.Ident) *schema.ForeignKey {
		return &schema.ForeignKey{Columns: []schema.Ident{from}, Ref: schema.TableRef{Namespace: "N", Name: table}, RefColumns: []schema.Ident{to}}
	}
	return &schema.Database{Namespaces: []*schema.Namespace{{Name: "N", Tables: []*schema.Table{
		{
			Name:    "USERS",
			Columns: []*schema.Column{{Name: "CODE", Type: str(100)}, {Name: "EMAIL", Type: str(100)}, {Name: "NAME", Type: str(100)}},
			Uniques: []*schema.Key{{Columns: []schema.Ident{"CODE"}}, {Columns: []schema.Ident{"EMAIL"}}},
		},
		{
			Name:    "ADMINS",
			Columns: []*schema.Column{{Name: "EMAIL", Type: str(100)}},
			Uniques: []*schema.Key{{Columns: []schema.Ident{"EMAIL"}}},
		},
		{
			Name: "REFS",
			Columns: []*schema.Column{
				{Name: "CODE_A", Type: str(30)},
				{Name: "CODE_B", Type: str(28)},
				{Name: "CONTACT", Type: str(100)},
				{Name: "NAME_KEY", Type: str(100)},
			},
			Uniques: []*schema.Key{{Columns: []schema.Ident{"NAME_KEY"}}},
			ForeignKeys: []*schema.ForeignKey{
				fk("CODE_A", "USERS", "CODE"), fk("CODE_B", "USERS", "CODE"),
				fk("CONTACT", "USERS", "EMAIL"), fk("CONTACT", "ADMINS", "EMAIL"),
				// A unique foreign key to a column that isn't unique, as
				// MariaDB allows.
				fk("NAME_KEY", "USERS", "NAME"),
			},
		},
		{
			Name:        "PAIRS",
			Columns:     []*schema.Column{{Name: "X", Type: str(100)}, {Name: "Y", Type: str(100)}, {Name: "Z", Type: str(100)}},
			ForeignKeys: []*schema.ForeignKey{fk("X", "PAIRS", "Y"), fk("Y", "PAIRS", "X"), fk("Z", "PAIRS", "X")},
		},
	}}}}
}

// linkRules decides every column of links, with the given changes.
func linkRules(changes map[string]string) string {
	rules := map[string]string{
		"users.code":   "{ use: hash, length: 28 }",
		"users.email":  "{ use: email, domain: person }",
		"users.name":   "hash",
		"admins.email": "{ use: email, domain: person }",
		"pairs.x":      "keep",
	}
	for k, v := range changes {
		rules[k] = v
	}
	var b strings.Builder
	b.WriteString("columns:\n")
	for _, table := range []string{"users", "admins", "refs", "pairs"} {
		var lines []string
		for k, v := range rules {
			if t, c, _ := strings.Cut(k, "."); t == table && v != "" {
				lines = append(lines, "    "+c+": "+v+"\n")
			}
		}
		if len(lines) > 0 {
			b.WriteString("  n." + table + ":\n")
			slices.Sort(lines)
			for _, l := range lines {
				b.WriteString(l)
			}
		}
	}
	return b.String()
}

func TestCheckReferences(t *testing.T) {
	t.Run("the whole rule follows", func(t *testing.T) {
		problems, lock := checkConfig(t, links(), linkRules(nil))
		if problems != "" {
			t.Fatalf("problems:\n%s", problems)
		}
		for _, want := range []string{
			// The two codes take the parent's options and domain.
			"CODE_A: {decision: hash, source: follows N.USERS.CODE, options: {length: \"28\"}, domain: N.USERS.CODE}",
			"CODE_B: {decision: hash, source: follows N.USERS.CODE, options: {length: \"28\"}, domain: N.USERS.CODE}",
			// Two parents in one domain: the column takes it.
			"CONTACT: {decision: email, source: follows N.USERS.EMAIL, domain: person}",
			"EMAIL: {decision: email, source: config, domain: person}",
			// One rule decides a cycle; the others follow it. keep has no
			// domain to record.
			"X: {decision: keep, source: config}",
			"Y: {decision: keep, source: follows N.PAIRS.X}",
			"Z: {decision: keep, source: follows N.PAIRS.X}",
		} {
			if !strings.Contains(lock, want) {
				t.Errorf("the lockfile does not contain %q:\n%s", want, lock)
			}
		}
	})

	tests := []struct {
		name    string
		changes map[string]string
		want    string
	}{
		{
			"two parents in different domains",
			map[string]string{"admins.email": "email"},
			"N.REFS.CONTACT references both N.USERS.EMAIL and N.ADMINS.EMAIL, which are in different domains (person and N.ADMINS.EMAIL), so it can't match both; give them one domain:",
		},
		{
			"two parents with different options",
			map[string]string{"admins.email": "{ use: email, domain: person, max_length: 60 }"},
			"N.REFS.CONTACT references both N.USERS.EMAIL and N.ADMINS.EMAIL, which give email different options, so it can't match both; give them the same options",
		},
		{
			"two parents masked differently",
			map[string]string{"admins.email": "{ use: hash, domain: person }"},
			"N.REFS.CONTACT references both N.USERS.EMAIL and N.ADMINS.EMAIL, which are masked differently (email and hash)",
		},
		{
			"two rules in one cycle",
			map[string]string{"pairs.y": "keep"},
			"N.PAIRS.Y references N.PAIRS.X, so it is masked like it and can't have a rule of its own",
		},
		{
			"a rule on a column that only references a cycle",
			map[string]string{"pairs.x": "", "pairs.z": "keep"},
			"N.PAIRS.Z references N.PAIRS.X, so it is masked like it and can't have a rule of its own; remove this rule, and to change how both are masked, write a rule for N.PAIRS.X",
		},
		{
			"followers too short for one length",
			map[string]string{"users.code": "{ use: hash, length: 40 }"},
			"stuntdb.yaml:6:5: N.REFS.CODE_A, N.REFS.CODE_B are too short for the values of N.USERS.CODE's rule (hash); set length: 28, which fits every column it masks",
		},
		{
			"a follower too short for any value",
			map[string]string{"users.code": "uuid"},
			"stuntdb.yaml:6:5: N.REFS.CODE_A: uuid writes UUIDs as the text had them, in 32 or 36 characters, and the column holds 30; it follows N.USERS.CODE, so change the rule of N.USERS.CODE",
		},
		{
			"a unique follower of a column that isn't unique",
			map[string]string{"users.name": "first_name"},
			"stuntdb.yaml:8:5: N.REFS.NAME_KEY: the column is part of a unique key, so its masked values must stay distinct, and first_name cannot promise that; these can: email, fpe, hash, random_token, username; it follows N.USERS.NAME, so change the rule of N.USERS.NAME",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			problems, _ := checkConfig(t, links(), linkRules(tt.changes))
			if !strings.Contains(problems, tt.want) {
				t.Errorf("the problems do not contain %q:\n%s", tt.want, problems)
			}
		})
	}
}

// A unique foreign key makes its domain distinct, so its parent, which
// isn't unique, takes the distinct form too, and both get equal values.
func TestCheckDistinctThroughAForeignKey(t *testing.T) {
	problems, _ := checkConfig(t, links(), linkRules(map[string]string{"users.name": "{ use: hash, length: 16 }"}))
	if !strings.Contains(problems, "N.USERS.NAME: option length: values here must stay distinct (a unique key shares the domain), so hash needs a length of at least 24") {
		t.Errorf("problems:\n%s", problems)
	}
}

// A secret decided by a rule pack, under masking.unclassified: keep, that
// can't fit its column: the message says to write a rule for it.
func TestCheckRefusalOfARulePack(t *testing.T) {
	db := &schema.Database{Namespaces: []*schema.Namespace{{Name: "N", Tables: []*schema.Table{{
		Name:    "ACCOUNTS",
		Columns: []*schema.Column{{Name: "PASSWORD", Type: value.Type{Kind: value.TypeString, MaxLen: 30}}},
	}}}}}
	problems, _ := checkConfig(t, db, "masking:\n  unclassified: keep\n")
	want := "N.ACCOUNTS.PASSWORD: password_hash writes up to 60 characters, and the column holds 30; set hash: to a hash that fits; it is decided by rule secrets.password; secrets are always replaced, so write a rule for it"
	if !strings.Contains(problems, want) {
		t.Errorf("problems:\n%s\nwant %q", problems, want)
	}
}

// Columns too short for the values of a rule that no line of stuntdb.yaml
// holds: the message says to write one, with the value that fits.
func TestTooShortWithoutARule(t *testing.T) {
	ref := func(table, column string) schema.ColumnRef {
		return schema.ColumnRef{Table: schema.TableRef{Namespace: "N", Name: schema.Ident(table)}, Column: schema.Ident(column)}
	}
	db := &schema.Database{Namespaces: []*schema.Namespace{{Name: "N", Tables: []*schema.Table{
		{Name: "P", Columns: []*schema.Column{{Name: "NAME", Type: value.Type{Kind: value.TypeString, MaxLen: 100}}}},
		{Name: "C", Columns: []*schema.Column{{Name: "NAME", Type: value.Type{Kind: value.TypeString, MaxLen: 6}}}},
	}}}}
	root := ref("P", "NAME")
	decisions := []*Decision{
		{Column: root, Use: "first_name", Source: "masking.unclassified", Reason: Policy},
		{Column: ref("C", "NAME"), Use: "first_name", Source: "follows N.P.NAME", Reason: Reference, Follows: &root, Domain: mask.DefaultDomain(root)},
	}
	problems := problemText(checkMasking(&source{db: db}, decisions))
	want := "N.C.NAME is too short for the values of N.P.NAME (first_name, decided by masking.unclassified); write a rule for N.P.NAME with max_length: 6, which fits every column it masks"
	if problems != want {
		t.Errorf("problems:\n%s\nwant %q", problems, want)
	}
}

// The lockfile records options, domains and per, so that changing any of
// them is a difference that check reports.
func TestLockfileRecordsTheWholeRule(t *testing.T) {
	const base = "columns:\n  app.customers:\n    email: email\n    first_name: { use: first_name, per: id, max_length: 20 }\n    api_token: random_token\n    notes: synthesize_text\n    tier: keep\n  app.orders:\n    total: keep\n"
	f := newFixture(t, shop())
	f.write(t, config.DefaultPath, "version: 1\nsource:\n  url: fake:shop\n"+base)
	if res, err := f.check(t, true); err != nil {
		t.Fatalf("check --update: %v\n%s", err, problemText(res.Problems))
	}
	lock := f.read(t, config.LockPath)
	if want := `FIRST_NAME: {decision: first_name, source: config, options: {max_length: "20"}, per: ID}`; !strings.Contains(lock, want) {
		t.Fatalf("the lockfile does not contain %q:\n%s", want, lock)
	}

	tests := []struct {
		name, from, to, want string
	}{
		{"an option", "max_length: 20", "max_length: 12", "APP.CUSTOMERS.FIRST_NAME changed: options changed: max_length;"},
		{"a new option", "random_token\n", "{ use: random_token, domain: tokens }\n", "APP.CUSTOMERS.API_TOKEN changed: domain tokens, was the column's own;"},
		{"per", "per: id, ", "", "APP.CUSTOMERS.FIRST_NAME changed: per none, was ID;"},
		{"a parent's options reach its foreign key", "email: email", "email: { use: email, max_length: 60 }", "APP.ORDERS.BILLING_EMAIL changed: options changed: max_length;"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f.write(t, config.DefaultPath, "version: 1\nsource:\n  url: fake:shop\n"+strings.Replace(base, tt.from, tt.to, 1))
			res, err := f.check(t, false)
			if errs.KindOf(err) != errs.CheckFailed || !strings.Contains(problemText(res.Problems), tt.want) {
				t.Errorf("check = %v, problems:\n%s\nwant %q", err, problemText(res.Problems), tt.want)
			}
			// Only the messages: a position holds a temporary path, whose
			// random digits could be anything.
			for _, p := range res.Problems {
				if strings.Contains(p.Message, "60") {
					t.Errorf("a message shows an option's value: %s", p.Message)
				}
			}
		})
	}
	if err := os.Remove(f.path(config.LockPath)); err != nil {
		t.Fatal(err)
	}
}

// A secret's options come from its rule pack, as the masking engine takes
// options from stuntdb.yaml.
func TestMatchOptions(t *testing.T) {
	if got := matchOptions(classify.Match{}); got != nil {
		t.Errorf("no options = %v", got)
	}
	got := matchOptions(classify.Match{Options: map[string]string{"part": "city"}})
	if len(got) != 1 || got["part"] != "city" {
		t.Errorf("options = %v", got)
	}
}

// A column too short for its own rule is reported as the masking engine
// says it, with the value that fits, at the rule.
func TestTooShortForItsOwnRule(t *testing.T) {
	ref := schema.ColumnRef{Table: schema.TableRef{Namespace: "N", Name: "C"}, Column: "NAME"}
	db := &schema.Database{Namespaces: []*schema.Namespace{{Name: "N", Tables: []*schema.Table{
		{Name: "C", Columns: []*schema.Column{{Name: "NAME", Type: value.Type{Kind: value.TypeString, MaxLen: 6}}}},
		{Name: "D", Columns: []*schema.Column{{Name: "NAME", Type: value.Type{Kind: value.TypeString, MaxLen: 5}}}},
	}}}}
	other := schema.ColumnRef{Table: schema.TableRef{Namespace: "N", Name: "D"}, Column: "NAME"}
	decisions := []*Decision{
		{Column: other, Use: "first_name", Source: "config", Reason: Configured, at: "stuntdb.yaml:9:5"},
		{Column: ref, Use: "first_name", Source: "config", Reason: Configured, at: "stuntdb.yaml:7:5"},
	}
	got := checkMasking(&source{db: db}, decisions)
	// One problem per rule, in the order of the columns they decide.
	if len(got) != 2 || got[0].At != "stuntdb.yaml:7:5" || !strings.HasSuffix(got[0].Message, "and the column holds 6; set max_length: 6") ||
		got[1].At != "stuntdb.yaml:9:5" || !strings.HasSuffix(got[1].Message, "set max_length: 5") {
		t.Errorf("problems = %v", got)
	}
}

// unjoin lists the errors errors.Join joined, and a lone error as itself.
func TestUnjoin(t *testing.T) {
	a, b := errors.New("a"), errors.New("b")
	if got := unjoin(errors.Join(a, b)); len(got) != 2 || !errors.Is(got[0], a) || !errors.Is(got[1], b) {
		t.Errorf("joined = %v", got)
	}
	if got := unjoin(a); len(got) != 1 || !errors.Is(got[0], a) {
		t.Errorf("lone = %v", got)
	}
	if unjoin(nil) != nil {
		t.Error("nil gives errors")
	}
}
