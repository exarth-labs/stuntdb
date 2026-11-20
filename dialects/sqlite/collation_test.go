package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"modernc.org/sqlite"

	"github.com/exarth-labs/stuntdb/dialect"
	"github.com/exarth-labs/stuntdb/internal/engine"
	"github.com/exarth-labs/stuntdb/internal/mask"
	"github.com/exarth-labs/stuntdb/internal/schema"
)

// appCollation is a collation an application registers, which stuntdb
// doesn't know. Registering it is process-wide, so its name is the test's.
const appCollation = "stuntdb_test_reverse"

func init() {
	sqlite.MustRegisterCollationUtf8(appCollation, func(a, b string) int { return strings.Compare(b, a) })
}

// TestIntrospectKeyCollations checks how each kind of key reports the
// collation of each column: the key's own, which may differ from the
// column's, in the key's column order.
func TestIntrospectKeyCollations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.db")
	rawExec(t, path, `
CREATE TABLE nocase_pk (email TEXT PRIMARY KEY COLLATE NOCASE);
CREATE TABLE rtrim_pk (k TEXT PRIMARY KEY COLLATE RTRIM, x TEXT) WITHOUT ROWID;
CREATE TABLE uniques (id INTEGER PRIMARY KEY, u TEXT, v TEXT COLLATE NOCASE, UNIQUE (u COLLATE RTRIM), UNIQUE (v COLLATE BINARY));
CREATE UNIQUE INDEX uniques_vu ON uniques (v, u COLLATE nocase);
CREATE INDEX uniques_u ON uniques (u);
CREATE TABLE composite (p TEXT, q TEXT COLLATE nocase, PRIMARY KEY (q, p));
CREATE TABLE app (k TEXT PRIMARY KEY COLLATE `+appCollation+`);
`)
	db, err := openConn(t, path, dialect.Source).Introspect(context.Background(), dialect.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	table := func(name schema.Ident) *schema.Table { return db.Table(schema.TableRef{Namespace: "main", Name: name}) }
	binary := schema.Collation{Name: "BINARY"}
	nocase := schema.Collation{Name: "NOCASE", Equality: schema.EqualIgnoringASCIICase}
	rtrim := schema.Collation{Name: "RTRIM", Equality: schema.EqualIgnoringTrailingSpaces}
	lowerNocase := schema.Collation{Name: "nocase", Equality: schema.EqualIgnoringASCIICase}

	tests := []struct {
		what string
		got  []schema.Collation
		want []schema.Collation
	}{
		{"a primary key", table("nocase_pk").PrimaryKey.Collations, []schema.Collation{nocase}},
		{"a WITHOUT ROWID primary key", table("rtrim_pk").PrimaryKey.Collations, []schema.Collation{rtrim}},
		{"a rowid alias, which has no index", table("uniques").PrimaryKey.Collations, nil},
		{"a unique constraint with its own collation", table("uniques").Uniques[0].Collations, []schema.Collation{rtrim}},
		{"a BINARY unique constraint on a NOCASE column", table("uniques").Uniques[1].Collations, []schema.Collation{binary}},
		{"a unique index", table("uniques").Indexes[0].Collations, []schema.Collation{nocase, lowerNocase}},
		{"an index that isn't unique", table("uniques").Indexes[1].Collations, []schema.Collation{binary}},
		{"a composite key, in the key's order", table("composite").PrimaryKey.Collations, []schema.Collation{lowerNocase, binary}},
		{"an application's collation", table("app").PrimaryKey.Collations, []schema.Collation{{Name: appCollation, Equality: schema.EqualOther}}},
	}
	for _, tt := range tests {
		if !slices.Equal(tt.got, tt.want) {
			t.Errorf("%s: %v, want %v", tt.what, tt.got, tt.want)
		}
	}
	if !slices.Equal(table("composite").PrimaryKey.Columns, []schema.Ident{"q", "p"}) {
		t.Errorf("composite key columns %v", table("composite").PrimaryKey.Columns)
	}
}

// FuzzFoldMatchesSQLite checks that two texts are equal under each of
// SQLite's collations exactly when their folds are identical, as SQLite
// itself decides it.
func FuzzFoldMatchesSQLite(f *testing.F) {
	for _, s := range [][2]string{
		{"bob", "BOB"},
		{"bob", "bob  "},
		{"Bob ", "bOB"},
		{"É", "é"},
		{"bob\t", "bob"},
		{"a\x00A", "a\x00B"},
		{"a\x00", "a\x00 "},
		{"", " "},
		{"[", "{"},
		{"@", "`"},
	} {
		f.Add(s[0], s[1])
	}
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	colls := map[string]schema.Equality{
		"BINARY": schema.EqualBytes, "NOCASE": schema.EqualIgnoringASCIICase, "RTRIM": schema.EqualIgnoringTrailingSpaces,
	}
	f.Fuzz(func(t *testing.T, a, b string) {
		for name, eq := range colls {
			var same bool
			if err := db.QueryRow(`SELECT ? = ? COLLATE `+name, a, b).Scan(&same); err != nil {
				t.Fatal(err)
			}
			fa, _ := eq.Fold(a)
			fb, _ := eq.Fold(b)
			if same != (fa == fb) {
				t.Errorf("%s: SQLite says %q = %q is %v, but the folds %q and %q say %v", name, a, b, same, fa, fb, fa == fb)
			}
		}
	})
}

// TestEngineCopyMasksEqualKeysAlike copies, through the engine, keys whose
// collations count different texts as equal, with foreign keys holding
// case and trailing-space variants of their parents, masked by every
// transformer that can mask a unique text key. The target's own unique
// indexes and foreign key check decide.
func TestEngineCopyMasksEqualKeysAlike(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src.db"), filepath.Join(dir, "dst.db")
	rawExec(t, src, `
CREATE TABLE users (
	email TEXT PRIMARY KEY COLLATE NOCASE,
	code TEXT NOT NULL UNIQUE COLLATE RTRIM,
	token TEXT NOT NULL UNIQUE COLLATE NOCASE,
	uid TEXT NOT NULL UNIQUE COLLATE NOCASE,
	handle TEXT NOT NULL UNIQUE COLLATE NOCASE,
	h TEXT NOT NULL UNIQUE COLLATE NOCASE,
	r TEXT NOT NULL UNIQUE COLLATE RTRIM,
	kept TEXT NOT NULL UNIQUE COLLATE NOCASE);
CREATE TABLE orders (
	id INTEGER PRIMARY KEY,
	email TEXT REFERENCES users (email), code TEXT REFERENCES users (code), token TEXT REFERENCES users (token),
	uid TEXT REFERENCES users (uid), handle TEXT REFERENCES users (handle), h TEXT REFERENCES users (h),
	r TEXT REFERENCES users (r), kept TEXT REFERENCES users (kept));
CREATE TABLE tags (tag TEXT NOT NULL COLLATE NOCASE, UNIQUE (tag COLLATE BINARY));
WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 300)
INSERT INTO users SELECT
	printf('User%d@Example.com', i), printf('code-%06d', i), printf('Token%dAbcdefgh', i),
	printf('%08X-4F89-11D3-9A0C-0305E82C3301', i), printf('Handle_%d', i), printf('Hash-Me-%d', i),
	printf('rand-%d-abcdefghijklmnopqrstuvwxyz', i), printf('Kept%d', i) FROM n;
INSERT INTO orders SELECT NULL, upper(email), code || '   ', lower(token), lower(uid), upper(handle), lower(h), r || ' ', lower(kept) FROM users;
INSERT INTO orders SELECT NULL, lower(email), code, upper(token), uid, lower(handle), upper(h), r, upper(kept) FROM users;
INSERT INTO tags VALUES ('abc'), ('ABC'), ('Abc');
`)
	source := openConn(t, src, dialect.Source)
	db, err := source.Introspect(ctx, dialect.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	// As check does: each foreign key takes its parent's rule and domain,
	// then the domains are resolved together.
	uses := map[schema.Ident]mask.Rule{
		"email": {Use: "email"}, "code": {Use: "fpe"}, "token": {Use: "fpe", Options: map[string]any{"alphabet": "lower"}},
		"uid": {Use: "uuid"}, "handle": {Use: "username"}, "h": {Use: "hash"}, "r": {Use: "random_token"}, "kept": {Use: "keep"},
		"id": {Use: "keep"}, "tag": {Use: "hash"},
	}
	var cols []mask.ColumnRule
	for _, tbl := range db.Namespace("main").Tables {
		ref := schema.TableRef{Namespace: "main", Name: tbl.Name}
		for _, c := range mask.Describe(ref, tbl, source.Capabilities()) {
			rule := uses[c.Ref.Column]
			if tbl.Name == "orders" && c.Ref.Column != "id" {
				rule.Domain = mask.DefaultDomain(schema.ColumnRef{Table: schema.TableRef{Namespace: "main", Name: "users"}, Column: c.Ref.Column})
			}
			cols = append(cols, mask.ColumnRule{Column: c, Rule: rule})
		}
	}
	resolved, err := mask.ResolveDomains(cols)
	if err != nil {
		t.Fatal(err)
	}
	rules := map[schema.TableRef]map[schema.Ident]mask.Rule{}
	for i, c := range cols {
		if rules[c.Column.Ref.Table] == nil {
			rules[c.Column.Ref.Table] = map[schema.Ident]mask.Rule{}
		}
		rules[c.Column.Ref.Table][c.Column.Ref.Column] = resolved[i]
	}

	snap, err := source.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = snap.Close() }()
	loader, err := openConn(t, dst, dialect.Target).Loader(ctx, dialect.LoadOptions{RunID: "keys"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = loader.Abort(ctx) }()
	if err := loader.CreateTables(ctx, db); err != nil {
		t.Fatal(err)
	}
	keys, err := mask.NewKeys(bytes.Repeat([]byte{5}, mask.MinKeyLength))
	if err != nil {
		t.Fatal(err)
	}
	job := engine.Job{Source: snap, Snapshot: source.Capabilities().Snapshot, Keys: keys, Sink: loader, Budget: 64 << 20}
	for _, tbl := range db.Namespace("main").Tables {
		ref := schema.TableRef{Namespace: "main", Name: tbl.Name}
		m, err := mask.NewTable(ref, tbl, rules[ref], source.Capabilities())
		if err != nil {
			t.Fatal(err)
		}
		job.Tables = append(job.Tables, engine.Table{Ref: ref, Schema: tbl, Mask: m})
	}
	if _, err := engine.Copy(ctx, job); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if err := loader.Finish(ctx, db); err != nil {
		t.Fatalf("Finish (the target's foreign key check): %v", err)
	}
	if err := loader.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	out, err := sql.Open("sqlite", dst)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = out.Close() }()
	if n := queryStrings(t, out, "SELECT count(*) FROM orders"); n[0] != "600" {
		t.Fatalf("orders: %s rows", n[0])
	}
	// Every masked foreign key holds its parent's fake value byte for byte,
	// not merely one its collation counts as equal.
	for _, c := range []string{"email", "code", "token", "uid", "handle", "h", "r"} {
		q := "SELECT count(*) FROM orders o WHERE NOT EXISTS (SELECT 1 FROM users u WHERE u." + c + " COLLATE BINARY = o." + c + " COLLATE BINARY)"
		if n := queryStrings(t, out, q); n[0] != "0" {
			t.Errorf("orders.%s: %s values aren't their parent's exact bytes", c, n[0])
		}
	}
	// And each fake value is in the form its key compares, so two of them
	// that differ in bytes differ under the key too.
	for _, c := range []string{"email", "token", "uid", "handle", "h"} { // NOCASE
		if n := queryStrings(t, out, "SELECT count(*) FROM users WHERE "+c+" GLOB '*[A-Z]*'"); n[0] != "0" {
			t.Errorf("users.%s: %s fake values hold capital letters", c, n[0])
		}
	}
	for _, c := range []string{"code", "r"} { // RTRIM
		if n := queryStrings(t, out, "SELECT count(*) FROM users WHERE "+c+" LIKE '% '"); n[0] != "0" {
			t.Errorf("users.%s: %s fake values end in a space", c, n[0])
		}
	}
	if n := queryStrings(t, out, "SELECT count(*) FROM users WHERE email LIKE 'user%@example.com' OR h LIKE 'hash-me-%'"); n[0] != "0" {
		t.Errorf("users: %s rows were not masked", n[0])
	}
	if got := queryStrings(t, out, "SELECT kept FROM orders ORDER BY id LIMIT 1"); got[0] != "kept1" {
		t.Errorf("keep changed a value: %q", got[0])
	}
	if got := queryStrings(t, out, "SELECT count(DISTINCT tag COLLATE BINARY) FROM tags"); got[0] != "3" {
		t.Errorf("tags: %s distinct fake values for three keys distinct in bytes", got[0])
	}
}
