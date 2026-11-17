package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/exarth-labs/stuntdb/dialect"
	"github.com/exarth-labs/stuntdb/internal/schema"
	"github.com/exarth-labs/stuntdb/internal/value"
)

// TestLoadDefaultRows checks that a load with no columns writes one row of
// the columns' defaults per row, so a table whose columns are all left to
// their defaults keeps its row count.
func TestLoadDefaultRows(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src.db"), filepath.Join(dir, "dst.db")
	rawExec(t, src, `CREATE TABLE ticks (id INTEGER PRIMARY KEY, at TEXT NOT NULL DEFAULT 'start', n INTEGER NOT NULL DEFAULT 7, note TEXT)`)
	db, err := openConn(t, src, dialect.Source).Introspect(ctx, dialect.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	ref := schema.TableRef{Namespace: "main", Name: "ticks"}

	l, err := openConn(t, dst, dialect.Target).Loader(ctx, dialect.LoadOptions{RunID: "defaults"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Abort(ctx) }()
	if err := l.CreateTables(ctx, db); err != nil {
		t.Fatal(err)
	}
	n, err := l.Load(ctx, ref, nil, &sliceRows{rows: [][]value.Value{{}, {}, {}}})
	if err != nil || n != 3 {
		t.Fatalf("Load with no columns = %d, %v; want 3 rows", n, err)
	}
	if err := l.Finish(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := l.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	out, err := sql.Open("sqlite", dst)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = out.Close() }()
	got := queryStrings(t, out, `SELECT id || ',' || at || ',' || n || ',' || coalesce(note, 'NULL') FROM ticks ORDER BY id`)
	if want := []string{"1,start,7,NULL", "2,start,7,NULL", "3,start,7,NULL"}; !slices.Equal(got, want) {
		t.Errorf("the default rows are %v, want %v", got, want)
	}
}

// TestLoadDefaultRowsRefusesValues checks that a row with values, in a load
// with no columns, fails the load.
func TestLoadDefaultRowsRefusesValues(t *testing.T) {
	ctx := context.Background()
	db := &schema.Database{Namespaces: []*schema.Namespace{{Name: "main", Tables: []*schema.Table{{
		Name:    "t",
		Columns: []*schema.Column{{Name: "note", Type: value.Type{Kind: value.TypeText}, Nullable: true}},
	}}}}}
	l, err := openConn(t, filepath.Join(t.TempDir(), "app.db"), dialect.Target).Loader(ctx, dialect.LoadOptions{RunID: "values"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Abort(ctx) }()
	if err := l.CreateTables(ctx, db); err != nil {
		t.Fatal(err)
	}
	rows := &sliceRows{rows: [][]value.Value{{}, {value.String("x")}}}
	_, err = l.Load(ctx, schema.TableRef{Namespace: "main", Name: "t"}, nil, rows)
	if err == nil || !strings.Contains(err.Error(), "row 2 has 1 values for no columns") {
		t.Errorf("a row with a value, in a load with no columns: %v", err)
	}
}

// TestKeysetReadsKeepStorageClasses checks that reading in chunks after the
// last key returns exactly the rows of one full read, for keys that hold
// mixed storage classes, a NOCASE collation, and dates that another program
// wrote in a form that isn't canonical. SQLite compares by storage class
// first, so the reader must take back each key in the form it returned it.
func TestKeysetReadsKeepStorageClasses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.db")
	rawExec(t, path, `
CREATE TABLE ts (k TIMESTAMP NOT NULL UNIQUE);
INSERT INTO ts VALUES ('2024-01-02T10:00:00'), ('2024-01-02T11:00:00'), ('2024-01-03 09:00:00'),
  ('2024-01-02T10:00:00.5'), ('2024-01-02 10:00:00+02:00');
CREATE TABLE d (k DATE NOT NULL UNIQUE);
INSERT INTO d VALUES ('2024-01-02'), ('2024-1-3'), ('2024-01-04'), (20240105);
CREATE TABLE dec (k DECIMAL(30,2) NOT NULL UNIQUE);
INSERT INTO dec VALUES (0.1), (0.1 + 0.2), (5), ('abc'), ('12345678901234567890123.45'), (-1.5), (2.25), (10);
CREATE TABLE txt (k TEXT NOT NULL UNIQUE COLLATE NOCASE);
INSERT INTO txt VALUES ('10'), ('9'), ('B'), ('a'), (x'00ff'), (5), (2.5);
CREATE TABLE untyped (k NOT NULL UNIQUE);
INSERT INTO untyped VALUES ('5'), (4), (6), ('x'), (x'01'), (1.5);
CREATE TABLE ints (k INTEGER NOT NULL UNIQUE);
INSERT INTO ints VALUES (1), (2.5), ('abc'), ('7'), (x'02');
CREATE TABLE pair (a TEXT NOT NULL, b DECIMAL(10,2) NOT NULL, PRIMARY KEY (a, b)) WITHOUT ROWID;
INSERT INTO pair VALUES ('x', 1), ('x', 0.5), ('x', 'n/a'), ('10', 2), ('9', 2), ('9', 1.25);
`)
	snap, err := openConn(t, path, dialect.Source).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = snap.Close() }()
	read := func(q dialect.ReadQuery) [][]value.Value {
		t.Helper()
		it, err := snap.Read(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = it.Close() }()
		var out [][]value.Value
		for it.Next() {
			out = append(out, slices.Clone(it.Row()))
		}
		if err := it.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	for _, c := range []struct {
		table string
		key   []schema.Ident
		rows  int
	}{
		{"ts", ids("k"), 5},
		{"d", ids("k"), 4},
		{"dec", ids("k"), 8},
		{"txt", ids("k"), 7},
		{"untyped", ids("k"), 6},
		{"ints", ids("k"), 5},
		{"pair", ids("a", "b"), 6},
	} {
		ref := schema.TableRef{Namespace: "main", Name: schema.Ident(c.table)}
		all := read(dialect.ReadQuery{Table: ref, Columns: c.key, OrderBy: c.key})
		if len(all) != c.rows {
			t.Fatalf("%s: one read returned %d rows, want %d", c.table, len(all), c.rows)
		}
		for _, limit := range []int{1, 2} {
			var chunked [][]value.Value
			var last []value.Value
			for range c.rows + 1 {
				q := dialect.ReadQuery{Table: ref, Columns: c.key, OrderBy: c.key, Limit: limit}
				if last != nil {
					q.Where = dialect.After{Columns: c.key, Key: last}
				}
				chunk := read(q)
				if len(chunk) == 0 {
					break
				}
				chunked = append(chunked, chunk...)
				last = chunk[len(chunk)-1]
			}
			if len(chunked) != len(all) {
				t.Fatalf("%s in chunks of %d: %d rows, want %d", c.table, limit, len(chunked), len(all))
			}
			for i := range all {
				if !slices.EqualFunc(all[i], chunked[i], value.Value.Equal) {
					t.Errorf("%s in chunks of %d: row %d differs from one full read", c.table, limit, i)
				}
			}
		}
	}
}

// TestNullablePrimaryKey checks that a SQLite primary key that can hold
// NULLs is not used to identify rows, and that a read of the whole table
// returns its NULL rows. A column declared INTEGER PRIMARY KEY DESC is not
// the rowid and holds NULLs; PRIMARY KEY (id DESC), a table constraint, is
// the rowid.
func TestNullablePrimaryKey(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "codes.db")
	rawExec(t, path, `
CREATE TABLE codes (code TEXT PRIMARY KEY, label TEXT);
INSERT INTO codes VALUES (NULL, 'x'), (NULL, 'y'), ('a', 'z');
CREATE TABLE tagged (code TEXT PRIMARY KEY, email TEXT NOT NULL UNIQUE);
CREATE TABLE numbered (id INTEGER PRIMARY KEY, code TEXT);
CREATE TABLE descending (id INTEGER PRIMARY KEY DESC, label TEXT);
INSERT INTO descending VALUES (NULL, 'x'), (NULL, 'y'), (5, 'z');
CREATE TABLE descending_key (id INTEGER, label TEXT, PRIMARY KEY (id DESC));
CREATE TABLE ascending (id INTEGER PRIMARY KEY ASC, label TEXT);
CREATE TABLE descending_not_null (id INTEGER NOT NULL PRIMARY KEY DESC, label TEXT);
`)
	conn := openConn(t, path, dialect.Source)
	db, err := conn.Introspect(ctx, dialect.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	ns := db.Namespace("main")
	for _, c := range []struct {
		table schema.Ident
		key   []schema.Ident
		rowid bool
	}{
		{"codes", nil, false},
		{"tagged", ids("email"), false},
		{"numbered", ids("id"), true},
		{"descending", nil, false},
		{"descending_key", ids("id"), true},
		{"ascending", ids("id"), true},
		{"descending_not_null", ids("id"), false},
	} {
		tbl := ns.Table(c.table)
		if key := tbl.IdentityKey(); !slices.Equal(key, c.key) {
			t.Errorf("%s: IdentityKey() = %v, want %v", c.table, key, c.key)
		}
		if rowid := tbl.Column(tbl.PrimaryKey.Columns[0]).Identity != nil; rowid != c.rowid {
			t.Errorf("%s: primary key taken for the rowid = %v, want %v", c.table, rowid, c.rowid)
		}
	}
	snap, err := conn.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = snap.Close() }()
	for _, table := range []schema.Ident{"codes", "descending"} {
		key := ns.Table(table).PrimaryKey.Columns[0]
		it, err := snap.Read(ctx, dialect.ReadQuery{Table: schema.TableRef{Namespace: "main", Name: table}, Columns: []schema.Ident{key, "label"}})
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for it.Next() {
			n++
		}
		if err := errors.Join(it.Err(), it.Close()); err != nil || n != 3 {
			t.Errorf("reading %s whole returned %d rows (%v), want 3", table, n, err)
		}
	}
}
