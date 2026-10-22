package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/exarth-labs/stuntdb/internal/schema"
)

// introspect reads the structure of the main database, inside one read
// transaction so that it is consistent.
//
// SQLite's catalog pragmas don't report check constraints, collations,
// generated-column expressions or deferrable foreign keys, so the canonical
// model leaves them out. Same-engine copies don't need them, because they
// replay each table's own CREATE TABLE statement, which has everything.
func introspect(ctx context.Context, db *sql.DB, version string, logger *slog.Logger) (*schema.Database, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("sqlite: introspect: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	r := &reader{ctx: ctx, tx: tx, logger: logger}
	ns, err := r.namespace()
	if err != nil {
		return nil, fmt.Errorf("sqlite: introspect: %w", err)
	}
	out := &schema.Database{Engine: "sqlite", Version: version, Namespaces: []*schema.Namespace{ns}}
	if err := out.Validate(); err != nil {
		return nil, fmt.Errorf("sqlite: introspect: %w", err)
	}
	return out, nil
}

type reader struct {
	ctx    context.Context
	tx     *sql.Tx
	logger *slog.Logger
}

// catalogEntry is one row of sqlite_schema.
type catalogEntry struct {
	kind, name, table, sql string
}

func (r *reader) namespace() (*schema.Namespace, error) {
	entries, err := r.catalog()
	if err != nil {
		return nil, err
	}
	kinds, err := r.tableKinds()
	if err != nil {
		return nil, err
	}
	hasStats, err := r.tableExists("sqlite_stat1")
	if err != nil {
		return nil, err
	}

	ns := &schema.Namespace{Name: mainNamespace}
	var indexes []catalogEntry
	for _, e := range entries {
		switch {
		case e.kind == "index":
			indexes = append(indexes, e)
		case e.kind == "view":
			ns.Objects = append(ns.Objects, &schema.Object{Kind: schema.ObjectView, Name: schema.Ident(e.name), Native: e.sql})
		case e.kind == "trigger":
			ns.Objects = append(ns.Objects, &schema.Object{Kind: schema.ObjectTrigger, Name: schema.Ident(e.name), Native: e.sql})
		case e.kind == "table" && kinds[e.name].kind == "virtual":
			// Virtual tables (full-text search and the like) are recreated
			// empty: their contents are a module's own storage.
			r.logger.Warn("sqlite: a virtual table is recreated without its contents", "table", e.name)
			ns.Objects = append(ns.Objects, &schema.Object{Kind: schema.ObjectOther, Name: schema.Ident(e.name), Native: e.sql})
		case e.kind == "table" && kinds[e.name].kind == "table" && !strings.HasPrefix(strings.ToLower(e.name), "sqlite_"):
			t, err := r.table(e.name, e.sql, kinds[e.name].withoutRowid)
			if err != nil {
				return nil, err
			}
			if hasStats {
				if t.EstimatedRows, err = r.estimatedRows(e.name); err != nil {
					return nil, err
				}
			}
			ns.Tables = append(ns.Tables, t)
		}
		// Shadow tables of virtual tables, and SQLite's own tables, are skipped.
	}
	for _, t := range ns.Tables {
		if err := r.indexes(t, indexes); err != nil {
			return nil, err
		}
	}
	for _, t := range ns.Tables {
		if err := r.foreignKeys(ns, t); err != nil {
			return nil, err
		}
	}
	return ns, nil
}

func (r *reader) catalog() ([]catalogEntry, error) {
	// rowid order is the order the objects were created in, so views and
	// triggers can be recreated in an order that works.
	rows, err := r.tx.QueryContext(r.ctx, `SELECT type, name, tbl_name, coalesce(sql, '') FROM main.sqlite_schema ORDER BY rowid`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var entries []catalogEntry
	for rows.Next() {
		var e catalogEntry
		if err := rows.Scan(&e.kind, &e.name, &e.table, &e.sql); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

type tableKind struct {
	kind         string // "table", "view", "shadow" or "virtual"
	withoutRowid bool
}

func (r *reader) tableKinds() (map[string]tableKind, error) {
	rows, err := r.tx.QueryContext(r.ctx, `SELECT name, type, wr FROM pragma_table_list WHERE schema = 'main'`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	kinds := map[string]tableKind{}
	for rows.Next() {
		var name, kind string
		var wr int64
		if err := rows.Scan(&name, &kind, &wr); err != nil {
			return nil, err
		}
		kinds[name] = tableKind{kind: kind, withoutRowid: wr == 1}
	}
	return kinds, rows.Err()
}

func (r *reader) tableExists(name string) (bool, error) {
	var n int64
	err := r.tx.QueryRowContext(r.ctx, `SELECT count(*) FROM main.sqlite_schema WHERE type = 'table' AND name = ?`, name).Scan(&n)
	return n > 0, err
}

func (r *reader) table(name, native string, withoutRowid bool) (*schema.Table, error) {
	rows, err := r.tx.QueryContext(r.ctx,
		`SELECT name, type, "notnull", dflt_value, pk, hidden FROM pragma_table_xinfo(?, 'main') ORDER BY cid`, name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	t := &schema.Table{Name: schema.Ident(name), Native: native, EstimatedRows: -1}
	var pk []*schema.Column
	pkOrder := map[*schema.Column]int64{}
	for rows.Next() {
		var col, declared string
		var notNull, pkPos, hidden int64
		var dflt sql.NullString
		if err := rows.Scan(&col, &declared, &notNull, &dflt, &pkPos, &hidden); err != nil {
			return nil, err
		}
		if hidden == 1 {
			continue // a hidden column of a virtual table
		}
		c := &schema.Column{
			Name:       schema.Ident(col),
			Type:       columnType(declared),
			NativeType: declared,
			Nullable:   notNull == 0,
		}
		if dflt.Valid {
			c.Default = classifyDefault(dflt.String)
		}
		if hidden == 2 || hidden == 3 {
			c.Generated = &schema.Generated{Stored: hidden == 3}
		}
		if pkPos > 0 {
			pk = append(pk, c)
			pkOrder[c] = pkPos
		}
		t.Columns = append(t.Columns, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if len(pk) > 0 {
		keyCols := make([]schema.Ident, len(pk))
		for _, c := range pk {
			keyCols[pkOrder[c]-1] = c.Name
		}
		t.PrimaryKey = &schema.Key{Columns: keyCols}
		switch {
		case withoutRowid:
			// WITHOUT ROWID tables enforce NOT NULL on primary-key columns.
			for _, c := range pk {
				c.Nullable = false
			}
		case len(pk) == 1 && strings.EqualFold(pk[0].NativeType, "INTEGER"):
			// INTEGER PRIMARY KEY is the rowid: never NULL, and numbered
			// automatically when a row is inserted without it.
			pk[0].Nullable = false
			pk[0].Identity = &schema.Identity{}
		}
	}
	return t, nil
}

// classifyDefault sorts a column default into a kind, so that common defaults
// can be recreated on other engines later.
func classifyDefault(expr string) *schema.Default {
	d := &schema.Default{Kind: schema.DefaultExpr, Expr: expr}
	upper := strings.ToUpper(strings.TrimSpace(expr))
	switch {
	case upper == "CURRENT_TIMESTAMP", upper == "CURRENT_DATE", upper == "CURRENT_TIME":
		d.Kind = schema.DefaultNow
	case upper == "NULL", upper == "TRUE", upper == "FALSE",
		strings.HasPrefix(upper, "'") && strings.HasSuffix(upper, "'"):
		d.Kind = schema.DefaultLiteral
	default:
		if _, err := strconv.ParseFloat(strings.Trim(upper, "()"), 64); err == nil {
			d.Kind = schema.DefaultLiteral
		}
	}
	return d
}

// indexes reads a table's unique constraints and indexes, in the order they
// were created. catalog holds every index in the database.
func (r *reader) indexes(t *schema.Table, catalog []catalogEntry) error {
	type info struct {
		unique, partial bool
		origin          string
	}
	rows, err := r.tx.QueryContext(r.ctx, `SELECT name, "unique", origin, partial FROM pragma_index_list(?, 'main')`, string(t.Name))
	if err != nil {
		return err
	}
	infos := map[string]info{}
	for rows.Next() {
		var name string
		var i info
		var unique, partial int64
		if err := rows.Scan(&name, &unique, &i.origin, &partial); err != nil {
			_ = rows.Close()
			return err
		}
		i.unique, i.partial = unique == 1, partial == 1
		infos[name] = i
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, e := range catalog {
		i, ok := infos[e.name]
		if !ok || i.origin == "pk" {
			continue // another table's index, or the primary key, already read from the columns
		}
		columns, expression, err := r.indexColumns(e.name)
		if err != nil {
			return err
		}
		if i.origin == "u" {
			// A UNIQUE constraint. SQLite doesn't report the constraint's
			// name, only its automatic index's.
			t.Uniques = append(t.Uniques, &schema.Key{Columns: columns})
			continue
		}
		t.Indexes = append(t.Indexes, &schema.Index{
			Name:       schema.Ident(e.name),
			Columns:    columns,
			Unique:     i.unique,
			Partial:    i.partial,
			Expression: expression,
			Native:     e.sql,
		})
	}
	return nil
}

func (r *reader) indexColumns(index string) (columns []schema.Ident, expression bool, err error) {
	rows, err := r.tx.QueryContext(r.ctx, `SELECT cid, coalesce(name, '') FROM pragma_index_xinfo(?, 'main') WHERE key = 1 ORDER BY seqno`, index)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var cid int64
		var name string
		if err := rows.Scan(&cid, &name); err != nil {
			return nil, false, err
		}
		if cid == -2 {
			expression = true
			continue
		}
		columns = append(columns, schema.Ident(name))
	}
	return columns, expression, rows.Err()
}

var actions = map[string]schema.Action{
	"NO ACTION":   schema.NoAction,
	"RESTRICT":    schema.Restrict,
	"CASCADE":     schema.Cascade,
	"SET NULL":    schema.SetNull,
	"SET DEFAULT": schema.SetDefault,
}

func (r *reader) foreignKeys(ns *schema.Namespace, t *schema.Table) error {
	rows, err := r.tx.QueryContext(r.ctx,
		`SELECT id, "table", "from", "to", on_update, on_delete FROM pragma_foreign_key_list(?, 'main') ORDER BY id, seq`, string(t.Name))
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	byID := map[int64]*schema.ForeignKey{}
	var order []int64
	for rows.Next() {
		var id int64
		var parent, from, onUpdate, onDelete string
		var to sql.NullString
		if err := rows.Scan(&id, &parent, &from, &to, &onUpdate, &onDelete); err != nil {
			return err
		}
		fk := byID[id]
		if fk == nil {
			// SQLite matches the parent's name without regard to case, so
			// find its exact spelling.
			ref := schema.TableRef{Namespace: ns.Name, Name: schema.Ident(parent)}
			if p := findTable(ns, parent); p != nil {
				ref.Name = p.Name
			}
			fk = &schema.ForeignKey{Ref: ref, OnUpdate: actions[onUpdate], OnDelete: actions[onDelete]}
			byID[id] = fk
			order = append(order, id)
		}
		fk.Columns = append(fk.Columns, schema.Ident(from))
		if to.Valid {
			fk.RefColumns = append(fk.RefColumns, schema.Ident(to.String))
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, id := range order {
		fk := byID[id]
		parent := ns.Table(fk.Ref.Name)
		switch {
		case parent == nil:
			// Validate reports the missing table.
		case len(fk.RefColumns) == 0 && parent.PrimaryKey != nil:
			// REFERENCES parent, without columns, means its primary key.
			fk.RefColumns = append([]schema.Ident(nil), parent.PrimaryKey.Columns...)
		default:
			for i, name := range fk.RefColumns {
				if c := findColumn(parent, string(name)); c != nil {
					fk.RefColumns[i] = c.Name
				}
			}
		}
		t.ForeignKeys = append(t.ForeignKeys, fk)
	}
	return nil
}

// findTable and findColumn match names the way SQLite does: ignoring the
// case of ASCII letters.
func findTable(ns *schema.Namespace, name string) *schema.Table {
	for _, t := range ns.Tables {
		if strings.EqualFold(string(t.Name), name) {
			return t
		}
	}
	return nil
}

func findColumn(t *schema.Table, name string) *schema.Column {
	for _, c := range t.Columns {
		if strings.EqualFold(string(c.Name), name) {
			return c
		}
	}
	return nil
}

// estimatedRows reads a table's row count from the statistics that ANALYZE
// keeps, or returns -1 when there are none.
func (r *reader) estimatedRows(table string) (int64, error) {
	var stat string
	err := r.tx.QueryRowContext(r.ctx,
		`SELECT stat FROM main.sqlite_stat1 WHERE tbl = ? ORDER BY idx IS NOT NULL LIMIT 1`, table).Scan(&stat)
	if errors.Is(err, sql.ErrNoRows) {
		return -1, nil
	}
	if err != nil {
		return -1, err
	}
	first, _, _ := strings.Cut(stat, " ")
	n, err := strconv.ParseInt(first, 10, 64)
	if err != nil {
		return -1, nil
	}
	return n, nil
}
