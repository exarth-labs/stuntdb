package schema

import (
	"errors"
	"fmt"
)

// Validate reports every structural problem in d: missing or duplicate
// names, invalid column types, and keys, indexes or foreign keys that refer
// to columns or tables that do not exist. Adapters call it after
// introspection, so that a bug shows up as a clear error instead of a bad copy.
func (d *Database) Validate() error {
	var problems []error
	add := func(format string, args ...any) {
		problems = append(problems, fmt.Errorf(format, args...))
	}

	namespaces := make(map[Ident]bool, len(d.Namespaces))
	for _, ns := range d.Namespaces {
		if namespaces[ns.Name] {
			add("namespace %q appears twice", ns.Name)
		}
		namespaces[ns.Name] = true

		tables := make(map[Ident]bool, len(ns.Tables))
		for _, t := range ns.Tables {
			ref := TableRef{ns.Name, t.Name}
			switch {
			case t.Name == "":
				add("namespace %q has a table without a name", ns.Name)
			case tables[t.Name]:
				add("table %s appears twice", ref)
			}
			tables[t.Name] = true
			d.validateTable(ref, t, add)
		}
	}
	return errors.Join(problems...)
}

func (d *Database) validateTable(ref TableRef, t *Table, add func(string, ...any)) {
	columns := make(map[Ident]bool, len(t.Columns))
	for _, c := range t.Columns {
		switch {
		case c.Name == "":
			add("table %s has a column without a name", ref)
		case columns[c.Name]:
			add("column %q of %s appears twice", c.Name, ref)
		}
		columns[c.Name] = true
		if err := c.Type.Validate(); err != nil {
			add("column %q of %s: %v", c.Name, ref, err)
		}
		if c.Generated != nil && c.Identity != nil {
			add("column %q of %s cannot be both generated and an identity", c.Name, ref)
		}
	}

	checkColumns := func(what string, names []Ident) {
		if len(names) == 0 {
			add("%s of %s has no columns", what, ref)
		}
		for _, name := range names {
			if !columns[name] {
				add("%s of %s uses column %q, which does not exist", what, ref, name)
			}
		}
	}
	if t.PrimaryKey != nil {
		checkColumns("the primary key", t.PrimaryKey.Columns)
	}
	for _, k := range t.Uniques {
		checkColumns(fmt.Sprintf("unique constraint %q", k.Name), k.Columns)
	}
	for _, ix := range t.Indexes {
		if !ix.Expression || len(ix.Columns) > 0 {
			checkColumns(fmt.Sprintf("index %q", ix.Name), ix.Columns)
		}
	}
	for _, fk := range t.ForeignKeys {
		what := fmt.Sprintf("foreign key %q", fk.Name)
		checkColumns(what, fk.Columns)
		if len(fk.RefColumns) != len(fk.Columns) {
			add("%s of %s has %d columns but references %d", what, ref, len(fk.Columns), len(fk.RefColumns))
		}
		target := d.Table(fk.Ref)
		if target == nil {
			add("%s of %s references table %s, which does not exist", what, ref, fk.Ref)
			continue
		}
		for _, name := range fk.RefColumns {
			if target.Column(name) == nil {
				add("%s of %s references column %q of %s, which does not exist", what, ref, name, fk.Ref)
			}
		}
	}
}
