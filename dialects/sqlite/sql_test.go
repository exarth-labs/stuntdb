package sqlite

import (
	"errors"
	"testing"

	"github.com/exarth-labs/stuntdb/dialect"
	"github.com/exarth-labs/stuntdb/internal/schema"
	"github.com/exarth-labs/stuntdb/internal/value"
)

func ids(names ...string) []schema.Ident {
	out := make([]schema.Ident, len(names))
	for i, n := range names {
		out[i] = schema.Ident(n)
	}
	return out
}

func intKeys(n int) [][]value.Value {
	keys := make([][]value.Value, n)
	for i := range keys {
		keys[i] = []value.Value{value.Int(int64(i + 1))}
	}
	return keys
}

func TestRenderRead(t *testing.T) {
	users := schema.TableRef{Namespace: "main", Name: "users"}
	order := schema.TableRef{Namespace: "main", Name: `my "order"`}
	tests := []struct {
		name  string
		q     dialect.ReadQuery
		want  string
		nargs int
	}{
		{
			"whole table",
			dialect.ReadQuery{Table: users, Columns: ids("id", "email")},
			`SELECT +"id", +"email" FROM "main"."users"`, 0,
		},
		{
			"quoting",
			dialect.ReadQuery{Table: order, Columns: ids(`Order "Id"`)},
			`SELECT +"Order ""Id""" FROM "main"."my ""order"""`, 0,
		},
		{
			"keys, ordered and limited",
			dialect.ReadQuery{
				Table: users, Columns: ids("id"), OrderBy: ids("id"), Limit: 10,
				Where: dialect.KeysIn{Columns: ids("id"), Keys: intKeys(3)},
			},
			`SELECT +"id" FROM "main"."users" WHERE "id" IN (?, ?, ?) ORDER BY "id" LIMIT 10`, 3,
		},
		{
			"composite keys, NULL keys dropped",
			dialect.ReadQuery{
				Table: users, Columns: ids("a"),
				Where: dialect.KeysIn{Columns: ids("a", "b"), Keys: [][]value.Value{
					{value.Int(1), value.Int(2)}, {value.Int(3), value.Null()}, {value.Int(4), value.Int(5)},
				}},
			},
			`SELECT +"a" FROM "main"."users" WHERE ("a", "b") IN (VALUES (?, ?), (?, ?))`, 4,
		},
		{
			"only NULL keys match nothing",
			dialect.ReadQuery{
				Table: users, Columns: ids("id"),
				Where: dialect.KeysIn{Columns: ids("nickname"), Keys: [][]value.Value{{value.Null()}}},
			},
			`SELECT +"id" FROM "main"."users" WHERE 0`, 0,
		},
		{
			"keyset page with a composite key",
			dialect.ReadQuery{
				Table: users, Columns: ids("a", "b"), OrderBy: ids("a", "b"), Limit: 100,
				Where: dialect.After{Columns: ids("a", "b"), Key: []value.Value{value.Int(1), value.String("x")}},
			},
			`SELECT +"a", +"b" FROM "main"."users" WHERE ("a", "b") > (?, ?) ORDER BY "a", "b" LIMIT 100`, 2,
		},
		{
			"native seed filter and a page",
			dialect.ReadQuery{
				Table: users, Columns: ids("id"), OrderBy: ids("id"), Limit: 5,
				Where: dialect.And{dialect.Native{SQL: "plan <> 'enterprise' OR id < 3"}, dialect.After{Columns: ids("id"), Key: []value.Value{value.Int(7)}}},
			},
			`SELECT +"id" FROM "main"."users" WHERE ((plan <> 'enterprise' OR id < 3)) AND ("id" > ?) ORDER BY "id" LIMIT 5`, 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stmt, err := renderRead(tt.q, maxKeyValues, nil)
			if err != nil {
				t.Fatalf("renderRead: %v", err)
			}
			if stmt.sql != tt.want {
				t.Errorf("SQL =\n  %s\nwant\n  %s", stmt.sql, tt.want)
			}
			if len(stmt.args) != tt.nargs {
				t.Errorf("%d arguments, want %d", len(stmt.args), tt.nargs)
			}
		})
	}
}

// TestRenderReadUsesTheKeysCollation checks that a read ordered by a unique
// key sorts and compares with the key's collations, in the read's column
// order, and that other reads are left as they are.
func TestRenderReadUsesTheKeysCollation(t *testing.T) {
	users := schema.TableRef{Namespace: "main", Name: "users"}
	keys := []uniqueKey{
		{columns: ids("email"), collations: []string{"NOCASE"}},
		{columns: ids("a", "b"), collations: []string{"BINARY", `we"ird`}},
	}
	page := func(key ...schema.Ident) dialect.ReadQuery {
		last := make([]value.Value, len(key))
		for i := range last {
			last[i] = value.String("x")
		}
		return dialect.ReadQuery{Table: users, Columns: key, OrderBy: key, Limit: 10, Where: dialect.After{Columns: key, Key: last}}
	}
	tests := []struct {
		name string
		q    dialect.ReadQuery
		want string
	}{
		{"one column", page("email"), `SELECT +"email" FROM "main"."users" WHERE "email" > ? COLLATE "NOCASE" ORDER BY "email" COLLATE "NOCASE" LIMIT 10`},
		{"two columns", page("a", "b"), `SELECT +"a", +"b" FROM "main"."users" WHERE ("a", "b") > (? COLLATE "BINARY", ? COLLATE "we""ird") ORDER BY "a" COLLATE "BINARY", "b" COLLATE "we""ird" LIMIT 10`},
		{"the key's columns in another order", page("b", "a"), `SELECT +"b", +"a" FROM "main"."users" WHERE ("b", "a") > (? COLLATE "we""ird", ? COLLATE "BINARY") ORDER BY "b" COLLATE "we""ird", "a" COLLATE "BINARY" LIMIT 10`},
		{"part of a key", page("a"), `SELECT +"a" FROM "main"."users" WHERE "a" > ? ORDER BY "a" LIMIT 10`},
		{"a column twice", page("a", "a"), `SELECT +"a", +"a" FROM "main"."users" WHERE ("a", "a") > (?, ?) ORDER BY "a", "a" LIMIT 10`},
		{"more than a key", page("a", "b", "email"), `SELECT +"a", +"b", +"email" FROM "main"."users" WHERE ("a", "b", "email") > (?, ?, ?) ORDER BY "a", "b", "email" LIMIT 10`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stmt, err := renderRead(tt.q, maxKeyValues, keys)
			if err != nil {
				t.Fatalf("renderRead: %v", err)
			}
			if stmt.sql != tt.want {
				t.Errorf("SQL =\n  %s\nwant\n  %s", stmt.sql, tt.want)
			}
		})
	}
	nested := page("email")
	nested.Where = dialect.And{dialect.Native{SQL: "1"}, nested.Where}
	if !hasAfter(nested.Where) || hasAfter(dialect.And{dialect.Native{SQL: "1"}}) || hasAfter(nil) {
		t.Error("hasAfter doesn't find After inside And, or finds one that isn't there")
	}
}

func TestRenderReadRefusesTooManyKeys(t *testing.T) {
	q := func(keys int) dialect.ReadQuery {
		pairs := make([][]value.Value, keys)
		for i := range pairs {
			pairs[i] = []value.Value{value.Int(int64(i)), value.String("x")}
		}
		return dialect.ReadQuery{
			Table: schema.TableRef{Namespace: "main", Name: "users"}, Columns: ids("id"), OrderBy: ids("id"),
			Where: dialect.And{
				dialect.After{Columns: ids("id"), Key: []value.Value{value.Int(0)}},
				dialect.KeysIn{Columns: ids("id", "name"), Keys: pairs},
			},
		}
	}
	// A limit of 4 values fits two keys of two columns; the After key doesn't count.
	stmt, err := renderRead(q(2), 4, nil)
	if err != nil {
		t.Fatalf("two keys, within the limit: %v", err)
	}
	if len(stmt.args) != 5 {
		t.Errorf("%d arguments, want 5", len(stmt.args))
	}
	if _, err := renderRead(q(3), 4, nil); !errors.Is(err, dialect.ErrTooManyKeys) {
		t.Errorf("three keys, over the limit = %v, want ErrTooManyKeys", err)
	}
	if maxParams-maxKeyValues < 100 {
		t.Errorf("maxKeyValues %d leaves too little room under SQLite's %d parameters", maxKeyValues, maxParams)
	}
}

func TestCreateTableSQL(t *testing.T) {
	tbl := &schema.Table{
		Name: "order_items",
		Columns: []*schema.Column{
			{Name: "order_id", Type: value.Type{Kind: value.TypeInt, Bits: 64}},
			{Name: "line", Type: value.Type{Kind: value.TypeInt, Bits: 32}},
			{Name: "sku", Type: value.Type{Kind: value.TypeString, MaxLen: 12}, Nullable: true},
		},
		PrimaryKey: &schema.Key{Columns: ids("order_id", "line")},
		Uniques:    []*schema.Key{{Columns: ids("sku")}},
		ForeignKeys: []*schema.ForeignKey{
			{Columns: ids("order_id"), Ref: schema.TableRef{Namespace: "main", Name: "orders"}, RefColumns: ids("id"), OnDelete: schema.Cascade},
			{Columns: ids("sku"), Ref: schema.TableRef{Namespace: "main", Name: "products"}, RefColumns: ids("sku"), Virtual: true},
		},
	}
	got, err := createTableSQL(tbl)
	if err != nil {
		t.Fatal(err)
	}
	want := `CREATE TABLE "order_items" (
  "order_id" INTEGER NOT NULL,
  "line" INTEGER NOT NULL,
  "sku" VARCHAR(12),
  PRIMARY KEY ("order_id", "line"),
  UNIQUE ("sku"),
  FOREIGN KEY ("order_id") REFERENCES "orders" ("id") ON DELETE CASCADE ON UPDATE NO ACTION
)`
	if got != want {
		t.Errorf("createTableSQL =\n%s\nwant\n%s", got, want)
	}

	tbl.Columns[2].Generated = &schema.Generated{Expr: "upper(sku)"}
	if _, err := createTableSQL(tbl); err == nil {
		t.Error("a generated column from another engine was accepted")
	}
}

func TestFilePath(t *testing.T) {
	tests := []struct {
		url, want string
	}{
		{"sqlite:app.db", "app.db"},
		{"sqlite:./data/app.db", "./data/app.db"},
		{`sqlite:C:\data\app.db`, `C:\data\app.db`},
		{"sqlite:///var/data/app.db", "/var/data/app.db"},
		{"sqlite://./app.db", "./app.db"},
		{"SQLite3:app.db", "app.db"},
	}
	for _, tt := range tests {
		got, err := filePath(tt.url)
		if err != nil || got != tt.want {
			t.Errorf("filePath(%q) = %q, %v; want %q", tt.url, got, err, tt.want)
		}
	}
	for _, bad := range []string{"sqlite:", "sqlite://", "sqlite::memory:", "sqlite:file::memory:?cache=shared", "sqlite:app.db?mode=rw", "postgres://x"} {
		if _, err := filePath(bad); err == nil {
			t.Errorf("filePath(%q) succeeded", bad)
		}
	}
}
