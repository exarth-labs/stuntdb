package app

import (
	"fmt"
	"strings"

	"github.com/exarth-labs/stuntdb/dialect"
	"github.com/exarth-labs/stuntdb/internal/config"
	"github.com/exarth-labs/stuntdb/internal/schema"
)

// resolveRules finds the column each rule of the configuration names, with
// the database's rules for upper and lower case. A rule for a table or
// column that does not exist, or that two rules name, is a problem.
func resolveRules(src *source, tables []*config.TableRules) (map[schema.ColumnRef]*config.ColumnRule, []Problem) {
	rules := map[schema.ColumnRef]*config.ColumnRule{}
	var problems []Problem
	add := func(at config.Position, format string, args ...any) {
		problems = append(problems, Problem{At: at.String(), Message: fmt.Sprintf(format, args...)})
	}
	seenTables := map[schema.TableRef]config.Position{}
	for _, tr := range tables {
		ref, table, err := findTable(src, tr.Table)
		if err != nil {
			add(tr.At, "%v", err)
			continue
		}
		if first, dup := seenTables[ref]; dup {
			add(tr.At, "%s already has rules, at %s", ref, first)
			continue
		}
		seenTables[ref] = tr.At

		for _, rule := range tr.Columns {
			name, err := findColumn(src.caps.ColumnNames, table, rule.Column)
			if err != nil {
				add(rule.At, "%s: %v", ref, err)
				continue
			}
			col := schema.ColumnRef{Table: ref, Column: name}
			if first, dup := rules[col]; dup {
				add(rule.At, "%s already has a rule, at %s", col, first.At)
				continue
			}
			rules[col] = rule
		}
	}
	return rules, problems
}

// findTable finds the table a configuration names. A name without a
// namespace may be in any namespace, but only in one.
func findTable(src *source, name config.TableName) (schema.TableRef, *schema.Table, error) {
	nsNames := make([]schema.Ident, len(src.db.Namespaces))
	for i, ns := range src.db.Namespaces {
		nsNames[i] = ns.Name
	}
	namespaces := src.db.Namespaces
	if name.Namespace != "" {
		namespaces = nil
		for _, i := range matching(src.caps.TableNames, nsNames, name.Namespace) {
			namespaces = append(namespaces, src.db.Namespaces[i])
		}
	}

	type candidate struct {
		ref   schema.TableRef
		table *schema.Table
		exact bool
	}
	var found []candidate
	for _, ns := range namespaces {
		tableNames := make([]schema.Ident, len(ns.Tables))
		for i, t := range ns.Tables {
			tableNames[i] = t.Name
		}
		for _, i := range matching(src.caps.TableNames, tableNames, name.Name) {
			t := ns.Tables[i]
			found = append(found, candidate{schema.TableRef{Namespace: ns.Name, Name: t.Name}, t, string(t.Name) == name.Name})
		}
	}
	// The exact spelling wins over names that only match after case folding.
	var exact []candidate
	for _, c := range found {
		if c.exact {
			exact = append(exact, c)
		}
	}
	if len(exact) > 0 {
		found = exact
	}

	switch len(found) {
	case 0:
		return schema.TableRef{}, nil, fmt.Errorf("table %s is not in the database", name)
	case 1:
		return found[0].ref, found[0].table, nil
	default:
		refs := make([]string, len(found))
		for i, c := range found {
			refs[i] = c.ref.String()
		}
		return schema.TableRef{}, nil, fmt.Errorf("table %s could be any of %s; write namespace.table", name, strings.Join(refs, ", "))
	}
}

// findColumn finds the column of a table that a configuration names.
func findColumn(c dialect.IdentifierCase, t *schema.Table, name string) (schema.Ident, error) {
	names := make([]schema.Ident, len(t.Columns))
	for i, col := range t.Columns {
		names[i] = col.Name
	}
	switch found := matching(c, names, name); len(found) {
	case 0:
		return "", fmt.Errorf("column %q is not in the table", name)
	case 1:
		return names[found[0]], nil
	default:
		return "", fmt.Errorf("column %q matches several columns that differ only in case; write the exact name", name)
	}
}

// matching returns the positions of the names that a name, written without
// quotes, refers to. The exact spelling wins over matches that only fold
// case, as in PostgreSQL, where "Users" and users can both exist.
func matching(c dialect.IdentifierCase, names []schema.Ident, written string) []int {
	var exact, folded []int
	for i, n := range names {
		switch {
		case string(n) == written:
			exact = append(exact, i)
		case c.Match(written, n):
			folded = append(folded, i)
		}
	}
	if len(exact) > 0 {
		return exact
	}
	return folded
}
