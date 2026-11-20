package engine

import (
	"bytes"
	"context"
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/exarth-labs/stuntdb/dialect"
	"github.com/exarth-labs/stuntdb/internal/fakedb"
	"github.com/exarth-labs/stuntdb/internal/mask"
	"github.com/exarth-labs/stuntdb/internal/schema"
	"github.com/exarth-labs/stuntdb/internal/value"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

var testKeys = func() *mask.Keys {
	k, err := mask.NewKeys(bytes.Repeat([]byte{7}, mask.MinKeyLength))
	if err != nil {
		panic(err)
	}
	return k
}()

var (
	tInt64     = value.Type{Kind: value.TypeInt, Bits: 64}
	tText      = value.Type{Kind: value.TypeText}
	tDecimal   = value.Type{Kind: value.TypeDecimal, Precision: 12, Scale: 2}
	tDate      = value.Type{Kind: value.TypeDate}
	tTimestamp = value.Type{Kind: value.TypeTimestamp, Precision: 6}
	tUUID      = value.Type{Kind: value.TypeUUID}
)

const ns = "APP"

func ref(name schema.Ident) schema.TableRef { return schema.TableRef{Namespace: ns, Name: name} }

func ids(names ...string) []schema.Ident {
	out := make([]schema.Ident, len(names))
	for i, n := range names {
		out[i] = schema.Ident(n)
	}
	return out
}

func pk(names ...string) *schema.Key { return &schema.Key{Columns: ids(names...)} }

var aDefault = &schema.Default{Kind: schema.DefaultLiteral, Expr: "0"}

// shape is a test table: its structure, its masking rules, and its rows,
// computed from the row number.
type shape struct {
	table *schema.Table
	rules map[schema.Ident]mask.Rule
	row   func(i int) []value.Value

	// generated are computed by the database. The fake can't compute
	// them, so the source stores them as plain columns.
	generated []schema.Ident
}

func text(format string, i int) value.Value { return value.String(fmt.Sprintf(format, i)) }

func decimal(i int) value.Value {
	v, err := value.Decimal(fmt.Sprintf("%d.%02d", i/7, i%100))
	if err != nil {
		panic(err)
	}
	return v
}

func date(i int) value.Value {
	t := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, i)
	v, err := value.Date(t.Year(), t.Month(), t.Day())
	if err != nil {
		panic(err)
	}
	return v
}

func nullEvery(n int, i int, v value.Value) value.Value {
	if i%n == 0 {
		return value.Null()
	}
	return v
}

// shapes covers every kind of key the engine pages along, a key it masks,
// a key it reads but doesn't write, no key, and no written column.
func shapes() map[string]shape {
	return map[string]shape{
		"INT_KEY": {
			table: &schema.Table{Columns: []*schema.Column{
				{Name: "ID", Type: tInt64},
				{Name: "EMAIL", Type: tText},
				{Name: "BORN", Type: tDate, Nullable: true},
				{Name: "NOTE", Type: tText, Nullable: true},
			}, PrimaryKey: pk("ID")},
			rules: map[schema.Ident]mask.Rule{"ID": {Use: "keep"}, "EMAIL": {Use: "email"}, "BORN": {Use: "date_shift", Per: "ID"}, "NOTE": {Use: "keep"}},
			row: func(i int) []value.Value {
				return []value.Value{value.Int(int64(i) * 3), text("person-%d@corp.invalid", i), nullEvery(5, i, date(i)), nullEvery(3, i, text("note %d", i))}
			},
		},
		"TEXT_KEY": {
			table: &schema.Table{Columns: []*schema.Column{
				{Name: "CODE", Type: tText},
				{Name: "LABEL", Type: tText},
			}, PrimaryKey: pk("CODE")},
			rules: map[schema.Ident]mask.Rule{"CODE": {Use: "keep"}, "LABEL": {Use: "hash"}},
			// "c-10" sorts before "c-9" as text: key order isn't number order.
			row: func(i int) []value.Value { return []value.Value{text("c-%d", i), text("label %d", i)} },
		},
		"DECIMAL_KEY": {
			table: &schema.Table{Columns: []*schema.Column{
				{Name: "AMOUNT", Type: tDecimal},
				{Name: "QTY", Type: tInt64},
			}, PrimaryKey: pk("AMOUNT")},
			rules: map[schema.Ident]mask.Rule{"AMOUNT": {Use: "keep"}, "QTY": {Use: "noise"}},
			row:   func(i int) []value.Value { return []value.Value{decimal(i), value.Int(int64(i) * 1000)} },
		},
		"DATE_KEY": {
			table: &schema.Table{Columns: []*schema.Column{
				{Name: "DAY", Type: tDate},
				{Name: "N", Type: tInt64},
			}, PrimaryKey: pk("DAY")},
			rules: map[schema.Ident]mask.Rule{"DAY": {Use: "keep"}, "N": {Use: "keep"}},
			row:   func(i int) []value.Value { return []value.Value{date(i * 40), value.Int(int64(i))} },
		},
		"TIMESTAMP_KEY": {
			table: &schema.Table{Columns: []*schema.Column{
				{Name: "AT", Type: tTimestamp},
				{Name: "NOTE", Type: tText},
			}, PrimaryKey: pk("AT")},
			rules: map[schema.Ident]mask.Rule{"AT": {Use: "keep"}, "NOTE": {Use: "redact"}},
			row: func(i int) []value.Value {
				at := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * 90 * time.Minute)
				return []value.Value{value.Timestamp(at), text("note %d", i)}
			},
		},
		"UUID_KEY": {
			table: &schema.Table{Columns: []*schema.Column{
				{Name: "ID", Type: tUUID},
				{Name: "NAME", Type: tText},
			}, PrimaryKey: pk("ID")},
			rules: map[schema.Ident]mask.Rule{"ID": {Use: "hash"}, "NAME": {Use: "full_name"}},
			row: func(i int) []value.Value {
				var u [16]byte
				u[0], u[15] = byte(i*37), byte(i)
				return []value.Value{value.UUID(u), text("name %d", i)}
			},
		},
		"COMPOSITE_KEY": {
			table: &schema.Table{Columns: []*schema.Column{
				{Name: "A", Type: tInt64},
				{Name: "B", Type: tText},
				{Name: "C", Type: tText, Nullable: true},
			}, PrimaryKey: pk("A", "B")},
			rules: map[schema.Ident]mask.Rule{"A": {Use: "keep"}, "B": {Use: "keep"}, "C": {Use: "keep"}},
			row: func(i int) []value.Value {
				return []value.Value{value.Int(int64(i % 4)), text("b-%d", i), nullEvery(4, i, text("c %d", i))}
			},
		},
		// The key is masked, so paging must pass back the source's keys.
		"MASKED_KEY": {
			table: &schema.Table{Columns: []*schema.Column{
				{Name: "ID", Type: tInt64},
				{Name: "NAME", Type: tText},
			}, PrimaryKey: pk("ID")},
			rules: map[schema.Ident]mask.Rule{"ID": {Use: "fpe"}, "NAME": {Use: "keep"}},
			row:   func(i int) []value.Value { return []value.Value{value.Int(int64(100_000 + i*11)), text("n %d", i)} },
		},
		// The key is a generated column: read for paging, never written.
		"UNWRITTEN_KEY": {
			table: &schema.Table{Columns: []*schema.Column{
				{Name: "ID", Type: tInt64},
				{Name: "NAME", Type: tText},
			}, Uniques: []*schema.Key{pk("ID")}},
			generated: ids("ID"),
			rules:     map[schema.Ident]mask.Rule{"NAME": {Use: "keep"}},
			row:       func(i int) []value.Value { return []value.Value{value.Int(int64(i)), text("n %d", i)} },
		},
		"NO_KEY": {
			table: &schema.Table{Columns: []*schema.Column{
				{Name: "A", Type: tText},
				{Name: "B", Type: tInt64, Nullable: true},
			}},
			rules: map[schema.Ident]mask.Rule{"A": {Use: "keep"}, "B": {Use: "keep"}},
			row: func(i int) []value.Value {
				return []value.Value{text("same %d", i%3), nullEvery(2, i, value.Int(int64(i)))}
			},
		},
		"ONLY_DEFAULTS": {
			table: &schema.Table{Columns: []*schema.Column{
				{Name: "A", Type: tInt64, Default: aDefault},
				{Name: "B", Type: tText, Nullable: true, Default: aDefault},
			}},
			rules: map[schema.Ident]mask.Rule{"A": {Use: "default"}, "B": {Use: "default"}},
			row:   func(i int) []value.Value { return []value.Value{value.Int(int64(i % 2)), value.Null()} },
		},
		"ONLY_DEFAULTS_KEYED": {
			table: &schema.Table{Columns: []*schema.Column{
				{Name: "ID", Type: tInt64},
				{Name: "B", Type: tText, Nullable: true, Default: aDefault},
			}, Uniques: []*schema.Key{pk("ID")}},
			generated: ids("ID"),
			rules:     map[schema.Ident]mask.Rule{"B": {Use: "default"}},
			row:       func(i int) []value.Value { return []value.Value{value.Int(int64(i)), value.Null()} },
		},
	}
}

// database returns the shapes as one database, tables in name order, with
// their generated columns, or as plain columns for seeding.
func database(shapes map[string]shape, generated bool) *schema.Database {
	nsp := &schema.Namespace{Name: ns}
	for _, name := range slices.Sorted(mapsKeys(shapes)) {
		s := shapes[name]
		t := *s.table
		t.Name = schema.Ident(name)
		t.Columns = make([]*schema.Column, len(s.table.Columns))
		for i, c := range s.table.Columns {
			col := *c
			if generated && slices.Contains(s.generated, c.Name) {
				col.Generated = &schema.Generated{Expr: "rowid"}
			}
			t.Columns[i] = &col
		}
		nsp.Tables = append(nsp.Tables, &t)
	}
	return &schema.Database{Namespaces: []*schema.Namespace{nsp}}
}

func mapsKeys[V any](m map[string]V) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// sliceRows is a RowIter over rows in memory.
type sliceRows struct {
	rows [][]value.Value
	next int
}

func (s *sliceRows) Next() bool         { s.next++; return s.next <= len(s.rows) }
func (s *sliceRows) Row() []value.Value { return s.rows[s.next-1] }
func (s *sliceRows) Err() error         { return nil }
func (s *sliceRows) Close() error       { return nil }

func columnNames(t *schema.Table) []schema.Ident {
	out := make([]schema.Ident, len(t.Columns))
	for i, c := range t.Columns {
		out[i] = c.Name
	}
	return out
}

// seedFake stores n rows of every shape in a fake database, in a shuffled
// order, so that key order differs from the order rows were stored in.
func seedFake(t *testing.T, d *fakedb.Dialect, name string, db *schema.Database, shapes map[string]shape, n int) {
	t.Helper()
	ctx := context.Background()
	conn, err := d.Open(ctx, "fake:"+name, dialect.OpenOptions{Role: dialect.Target})
	if err != nil {
		t.Fatal(err)
	}
	l, err := conn.Loader(ctx, dialect.LoadOptions{RunID: "seed", Replace: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.CreateTables(ctx, db); err != nil {
		t.Fatal(err)
	}
	order := rand.New(rand.NewPCG(1, 2)).Perm(n)
	for _, tbl := range db.Namespaces[0].Tables {
		rows := make([][]value.Value, n)
		for i, j := range order {
			rows[i] = shapes[string(tbl.Name)].row(j)
		}
		if _, err := l.Load(ctx, ref(tbl.Name), columnNames(tbl), &sliceRows{rows: rows}); err != nil {
			t.Fatalf("seeding %s: %v", tbl.Name, err)
		}
	}
	if err := l.Finish(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := l.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func snapshot(t *testing.T, d *fakedb.Dialect, name string) dialect.Snapshot {
	t.Helper()
	conn, err := d.Open(context.Background(), "fake:"+name, dialect.OpenOptions{Role: dialect.Source})
	if err != nil {
		t.Fatal(err)
	}
	s, err := conn.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// readAll reads columns of a table in one read, in key order when key is
// set, else in stored order.
func readAll(t *testing.T, s dialect.Snapshot, table schema.TableRef, columns, key []schema.Ident) [][]value.Value {
	t.Helper()
	it, err := s.Read(context.Background(), dialect.ReadQuery{Table: table, Columns: columns, OrderBy: key})
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

func maskTable(t testing.TB, r schema.TableRef, tbl *schema.Table, rules map[schema.Ident]mask.Rule) *mask.Table {
	t.Helper()
	m, err := mask.NewTable(r, tbl, rules, dialect.Capabilities{EmptyStringIsNull: true})
	if err != nil {
		t.Fatalf("masking %s: %v", r, err)
	}
	return m
}

// maskRows masks rows one by one, as the engine's output must.
func maskRows(t testing.TB, m *mask.Table, rows [][]value.Value) [][]value.Value {
	t.Helper()
	rm := m.Bind(testKeys)
	out := make([][]value.Value, len(rows))
	for i, row := range rows {
		out[i] = make([]value.Value, len(row))
		if err := rm.Mask(out[i], row); err != nil {
			t.Fatalf("masking row %d: %v", i, err)
		}
	}
	return out
}

// sameRows compares without printing values.
func sameRows(t testing.TB, what string, got, want [][]value.Value) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s: %d rows, want %d", what, len(got), len(want))
		return
	}
	for i := range want {
		if len(got[i]) != len(want[i]) {
			t.Errorf("%s: row %d has %d values, want %d", what, i, len(got[i]), len(want[i]))
			return
		}
		for j := range want[i] {
			if !got[i][j].Equal(want[i][j]) {
				t.Errorf("%s: row %d, column %d differs (%v, want %v)", what, i, j, got[i][j], want[i][j])
				return
			}
		}
	}
}

// copyTuned is Copy with the pipelines' shape changed by adjust.
func copyTuned(ctx context.Context, job Job, adjust func(*tuning)) (Report, error) {
	if err := job.validate(); err != nil {
		return Report{}, err
	}
	plans := make([]*plan, len(job.Tables))
	for i, t := range job.Tables {
		p, err := newPlan(t)
		if err != nil {
			return Report{}, err
		}
		plans[i] = p
	}
	tn, err := tune(job, len(plans))
	if err != nil {
		return Report{}, err
	}
	if adjust != nil {
		adjust(&tn)
	}
	return run(ctx, job, plans, tn)
}

// TestCopyMatchesRowByRowMasking copies every shape through the strict
// fake engine, and checks that the target holds each table's rows masked
// one by one, in key order, under many pipeline shapes.
func TestCopyMatchesRowByRowMasking(t *testing.T) {
	all := shapes()
	db := database(all, true)
	tunings := []struct {
		name   string
		adjust func(*tuning)
	}{
		{"one-row chunks", func(tn *tuning) { tn.batchRows, tn.chunkBytes, tn.workers, tn.queue = 4, 1, 3, 2 }},
		{"one big chunk after the first", func(tn *tuning) { tn.batchRows, tn.chunkBytes, tn.workers, tn.queue = 4, 1<<40, 2, 1 }},
		{"mid-size chunks", func(tn *tuning) { tn.batchRows, tn.chunkBytes, tn.workers, tn.queue = 3, 1200, 4, 3 }},
		{"defaults", nil},
	}
	for _, mode := range []dialect.SnapshotMode{dialect.SnapshotSingle, dialect.SnapshotShared} {
		for _, n := range []int{0, 1, 4, 8, 37} {
			for _, tc := range tunings {
				t.Run(fmt.Sprintf("%s/%d rows/%s", mode, n, tc.name), func(t *testing.T) {
					d := fakedb.New(fakedb.WithSnapshot(mode))
					seedFake(t, d, "src", database(all, false), all, n)
					src := snapshot(t, d, "src")

					target, err := d.Open(t.Context(), "fake:dst", dialect.OpenOptions{Role: dialect.Target})
					if err != nil {
						t.Fatal(err)
					}
					l, err := target.Loader(t.Context(), dialect.LoadOptions{RunID: "test"})
					if err != nil {
						t.Fatal(err)
					}
					if err := l.CreateTables(t.Context(), db); err != nil {
						t.Fatal(err)
					}
					job := Job{Source: src, Snapshot: mode, Keys: testKeys, Sink: l, Budget: 64 << 20}
					masks := map[schema.Ident]*mask.Table{}
					for _, tbl := range db.Namespaces[0].Tables {
						masks[tbl.Name] = maskTable(t, ref(tbl.Name), tbl, all[string(tbl.Name)].rules)
						job.Tables = append(job.Tables, Table{Ref: ref(tbl.Name), Schema: tbl, Mask: masks[tbl.Name]})
					}
					report, err := copyTuned(t.Context(), job, tc.adjust)
					if err != nil {
						t.Fatalf("Copy: %v", err)
					}
					if err := l.Finish(t.Context(), db); err != nil {
						t.Fatalf("Finish: %v", err)
					}
					if err := l.Commit(t.Context()); err != nil {
						t.Fatal(err)
					}

					out := snapshot(t, d, "dst")
					for i, tbl := range db.Namespaces[0].Tables {
						r := ref(tbl.Name)
						if got := report.Tables[i]; got.Ref != r || got.Rows != int64(n) {
							t.Errorf("report for %s: %+v, want %d rows", r, got, n)
						}
						write := masks[tbl.Name].Columns()
						if len(write) == 0 {
							if got := len(readAll(t, out, r, columnNames(tbl)[:1], nil)); got != n {
								t.Errorf("%s: %d default rows, want %d", r, got, n)
							}
							continue
						}
						want := maskRows(t, masks[tbl.Name], readAll(t, src, r, write, tbl.IdentityKey()))
						sameRows(t, string(tbl.Name), readAll(t, out, r, write, nil), want)
					}
				})
			}
		}
	}
}
