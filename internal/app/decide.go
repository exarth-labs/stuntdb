package app

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/exarth-labs/stuntdb/internal/classify"
	"github.com/exarth-labs/stuntdb/internal/config"
	"github.com/exarth-labs/stuntdb/internal/mask"
	"github.com/exarth-labs/stuntdb/internal/schema"
)

// The transformers that decide differently from the others: copy the value,
// empty it, or leave the column to its database default.
const (
	useKeep    = "keep"
	useNull    = "null"
	useDefault = "default"
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

	// The rest of the rule, for a transformer: its options, its domain as
	// written (empty means the column's default) and the column that seeds
	// it. A foreign key takes its root's options, and the root's domain, so
	// that both get the same fake values.
	Options map[string]any
	Domain  string
	Per     schema.Ident

	at       string // where the rule that decides it is written: its own, or a foreign key's root's; empty when no rule does
	note     string // why a policy could not decide, for the problem message
	reported bool   // a problem already explains why it is unreviewed
	refused  bool   // its own rule is not allowed, so its masking is not checked further
}

// maskRule returns the decision as the masking engine takes it.
func (dec *Decision) maskRule() mask.Rule {
	return mask.Rule{Use: dec.Use, Options: dec.Options, Domain: dec.Domain, Per: dec.Per}
}

// domain returns the column's domain under its decision.
func (dec *Decision) domain() string { return dec.maskRule().DomainFor(dec.Column) }

// masked reports whether the decision masks the column with a transformer
// that the masking engine checks.
func (dec *Decision) masked() bool {
	return dec.Use != config.Unreviewed && dec.Use != config.Generated && !dec.refused
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
	children map[schema.ColumnRef][]schema.ColumnRef // the columns that reference each column
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
		children: map[schema.ColumnRef][]schema.ColumnRef{},
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
			d.checkOwnRule(dec)
		}
	}
	for _, dec := range decisions {
		if dec.Per != "" && !dec.refused {
			d.checkPer(dec)
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
						d.children[parent] = append(d.children[parent], child)
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
		dec.Options, dec.Domain, dec.Per, dec.at = rule.Options, rule.Domain, schema.Ident(rule.Per), rule.At.String()
		d.checkSecret(dec)
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
			dec.Options = matchOptions(dec.Match)
		}
	}
}

// matchOptions returns a rule pack's suggested options the way the masking
// engine takes options from stuntdb.yaml.
func matchOptions(m classify.Match) map[string]any {
	if len(m.Options) == 0 {
		return nil
	}
	out := make(map[string]any, len(m.Options))
	for k, v := range m.Options {
		out[k] = v
	}
	return out
}

// checkSecret reports keep on a column that holds a secret: secrets are
// always replaced. Whether a transformer fits the column is for the
// masking engine (checkMasking).
func (d *decider) checkSecret(dec *Decision) {
	if err := dec.Match.Allows(dec.Use); err != nil {
		d.problem(dec.at, "%s: %v", dec.Column, err)
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

// follow decides a foreign key without a rule: it takes the whole rule of
// the columns it depends on (the transformer, its options and its domain),
// once all of them are decided and agree, so that its values match theirs.
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
		if decided != nil {
			if why := disagree(decided, root); why != "" {
				d.problem("", "%s references both %s and %s, %s", dec.Column, decided.Column, root.Column, why)
				dec.reported = true
				return
			}
		}
		if decided == nil {
			decided = root
		}
	}
	dec.Use, dec.Source, dec.Reason, dec.Follows = decided.Use, "follows "+decided.Column.String(), Reference, &decided.Column
	dec.Options, dec.Domain, dec.at = decided.Options, decided.domain(), decided.at
}

// disagree says why one column can't hold the values of two columns it
// references, or returns "" when they mask equal values alike.
func disagree(a, b *Decision) string {
	switch {
	case a.Use != b.Use:
		return fmt.Sprintf("which are masked differently (%s and %s)", a.Use, b.Use)
	case !maps.Equal(optionTexts(a.Options), optionTexts(b.Options)):
		return fmt.Sprintf("which give %s different options, so it can't match both; give them the same options", a.Use)
	case usesDomain(a.Use) && a.domain() != b.domain():
		return fmt.Sprintf("which are in different domains (%s and %s), so it can't match both; give them one domain:", a.domain(), b.domain())
	}
	return ""
}

// usesDomain reports whether a transformer's outputs depend on the domain.
// An unknown transformer is taken to: the masking engine reports it.
func usesDomain(use string) bool {
	t, ok := mask.Lookup(use)
	return !ok || t.UsesDomain()
}

// optionTexts writes options as text, the way the lockfile records them.
func optionTexts(options map[string]any) map[string]string {
	if len(options) == 0 {
		return nil
	}
	out := make(map[string]string, len(options))
	for k, v := range options {
		out[k] = config.OptionText(v)
	}
	return out
}

// checkOwnRule refuses a rule on a foreign key: a foreign key is masked
// like the columns it references, so that its values still match theirs,
// and one rule in one place can't drift. A cycle of foreign keys, where no
// column has a root outside the cycle, is the exception: its first column
// with a rule decides it, and the others follow that column.
func (d *decider) checkOwnRule(dec *Decision) {
	ref := dec.Column
	if len(d.parents[ref]) == 0 {
		return
	}
	roots := d.roots(ref)
	if d.reaches(ref)[ref] && !slices.ContainsFunc(roots, func(r schema.ColumnRef) bool {
		return d.rules[r] == nil || !d.reaches(r)[ref] || r.String() < ref.String()
	}) {
		return // the first column of its cycle with a rule
	}
	target := d.parents[ref][0]
	if len(roots) > 0 {
		target = roots[0]
	}
	d.problem(dec.at, "%s references %s, so it is masked like it and can't have a rule of its own; remove this rule, and to change how both are masked, %s",
		ref, target, ruleAdvice(d.rules[target] != nil, target))
	dec.refused = true
}

// reaches returns the columns a column references, directly or through
// other foreign keys, whatever their rules. It holds the column itself when
// the column is part of a cycle.
func (d *decider) reaches(ref schema.ColumnRef) map[schema.ColumnRef]bool {
	out := map[schema.ColumnRef]bool{}
	queue := append([]schema.ColumnRef(nil), d.parents[ref]...)
	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]
		if out[next] {
			continue
		}
		out[next] = true
		queue = append(queue, d.parents[next]...)
	}
	return out
}

// ruleAdvice tells the user where to change how a column is masked: its
// rule in stuntdb.yaml, or a rule to write there.
func ruleAdvice(hasRule bool, ref schema.ColumnRef) string {
	if hasRule {
		return "change the rule of " + ref.String()
	}
	return "write a rule for " + ref.String()
}

// checkPer reports a per: that can't seed the column: per names a column
// the copy never reads, or the column is referenced by foreign keys, whose
// rows don't hold the per column.
func (d *decider) checkPer(dec *Decision) {
	per := schema.ColumnRef{Table: dec.Column.Table, Column: dec.Per}
	switch {
	case per == dec.Column:
		d.problem(dec.at, "%s: per names the column itself; it seeds a column from another column of its row", dec.Column)
	case d.columns[per].Generated != nil:
		d.problem(dec.at, "%s: per names %s, a generated column, which the copy never reads", dec.Column, per)
	case d.byColumn[per].Use == useDefault:
		d.problem(dec.at, "%s: per names %s, which is left to its default, so the copy never reads it", dec.Column, per)
	}
	if children := d.children[dec.Column]; len(children) > 0 {
		d.problem(dec.at, "%s: foreign keys reference it (%s), and their rows don't hold its per column, so it can't use per",
			dec.Column, refsText(children))
	}
}

// refsText lists columns for a message.
func refsText(refs []schema.ColumnRef) string {
	names := make([]string, len(refs))
	for i, r := range refs {
		names[i] = r.String()
	}
	return strings.Join(names, ", ")
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
		b.WriteString(", " + yamlScalar(k) + ": " + optionScalar(m.Options[k]))
	}
	b.WriteString(" }")
	return b.String()
}
