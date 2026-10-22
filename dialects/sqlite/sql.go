package sqlite

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/exarth-labs/stuntdb/dialect"
	"github.com/exarth-labs/stuntdb/internal/schema"
	"github.com/exarth-labs/stuntdb/internal/value"
)

// maxParams is SQLite's limit on bound parameters per statement
// (SQLITE_MAX_VARIABLE_NUMBER, 32,766 since SQLite 3.32).
const maxParams = 32766

// quote quotes an identifier: "name", with any " doubled.
func quote(name schema.Ident) string {
	return `"` + strings.ReplaceAll(string(name), `"`, `""`) + `"`
}

func quoteTable(ref schema.TableRef) string { return quote(ref.Namespace) + "." + quote(ref.Name) }

func quoteList(names []schema.Ident) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = quote(n)
	}
	return strings.Join(quoted, ", ")
}

// maxKeyValues is the most KeysIn values one read takes
// (Capabilities.MaxKeyValues). It leaves room under maxParams for a read's
// other parameters, such as an After key.
const maxKeyValues = 32000

// statement is one SQL statement and its arguments.
type statement struct {
	sql  string
	args []any
}

// renderRead turns q into one statement. A read with more than limit KeysIn
// values fails with ErrTooManyKeys: splitting it would break its OrderBy and
// Limit, so the caller sends such lists in batches.
//
// Each column is read as +"column": the unary plus changes nothing in the
// value, but hides the declared type from the driver, which would otherwise
// turn text in DATE, DATETIME and TIMESTAMP columns into time.Time and lose
// the original text.
func renderRead(q dialect.ReadQuery, limit int) (statement, error) {
	if err := q.Validate(); err != nil {
		return statement{}, err
	}
	if n := keyValues(q.Where); n > limit {
		return statement{}, fmt.Errorf("sqlite: read %s: %w: %d key values, at most %d", q.Table, dialect.ErrTooManyKeys, n, limit)
	}

	var b strings.Builder
	b.WriteString("SELECT ")
	for i, c := range q.Columns {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString("+" + quote(c))
	}
	b.WriteString(" FROM " + quoteTable(q.Table))

	var args []any
	if q.Where != nil {
		where, err := renderPredicate(q.Where, &args)
		if err != nil {
			return statement{}, fmt.Errorf("sqlite: read %s: %w", q.Table, err)
		}
		b.WriteString(" WHERE " + where)
	}
	if len(q.OrderBy) > 0 {
		b.WriteString(" ORDER BY " + quoteList(q.OrderBy))
	}
	if q.Limit > 0 {
		b.WriteString(" LIMIT " + strconv.Itoa(q.Limit))
	}
	return statement{sql: b.String(), args: args}, nil
}

// keyValues counts the KeysIn values in a condition.
func keyValues(p dialect.Predicate) int {
	switch p := p.(type) {
	case dialect.KeysIn:
		return len(p.Columns) * len(p.Keys)
	case dialect.And:
		n := 0
		for _, sub := range p {
			n += keyValues(sub)
		}
		return n
	default:
		return 0
	}
}

// withoutNullKeys drops keys that contain NULL: NULL never equals anything,
// so they could never match, and they would only use up parameters.
func withoutNullKeys(keys [][]value.Value) [][]value.Value {
	kept := make([][]value.Value, 0, len(keys))
	for _, key := range keys {
		if !slices.ContainsFunc(key, value.Value.IsNull) {
			kept = append(kept, key)
		}
	}
	return kept
}

func renderPredicate(p dialect.Predicate, args *[]any) (string, error) {
	switch p := p.(type) {
	case dialect.KeysIn:
		keys := withoutNullKeys(p.Keys)
		if len(keys) == 0 {
			return "0", nil // matches nothing
		}
		var b strings.Builder
		if len(p.Columns) == 1 {
			b.WriteString(quote(p.Columns[0]) + " IN (")
		} else {
			b.WriteString("(" + quoteList(p.Columns) + ") IN (VALUES ")
		}
		for i, key := range keys {
			if i > 0 {
				b.WriteString(", ")
			}
			if len(p.Columns) > 1 {
				b.WriteString("(")
			}
			if err := appendArgs(args, key); err != nil {
				return "", err
			}
			b.WriteString(placeholders(len(key)))
			if len(p.Columns) > 1 {
				b.WriteString(")")
			}
		}
		b.WriteString(")")
		return b.String(), nil
	case dialect.After:
		if err := appendArgs(args, p.Key); err != nil {
			return "", err
		}
		if len(p.Columns) == 1 {
			return quote(p.Columns[0]) + " > ?", nil
		}
		return "(" + quoteList(p.Columns) + ") > (" + placeholders(len(p.Key)) + ")", nil
	case dialect.Native:
		return "(" + p.SQL + ")", nil
	case dialect.And:
		if len(p) == 0 {
			return "1", nil
		}
		parts := make([]string, len(p))
		for i, sub := range p {
			s, err := renderPredicate(sub, args)
			if err != nil {
				return "", err
			}
			parts[i] = s
		}
		if len(parts) == 1 {
			return parts[0], nil
		}
		return "(" + strings.Join(parts, ") AND (") + ")", nil
	default:
		return "", fmt.Errorf("unknown condition %T", p)
	}
}

func appendArgs(args *[]any, key []value.Value) error {
	for _, v := range key {
		a, err := toStorage(v)
		if err != nil {
			return err
		}
		*args = append(*args, a)
	}
	return nil
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}

// insertSQL is a multi-row INSERT of rows rows into table.
func insertSQL(table schema.TableRef, columns []schema.Ident, rows int) string {
	var b strings.Builder
	b.WriteString("INSERT INTO " + quoteTable(table) + " (" + quoteList(columns) + ") VALUES ")
	row := "(" + placeholders(len(columns)) + ")"
	for i := range rows {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(row)
	}
	return b.String()
}

// createTableSQL builds a CREATE TABLE statement from the canonical model,
// for tables that don't come from a SQLite source. It covers columns, NOT
// NULL, keys and foreign keys. Defaults, checks and generated columns are
// written in their source engine's own SQL, so they wait for cross-engine
// type mapping (v0.2).
func createTableSQL(t *schema.Table) (string, error) {
	var parts []string
	for _, c := range t.Columns {
		if c.Generated != nil {
			return "", fmt.Errorf("sqlite: generated column %q of %s cannot be created from another engine yet", c.Name, t.Name)
		}
		typ, err := sqlType(c.Type)
		if err != nil {
			return "", fmt.Errorf("column %q of %s: %w", c.Name, t.Name, err)
		}
		def := quote(c.Name) + " " + typ
		if !c.Nullable {
			def += " NOT NULL"
		}
		parts = append(parts, def)
	}
	if t.PrimaryKey != nil {
		parts = append(parts, "PRIMARY KEY ("+quoteList(t.PrimaryKey.Columns)+")")
	}
	for _, k := range t.Uniques {
		parts = append(parts, "UNIQUE ("+quoteList(k.Columns)+")")
	}
	for _, fk := range t.ForeignKeys {
		if fk.Virtual {
			continue
		}
		parts = append(parts, fmt.Sprintf("FOREIGN KEY (%s) REFERENCES %s (%s) ON DELETE %s ON UPDATE %s",
			quoteList(fk.Columns), quote(fk.Ref.Name), quoteList(fk.RefColumns), fk.OnDelete, fk.OnUpdate))
	}
	return "CREATE TABLE " + quote(t.Name) + " (\n  " + strings.Join(parts, ",\n  ") + "\n)", nil
}

// createIndexSQL builds a CREATE INDEX statement from the canonical model.
// Expression and partial indexes are written in their source engine's own
// SQL, so they wait for cross-engine support (v0.2).
func createIndexSQL(t *schema.Table, ix *schema.Index) (string, error) {
	if ix.Expression || ix.Partial {
		return "", fmt.Errorf("sqlite: expression or partial index %q of %s cannot be created from another engine yet", ix.Name, t.Name)
	}
	name := ix.Name
	if name == "" {
		name = schema.Ident(string(t.Name) + "_" + joinIdents(ix.Columns, "_") + "_idx")
	}
	unique := ""
	if ix.Unique {
		unique = "UNIQUE "
	}
	return "CREATE " + unique + "INDEX " + quote(name) + " ON " + quote(t.Name) + " (" + quoteList(ix.Columns) + ")", nil
}

func joinIdents(names []schema.Ident, sep string) string {
	s := make([]string, len(names))
	for i, n := range names {
		s[i] = string(n)
	}
	return strings.Join(s, sep)
}
