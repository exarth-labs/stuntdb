package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/exarth-labs/stuntdb/dialect"
	"github.com/exarth-labs/stuntdb/internal/schema"
	"github.com/exarth-labs/stuntdb/internal/value"
)

// --- helpers ---

// rawExec runs SQL on a file directly through the driver, as another
// program would.
func rawExec(t *testing.T, path, script string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(script); err != nil {
		t.Fatalf("running SQL: %v", err)
	}
}

func openConn(t *testing.T, path string, role dialect.Role) dialect.Conn {
	t.Helper()
	c, err := Dialect{}.Open(context.Background(), "sqlite:"+path, dialect.OpenOptions{Role: role})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// copyDatabase copies src to dst through the SDK, the way stuntdb's engine
// will: introspect, create, read each table in key order, load, finish, commit.
func copyDatabase(t *testing.T, src, dst string, replace bool) *schema.Database {
	t.Helper()
	ctx := context.Background()
	source := openConn(t, src, dialect.Source)
	db, err := source.Introspect(ctx, dialect.Filter{})
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	snap, err := source.Snapshot(ctx)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	defer func() { _ = snap.Close() }()

	l, err := openConn(t, dst, dialect.Target).Loader(ctx, dialect.LoadOptions{RunID: "copy", Replace: replace})
	if err != nil {
		t.Fatalf("Loader: %v", err)
	}
	defer func() { _ = l.Abort(ctx) }()
	if err := l.CreateTables(ctx, db); err != nil {
		t.Fatalf("CreateTables: %v", err)
	}
	for _, ns := range db.Namespaces {
		for _, tbl := range ns.Tables {
			var cols []schema.Ident
			for _, c := range tbl.Columns {
				if c.Generated == nil {
					cols = append(cols, c.Name)
				}
			}
			ref := schema.TableRef{Namespace: ns.Name, Name: tbl.Name}
			it, err := snap.Read(ctx, dialect.ReadQuery{Table: ref, Columns: cols, OrderBy: tbl.IdentityKey()})
			if err != nil {
				t.Fatalf("Read %s: %v", ref, err)
			}
			if _, err := l.Load(ctx, ref, cols, it); err != nil {
				t.Fatalf("Load %s: %v", ref, err)
			}
			if err := it.Err(); err != nil {
				t.Fatalf("Read %s: %v", ref, err)
			}
			_ = it.Close()
		}
	}
	if err := l.Finish(ctx, db); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if err := l.Commit(ctx); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return db
}

// dump returns every table's rows, each encoded exactly (storage class and
// bytes), sorted, so two databases can be compared without showing values.
func dump(t *testing.T, path string) map[string][]string {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	tables := queryStrings(t, db, `SELECT name FROM sqlite_schema WHERE type = 'table' AND name NOT LIKE 'sqlite_%' AND sql NOT LIKE 'CREATE VIRTUAL%'`)
	out := map[string][]string{}
	for _, table := range tables {
		cols := queryStrings(t, db, `SELECT name FROM pragma_table_xinfo(?)`, table)
		var exprs []string
		for _, c := range cols {
			q := quote(schema.Ident(c))
			exprs = append(exprs, "typeof("+q+") || ':' || CASE typeof("+q+") WHEN 'real' THEN printf('%!.17g', "+q+
				") WHEN 'integer' THEN CAST("+q+" AS TEXT) WHEN 'null' THEN '' ELSE hex("+q+") END")
		}
		rows := queryStrings(t, db, "SELECT "+strings.Join(exprs, " || '|' || ")+" FROM "+quote(schema.Ident(table)))
		slices.Sort(rows)
		out[table] = rows
	}
	return out
}

func queryStrings(t *testing.T, db *sql.DB, query string, args ...any) []string {
	t.Helper()
	rows, err := db.Query(query, args...)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func fileHash(t *testing.T, path string) [32]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(data)
}

func readFixture(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "hard_schema.sql"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// --- tests ---

func TestCopyHardSchema(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src.db"), filepath.Join(dir, "dst.db")
	rawExec(t, src, readFixture(t))
	before := fileHash(t, src)

	copyDatabase(t, src, dst, false)

	if fileHash(t, src) != before {
		t.Error("the copy changed the source file")
	}
	srcDB, err := sql.Open("sqlite", src)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srcDB.Close() }()
	dstDB, err := sql.Open("sqlite", dst)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dstDB.Close() }()

	// The same objects, with the same definitions.
	const catalogQuery = `SELECT type || ' ' || name || ' ' || tbl_name || ' ' || coalesce(sql, '') FROM sqlite_schema ORDER BY type, name`
	if got, want := queryStrings(t, dstDB, catalogQuery), queryStrings(t, srcDB, catalogQuery); !slices.Equal(got, want) {
		t.Errorf("the copy's catalog differs:\n got %d entries\nwant %d entries", len(got), len(want))
		for _, e := range want {
			if !slices.Contains(got, e) {
				t.Errorf("missing: %s", e)
			}
		}
	}

	// The same rows, byte for byte. No values are printed.
	got, want := dump(t, dst), dump(t, src)
	for table, rows := range want {
		if len(got[table]) != len(rows) {
			t.Errorf("%s: %d rows, want %d", table, len(got[table]), len(rows))
			continue
		}
		for i := range rows {
			if got[table][i] != rows[i] {
				t.Errorf("%s: sorted row %d differs", table, i)
			}
		}
	}
	if len(got["audit"]) != 0 {
		t.Error("the users_audit trigger fired while the copy loaded")
	}

	if res := queryStrings(t, dstDB, "PRAGMA integrity_check"); !slices.Equal(res, []string{"ok"}) {
		t.Errorf("integrity_check: %d problems", len(res))
	}
	if res := queryStrings(t, dstDB, `SELECT "table" FROM pragma_foreign_key_check`); len(res) != 0 {
		t.Errorf("foreign_key_check: %d broken rows", len(res))
	}
	// AUTOINCREMENT continues past the copied rows.
	var id int64
	if err := dstDB.QueryRow(`INSERT INTO "Accounts" (name) VALUES ('New') RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if id <= 2 {
		t.Errorf("a new account got id %d, which a copied row already uses", id)
	}
}

func TestIntrospectHardSchema(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src.db")
	rawExec(t, src, readFixture(t))
	db, err := openConn(t, src, dialect.Source).Introspect(context.Background(), dialect.Filter{})
	if err != nil {
		t.Fatalf("Introspect: %v", err)
	}
	ns := db.Namespace("main")
	if ns == nil {
		t.Fatal("no main namespace")
	}
	var names []string
	for _, tbl := range ns.Tables {
		names = append(names, string(tbl.Name))
	}
	if want := []string{"Accounts", "users", "order", "order_notes", "measurements", "untyped", "cyc_a", "cyc_b", "audit"}; !slices.Equal(names, want) {
		t.Errorf("tables = %v, want %v (in creation order, without sqlite_sequence)", names, want)
	}

	users := ns.Table("users")
	if id := users.Column("id"); id.Identity == nil || id.Nullable {
		t.Error("users.id should be a NOT NULL identity (INTEGER PRIMARY KEY)")
	}
	if g := users.Column("email_domain").Generated; g == nil || g.Stored {
		t.Error("users.email_domain should be a virtual generated column")
	}
	if g := users.Column("email_upper").Generated; g == nil || !g.Stored {
		t.Error("users.email_upper should be a stored generated column")
	}
	if c := users.Column("balance"); c.Type.Kind != value.TypeDecimal || c.NativeType != "DECIMAL(12, 2)" {
		t.Errorf("users.balance is %s (%q)", c.Type, c.NativeType)
	}
	if d := users.Column("active").Default; d == nil || d.Kind != schema.DefaultLiteral {
		t.Error("users.active should have a literal default")
	}
	if d := ns.Table("Accounts").Column("created_at").Default; d == nil || d.Kind != schema.DefaultNow {
		t.Error("Accounts.created_at should default to the current time")
	}
	if len(users.Uniques) != 1 || !slices.Equal(users.Uniques[0].Columns, ids("email")) {
		t.Errorf("users unique constraints = %v", users.Uniques)
	}
	fks := map[schema.Ident]*schema.ForeignKey{}
	for _, fk := range users.ForeignKeys {
		fks[fk.Columns[0]] = fk
	}
	if fk := fks["account_id"]; fk == nil || fk.Ref.Name != "Accounts" || fk.OnDelete != schema.Cascade {
		t.Error("users.account_id should reference Accounts, spelled as the table is, ON DELETE CASCADE")
	}
	if fk := fks["referred_by"]; fk == nil || fk.Ref.Name != "users" || !slices.Equal(fk.RefColumns, ids("id")) {
		t.Error("users.referred_by should reference users(id), the implied primary key")
	}
	byName := map[schema.Ident]*schema.Index{}
	for _, ix := range users.Indexes {
		byName[ix.Name] = ix
	}
	if ix := byName["users_active_email"]; ix == nil || !ix.Partial || ix.Expression || ix.Native == "" {
		t.Error("users_active_email should be a partial index with its native definition")
	}
	if ix := byName["users_lower_email"]; ix == nil || !ix.Expression || len(ix.Columns) != 0 {
		t.Error("users_lower_email should be a pure expression index")
	}

	order := ns.Table("order")
	if !slices.Equal(order.PrimaryKey.Columns, ids("Order Id", "line")) || order.Column("Order Id").Nullable {
		t.Error(`"order" should have the NOT NULL primary key ("Order Id", line)`)
	}
	notes := ns.Table("order_notes")
	if len(notes.ForeignKeys) != 1 || !slices.Equal(notes.ForeignKeys[0].RefColumns, ids("Order Id", "line")) {
		t.Error("order_notes should have one composite foreign key")
	}
	if notes.IdentityKey() != nil {
		t.Error("order_notes has no key, so no identity key")
	}

	kinds := map[schema.ObjectKind]int{}
	for _, obj := range ns.Objects {
		kinds[obj.Kind]++
	}
	if kinds[schema.ObjectView] != 2 || kinds[schema.ObjectTrigger] != 1 {
		t.Errorf("objects: %d views and %d triggers, want 2 and 1", kinds[schema.ObjectView], kinds[schema.ObjectTrigger])
	}
}

// Values at the edges go through a load and come back equal. The table is
// built from the canonical model, as for a source from another engine.
func TestValueEdges(t *testing.T) {
	ts := time.Date(2026, 10, 19, 13, 45, 0, 123456789, time.UTC)
	big := strings.Repeat("x", 1<<20)
	col := func(name string, typ value.Type) *schema.Column {
		return &schema.Column{Name: schema.Ident(name), Type: typ, Nullable: true}
	}
	tbl := &schema.Table{
		Name: "edges",
		Columns: []*schema.Column{
			{Name: "id", Type: value.Type{Kind: value.TypeInt, Bits: 64}},
			col("b", value.Type{Kind: value.TypeBool}),
			col("i", value.Type{Kind: value.TypeInt, Bits: 64}),
			col("f", value.Type{Kind: value.TypeFloat, Bits: 64}),
			col("d", value.Type{Kind: value.TypeDecimal, Precision: 12, Scale: 4}),
			col("s", value.Type{Kind: value.TypeString, MaxLen: 50}),
			col("x", value.Type{Kind: value.TypeText}),
			col("bin", value.Type{Kind: value.TypeBinary}),
			col("dt", value.Type{Kind: value.TypeDate}),
			col("tm", value.Type{Kind: value.TypeTime, Precision: 9}),
			col("tmz", value.Type{Kind: value.TypeTime, Precision: 9, TZ: true}),
			col("ts", value.Type{Kind: value.TypeTimestamp, Precision: 9}),
			col("tsz", value.Type{Kind: value.TypeTimestamp, Precision: 9, TZ: true}),
			col("u", value.Type{Kind: value.TypeUUID}),
			col("j", value.Type{Kind: value.TypeJSON}),
		},
		PrimaryKey: &schema.Key{Columns: ids("id")},
	}
	must := func(v value.Value, err error) value.Value {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	uuid := must(value.ParseUUID("123e4567-e89b-12d3-a456-426614174000"))
	nulls := make([]value.Value, len(tbl.Columns))
	rows := [][]value.Value{
		{
			value.Int(1), value.Bool(true), value.Int(math.MinInt64), value.Float(math.MaxFloat64),
			must(value.Decimal("-12345678.1234")), value.String(""), value.String("a\x00b"), value.Bytes(nil),
			must(value.Date(-43, 3, 15)), must(value.TimeOfDay(0)), must(value.TimeOfDayWithZone(13*time.Hour, 5*3600+1800)),
			value.Timestamp(ts), value.TimestampTZ(ts), uuid, value.String(`{"a":[1,2]}`),
		},
		{
			value.Int(2), value.Bool(false), value.Int(math.MaxInt64), value.Float(math.Inf(-1)),
			must(value.Decimal("0.0001")), value.String("naïve ☃"), value.String("\xff\xfe"), value.Bytes([]byte{0, 0, 1}),
			must(value.Date(12026, 1, 1)), must(value.TimeOfDay(24 * time.Hour)), must(value.TimeOfDayWithZone(0, -12*3600)),
			value.Timestamp(time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC)), value.TimestampTZWithZone(ts.In(time.FixedZone("", -(3*3600 + 30*60)))),
			uuid, value.String("[]"),
		},
		{
			value.Int(3), value.Bool(true), value.Int(0), value.Float(math.SmallestNonzeroFloat64),
			must(value.Decimal("100")), value.String(strings.Repeat("é", 50)), value.String(big), value.Bytes([]byte(big)),
			must(value.Date(2026, 2, 28)), must(value.TimeOfDay(time.Nanosecond)), nulls[0],
			nulls[0], nulls[0], nulls[0], nulls[0],
		},
		append([]value.Value{value.Int(4)}, nulls[1:]...),
	}

	path := filepath.Join(t.TempDir(), "edges.db")
	ctx := context.Background()
	db := &schema.Database{Namespaces: []*schema.Namespace{{Name: "main", Tables: []*schema.Table{tbl}}}}
	l, err := openConn(t, path, dialect.Target).Loader(ctx, dialect.LoadOptions{RunID: "edges"})
	if err != nil {
		t.Fatal(err)
	}
	ref := schema.TableRef{Namespace: "main", Name: "edges"}
	cols := make([]schema.Ident, len(tbl.Columns))
	for i, c := range tbl.Columns {
		cols[i] = c.Name
	}
	if err := l.CreateTables(ctx, db); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Load(ctx, ref, cols, &sliceRows{rows: rows}); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := l.Finish(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := l.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	snap, err := openConn(t, path, dialect.Source).Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = snap.Close() }()
	it, err := snap.Read(ctx, dialect.ReadQuery{Table: ref, Columns: cols, OrderBy: ids("id")})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = it.Close() }()
	n := 0
	for it.Next() {
		for j, got := range it.Row() {
			if want := rows[n][j]; !got.Equal(want) {
				t.Errorf("row %d, column %s: got a %s value, want an equal %s value", n, cols[j], got.Kind(), want.Kind())
			}
		}
		n++
	}
	if err := it.Err(); err != nil {
		t.Fatal(err)
	}
	if n != len(rows) {
		t.Errorf("read %d rows, want %d", n, len(rows))
	}
}

// SQLite keeps a decimal in a NUMERIC column as a REAL, so a decimal with
// more digits than a float holds comes back as the nearest float. This is
// SQLite's own behavior; the test pins it down.
func TestDecimalsAreFloatsInSQLite(t *testing.T) {
	v, err := value.Decimal("123456789012345678901234567890.5")
	if err != nil {
		t.Fatal(err)
	}
	stored, err := toStorage(v)
	if err != nil {
		t.Fatal(err)
	}
	back, err := fromStorage(stored, columnType("DECIMAL"))
	if err != nil {
		t.Fatal(err)
	}
	if back.Kind() != value.KindDecimal || back.DecimalText() != "123456789012345680000000000000" {
		t.Errorf("the decimal did not come back as its nearest float")
	}
}

func TestSnapshotIsolation(t *testing.T) {
	for _, mode := range []string{"WAL", "DELETE"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "live.db")
			rawExec(t, path, "PRAGMA journal_mode = "+mode+"; CREATE TABLE t (id INTEGER PRIMARY KEY); INSERT INTO t VALUES (1), (2), (3);")
			ctx := context.Background()
			source := openConn(t, path, dialect.Source)
			snap, err := source.Snapshot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			count := func(s dialect.Snapshot) int {
				t.Helper()
				it, err := s.Read(ctx, dialect.ReadQuery{Table: schema.TableRef{Namespace: "main", Name: "t"}, Columns: ids("id")})
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = it.Close() }()
				n := 0
				for it.Next() {
					n++
				}
				if err := it.Err(); err != nil {
					t.Fatal(err)
				}
				return n
			}

			writer, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = writer.Close() }()
			_, writeErr := writer.Exec("INSERT INTO t VALUES (4), (5)")
			switch mode {
			case "WAL":
				if writeErr != nil {
					t.Fatalf("in WAL mode, a writer should carry on during a snapshot: %v", writeErr)
				}
			default:
				if writeErr == nil {
					t.Fatal("in rollback-journal mode, a writer should have to wait for the snapshot")
				}
			}
			if n := count(snap); n != 3 {
				t.Errorf("the snapshot sees %d rows, want the 3 from when it started", n)
			}
			if err := snap.Close(); err != nil {
				t.Fatal(err)
			}
			if mode != "WAL" {
				if _, err := writer.Exec("INSERT INTO t VALUES (4), (5)"); err != nil {
					t.Fatalf("after the snapshot, the writer should succeed: %v", err)
				}
			}
			fresh, err := source.Snapshot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = fresh.Close() }()
			if n := count(fresh); n != 5 {
				t.Errorf("a new snapshot sees %d rows, want 5", n)
			}
		})
	}
}

func TestSourceIsReadOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "src.db")
	rawExec(t, path, "CREATE TABLE t (id INTEGER PRIMARY KEY); INSERT INTO t VALUES (1);")
	before := fileHash(t, path)
	c := openConn(t, path, dialect.Source).(*conn)
	for _, stmt := range []string{
		"INSERT INTO t VALUES (2)",
		"CREATE TABLE u (a)",
		"CREATE TEMP TABLE u (a)",
		"PRAGMA user_version = 7",
	} {
		if _, err := c.db.Exec(stmt); err == nil {
			t.Errorf("a source connection ran %q", stmt)
		}
	}
	if fileHash(t, path) != before {
		t.Error("the source file changed")
	}
}

// Long key lists are read in batches, as the core sends them, and a read
// over the limit is refused rather than split.
func TestReadKeysInBatches(t *testing.T) {
	path := filepath.Join(t.TempDir(), "src.db")
	rawExec(t, path, "CREATE TABLE t (a INTEGER, b TEXT, PRIMARY KEY (a, b)); INSERT INTO t VALUES (1, 'x'), (2, 'y'), (3, 'z'), (4, 'w'), (5, 'v');")
	snap, err := openConn(t, path, dialect.Source).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = snap.Close() }()
	const limit = 4
	snap.(*snapshot).maxKeys = limit
	table := schema.TableRef{Namespace: "main", Name: "t"}
	cols := ids("a", "b")
	keys := [][]value.Value{
		{value.Int(1), value.String("x")},
		{value.Int(2), value.String("nope")},
		{value.Int(3), value.String("z")},
		{value.Int(5), value.String("v")},
		{value.Int(9), value.String("x")},
	}

	var got []int64
	for batch := range slices.Chunk(keys, dialect.Capabilities{MaxKeyValues: limit}.KeysPerRead(len(cols))) {
		it, err := snap.Read(context.Background(), dialect.ReadQuery{
			Table: table, Columns: ids("a"), OrderBy: cols, Where: dialect.KeysIn{Columns: cols, Keys: batch},
		})
		if err != nil {
			t.Fatal(err)
		}
		for it.Next() {
			got = append(got, it.Row()[0].Int())
		}
		if err := it.Err(); err != nil {
			t.Fatal(err)
		}
		_ = it.Close()
	}
	if !slices.Equal(got, []int64{1, 3, 5}) {
		t.Errorf("matched keys %v, want [1 3 5] in order", got)
	}

	if _, err := snap.Read(context.Background(), dialect.ReadQuery{
		Table: table, Columns: ids("a"), Where: dialect.KeysIn{Columns: cols, Keys: keys},
	}); !errors.Is(err, dialect.ErrTooManyKeys) {
		t.Errorf("a read over the limit = %v, want ErrTooManyKeys", err)
	}
}

func TestLoaderRules(t *testing.T) {
	ctx := context.Background()
	db := &schema.Database{Namespaces: []*schema.Namespace{{Name: "main", Tables: []*schema.Table{{
		Name: "t",
		Columns: []*schema.Column{
			{Name: "id", Type: value.Type{Kind: value.TypeInt, Bits: 64}},
			{Name: "email", Type: value.Type{Kind: value.TypeText}},
		},
		PrimaryKey: &schema.Key{Columns: ids("id")},
		Uniques:    []*schema.Key{{Columns: ids("email")}},
	}}}}}
	ref := schema.TableRef{Namespace: "main", Name: "t"}
	cols := ids("id", "email")

	t.Run("BadRunID", func(t *testing.T) {
		target := openConn(t, filepath.Join(t.TempDir(), "app.db"), dialect.Target)
		for _, id := range []string{"", "../x", `a\b`, "a/b", "c:x"} {
			if l, err := target.Loader(ctx, dialect.LoadOptions{RunID: id}); err == nil {
				_ = l.Abort(ctx)
				t.Errorf("run ID %q was accepted", id)
			}
		}
	})

	t.Run("StagingFileLifecycle", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "app.db")
		staging := filepath.Join(dir, ".app.db.partial-run1")
		target := openConn(t, path, dialect.Target)
		l, err := target.Loader(ctx, dialect.LoadOptions{RunID: "run1"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(staging); err != nil {
			t.Fatalf("the staging file is missing: %v", err)
		}
		if l2, err := target.Loader(ctx, dialect.LoadOptions{RunID: "run1"}); err == nil {
			_ = l2.Abort(ctx)
			t.Error("a second loader with the same run ID shared the staging file")
		}
		if err := l.Commit(ctx); err == nil {
			t.Error("Commit before Finish succeeded")
		}
		if _, err := l.Load(ctx, ref, cols, &sliceRows{}); err == nil {
			t.Error("Load before CreateTables succeeded")
		}
		if err := l.Abort(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(staging); !os.IsNotExist(err) {
			t.Errorf("the staging file is still there after Abort: %v", err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("Abort created the target: %v", err)
		}
	})

	t.Run("FailedLoadCanOnlyAbort", func(t *testing.T) {
		const canary = "canary-7f3a@example.com"
		path := filepath.Join(t.TempDir(), "app.db")
		l, err := openConn(t, path, dialect.Target).Loader(ctx, dialect.LoadOptions{RunID: "dup"})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = l.Abort(ctx) }()
		if err := l.CreateTables(ctx, db); err != nil {
			t.Fatal(err)
		}
		dup := [][]value.Value{{value.Int(1), value.String(canary)}, {value.Int(2), value.String(canary)}}
		_, err = l.Load(ctx, ref, cols, &sliceRows{rows: dup})
		if err == nil {
			t.Fatal("a duplicate in a UNIQUE column was accepted")
		}
		if strings.Contains(err.Error(), canary) {
			t.Errorf("the error contains a row value: %q", err)
		}
		if err := l.Finish(ctx, db); err == nil {
			t.Error("Finish after a failed load succeeded")
		}
		if err := l.Commit(ctx); err == nil {
			t.Error("Commit after a failed load succeeded")
		}
	})

	t.Run("UnstorableValue", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "app.db")
		l, err := openConn(t, path, dialect.Target).Loader(ctx, dialect.LoadOptions{RunID: "nan"})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = l.Abort(ctx) }()
		if err := l.CreateTables(ctx, db); err != nil {
			t.Fatal(err)
		}
		bad := [][]value.Value{{value.Int(1), value.Float(math.NaN())}}
		if _, err := l.Load(ctx, ref, cols, &sliceRows{rows: bad}); !errors.Is(err, errCannotStore) {
			t.Errorf("loading a NaN = %v, want errCannotStore", err)
		}
	})

	t.Run("CommitRefusesAnOpenTarget", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "app.db")
		rawExec(t, path, "CREATE TABLE old (a)")
		if err := os.WriteFile(path+"-wal", []byte("not empty"), 0o600); err != nil {
			t.Fatal(err)
		}
		l, err := openConn(t, path, dialect.Target).Loader(ctx, dialect.LoadOptions{RunID: "wal", Replace: true})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = l.Abort(ctx) }()
		if err := l.CreateTables(ctx, db); err != nil {
			t.Fatal(err)
		}
		if err := l.Finish(ctx, db); err != nil {
			t.Fatal(err)
		}
		if err := l.Commit(ctx); err == nil {
			t.Error("Commit replaced a target that has a WAL file")
		}
	})

	t.Run("ReplaceSwapsTheWholeFile", func(t *testing.T) {
		dir := t.TempDir()
		src, dst := filepath.Join(dir, "src.db"), filepath.Join(dir, "dst.db")
		rawExec(t, src, "CREATE TABLE t (id INTEGER PRIMARY KEY); INSERT INTO t VALUES (1);")
		rawExec(t, dst, "CREATE TABLE stale (a); INSERT INTO stale VALUES (1);")
		copyDatabase(t, src, dst, true)
		got := dump(t, dst)
		if _, ok := got["stale"]; ok || len(got["t"]) != 1 {
			t.Errorf("after Replace the target has tables %v", slices.Collect(mapKeys(got)))
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if strings.Contains(e.Name(), ".partial-") {
				t.Errorf("a staging file is left behind: %s", e.Name())
			}
		}
	})
}

func mapKeys[V any](m map[string]V) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

func TestOpenErrors(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "secret-folder-7f3a")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	garbage := filepath.Join(dir, "garbage.db")
	if err := os.WriteFile(garbage, []byte(strings.Repeat("not a database ", 100)), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		path string
		role dialect.Role
	}{
		{"missing source", filepath.Join(dir, "missing.db"), dialect.Source},
		{"folder as source", dir, dialect.Source},
		{"not a database", garbage, dialect.Source},
		{"missing target folder", filepath.Join(dir, "no-such-folder", "app.db"), dialect.Target},
		{"folder as target", dir, dialect.Target},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := Dialect{}.Open(ctx, "sqlite:"+tt.path, dialect.OpenOptions{Role: tt.role})
			if err == nil {
				_ = c.Close()
				t.Fatal("Open succeeded")
			}
			if strings.Contains(err.Error(), "secret-folder-7f3a") {
				t.Errorf("the error contains the path: %q", err)
			}
		})
	}
}

func TestTargetIntrospect(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "app.db")
	db, err := openConn(t, path, dialect.Target).Introspect(ctx, dialect.Filter{})
	if err != nil {
		t.Fatalf("Introspect of a missing target: %v", err)
	}
	if ns := db.Namespace("main"); ns == nil || len(ns.Tables) != 0 {
		t.Error("a missing target should look like an empty database")
	}
	rawExec(t, path, "CREATE TABLE t (a)")
	db, err = openConn(t, path, dialect.Target).Introspect(ctx, dialect.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if db.Table(schema.TableRef{Namespace: "main", Name: "t"}) == nil {
		t.Error("the target's table is missing")
	}
	if _, err := openConn(t, path, dialect.Target).Introspect(ctx, dialect.Filter{Namespaces: ids("public")}); err == nil {
		t.Error("a namespace other than main was accepted")
	}
}

func TestRegistered(t *testing.T) {
	for _, url := range []string{"sqlite:app.db", "sqlite3:app.db", "SQLITE:app.db"} {
		d, err := dialect.Lookup(url)
		if err != nil || d.Name() != "sqlite" {
			t.Errorf("Lookup(%q) = %v, %v", url, d, err)
		}
	}
	if !slices.Contains(dialect.Registered(), "sqlite") {
		t.Error("sqlite is not registered")
	}
}

type sliceRows struct {
	rows [][]value.Value
	next int
}

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
