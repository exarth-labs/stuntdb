// Package dialecttest is the conformance suite that every stuntdb database
// adapter must pass. An adapter's tests describe how to get fresh databases
// in a Harness, and call Run.
//
// The suite works only through the dialect SDK. It loads sample data with the
// adapter's Loader, then checks that introspection and every kind of read
// return exactly what was loaded. It also checks that the adapter's
// Capabilities are true, and that errors never leak URLs.
package dialecttest

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/exarth-labs/stuntdb/dialect"
	"github.com/exarth-labs/stuntdb/internal/schema"
	"github.com/exarth-labs/stuntdb/internal/value"
)

// Harness tells the suite how to use an adapter.
type Harness struct {
	// Dialect is the adapter under test.
	Dialect dialect.Dialect

	// NewDatabase returns the URL of a new, empty database for one test, and
	// removes it afterwards with t.Cleanup.
	NewDatabase func(t *testing.T) string

	// Namespace is the namespace tables are created in, such as "main"
	// (SQLite) or "public" (PostgreSQL).
	Namespace schema.Ident

	// BrokenURL returns a URL that contains secret and cannot be opened.
	// The suite checks that the error does not contain secret. Nil skips that check.
	BrokenURL func(secret string) string
}

// Run runs the whole suite against h.
func Run(t *testing.T, h Harness) {
	t.Helper()
	if h.Dialect == nil || h.NewDatabase == nil || h.Namespace == "" {
		t.Fatal("dialecttest: Harness needs Dialect, NewDatabase and Namespace")
	}
	t.Run("OpenErrorsHideTheURL", func(t *testing.T) { testOpenErrors(t, h) })
	t.Run("Roles", func(t *testing.T) { testRoles(t, h) })
	t.Run("RoundTrip", func(t *testing.T) { testRoundTrip(t, h) })
	t.Run("KeysIn", func(t *testing.T) { testKeysIn(t, h) })
	t.Run("KeysetPagination", func(t *testing.T) { testKeysetPagination(t, h) })
	t.Run("Join", func(t *testing.T) { testJoin(t, h) })
	t.Run("AbortLeavesNothing", func(t *testing.T) { testAbort(t, h) })
	t.Run("CommitNeedsReplace", func(t *testing.T) { testReplace(t, h) })
	t.Run("FinishChecksForeignKeys", func(t *testing.T) { testForeignKeys(t, h) })
	t.Run("EmptyText", func(t *testing.T) { testEmptyText(t, h) })
}

func testOpenErrors(t *testing.T, h Harness) {
	if h.BrokenURL == nil {
		t.Skip("Harness.BrokenURL is not set")
	}
	const secret = "hunter2-do-not-print"
	conn, err := h.Dialect.Open(context.Background(), h.BrokenURL(secret), dialect.OpenOptions{Role: dialect.Source})
	if err == nil {
		_ = conn.Close()
		t.Fatal("opening the broken URL succeeded")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("the error contains part of the URL: %q", err)
	}
}

func testRoles(t *testing.T, h Harness) {
	url := h.NewDatabase(t)
	source := open(t, h, url, dialect.Source)
	if l, err := source.Loader(context.Background(), dialect.LoadOptions{RunID: "roles"}); err == nil {
		_ = l.Abort(context.Background())
		t.Error("a source connection must not create a Loader")
	}
	target := open(t, h, url, dialect.Target)
	if s, err := target.Snapshot(context.Background()); err == nil {
		_ = s.Close()
		t.Error("a target connection must not create a Snapshot")
	}
}

func testRoundTrip(t *testing.T, h Harness) {
	url := h.NewDatabase(t)
	want, data := shop(h.Namespace)
	load(t, h, url, want, data, dialect.LoadOptions{RunID: "round-trip"})

	source := open(t, h, url, dialect.Source)
	got, err := source.Introspect(context.Background(), dialect.Filter{})
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	compareStructure(t, want, got)

	snap := snapshot(t, source)
	for ref, rows := range data {
		tbl := want.Table(ref)
		q := dialect.ReadQuery{Table: ref, Columns: columnNames(tbl), OrderBy: tbl.PrimaryKey.Columns}
		compareRows(t, ref.String(), rows, readAll(t, snap, q))
	}
}

func testKeysIn(t *testing.T, h Harness) {
	url := h.NewDatabase(t)
	db, data := shop(h.Namespace)
	load(t, h, url, db, data, dialect.LoadOptions{RunID: "keys-in"})
	source := open(t, h, url, dialect.Source)
	caps := source.Capabilities()
	snap := snapshot(t, source)
	users := schema.TableRef{Namespace: h.Namespace, Name: "users"}
	items := schema.TableRef{Namespace: h.Namespace, Name: "order_items"}
	id := []schema.Ident{"id"}

	// More keys than the smallest engine limit, plus keys that don't exist,
	// sent in batches the way the core sends them.
	var keys [][]value.Value
	for n := int64(40); n >= 2; n -= 2 {
		keys = append(keys, []value.Value{value.Int(n)})
	}
	var got [][]value.Value
	for batch := range slices.Chunk(keys, caps.KeysPerRead(1)) {
		got = append(got, readAll(t, snap, dialect.ReadQuery{
			Table: users, Columns: id, Where: dialect.KeysIn{Columns: id, Keys: batch},
		})...)
	}
	slices.SortFunc(got, func(a, b []value.Value) int { return cmp.Compare(a[0].Int(), b[0].Int()) })
	compareRows(t, "even user ids", intRows(2, 4, 6, 8, 10, 12), got)

	// One full batch is one read, so its order and its limit hold.
	full := keys[len(keys)-min(len(keys), caps.KeysPerRead(1)):] // ..., 6, 4, 2
	got = readAll(t, snap, dialect.ReadQuery{
		Table: users, Columns: id, OrderBy: id, Limit: 2,
		Where: dialect.KeysIn{Columns: id, Keys: full},
	})
	compareRows(t, "a full batch, ordered and limited", intRows(2, 4), got)

	// A read over the limit is refused, not split.
	over := make([][]value.Value, caps.KeysPerRead(1)+1)
	for i := range over {
		over[i] = []value.Value{value.Int(int64(i + 1))}
	}
	if it, err := snap.Read(context.Background(), dialect.ReadQuery{
		Table: users, Columns: id, OrderBy: id, Where: dialect.KeysIn{Columns: id, Keys: over},
	}); !errors.Is(err, dialect.ErrTooManyKeys) {
		if err == nil {
			_ = it.Close()
		}
		t.Errorf("a read over MaxKeyValues = %v, want ErrTooManyKeys", err)
	}

	pairs := [][]value.Value{
		{value.Int(1), value.Int(1)}, {value.Int(1), value.Int(2)}, {value.Int(3), value.Int(1)}, {value.Int(99), value.Int(1)},
	}
	got = nil
	for batch := range slices.Chunk(pairs, caps.KeysPerRead(2)) {
		got = append(got, readAll(t, snap, dialect.ReadQuery{
			Table: items, Columns: []schema.Ident{"order_id", "line"}, OrderBy: []schema.Ident{"order_id", "line"},
			Where: dialect.KeysIn{Columns: []schema.Ident{"order_id", "line"}, Keys: batch},
		})...)
	}
	compareRows(t, "composite keys", [][]value.Value{
		{value.Int(1), value.Int(1)}, {value.Int(1), value.Int(2)}, {value.Int(3), value.Int(1)},
	}, got)

	got = readAll(t, snap, dialect.ReadQuery{
		Table: users, Columns: id,
		Where: dialect.KeysIn{Columns: []schema.Ident{"nickname"}, Keys: [][]value.Value{{value.Null()}}},
	})
	if len(got) != 0 {
		t.Errorf("KeysIn with a NULL key matched %d rows; NULL must never match", len(got))
	}
}

func testKeysetPagination(t *testing.T, h Harness) {
	url := h.NewDatabase(t)
	db, data := shop(h.Namespace)
	load(t, h, url, db, data, dialect.LoadOptions{RunID: "keyset"})
	snap := snapshot(t, open(t, h, url, dialect.Source))
	users := schema.TableRef{Namespace: h.Namespace, Name: "users"}
	cols := columnNames(db.Table(users))
	id := []schema.Ident{"id"}

	var all [][]value.Value
	var last []value.Value
	for range 100 { // more than enough chunks; stops early
		q := dialect.ReadQuery{Table: users, Columns: cols, OrderBy: id, Limit: 5}
		if last != nil {
			q.Where = dialect.After{Columns: id, Key: last}
		}
		chunk := readAll(t, snap, q)
		if len(chunk) == 0 {
			break
		}
		if len(chunk) > 5 {
			t.Fatalf("a chunk returned %d rows, more than the limit of 5", len(chunk))
		}
		all = append(all, chunk...)
		last = []value.Value{chunk[len(chunk)-1][0]}
	}
	compareRows(t, "users read in chunks", data[users], all)
}

func testJoin(t *testing.T, h Harness) {
	url := h.NewDatabase(t)
	db, data := shop(h.Namespace)
	load(t, h, url, db, data, dialect.LoadOptions{RunID: "join"})
	source := open(t, h, url, dialect.Source)
	snap := snapshot(t, source)

	joined, err := snap.Join(context.Background())
	if source.Capabilities().Snapshot != dialect.SnapshotShared {
		if !errors.Is(err, dialect.ErrNotSupported) {
			t.Errorf("Join without a shared snapshot = %v, want ErrNotSupported", err)
		}
		return
	}
	if err != nil {
		t.Fatalf("Join with a shared snapshot: %v", err)
	}
	t.Cleanup(func() { _ = joined.Close() })
	users := schema.TableRef{Namespace: h.Namespace, Name: "users"}
	q := dialect.ReadQuery{Table: users, Columns: columnNames(db.Table(users)), OrderBy: []schema.Ident{"id"}}
	compareRows(t, "rows seen by a joined reader", readAll(t, snap, q), readAll(t, joined, q))
}

func testAbort(t *testing.T, h Harness) {
	url := h.NewDatabase(t)
	db, data := shop(h.Namespace)
	ctx := context.Background()
	l, err := open(t, h, url, dialect.Target).Loader(ctx, dialect.LoadOptions{RunID: "abort"})
	if err != nil {
		t.Fatalf("Loader: %v", err)
	}
	if err := l.CreateTables(ctx, db); err != nil {
		t.Fatalf("CreateTables: %v", err)
	}
	users := schema.TableRef{Namespace: h.Namespace, Name: "users"}
	if _, err := l.Load(ctx, users, columnNames(db.Table(users)), rowsOf(data[users])); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := l.Abort(ctx); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if err := l.Abort(ctx); err != nil {
		t.Errorf("a second Abort must be safe: %v", err)
	}
	got, err := open(t, h, url, dialect.Source).Introspect(ctx, dialect.Filter{})
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	for _, ns := range got.Namespaces {
		if len(ns.Tables) > 0 {
			t.Errorf("after Abort, namespace %q still has %d tables", ns.Name, len(ns.Tables))
		}
	}
}

func testReplace(t *testing.T, h Harness) {
	url := h.NewDatabase(t)
	db, data := shop(h.Namespace)
	load(t, h, url, db, data, dialect.LoadOptions{RunID: "first"})

	ctx := context.Background()
	users := schema.TableRef{Namespace: h.Namespace, Name: "users"}
	smaller := map[schema.TableRef][][]value.Value{users: data[users][:3]}
	l := prepare(t, h, url, db, smaller, dialect.LoadOptions{RunID: "second"})
	if err := l.Commit(ctx); err == nil {
		t.Fatal("Commit replaced existing tables although Replace was not set")
	}
	_ = l.Abort(ctx)

	load(t, h, url, db, smaller, dialect.LoadOptions{RunID: "third", Replace: true})
	snap := snapshot(t, open(t, h, url, dialect.Source))
	got := readAll(t, snap, dialect.ReadQuery{Table: users, Columns: columnNames(db.Table(users)), OrderBy: []schema.Ident{"id"}})
	compareRows(t, "users after Replace", data[users][:3], got)
}

func testForeignKeys(t *testing.T, h Harness) {
	url := h.NewDatabase(t)
	db, data := shop(h.Namespace)
	orders := schema.TableRef{Namespace: h.Namespace, Name: "orders"}
	broken := map[schema.TableRef][][]value.Value{
		{Namespace: h.Namespace, Name: "users"}: data[schema.TableRef{Namespace: h.Namespace, Name: "users"}],
		orders:                                  {{value.Int(1), value.Int(999), value.Null()}}, // user 999 does not exist
	}
	ctx := context.Background()
	l, err := open(t, h, url, dialect.Target).Loader(ctx, dialect.LoadOptions{RunID: "broken-fk"})
	if err != nil {
		t.Fatalf("Loader: %v", err)
	}
	t.Cleanup(func() { _ = l.Abort(ctx) })
	if err := l.CreateTables(ctx, db); err != nil {
		t.Fatalf("CreateTables: %v", err)
	}
	var loadErr error
	for ref, rows := range broken {
		if _, err := l.Load(ctx, ref, columnNames(db.Table(ref)), rowsOf(rows)); err != nil {
			loadErr = err
		}
	}
	if loadErr == nil && l.Finish(ctx, db) == nil {
		t.Error("a row referencing a missing parent was accepted by both Load and Finish")
	}
}

func testEmptyText(t *testing.T, h Harness) {
	ns := h.Namespace
	notes := schema.TableRef{Namespace: ns, Name: "notes"}
	db := &schema.Database{Namespaces: []*schema.Namespace{{Name: ns, Tables: []*schema.Table{{
		Name: "notes",
		Columns: []*schema.Column{
			{Name: "id", Type: intType},
			{Name: "required", Type: textType},
			{Name: "optional", Type: textType, Nullable: true},
		},
		PrimaryKey: &schema.Key{Columns: []schema.Ident{"id"}},
	}}}}}
	cols := []schema.Ident{"id", "required", "optional"}
	ctx := context.Background()

	url := h.NewDatabase(t)
	caps := open(t, h, url, dialect.Target).Capabilities()

	// '' in a NOT NULL column: kept as '' unless the engine stores '' as NULL.
	l := mustLoader(t, h, url, "empty-required")
	err := loadAll(ctx, l, db, map[schema.TableRef][][]value.Value{
		notes: {{value.Int(1), value.String(""), value.String("x")}},
	})
	if caps.EmptyStringIsNull {
		if err == nil {
			t.Error("EmptyStringIsNull is true, but '' was accepted in a NOT NULL column")
		}
		_ = l.Abort(ctx)
	} else if err != nil {
		_ = l.Abort(ctx)
		t.Fatalf("loading '' into a NOT NULL column: %v", err)
	} else {
		if err := l.Commit(ctx); err != nil {
			_ = l.Abort(ctx)
			t.Fatalf("Commit: %v", err)
		}
		snap := snapshot(t, open(t, h, url, dialect.Source))
		got := readAll(t, snap, dialect.ReadQuery{Table: notes, Columns: cols, OrderBy: []schema.Ident{"id"}})
		compareRows(t, "'' read back", [][]value.Value{{value.Int(1), value.String(""), value.String("x")}}, got)
		return
	}

	// '' in a nullable column comes back as NULL when the engine stores '' as NULL.
	url = h.NewDatabase(t)
	load(t, h, url, db, map[schema.TableRef][][]value.Value{
		notes: {{value.Int(1), value.String("x"), value.String("")}},
	}, dialect.LoadOptions{RunID: "empty-optional"})
	snap := snapshot(t, open(t, h, url, dialect.Source))
	got := readAll(t, snap, dialect.ReadQuery{Table: notes, Columns: cols, OrderBy: []schema.Ident{"id"}})
	compareRows(t, "'' in a nullable column", [][]value.Value{{value.Int(1), value.String("x"), value.Null()}}, got)
}

// --- helpers ---

func open(t *testing.T, h Harness, url string, role dialect.Role) dialect.Conn {
	t.Helper()
	conn, err := h.Dialect.Open(context.Background(), url, dialect.OpenOptions{Role: role, AppName: "stuntdb-dialecttest"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func snapshot(t *testing.T, conn dialect.Conn) dialect.Snapshot {
	t.Helper()
	s, err := conn.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func mustLoader(t *testing.T, h Harness, url, runID string) dialect.Loader {
	t.Helper()
	l, err := open(t, h, url, dialect.Target).Loader(context.Background(), dialect.LoadOptions{RunID: runID})
	if err != nil {
		t.Fatalf("Loader: %v", err)
	}
	return l
}

// prepare runs every step of a load except Commit.
func prepare(t *testing.T, h Harness, url string, db *schema.Database, data map[schema.TableRef][][]value.Value, opts dialect.LoadOptions) dialect.Loader {
	t.Helper()
	l, err := open(t, h, url, dialect.Target).Loader(context.Background(), opts)
	if err != nil {
		t.Fatalf("Loader: %v", err)
	}
	if err := loadAll(context.Background(), l, db, data); err != nil {
		_ = l.Abort(context.Background())
		t.Fatalf("loading: %v", err)
	}
	return l
}

func load(t *testing.T, h Harness, url string, db *schema.Database, data map[schema.TableRef][][]value.Value, opts dialect.LoadOptions) {
	t.Helper()
	l := prepare(t, h, url, db, data, opts)
	if err := l.Commit(context.Background()); err != nil {
		_ = l.Abort(context.Background())
		t.Fatalf("Commit: %v", err)
	}
}

func loadAll(ctx context.Context, l dialect.Loader, db *schema.Database, data map[schema.TableRef][][]value.Value) error {
	if err := l.CreateTables(ctx, db); err != nil {
		return err
	}
	for ref, rows := range data {
		n, err := l.Load(ctx, ref, columnNames(db.Table(ref)), rowsOf(rows))
		if err != nil {
			return err
		}
		if n != int64(len(rows)) {
			return errors.New("dialecttest: Load reported a different number of rows than it was given")
		}
	}
	return l.Finish(ctx, db)
}

// readAll copies every row, because a RowIter may reuse its slice.
func readAll(t *testing.T, s dialect.Snapshot, q dialect.ReadQuery) [][]value.Value {
	t.Helper()
	it, err := s.Read(context.Background(), q)
	if err != nil {
		t.Fatalf("Read %s: %v", q.Table, err)
	}
	defer func() { _ = it.Close() }()
	var rows [][]value.Value
	for it.Next() {
		rows = append(rows, slices.Clone(it.Row()))
	}
	if err := it.Err(); err != nil {
		t.Fatalf("Read %s: %v", q.Table, err)
	}
	return rows
}

// compareRows reports differences by position only, never by value, as no
// test output may contain row data.
func compareRows(t *testing.T, what string, want, got [][]value.Value) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: got %d rows, want %d", what, len(got), len(want))
		return
	}
	for i, wantRow := range want {
		gotRow := got[i]
		if len(gotRow) != len(wantRow) {
			t.Errorf("%s: row %d has %d values, want %d", what, i, len(gotRow), len(wantRow))
			continue
		}
		for j, g := range gotRow {
			if w := wantRow[j]; !g.Equal(w) {
				t.Errorf("%s: row %d, column %d differs (got a %s value, want a %s value)", what, i, j, g.Kind(), w.Kind())
			}
		}
	}
}

func compareStructure(t *testing.T, want, got *schema.Database) {
	t.Helper()
	for _, ns := range want.Namespaces {
		for _, wt := range ns.Tables {
			ref := schema.TableRef{Namespace: ns.Name, Name: wt.Name}
			gt := got.Table(ref)
			if gt == nil {
				t.Errorf("introspection is missing table %s", ref)
				continue
			}
			if !slices.Equal(columnNames(gt), columnNames(wt)) {
				t.Errorf("%s: columns = %v, want %v", ref, columnNames(gt), columnNames(wt))
				continue
			}
			for i, wc := range wt.Columns {
				gc := gt.Columns[i]
				if gc.Nullable != wc.Nullable {
					t.Errorf("%s.%s: nullable = %v, want %v", ref, wc.Name, gc.Nullable, wc.Nullable)
				}
				if gc.Type.Kind != wc.Type.Kind {
					t.Errorf("%s.%s: type = %s, want %s", ref, wc.Name, gc.Type, wc.Type)
				}
			}
			if gt.PrimaryKey == nil || !slices.Equal(gt.PrimaryKey.Columns, wt.PrimaryKey.Columns) {
				t.Errorf("%s: primary key differs", ref)
			}
			if len(gt.ForeignKeys) != len(wt.ForeignKeys) {
				t.Errorf("%s: %d foreign keys, want %d", ref, len(gt.ForeignKeys), len(wt.ForeignKeys))
				continue
			}
			for _, wfk := range wt.ForeignKeys {
				if !slices.ContainsFunc(gt.ForeignKeys, func(g *schema.ForeignKey) bool {
					return g.Ref == wfk.Ref && slices.Equal(g.Columns, wfk.Columns) && slices.Equal(g.RefColumns, wfk.RefColumns)
				}) {
					t.Errorf("%s: missing foreign key (%v) → %s", ref, wfk.Columns, wfk.Ref)
				}
			}
		}
	}
}

func columnNames(t *schema.Table) []schema.Ident {
	names := make([]schema.Ident, len(t.Columns))
	for i, c := range t.Columns {
		names[i] = c.Name
	}
	return names
}

func intRows(ids ...int64) [][]value.Value {
	rows := make([][]value.Value, len(ids))
	for i, id := range ids {
		rows[i] = []value.Value{value.Int(id)}
	}
	return rows
}

type sliceRows struct {
	rows [][]value.Value
	next int
}

func rowsOf(rows [][]value.Value) dialect.RowIter { return &sliceRows{rows: rows} }

func (s *sliceRows) Next() bool {
	if s.next >= len(s.rows) {
		return false
	}
	s.next++
	return true
}

func (s *sliceRows) Row() []value.Value { return s.rows[s.next-1] }
func (s *sliceRows) Err() error         { return nil }
func (s *sliceRows) Close() error       { return nil }
