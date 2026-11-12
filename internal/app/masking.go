package app

import (
	"cmp"
	"errors"
	"fmt"
	"slices"

	"github.com/exarth-labs/stuntdb/internal/mask"
	"github.com/exarth-labs/stuntdb/internal/schema"
)

// checkMasking runs every decided column through the masking engine,
// without a masking key: the domains of the whole database (which also
// decide where values must stay distinct), then each column's transformer,
// options, type, uniqueness and length. A problem names the rule to change:
// a foreign key's is its root's.
func checkMasking(src *source, decisions []*Decision) []Problem {
	columns := describeColumns(src)
	byColumn := map[schema.ColumnRef]*Decision{}
	var decs []*Decision
	var cols []mask.ColumnRule
	for _, dec := range decisions {
		byColumn[dec.Column] = dec
		if dec.masked() {
			decs = append(decs, dec)
			cols = append(cols, mask.ColumnRule{Column: columns[dec.Column], Rule: dec.maskRule()})
		}
	}

	var problems []Problem
	rules, err := mask.ResolveDomains(cols)
	for _, e := range unjoin(err) {
		problems = append(problems, Problem{Message: e.Error()})
	}

	// One value of the option must fit every column of its scope.
	short := map[mask.OptionScope]*tooShortGroup{}
	var groups []*tooShortGroup
	for i, dec := range decs {
		_, err := mask.Configure(cols[i].Column, rules[i])
		if err == nil {
			continue
		}
		var tooShort *mask.TooShortError
		if errors.As(err, &tooShort) && tooShort.Option != "" {
			scope := mask.ColumnRule{Column: cols[i].Column, Rule: rules[i]}.Scope(tooShort.Option)
			g := short[scope]
			if g == nil {
				g = &tooShortGroup{first: dec, err: err, option: tooShort.Option, fits: tooShort.Fits}
				short[scope] = g
				groups = append(groups, g)
			}
			g.add(dec, tooShort.Fits)
			continue
		}
		problems = append(problems, refusal(dec, byColumn, err))
	}

	for _, g := range groups {
		slices.SortFunc(g.owners, func(a, b schema.ColumnRef) int { return cmp.Compare(a.String(), b.String()) })
	}
	slices.SortFunc(groups, func(a, b *tooShortGroup) int {
		return cmp.Or(cmp.Compare(a.owners[0].String(), b.owners[0].String()), cmp.Compare(a.columns[0].String(), b.columns[0].String()))
	})
	for _, g := range groups {
		problems = append(problems, g.problems(byColumn)...)
	}
	return problems
}

// describeColumns describes every column of the database for the masking
// engine, with the engine's rules.
func describeColumns(src *source) map[schema.ColumnRef]mask.Column {
	out := map[schema.ColumnRef]mask.Column{}
	for _, ns := range src.db.Namespaces {
		for _, t := range ns.Tables {
			for _, c := range mask.Describe(schema.TableRef{Namespace: ns.Name, Name: t.Name}, t, src.caps) {
				out[c.Ref] = c
			}
		}
	}
	return out
}

// unjoin returns the errors that errors.Join joined, one by one.
func unjoin(err error) []error {
	if err == nil {
		return nil
	}
	if j, ok := err.(interface{ Unwrap() []error }); ok { // the errors one errors.Join joined, not a chain to search
		return j.Unwrap()
	}
	return []error{err}
}

// ruleOwner returns the column whose rule decides a column: its own, or a
// foreign key's root, which follow always records.
func ruleOwner(dec *Decision) schema.ColumnRef {
	if dec.Reason == Reference {
		return *dec.Follows
	}
	return dec.Column
}

// refusal turns the masking engine's refusal of a column into a problem
// at the rule to change.
func refusal(dec *Decision, byColumn map[schema.ColumnRef]*Decision, err error) Problem {
	switch dec.Reason {
	case Configured:
		return Problem{At: dec.at, Message: fmt.Sprintf("%s: %v", dec.Column, err)}
	case Reference:
		root := byColumn[ruleOwner(dec)]
		return Problem{At: dec.at, Message: fmt.Sprintf("%s: %v; it follows %s, so %s", dec.Column, err, root.Column, ruleAdvice(root.Reason == Configured, root.Column))}
	default:
		return Problem{Message: fmt.Sprintf("%s: %v; it is decided by %s, so write a rule for it", dec.Column, err, dec.Source)}
	}
}

// tooShortGroup holds the too-short columns of one mask.OptionScope.
type tooShortGroup struct {
	first   *Decision
	err     error // first's refusal
	option  string
	fits    int // the option's largest value that fits every column
	columns []schema.ColumnRef
	owners  []schema.ColumnRef // the columns whose rules decide them (ruleOwner)
}

func (g *tooShortGroup) add(dec *Decision, fits int) {
	g.columns = append(g.columns, dec.Column)
	g.fits = min(g.fits, fits)
	if o := ruleOwner(dec); !slices.Contains(g.owners, o) {
		g.owners = append(g.owners, o)
	}
}

// problems reports the group at each rule to change. A lone column with its
// own rule gets the engine's message.
func (g *tooShortGroup) problems(byColumn map[schema.ColumnRef]*Decision) []Problem {
	if len(g.columns) == 1 && g.owners[0] == g.columns[0] {
		return []Problem{refusal(g.first, byColumn, g.err)}
	}
	what := fmt.Sprintf("%s is too short", g.columns[0])
	if len(g.columns) > 1 {
		what = fmt.Sprintf("%s are too short", refsText(g.columns))
	}
	out := make([]Problem, len(g.owners))
	for i, owner := range g.owners {
		set := fmt.Sprintf("%s: %d, which fits every column it masks", g.option, g.fits)
		if others := slices.DeleteFunc(slices.Clone(g.owners), func(o schema.ColumnRef) bool { return o == owner }); len(others) > 0 {
			whose := "whose rule must"
			if len(others) > 1 {
				whose = "whose rules must"
			}
			set = fmt.Sprintf("%s: %d, and the same for %s, %s agree with it: that value fits every column they mask",
				g.option, g.fits, refsText(others), whose)
		}
		root := byColumn[owner]
		if root.Reason == Configured {
			out[i] = Problem{At: root.at, Message: fmt.Sprintf("%s for the values of %s's rule (%s); set %s", what, owner, root.Use, set)}
			continue
		}
		out[i] = Problem{Message: fmt.Sprintf("%s for the values of %s (%s, decided by %s); write a rule for %s with %s",
			what, owner, root.Use, root.Source, owner, set)}
	}
	return out
}
