package schema

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
)

// Fingerprint returns a hash, "sha256:…", of the structure that matters for
// masking: namespaces, tables, columns with their types, nullability and
// whether they are generated, keys, unique indexes and foreign keys. It
// ignores the order of tables and columns, native DDL, index names, indexes
// that aren't unique, statistics and other objects, so that it changes only
// when what is copied, or how it is constrained, changes. The lockfile
// records it, to detect drift.
func (d *Database) Fingerprint() string {
	var lines []string
	add := func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }
	for _, ns := range d.Namespaces {
		add("namespace %q", ns.Name)
		for _, t := range ns.Tables {
			table := fmt.Sprintf("%q.%q", ns.Name, t.Name)
			add("table %s", table)
			for _, c := range t.Columns {
				add("column %s.%q type=%q native=%q nullable=%t generated=%t", table, c.Name, c.Type.String(), c.NativeType, c.Nullable, c.Generated != nil)
			}
			if t.PrimaryKey != nil {
				add("primary key %s %s", table, quotedList(t.PrimaryKey.Columns))
			}
			for _, k := range t.Uniques {
				add("unique %s %s", table, quotedList(k.Columns))
			}
			// Masking keeps a unique index's columns distinct, which changes
			// their fake values, as a unique constraint does.
			for _, ix := range t.Indexes {
				if ix.Unique {
					add("unique index %s %s partial=%t expression=%t", table, quotedList(ix.Columns), ix.Partial, ix.Expression)
				}
			}
			for _, fk := range t.ForeignKeys {
				add("foreign key %s %s -> %q.%q %s virtual=%t", table, quotedList(fk.Columns), fk.Ref.Namespace, fk.Ref.Name, quotedList(fk.RefColumns), fk.Virtual)
			}
		}
	}
	slices.Sort(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func quotedList(names []Ident) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = fmt.Sprintf("%q", n)
	}
	return "(" + strings.Join(quoted, ",") + ")"
}
