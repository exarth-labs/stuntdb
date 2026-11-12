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

	short := map[tooShortKey]*tooShortGroup{}
	for i, dec := range decs {
		_, err := mask.Configure(cols[i].Column, rules[i])
		if err == nil {
			continue
		}
		var tooShort *mask.TooShortError
		if errors.As(err, &tooShort) && tooShort.Option != "" {
			k := tooShortKey{owner: ruleOwner(dec), domain: rules[i].DomainFor(dec.Column), option: tooShort.Option}
			g := short[k]
			if g == nil {
				g = &tooShortGroup{dec: dec, fits: tooShort.Fits, err: err}
				short[k] = g
			}
			g.columns = append(g.columns, dec.Column)
			g.fits = min(g.fits, tooShort.Fits)
			continue
		}
		problems = append(problems, refusal(dec, byColumn, err))
	}

	keys := make([]tooShortKey, 0, len(short))
	for k := range short {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b tooShortKey) int {
		return cmp.Or(cmp.Compare(a.owner.String(), b.owner.String()), cmp.Compare(a.domain, b.domain), cmp.Compare(a.option, b.option))
	})
	for _, k := range keys {
		problems = append(problems, short[k].problem(k, byColumn))
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

// tooShortKey groups the columns too short for one rule's values: the column
// whose rule decides them, their domain, and the option that bounds the
// values. The columns of one domain share their options, so one value of
// the option must fit them all.
type tooShortKey struct {
	owner  schema.ColumnRef
	domain string
	option string
}

type tooShortGroup struct {
	dec     *Decision // the first column of the group
	columns []schema.ColumnRef
	fits    int   // the largest value of the option that fits every column
	err     error // the first column's refusal
}

// problem reports a group: one column decided by its own rule as the
// masking engine says it, and otherwise with the value that fits them all.
func (g *tooShortGroup) problem(k tooShortKey, byColumn map[schema.ColumnRef]*Decision) Problem {
	if len(g.columns) == 1 && ruleOwner(g.dec) == g.dec.Column {
		return refusal(g.dec, byColumn, g.err)
	}
	root := byColumn[k.owner]
	what := fmt.Sprintf("%s is too short", g.columns[0])
	if len(g.columns) > 1 {
		what = fmt.Sprintf("%s are too short", refsText(g.columns))
	}
	if root.Reason == Configured {
		return Problem{At: root.at, Message: fmt.Sprintf("%s for the values of %s's rule (%s); set %s: %d, which fits every column it masks",
			what, k.owner, root.Use, k.option, g.fits)}
	}
	return Problem{Message: fmt.Sprintf("%s for the values of %s (%s, decided by %s); write a rule for %s with %s: %d, which fits every column it masks",
		what, k.owner, root.Use, root.Source, k.owner, k.option, g.fits)}
}
