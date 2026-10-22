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
// whether they are generated, keys and foreign keys. It ignores the order of
// tables and columns, native DDL, indexes, statistics and other objects, so
// that it changes only when what is copied, or how it is constrained,
// changes. The lockfile records it, to detect drift.
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
