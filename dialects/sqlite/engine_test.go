package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"maps"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/exarth-labs/stuntdb/dialect"
	"github.com/exarth-labs/stuntdb/internal/engine"
	"github.com/exarth-labs/stuntdb/internal/mask"
	"github.com/exarth-labs/stuntdb/internal/schema"
)

// TestEngineCopy copies a SQLite database through the engine: tables whose
// primary keys hold NULLs (read whole, not in chunks), a table read in
// many chunks whose row order must be kept, a table read in many chunks
// along a key unique under another collation than its column's, a masked
// column, and a table whose columns are all left to their defaults.
func TestEngineCopy(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src.db"), filepath.Join(dir, "dst.db")
	rawExec(t, src, `
CREATE TABLE codes (code TEXT PRIMARY KEY, label TEXT);
INSERT INTO codes VALUES (NULL, 'x'), (NULL, 'y'), ('a', 'z');
CREATE TABLE descending (id INTEGER PRIMARY KEY DESC, label TEXT);
INSERT INTO descending VALUES (NULL, 'x'), (NULL, 'y'), (5, 'z');
CREATE TABLE people (slug TEXT NOT NULL PRIMARY KEY, name TEXT NOT NULL, price DECIMAL(12,2));
WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 12000)
INSERT INTO people SELECT printf('p-%05d', (i * 7919) % 12007), 'name ' || i, i * 0.01 FROM n;
CREATE TABLE tags (tag TEXT NOT NULL COLLATE NOCASE, UNIQUE (tag COLLATE BINARY));
WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 3500)
INSERT INTO tags SELECT printf('ab%04d', i) FROM n UNION ALL SELECT printf('aB%04d', i) FROM n UNION ALL SELECT printf('Ab%04d', i) FROM n;
CREATE TABLE stamps (made TEXT NOT NULL DEFAULT 'made', n INTEGER DEFAULT 7);
INSERT INTO stamps VALUES ('a', 1), ('b', 2), ('c', 3);
`)
	source := openConn(t, src, dialect.Source)
	db, err := source.Introspect(ctx, dialect.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := source.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = snap.Close() }()
	reads := &countReads{Snapshot: snap, n: map[schema.Ident]int{}}
	loader, err := openConn(t, dst, dialect.Target).Loader(ctx, dialect.LoadOptions{RunID: "engine"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = loader.Abort(ctx) }()
	if err := loader.CreateTables(ctx, db); err != nil {
		t.Fatal(err)
	}

	keys, err := mask.NewKeys(bytes.Repeat([]byte{3}, mask.MinKeyLength))
	if err != nil {
		t.Fatal(err)
	}
	special := map[schema.Ident]mask.Rule{"name": {Use: "full_name"}, "made": {Use: "default"}, "n": {Use: "default"}}
	job := engine.Job{
		Source: reads, Snapshot: source.Capabilities().Snapshot, Keys: keys, Sink: loader,
		Budget: 64 << 20, ChunkBytes: 4096, // after the first chunk, a few dozen rows each
	}
	for _, tbl := range db.Namespace("main").Tables {
		rules := map[schema.Ident]mask.Rule{}
		for _, c := range tbl.Columns {
			rules[c.Name] = special[c.Name]
			if rules[c.Name].Use == "" {
				rules[c.Name] = mask.Rule{Use: "keep"}
			}
		}
		r := schema.TableRef{Namespace: "main", Name: tbl.Name}
		m, err := mask.NewTable(r, tbl, rules, source.Capabilities())
		if err != nil {
			t.Fatal(err)
		}
		job.Tables = append(job.Tables, engine.Table{Ref: r, Schema: tbl, Mask: m})
	}
	report, err := engine.Copy(ctx, job)
	if err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if err := loader.Finish(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := loader.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	rows := map[schema.Ident]int64{}
	for _, tr := range report.Tables {
		rows[tr.Ref.Name] = tr.Rows
	}
	if want := map[schema.Ident]int64{"codes": 3, "descending": 3, "people": 12000, "stamps": 3, "tags": 10500}; !maps.Equal(rows, want) {
		t.Errorf("report %v, want %v", rows, want)
	}
	for _, table := range []schema.Ident{"people", "tags"} {
		if reads.n[table] < 10 {
			t.Errorf("%s was read in %d chunks; the test needs many", table, reads.n[table])
		}
	}
	before, after := dump(t, src), dump(t, dst)
	for _, table := range []string{"codes", "descending", "tags"} {
		if !slices.Equal(before[table], after[table]) {
			t.Errorf("%s differs after the copy", table)
		}
	}

	out, err := sql.Open("sqlite", dst)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = out.Close() }()
	in, err := sql.Open("sqlite", src)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = in.Close() }()

	// Rows were written in key order, so the new hidden rowids follow it,
	// though the source stored them shuffled.
	byRowid := queryStrings(t, out, "SELECT slug FROM people ORDER BY rowid")
	if !slices.IsSorted(byRowid) || len(byRowid) != 12000 {
		t.Errorf("people: %d rows, in key order: %v", len(byRowid), slices.IsSorted(byRowid))
	}
	if slices.IsSorted(queryStrings(t, in, "SELECT slug FROM people ORDER BY rowid")) {
		t.Error("the source's rows are already in key order, so the test proves nothing")
	}
	prices := "SELECT slug || '|' || quote(price) FROM people ORDER BY slug"
	if !slices.Equal(queryStrings(t, in, prices), queryStrings(t, out, prices)) {
		t.Error("people: the kept prices differ")
	}
	if kept := queryStrings(t, out, "SELECT count(*) FROM people WHERE name LIKE 'name %'"); kept[0] != "0" {
		t.Errorf("people: %s names were not masked", kept[0])
	}
	if got := queryStrings(t, out, "SELECT made || '|' || n FROM stamps"); !slices.Equal(got, []string{"made|7", "made|7", "made|7"}) {
		t.Errorf("stamps: %v, want three rows of defaults", got)
	}
}

// countReads counts the reads of each table.
type countReads struct {
	dialect.Snapshot
	mu sync.Mutex
	n  map[schema.Ident]int
}

func (c *countReads) Read(ctx context.Context, q dialect.ReadQuery) (dialect.RowIter, error) {
	c.mu.Lock()
	c.n[q.Table.Name]++
	c.mu.Unlock()
	return c.Snapshot.Read(ctx, q)
}
