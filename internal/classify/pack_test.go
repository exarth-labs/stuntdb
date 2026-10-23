package classify

import (
	"strings"
	"testing"

	"github.com/exarth-labs/stuntdb/rules"
)

// plannedTransformers are the transformers planned for v0.1. Until the
// transformer catalog exists, built-in rules may suggest only these.
var plannedTransformers = []string{
	"keep", "null", "constant", "default", "first_name", "last_name", "full_name",
	"username", "email", "phone", "address", "company", "uuid", "random_token",
	"password_hash", "redact", "noise", "date_shift", "synthesize_text", "hash",
}

func builtinRules(t *testing.T) []*rule {
	t.Helper()
	c, err := Default()
	if err != nil {
		t.Fatalf("the built-in packs do not load: %v", err)
	}
	return c.rules
}

func TestBuiltinPacks(t *testing.T) {
	rs := builtinRules(t)
	counts := map[string]int{}
	for _, r := range rs {
		counts[strings.SplitN(r.id, ".", 2)[0]]++
		if !contains(plannedTransformers, []string{r.use}) {
			t.Errorf("%s suggests %s, which is not a planned transformer", r.id, r.use)
		}
		if strings.HasPrefix(r.id, "secrets.") && r.class != Secret {
			t.Errorf("%s is in the secrets pack, but its class is %s", r.id, r.class)
		}
	}
	if counts["core"] == 0 || counts["secrets"] == 0 {
		t.Errorf("rules per pack = %v", counts)
	}
}

// TestBuiltinRulesAreUnambiguous checks that no name belongs to two rules
// that apply to the same types: which one wins would depend on their order.
func TestBuiltinRulesAreUnambiguous(t *testing.T) {
	rs := builtinRules(t)
	for i, a := range rs {
		for _, b := range rs[i+1:] {
			if a.types&b.types == 0 {
				continue
			}
			for _, p := range a.names {
				for _, q := range b.names {
					if strings.Join(p, " ") == strings.Join(q, " ") {
						t.Errorf("%q is a name in both %s and %s", strings.Join(p, " "), a.id, b.id)
					}
				}
			}
		}
	}
}

func TestPackMistakes(t *testing.T) {
	const good = "version: 1\npack: test\nrules:\n  - {id: email, class: direct, use: email, confidence: 0.9, types: [string], names: [email]}\n"
	if _, err := parsePack(rules.Pack{Name: "test", Data: []byte(good)}); err != nil {
		t.Fatalf("a good pack is refused: %v", err)
	}

	head := "version: 1\npack: test\nrules:\n"
	tests := []struct {
		name string
		data string
		want []string
	}{
		{"empty", "", []string{"test.yaml: the file is empty"}},
		{"not YAML", "version: [", []string{"test.yaml: "}},
		{"wrong version", strings.Replace(good, "version: 1", "version: 2", 1), []string{"test.yaml:1: version must be 1"}},
		{"wrong pack name", strings.Replace(good, "pack: test", "pack: other", 1), []string{`pack must be "test"`}},
		{"no rules", "version: 1\npack: test\nrules: []\n", []string{"the pack has no rules"}},
		{"unknown setting", strings.Replace(good, "names:", "nmaes:", 1), []string{"field nmaes not found"}},
		{"duplicate setting", head + "  - id: a\n    id: b\n", []string{`"id" already defined`}},
		{
			"every mistake in a rule, at once",
			head + "  - id: x\n    class: direct\n    use: email\n    confidence: 0.9\n    types: [string]\n    names: [email]\n" +
				"  - id: Bad-Id\n    class: personal\n    use: Email\n    options: {Part: city}\n    confidence: 1.5\n    types: [str]\n    names: [E-Mail, email, email]\n    tables: [\"users \"]\n",
			[]string{
				"test.yaml:10: rule Bad-Id: id must be lowercase",
				`unknown class "personal"`,
				"use must name a transformer",
				`option "Part" is not a valid name`,
				"confidence must be more than 0 and at most 1",
				`unknown type "str"`,
				`names: "E-Mail" must be lowercase words`,
				`names: "email" appears twice`,
				`tables: "users " must be lowercase words`,
			},
		},
		{"no types", head + "  - {id: a, class: direct, use: email, confidence: 0.5, names: [email]}\n", []string{"types is missing"}},
		{"no names", head + "  - {id: a, class: direct, use: email, confidence: 0.5, types: [string]}\n", []string{"a rule needs names, or identity: true"}},
		{"no confidence", head + "  - {id: a, class: direct, use: email, types: [string], names: [email]}\n", []string{"confidence must be more than 0"}},
		{"no class", head + "  - {id: a, use: email, confidence: 0.5, types: [string], names: [email]}\n", []string{`unknown class ""`}},
		{"unclassified is not a class for rules", head + "  - {id: a, class: unclassified, use: email, confidence: 0.5, types: [string], names: [email]}\n", []string{`unknown class "unclassified"`}},
		{"safe must keep", head + "  - {id: a, class: safe, use: email, confidence: 0.5, types: [string], names: [email]}\n", []string{"a safe rule must use keep"}},
		{"a secret is never kept", head + "  - {id: a, class: secret, use: keep, confidence: 0.5, types: [string], names: [token]}\n", []string{"a secret rule cannot use keep"}},
		{"a rule without an id", head + "  - {class: direct, use: email, confidence: 0.5, types: [string], names: [email]}\n", []string{"rule #1: id must be"}},
		{
			"duplicate ids",
			head + "  - {id: a, class: direct, use: email, confidence: 0.5, types: [string], names: [email]}\n" +
				"  - {id: a, class: direct, use: phone, confidence: 0.5, types: [string], names: [phone]}\n",
			[]string{"test.yaml:5: rule a appears twice"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parsePack(rules.Pack{Name: "test", Data: []byte(tt.data)})
			if err == nil {
				t.Fatal("the pack is accepted")
			}
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the error does not contain %q:\n%v", want, err)
				}
			}
		})
	}
}

func TestNewRefusesAPackTwice(t *testing.T) {
	core := rules.Builtin()[0]
	if _, err := New(core, core); err == nil || !strings.Contains(err.Error(), "loaded twice") {
		t.Errorf("New(core, core) = %v", err)
	}
	if _, err := New(rules.Pack{Name: "broken", Data: []byte("version: 2\npack: broken\n")}); err == nil || !strings.HasPrefix(err.Error(), "classify: broken.yaml") {
		t.Errorf("New(broken) = %v", err)
	}
}
