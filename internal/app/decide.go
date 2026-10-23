package app

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/exarth-labs/stuntdb/internal/classify"
	"github.com/exarth-labs/stuntdb/internal/config"
	"github.com/exarth-labs/stuntdb/internal/schema"
)

// The transformers that need no catalog: copy the value, or empty it.
const (
	useKeep = "keep"
	useNull = "null"
)

// Reason says why a column has its decision.
type Reason uint8

const (
	Unreviewed Reason = iota // nothing decides: someone must review the column
	Configured               // a rule in stuntdb.yaml
	SafeRule                 // a classification rule of class safe keeps it
	Reference                // a foreign key, masked like the columns it references
	Policy                   // masking.unclassified
	Computed                 // a generated column, which the target computes again
)

// Decision is what happens to one column, as the lockfile records it.
type Decision struct {
	Column schema.ColumnRef
	Use    string // keep, null, a transformer name, config.Generated or config.Unreviewed
	Source string // why, for the lockfile; empty for Unreviewed and Computed
	Reason Reason
	Match  classify.Match // what the column looks like, for suggestions

	// Follows is set for a foreign key: the column whose decision it takes,
	// or, while that column is unreviewed, the column it waits for.
	Follows *schema.ColumnRef

	note     string // why a policy could not decide, for the problem message
	reported bool   // a problem already explains why it is unreviewed
}

// decider makes the decision for every column. Most columns are decided
// on their own: by a rule in the configuration, as generated columns, by a
// safe classification, or by the masking.unclassified policy. A foreign key
// without a rule is masked like the columns it references, so that its
// values still match theirs.
type decider struct {
	matches  map[schema.ColumnRef]classify.Match
	rules    map[schema.ColumnRef]*config.ColumnRule
	policy   config.Policy
	columns  map[schema.ColumnRef]*schema.Column
	parents  map[schema.ColumnRef][]schema.ColumnRef // the columns each column references
	byColumn map[schema.ColumnRef]*Decision
	problems []Problem
}

// decide returns a decision for every column, in the database's order, and
// the mistakes it found: rules that are not allowed, and foreign keys whose
// references disagree. Columns that still need a decision are not mistakes;
// unreviewedProblems lists them.
func decide(src *source, matches map[schema.ColumnRef]classify.Match, rules map[schema.ColumnRef]*config.ColumnRule, policy config.Policy) ([]*Decision, []Problem) {
	d := &decider{
		matches:  matches,
		rules:    rules,
		policy:   policy,
		columns:  map[schema.ColumnRef]*schema.Column{},
		parents:  map[schema.ColumnRef][]schema.ColumnRef{},
		byColumn: map[schema.ColumnRef]*Decision{},
	}
	var order []schema.ColumnRef
	for _, ns := range src.db.Namespaces {
		for _, t := range ns.Tables {
			table := schema.TableRef{Namespace: ns.Name, Name: t.Name}
			for _, col := range t.Columns {
				ref := schema.ColumnRef{Table: table, Column: col.Name}
				d.columns[ref] = col
				order = append(order, ref)
			}
		}
	}
	d.findParents(src.db)

	decisions := make([]*Decision, len(order))
	for i, ref := range order {
		decisions[i] = d.own(ref)
		d.byColumn[ref] = decisions[i]
	}
	for _, dec := range decisions {
		switch {
		case d.followsOthers(dec.Column):
			d.follow(dec)
		case dec.Reason == Configured:
			d.checkConfiguredReference(dec)
		}
	}
	return decisions, d.problems
}

// unreviewedProblems lists the columns that need a decision. A foreign key
// that waits for the column it references is not listed: deciding that
// column decides it too.
func unreviewedProblems(decisions []*Decision) []Problem {
	var out []Problem
	for _, dec := range decisions {
		if needsRule(dec) {
			out = append(out, Problem{Message: fmt.Sprintf("%s needs a decision; %s", dec.Column, suggestion(dec))})
		}
	}
	return out
}

// needsRule reports whether a column needs a rule in the configuration.
func needsRule(dec *Decision) bool {
	return dec.Reason == Unreviewed && dec.Follows == nil && !dec.reported
}

// findParents records, for every column of a foreign key, the column it
// references. Virtual foreign keys are not enforced by the database, and
// generated columns are computed again on the target, so neither passes a
// decision on. The structure is valid (schema.Database.Validate), so each
// foreign key references as many columns as it has.
func (d *decider) findParents(db *schema.Database) {
	for _, ns := range db.Namespaces {
		for _, t := range ns.Tables {
			table := schema.TableRef{Namespace: ns.Name, Name: t.Name}
			for _, fk := range t.ForeignKeys {
				if fk.Virtual {
					continue
				}
				for i, name := range fk.Columns {
					parent := schema.ColumnRef{Table: fk.Ref, Column: fk.RefColumns[i]}
					if col, ok := d.columns[parent]; ok && col.Generated == nil {
						child := schema.ColumnRef{Table: table, Column: name}
						d.parents[child] = append(d.parents[child], parent)
					}
				}
			}
		}
	}
}

// followsOthers reports whether a column takes its decision from the
// columns it references: a foreign key without its own rule.
func (d *decider) followsOthers(ref schema.ColumnRef) bool {
	return len(d.parents[ref]) > 0 && d.rules[ref] == nil && d.columns[ref].Generated == nil
}

// own decides a column on its own. A column that follows others is left
// Unreviewed here, for follow to decide.
func (d *decider) own(ref schema.ColumnRef) *Decision {
	col, rule := d.columns[ref], d.rules[ref]
	dec := &Decision{Column: ref, Use: config.Unreviewed, Match: d.matches[ref]}
	switch {
	case col.Generated != nil:
		dec.Use, dec.Reason = config.Generated, Computed
		if rule != nil {
			d.problem(rule.At.String(), "%s is a generated column: the database computes it from other columns, so it is never copied and needs no rule", ref)
		}
	case rule != nil:
		dec.Use, dec.Source, dec.Reason = rule.Use, "config", Configured
		d.checkUse(rule.At.String(), dec, col)
	case d.followsOthers(ref):
		// decided by follow
	default:
		d.alone(dec, col)
	}
	return dec
}

// alone decides a column that has no rule and references nothing: a safe
// classification keeps it, and otherwise masking.unclassified decides.
func (d *decider) alone(dec *Decision, col *schema.Column) {
	if dec.Match.Class == classify.Safe {
		dec.Use, dec.Source, dec.Reason = useKeep, dec.Match.Source(), SafeRule
		return
	}
	d.applyPolicy(dec, col)
}

// applyPolicy decides a column with masking.unclassified. Under fail, the
// default, it stays Unreviewed.
func (d *decider) applyPolicy(dec *Decision, col *schema.Column) {
	switch d.policy {
	case config.PolicyNull:
		if !col.Nullable {
			dec.note = "masking.unclassified is null, but the column is NOT NULL"
			return
		}
		dec.Use, dec.Source, dec.Reason = useNull, "masking.unclassified", Policy
	case config.PolicyKeep:
		dec.Use, dec.Source, dec.Reason = useKeep, "masking.unclassified", Policy
		if dec.Match.Class == classify.Secret {
			dec.Use, dec.Source = dec.Match.Use, dec.Match.Source()+"; secrets are always replaced"
		}
	}
}

// checkUse reports a transformer that cannot go in a column: keep on a
// secret, or null in a NOT NULL column.
func (d *decider) checkUse(at string, dec *Decision, col *schema.Column) {
	if err := dec.Match.Allows(dec.Use); err != nil {
		d.problem(at, "%s: %v", dec.Column, err)
	}
	if dec.Use == useNull && !col.Nullable {
		d.problem(at, "%s is NOT NULL, so it cannot be null", dec.Column)
	}
}

// roots returns the columns whose decisions a column's values depend on:
// the columns it references, and, where those follow others in turn, the
// columns they reference, until a column that is decided on its own. Each
// root is listed once, in the order the foreign keys give.
func (d *decider) roots(ref schema.ColumnRef) []schema.ColumnRef {
	var out []schema.ColumnRef
	seen := map[schema.ColumnRef]bool{ref: true}
	queue := append([]schema.ColumnRef(nil), d.parents[ref]...)
	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]
		if seen[next] {
			continue
		}
		seen[next] = true
		if d.followsOthers(next) {
			queue = append(queue, d.parents[next]...)
			continue
		}
		out = append(out, next)
	}
	return out
}

// follow decides a foreign key without a rule: it is masked like the
// columns it depends on, once all of them are decided and agree.
func (d *decider) follow(dec *Decision) {
	roots := d.roots(dec.Column)
	if len(roots) == 0 {
		// The references only lead around a cycle of foreign keys, so
		// nothing outside the cycle decides it.
		d.alone(dec, d.columns[dec.Column])
		return
	}
	var decided *Decision
	for _, r := range roots {
		root := d.byColumn[r]
		if root.Use == config.Unreviewed {
			dec.Follows = &root.Column
			return // waits for the root's review
		}
		if decided != nil && root.Use != decided.Use {
			d.problem("", "%s references both %s (%s) and %s (%s), which are masked differently", dec.Column, decided.Column, decided.Use, root.Column, root.Use)
			dec.reported = true
			return
		}
		if decided == nil {
			decided = root
		}
	}
	dec.Use, dec.Source, dec.Reason, dec.Follows = decided.Use, "follows "+decided.Column.String(), Reference, &decided.Column
	d.checkUse("", dec, d.columns[dec.Column])
}

// checkConfiguredReference reports a foreign key whose rule masks it unlike
// the columns it references, which would break the reference.
func (d *decider) checkConfiguredReference(dec *Decision) {
	at := d.rules[dec.Column].At.String()
	for _, r := range d.roots(dec.Column) {
		if root := d.byColumn[r]; root.Use != config.Unreviewed && root.Use != dec.Use {
			d.problem(at, "%s references %s, so it must be masked the same way (%s); remove its rule", dec.Column, root.Column, root.Use)
		}
	}
}

func (d *decider) problem(at, format string, args ...any) {
	d.problems = append(d.problems, Problem{At: at, Message: fmt.Sprintf(format, args...)})
}

// suggestion describes what the classification suggests for a column.
func suggestion(dec *Decision) string {
	var s string
	if m := dec.Match; m.Class == classify.Unclassified {
		s = "no rule suggests one"
	} else {
		s = fmt.Sprintf("suggested: %s (%s, rule %s, %s)", ruleText(m), m.Class, m.Rule, strconv.FormatFloat(m.Confidence, 'f', 2, 64))
	}
	if dec.note != "" {
		s += "; " + dec.note
	}
	return s
}

// ruleText writes a suggested rule the way stuntdb.yaml takes it: a
// transformer name, or { use: name, option: value } with its options.
func ruleText(m classify.Match) string {
	if len(m.Options) == 0 {
		return m.Use
	}
	var b strings.Builder
	b.WriteString("{ use: " + m.Use)
	for _, k := range slices.Sorted(maps.Keys(m.Options)) {
		b.WriteString(", " + yamlScalar(k) + ": " + yamlScalar(m.Options[k]))
	}
	b.WriteString(" }")
	return b.String()
}
