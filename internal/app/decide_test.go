package app

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/exarth-labs/stuntdb/internal/classify"
	"github.com/exarth-labs/stuntdb/internal/config"
	"github.com/exarth-labs/stuntdb/internal/schema"
)

// graph is a database whose tables reference each other in the shapes
// that are hard to decide: a chain, a cycle, a column that references two
// columns, a virtual foreign key, and a reference to a generated column.
// Every column is text, so nothing is safe on its own.
func graph() *schema.Database {
	col := func(name schema.Ident) *schema.Column { return &schema.Column{Name: name, Type: strType} }
	fk := func(from schema.Ident, table, to schema.Ident) *schema.ForeignKey {
		return &schema.ForeignKey{Columns: []schema.Ident{from}, Ref: schema.TableRef{Namespace: "N", Name: table}, RefColumns: []schema.Ident{to}}
	}
	return &schema.Database{Namespaces: []*schema.Namespace{{Name: "N", Tables: []*schema.Table{
		{Name: "ROOTS", Columns: []*schema.Column{
			col("A"), col("B"), col("UNDECIDED"),
			{Name: "G", Type: strType, Generated: &schema.Generated{Expr: "a"}},
		}},
		{
			Name:    "CHAIN",
			Columns: []*schema.Column{col("C1"), col("C2"), col("TWO"), col("TWO_AGREE"), col("WAITS"), col("VIRTUAL"), col("TO_GENERATED")},
			ForeignKeys: []*schema.ForeignKey{
				fk("C1", "ROOTS", "A"),
				fk("C2", "CHAIN", "C1"),
				fk("TWO", "ROOTS", "A"), fk("TWO", "ROOTS", "B"),
				fk("TWO_AGREE", "ROOTS", "A"), fk("TWO_AGREE", "CHAIN", "C2"),
				fk("WAITS", "ROOTS", "A"), fk("WAITS", "ROOTS", "UNDECIDED"),
				{Columns: []schema.Ident{"VIRTUAL"}, Ref: schema.TableRef{Namespace: "N", Name: "ROOTS"}, RefColumns: []schema.Ident{"A"}, Virtual: true},
				fk("TO_GENERATED", "ROOTS", "G"),
			},
		},
		{
			Name:        "CYCLE",
			Columns:     []*schema.Column{col("X"), col("Y"), col("Z")},
			ForeignKeys: []*schema.ForeignKey{fk("X", "CYCLE", "Y"), fk("Y", "CYCLE", "X"), fk("Z", "CYCLE", "X")},
		},
	}}}}
}

func decideGraph(t *testing.T, db *schema.Database, policy config.Policy) (map[string]*Decision, []Problem) {
	t.Helper()
	rules := map[schema.ColumnRef]*config.ColumnRule{
		{Table: schema.TableRef{Namespace: "N", Name: "ROOTS"}, Column: "A"}: {Use: "hash"},
		{Table: schema.TableRef{Namespace: "N", Name: "ROOTS"}, Column: "B"}: {Use: "redact"},
	}
	decisions, problems := decide(&source{db: db}, classify.Default().Database(db), rules, policy)
	out := map[string]*Decision{}
	for _, d := range decisions {
		out[string(d.Column.Table.Name)+"."+string(d.Column.Column)] = d
	}
	return out, problems
}

func TestReferences(t *testing.T) {
	got, problems := decideGraph(t, graph(), config.PolicyFail)
	tests := []struct {
		column, use, source, follows string
		why                          string
	}{
		{"CHAIN.C1", "hash", "follows N.ROOTS.A", "N.ROOTS.A", "a foreign key is masked like what it references"},
		{"CHAIN.C2", "hash", "follows N.ROOTS.A", "N.ROOTS.A", "through a chain, it follows the column at its end"},
		{"CHAIN.TWO", config.Unreviewed, "", "", "two references masked differently cannot both hold"},
		{"CHAIN.TWO_AGREE", "hash", "follows N.ROOTS.A", "N.ROOTS.A", "two references that agree are fine"},
		{"CHAIN.WAITS", config.Unreviewed, "", "N.ROOTS.UNDECIDED", "it waits until every column it depends on is decided"},
		{"CHAIN.VIRTUAL", config.Unreviewed, "", "", "a virtual foreign key passes nothing on"},
		{"CHAIN.TO_GENERATED", config.Unreviewed, "", "", "a generated column passes nothing on"},
		{"CYCLE.X", config.Unreviewed, "", "", "a cycle with nothing outside it is decided like a column alone"},
		{"CYCLE.Z", config.Unreviewed, "", "", ""},
	}
	for _, tt := range tests {
		d := got[tt.column]
		follows := ""
		if d.Follows != nil {
			follows = d.Follows.String()
		}
		if d.Use != tt.use || d.Source != tt.source || follows != tt.follows {
			t.Errorf("%s = %q (%q), follows %q; want %q (%q), follows %q: %s", tt.column, d.Use, d.Source, follows, tt.use, tt.source, tt.follows, tt.why)
		}
	}
	text := problemText(problems)
	if !strings.Contains(text, "N.CHAIN.TWO references both N.ROOTS.A (hash) and N.ROOTS.B (redact), which are masked differently") || len(problems) != 1 {
		t.Errorf("problems:\n%s", text)
	}

	// TWO has a problem already; CHAIN.WAITS waits for UNDECIDED.
	var listed []string
	for _, p := range unreviewedProblems(slices.Collect(maps.Values(got))) {
		listed = append(listed, strings.Fields(p.Message)[0])
	}
	slices.Sort(listed)
	want := []string{"N.CHAIN.TO_GENERATED", "N.CHAIN.VIRTUAL", "N.CYCLE.X", "N.CYCLE.Y", "N.CYCLE.Z", "N.ROOTS.UNDECIDED"}
	if !slices.Equal(listed, want) {
		t.Errorf("listed as needing a decision: %v, want %v", listed, want)
	}
}

// A cycle of foreign keys with nothing outside it is decided like columns
// alone: here, by the keep policy.
func TestCycleUnderPolicy(t *testing.T) {
	got, _ := decideGraph(t, graph(), config.PolicyKeep)
	for _, c := range []string{"CYCLE.X", "CYCLE.Y", "CYCLE.Z"} {
		if d := got[c]; d.Use != "keep" || d.Reason != Policy && d.Reason != Reference {
			t.Errorf("%s = %q (%v)", c, d.Use, d.Reason)
		}
	}
}

// The decisions do not depend on the order of the tables.
func TestDecisionsIgnoreTableOrder(t *testing.T) {
	first, _ := decideGraph(t, graph(), config.PolicyFail)
	reversed := graph()
	slices.Reverse(reversed.Namespaces[0].Tables)
	second, _ := decideGraph(t, reversed, config.PolicyFail)
	for name, d := range first {
		if e := second[name]; e.Use != d.Use || e.Source != d.Source {
			t.Errorf("%s = %q (%q), but %q (%q) with the tables reversed", name, d.Use, d.Source, e.Use, e.Source)
		}
	}
}

func TestSuggestionText(t *testing.T) {
	tests := []struct {
		m    classify.Match
		want string
	}{
		{classify.Match{}, "no rule suggests one"},
		{classify.Match{Class: classify.Direct, Rule: "core.email", Use: "email", Confidence: 0.95}, "suggested: email (direct, rule core.email, 0.95)"},
		{classify.Match{Class: classify.Quasi, Rule: "core.city", Use: "address", Options: map[string]string{"part": "city", "a": "x: y"}, Confidence: 0.8}, "suggested: { use: address, a: 'x: y', part: city } (quasi, rule core.city, 0.80)"},
	}
	for _, tt := range tests {
		if got := suggestion(&Decision{Match: tt.m}); got != tt.want {
			t.Errorf("suggestion = %q, want %q", got, tt.want)
		}
	}
}
