package mask

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/exarth-labs/stuntdb/internal/schema"
	"github.com/exarth-labs/stuntdb/internal/value"
)

// ColumnRule is a column that a copy writes, with its rule.
type ColumnRule struct {
	Column Column
	Rule   Rule
}

// ResolveDomains checks the consistency domains of a whole copy, and sets
// each rule's Distinct. cols holds every column the copy writes, with its
// rule, after foreign keys have taken their root's rule and domain.
//
// A value column is one without Per: it masks its own value, and its
// domain makes equal values equal across columns. A Per column masks a
// part of the entity its key names, and is checked with the other parts.
//
//   - Distinct is set on every value column of a domain that holds a
//     unique value column, and cleared on every other column. A Per column
//     neither gets nor gives it: it can't be unique, and nothing may
//     reference it, but by default it shares its key's domain.
//   - The value columns of a domain use the same transformer with the same
//     options, after their defaults are filled in, in column types the
//     transformer masks alike (Transformer.MasksAlike). That transformer
//     isn't default, which gives each column its own database default.
//   - The columns of a per group (the same Per key, domain and transformer)
//     use the same options, except the transformer's entity options, such
//     as address's part.
//
// Lengths are never taken from the columns: a domain's values would then
// change whenever one of its columns did. A column that can't hold them is
// for Configure to refuse.
//
// It returns the rules in the order of cols, even when it reports problems,
// and every problem at once, sorted. Rules that Configure would refuse
// (unknown transformers, types they don't take, bad options) are left out
// of the comparisons, so that one mistake is reported once. It needs no
// key, and its errors name domains, columns, transformers and options,
// never a value, not even an option's.
func ResolveDomains(cols []ColumnRule) ([]Rule, error) {
	rules := make([]Rule, len(cols))
	values := map[string][]int{}
	groups := map[perKey][]int{}
	seen := map[schema.ColumnRef]bool{}
	var problems []string
	for i, c := range cols {
		ref := c.Column.Ref
		if seen[ref] {
			problems = append(problems, fmt.Sprintf("%s has more than one rule", ref))
		}
		seen[ref] = true
		r := c.Rule
		r.Distinct = false
		rules[i] = r
		domain := r.DomainFor(ref)
		if r.Per == "" {
			values[domain] = append(values[domain], i)
			continue
		}
		k := perKey{key: schema.ColumnRef{Table: ref.Table, Column: r.Per}, domain: domain, use: r.Use}
		groups[k] = append(groups[k], i)
	}
	for domain, members := range values {
		distinct := slices.ContainsFunc(members, func(i int) bool { return cols[i].Column.Unique })
		for _, i := range members {
			rules[i].Distinct = distinct
		}
		if p := checkDomain(domain, cols, members, distinct); p != "" {
			problems = append(problems, p)
		}
	}
	for k, members := range groups {
		if p := checkPerGroup(k, cols, members); p != "" {
			problems = append(problems, p)
		}
	}
	slices.Sort(problems)
	errs := make([]error, len(problems))
	for i, p := range problems {
		errs[i] = errors.New(p)
	}
	return rules, errors.Join(errs...)
}

// perKey identifies a per group: the columns seeded from one key column
// with one transformer in one domain.
type perKey struct {
	key    schema.ColumnRef
	domain string
	use    string
}

// An OptionScope holds the columns whose rules must give an option one
// value (ResolveDomains): a domain's value columns, or a per group's,
// where an entity option is each column's own.
type OptionScope struct {
	group  perKey
	column schema.ColumnRef // set for an entity option
	option string
}

// Scope returns the scope of one of the column's options.
func (c ColumnRule) Scope(option string) OptionScope {
	r := c.Rule
	s := OptionScope{group: perKey{domain: r.DomainFor(c.Column.Ref)}, option: option}
	if r.Per == "" {
		return s
	}
	s.group.key = schema.ColumnRef{Table: c.Column.Ref.Table, Column: r.Per}
	s.group.use = r.Use
	if t, ok := Lookup(r.Use); ok && slices.Contains(t.entityOptions, option) {
		s.column = c.Column.Ref
	}
	return s
}

// usable returns the transformer of a column's rule, when it exists and
// takes the column's type. Otherwise Configure reports the column.
func usable(c ColumnRule) (*Transformer, bool) {
	t, ok := Lookup(c.Rule.Use)
	if !ok || !t.Accepts(c.Column.Type.Kind) {
		return nil, false
	}
	return t, true
}

// checkDomain checks that the value columns of a domain mask equal values
// alike, and returns the problem, or "". It reports the first of three
// things that differ, since the later ones follow from it: the
// transformers, the classes of the column types, the options.
func checkDomain(domain string, cols []ColumnRule, members []int, distinct bool) string {
	byUse := map[string][]schema.ColumnRef{}
	var ok []int
	for _, i := range members {
		if _, found := usable(cols[i]); found {
			ok = append(ok, i)
			byUse[cols[i].Rule.Use] = append(byUse[cols[i].Rule.Use], cols[i].Column.Ref)
		}
	}
	if len(byUse) > 1 {
		var uses []string
		for _, use := range slices.Sorted(maps.Keys(byUse)) {
			uses = append(uses, fmt.Sprintf("%s (%s)", use, refList(byUse[use])))
		}
		return fmt.Sprintf("domain %s: its columns must mask equal values alike, but they use different transformers: %s; "+
			"give them one transformer, or separate domains", domain, strings.Join(uses, ", "))
	}
	if len(ok) < 2 {
		return ""
	}
	t, _ := usable(cols[ok[0]])
	if t.class(cols[ok[0]].Column.Type) == noClass {
		return fmt.Sprintf("domain %s: %s gives each column a value of its own, not one from the input, so equal values "+
			"can't stay equal in %s; give them another transformer, or separate domains", domain, t.Name, refList(refsOf(cols, ok)))
	}

	byClass := map[string][]int{}
	for _, i := range ok {
		c := t.class(cols[i].Column.Type)
		byClass[c] = append(byClass[c], i)
	}
	if len(byClass) > 1 {
		var parts []string
		var types []value.Type
		for _, members := range byClass {
			parts = append(parts, typesAndColumns(cols, members))
			for _, i := range members {
				types = append(types, cols[i].Column.Type)
			}
		}
		slices.Sort(parts)
		return fmt.Sprintf("domain %s: %s masks equal values differently in %s, so they can't share a domain; %s",
			domain, t.Name, strings.Join(parts, " and "), alikeAlternatives(types, distinct, t.Name))
	}

	if diff := optionDiffs(t, cols, ok, distinct, nil); diff != "" {
		return fmt.Sprintf("domain %s: its columns must mask equal values alike, but they give %s different options "+
			"(an option left out counts as its default): %s", domain, t.Name, diff)
	}
	return ""
}

// checkPerGroup checks that the columns seeded from one key with one
// transformer agree on their options, so that, for example, all of one
// person's dates move by the same number of days. It returns the problem,
// or "".
func checkPerGroup(k perKey, cols []ColumnRule, members []int) string {
	var ok []int
	for _, i := range members {
		if _, found := usable(cols[i]); found {
			ok = append(ok, i)
		}
	}
	if len(ok) < 2 {
		return ""
	}
	t, _ := usable(cols[ok[0]])
	diff := optionDiffs(t, cols, ok, false, t.entityOptions)
	if diff == "" {
		return ""
	}
	return fmt.Sprintf("domain %s: the columns seeded per %s with %s describe one entity, so they must use the same options, "+
		"but these differ (an option left out counts as its default): %s; give them the same options, or a domain of their own",
		k.domain, k.key, t.Name, diff)
}

// optionDiffs builds every member's options with t and returns the options
// they use with different values, each with the groups of columns that
// agree, or "" when they all agree. An option that only some members read
// can't make their outputs differ, so it is compared among those. Options
// in except, and members whose options don't build, are left out. Values
// are compared, never written.
func optionDiffs(t *Transformer, cols []ColumnRule, members []int, distinct bool, except []string) string {
	byOption := map[string]map[string][]schema.ColumnRef{}
	for _, i := range members {
		c := cols[i]
		_, resolved, err := t.configure(c.Column.Type.Kind, c.Rule.Options, distinct)
		if err != nil {
			continue
		}
		for name, v := range resolved {
			if slices.Contains(except, name) {
				continue
			}
			if byOption[name] == nil {
				byOption[name] = map[string][]schema.ColumnRef{}
			}
			byOption[name][v] = append(byOption[name][v], c.Column.Ref)
		}
	}
	var diffs []string
	for _, name := range slices.Sorted(maps.Keys(byOption)) {
		byValue := byOption[name]
		if len(byValue) < 2 {
			continue
		}
		var agree []string
		for _, refs := range byValue {
			agree = append(agree, refList(refs))
		}
		slices.Sort(agree)
		diffs = append(diffs, fmt.Sprintf("%s (%s)", name, strings.Join(agree, " against ")))
	}
	return strings.Join(diffs, "; ")
}

// typesAndColumns describes some columns of a domain for an error: their
// types, then the columns.
func typesAndColumns(cols []ColumnRule, members []int) string {
	var types []string
	var refs []schema.ColumnRef
	for _, i := range members {
		if s := cols[i].Column.Type.String(); !slices.Contains(types, s) {
			types = append(types, s)
		}
		refs = append(refs, cols[i].Column.Ref)
	}
	slices.Sort(types)
	return fmt.Sprintf("%s (%s)", strings.Join(types, ", "), refList(refs))
}

// refsOf returns the columns of some members.
func refsOf(cols []ColumnRule, members []int) []schema.ColumnRef {
	refs := make([]schema.ColumnRef, len(members))
	for i, m := range members {
		refs[i] = cols[m].Column.Ref
	}
	return refs
}

// refList lists columns, sorted.
func refList(refs []schema.ColumnRef) string {
	names := make([]string, len(refs))
	for i, r := range refs {
		names[i] = r.String()
	}
	slices.Sort(names)
	return strings.Join(names, ", ")
}

// alikeAlternatives tells the user, for an error message, which other
// transformers mask equal values alike in all the given column types: those
// that take them all with their default options, and keep values distinct
// where the domain needs it. It leaves out except, the transformer just
// refused, and those that mask nothing: keep and null, and default, which
// gives each column its own default.
func alikeAlternatives(types []value.Type, distinct bool, except string) string {
	var names []string
	for _, t := range Catalog() {
		if t.Name == except || t.Name == "keep" || t.Name == "null" || t.Name == "default" || !fitsAlike(t, types, distinct) {
			continue
		}
		names = append(names, t.Name)
	}
	if len(names) == 0 {
		return "no transformer masks them alike, so give them one type, or separate domains"
	}
	return "these transformers mask them alike: " + strings.Join(names, ", ")
}

// fitsAlike reports whether t masks equal values alike in every type, with
// its default options, and keeps them distinct when distinct is set.
func fitsAlike(t *Transformer, types []value.Type, distinct bool) bool {
	for _, typ := range types {
		if !t.MasksAlike(types[0], typ) {
			return false
		}
		f, _, err := t.configure(typ.Kind, nil, distinct)
		if err != nil || (distinct && f.guarantee == None) {
			return false
		}
	}
	return true
}
