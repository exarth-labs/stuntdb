package fakedb

import (
	"cmp"
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/exarth-labs/stuntdb/dialect"
	"github.com/exarth-labs/stuntdb/internal/schema"
	"github.com/exarth-labs/stuntdb/internal/value"
)

func TestSnapshotModes(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		d    *Dialect
		want dialect.SnapshotMode
	}{
		{New(), dialect.SnapshotShared},
		{New(WithSnapshot(dialect.SnapshotSingle)), dialect.SnapshotSingle},
		{New(WithSnapshot(dialect.SnapshotNone)), dialect.SnapshotNone},
	} {
		seed(t, c.d, "fake:db", 1)
		conn, err := c.d.Open(ctx, "fake:db", dialect.OpenOptions{Role: dialect.Source})
		if err != nil {
			t.Fatal(err)
		}
		if got := conn.Capabilities().Snapshot; got != c.want {
			t.Errorf("Capabilities().Snapshot = %s, want %s", got, c.want)
		}
		_, err = source(t, c.d, "fake:db").Join(ctx)
		if shared := c.want == dialect.SnapshotShared; shared != (err == nil) || !shared && !errors.Is(err, dialect.ErrNotSupported) {
			t.Errorf("%s snapshot: Join = %v", c.want, err)
		}
	}
}

// TestOneOpenReadPerReader checks the rule MySQL imposes: one open result
// per connection. A joined reader has its own.
func TestOneOpenReadPerReader(t *testing.T) {
	ctx := context.Background()
	d := New()
	seed(t, d, "fake:db", 1, 2)
	s := source(t, d, "fake:db")
	q := dialect.ReadQuery{Table: people, Columns: []schema.Ident{"ID"}}
	first, err := s.Read(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(ctx, q); err == nil || !strings.Contains(err.Error(), "still open") {
		t.Errorf("a second read while one is open: %v", err)
	}
	joined, err := s.Join(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := joined.Read(ctx, q); err != nil {
		t.Errorf("a joined reader's read while the first reader's is open: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(ctx, q); err != nil {
		t.Errorf("a read after the open one was closed: %v", err)
	}
}

// loadTarget returns a loader with one table: ID, a NOT NULL integer, and
// the given extra columns.
func loadTarget(t *testing.T, extra ...*schema.Column) dialect.Loader {
	t.Helper()
	ctx := context.Background()
	conn, err := New().Open(ctx, "fake:db", dialect.OpenOptions{Role: dialect.Target})
	if err != nil {
		t.Fatal(err)
	}
	l, err := conn.Loader(ctx, dialect.LoadOptions{RunID: "t"})
	if err != nil {
		t.Fatal(err)
	}
	db := &schema.Database{Namespaces: []*schema.Namespace{{Name: "APP", Tables: []*schema.Table{{
		Name:    "PEOPLE",
		Columns: append([]*schema.Column{{Name: "ID", Type: intType, Nullable: true}}, extra...),
	}}}}}
	if err := l.CreateTables(ctx, db); err != nil {
		t.Fatal(err)
	}
	return l
}

func rowsOf(ctx context.Context, rows ...[]value.Value) *rowIter {
	width := 0
	if len(rows) > 0 {
		width = len(rows[0])
	}
	return &rowIter{ctx: ctx, rows: rows, buf: make([]value.Value, width)}
}

func TestOneLoadPerTable(t *testing.T) {
	ctx := context.Background()
	l := loadTarget(t)
	cols := []schema.Ident{"ID"}
	if _, err := l.Load(ctx, people, cols, rowsOf(ctx, []value.Value{value.Int(1)})); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Load(ctx, people, cols, rowsOf(ctx, []value.Value{value.Int(2)})); err == nil || !strings.Contains(err.Error(), "already loaded") {
		t.Errorf("a second load of one table: %v", err)
	}
}

func TestOmittedColumns(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name string
		col  *schema.Column
		ok   bool
	}{
		{"nullable", &schema.Column{Name: "NOTE", Type: intType, Nullable: true}, true},
		{"default", &schema.Column{Name: "N", Type: intType, Default: &schema.Default{Kind: schema.DefaultLiteral, Expr: "7"}}, true},
		{"identity", &schema.Column{Name: "SEQ", Type: intType, Identity: &schema.Identity{}}, true},
		{"generated", &schema.Column{Name: "TWICE", Type: intType, Generated: &schema.Generated{Expr: "ID * 2"}}, true},
		{"NOT NULL without a default", &schema.Column{Name: "CODE", Type: intType}, false},
	} {
		l := loadTarget(t, c.col)
		_, err := l.Load(ctx, people, []schema.Ident{"ID"}, rowsOf(ctx, []value.Value{value.Int(1)}))
		if (err == nil) != c.ok {
			t.Errorf("%s column left out: %v", c.name, err)
		}
	}
}

func TestLoadWithoutColumns(t *testing.T) {
	ctx := context.Background()
	l := loadTarget(t)
	n, err := l.Load(ctx, people, nil, rowsOf(ctx, []value.Value{}, []value.Value{}, []value.Value{}))
	if err != nil || n != 3 {
		t.Errorf("Load with no columns = %d, %v; want 3 rows", n, err)
	}
	l = loadTarget(t)
	_, err = l.Load(ctx, people, nil, &rowIter{ctx: ctx, rows: [][]value.Value{{value.Int(1)}}, buf: make([]value.Value, 1)})
	if err == nil || !strings.Contains(err.Error(), "row 1 of APP.PEOPLE has 1 values for 0 columns") {
		t.Errorf("a row with a value, in a load with no columns: %v", err)
	}
}

func TestCancel(t *testing.T) {
	d := New()
	seed(t, d, "fake:db", 1, 2, 3)
	s := source(t, d, "fake:db")
	q := dialect.ReadQuery{Table: people, Columns: []schema.Ident{"ID"}}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Read(canceled, q); !errors.Is(err, context.Canceled) {
		t.Errorf("Read with a canceled context: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	it, err := s.Read(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = it.Close() }()
	if !it.Next() {
		t.Fatal("no first row")
	}
	cancel()
	if it.Next() || !errors.Is(it.Err(), context.Canceled) {
		t.Errorf("Next after a cancel: Err = %v", it.Err())
	}

	l := loadTarget(t)
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	rows := rowsOf(context.Background(), []value.Value{value.Int(1)}, []value.Value{value.Int(2)})
	if n, err := l.Load(ctx, people, []schema.Ident{"ID"}, rows); !errors.Is(err, context.Canceled) || n != 0 {
		t.Errorf("Load with a canceled context = %d, %v", n, err)
	}
}

// TestOrderEveryKeyKind checks that the fake orders every kind of value a
// key can have, as every real engine does, so it can read such keys in
// chunks. Each list is in ascending order.
func TestOrderEveryKeyKind(t *testing.T) {
	dec := func(s string) value.Value {
		v, err := value.Decimal(s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	date := func(y int, m time.Month, d int) value.Value {
		v, err := value.Date(y, m, d)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	tod := func(d time.Duration) value.Value {
		v, err := value.TimeOfDay(d)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	ts := func(s string) value.Value {
		x, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			t.Fatal(err)
		}
		return value.TimestampTZ(x)
	}
	for _, sorted := range [][]value.Value{
		{value.Bool(false), value.Bool(true)},
		{value.Int(-5), value.Int(0), value.Int(10)},
		{value.Uint(0), value.Uint(math.MaxUint64)},
		{value.Float(math.Inf(-1)), value.Float(-0.5), value.Float(2), value.Float(math.Inf(1)), value.Float(math.NaN())},
		{value.String("10"), value.String("9"), value.String("B"), value.String("a")},
		{value.Bytes([]byte{0}), value.Bytes([]byte{0, 1}), value.Bytes([]byte{1})},
		{value.UUID([16]byte{0: 1}), value.UUID([16]byte{0: 2}), value.UUID([16]byte{0: 2, 15: 1})},
		{value.DecimalInfinity(true), dec("-1.50"), dec("0.1"), dec("2.25"), dec("10"), value.DecimalInfinity(false), value.DecimalNaN()},
		{value.DateInfinity(true), value.MySQLZeroDate("0000-00-00"), value.MySQLZeroDate("2024-00-01"), date(1999, 12, 31), date(2024, 2, 29), value.DateInfinity(false)},
		{ts("2024-01-02T10:00:00+02:00"), ts("2024-01-02T09:00:00Z"), ts("2024-01-02T09:00:00.000001Z"), value.TimestampInfinity(false, true)},
		{tod(time.Hour), tod(time.Hour + time.Microsecond), tod(23 * time.Hour)},
	} {
		for i := range sorted {
			for j := range sorted {
				c, err := compareValues(sorted[i], sorted[j])
				if err != nil {
					t.Fatalf("%s: %v", sorted[i].Kind(), err)
				}
				if want := cmp.Compare(i, j); c != want {
					t.Errorf("%s: comparing positions %d and %d gave %d, want %d", sorted[i].Kind(), i, j, c, want)
				}
			}
		}
	}
	if _, err := compareValues(value.Int(1), value.String("1")); err == nil {
		t.Error("an integer and a text value were ordered")
	}
	if _, err := compareValues(value.Interval(0, 1, 0), value.Interval(0, 2, 0)); !errors.Is(err, dialect.ErrNotSupported) {
		t.Errorf("ordering intervals: %v", err)
	}
}

// TestKeysCompareAsTheirCollations checks that unique keys and foreign keys
// compare text as each key's collations say, the referenced key's for a
// foreign key, as every engine does.
func TestKeysCompareAsTheirCollations(t *testing.T) {
	text := value.Type{Kind: value.TypeText}
	nocase := []schema.Collation{{Name: "NOCASE", Equality: schema.EqualIgnoringASCIICase}}
	users := schema.TableRef{Namespace: "APP", Name: "USERS"}
	orders := schema.TableRef{Namespace: "APP", Name: "ORDERS"}
	tests := []struct {
		name        string
		key         string // which key of USERS holds EMAIL: pk, unique, index, or wide (with ID)
		coll        []schema.Collation
		users       []string
		orderEmails []string
		want        string // a substring of Finish's error, or "" for none
	}{
		{"a foreign key matches under its key's collation", "pk", nocase, []string{"bob@x.com"}, []string{"BOB@X.COM", "Bob@x.com"}, ""},
		{"a key that compares bytes", "pk", nil, []string{"bob@x.com"}, []string{"BOB@X.COM"}, "1 rows reference rows that do not exist"},
		{"duplicates under the key's collation", "pk", nocase, []string{"bob@x.com", "BOB@X.COM"}, nil, "1 duplicate keys on (EMAIL) of APP.USERS"},
		{"no duplicates in bytes", "pk", nil, []string{"bob@x.com", "BOB@X.COM"}, []string{"BOB@X.COM"}, ""},
		{"a unique constraint's collation", "unique", nocase, []string{"bob@x.com"}, []string{"BOB@X.COM"}, ""},
		{"a unique index's collation", "index", nocase, []string{"bob@x.com"}, []string{"BOB@X.COM"}, ""},
		{"no key on the referenced columns alone", "wide", []schema.Collation{{}, nocase[0]}, []string{"bob@x.com"}, []string{"BOB@X.COM"}, "1 rows reference rows that do not exist"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			conn, err := New().Open(ctx, "fake:db", dialect.OpenOptions{Role: dialect.Target})
			if err != nil {
				t.Fatal(err)
			}
			l, err := conn.Loader(ctx, dialect.LoadOptions{RunID: "t"})
			if err != nil {
				t.Fatal(err)
			}
			usersTable := &schema.Table{Name: "USERS", Columns: []*schema.Column{{Name: "ID", Type: intType, Nullable: true}, {Name: "EMAIL", Type: text}}}
			email := []schema.Ident{"EMAIL"}
			switch tt.key {
			case "pk":
				usersTable.PrimaryKey = &schema.Key{Columns: email, Collations: tt.coll}
			case "unique":
				usersTable.Uniques = []*schema.Key{{Columns: email, Collations: tt.coll}}
			case "index":
				usersTable.Indexes = []*schema.Index{
					{Name: "PLAIN", Columns: email, Collations: tt.coll},              // not unique: no key
					{Name: "WIDE", Unique: true, Columns: []schema.Ident{"ID", "ID"}}, // other columns
					{Name: "KEY", Unique: true, Columns: email, Collations: tt.coll},
				}
			case "wide":
				usersTable.PrimaryKey = &schema.Key{Columns: []schema.Ident{"ID", "EMAIL"}, Collations: tt.coll}
			}
			db := &schema.Database{Namespaces: []*schema.Namespace{{Name: "APP", Tables: []*schema.Table{
				usersTable,
				{
					Name: "ORDERS", Columns: []*schema.Column{{Name: "EMAIL", Type: text, Nullable: true}},
					ForeignKeys: []*schema.ForeignKey{{Name: "FK", Columns: []schema.Ident{"EMAIL"}, Ref: users, RefColumns: []schema.Ident{"EMAIL"}}},
				},
			}}}}
			if err := l.CreateTables(ctx, db); err != nil {
				t.Fatal(err)
			}
			for ref, texts := range map[schema.TableRef][]string{users: tt.users, orders: tt.orderEmails} {
				var rows [][]value.Value
				for _, s := range texts {
					rows = append(rows, []value.Value{value.String(s)})
				}
				if _, err := l.Load(ctx, ref, []schema.Ident{"EMAIL"}, rowsOf(ctx, rows...)); err != nil {
					t.Fatal(err)
				}
			}
			err = l.Finish(ctx, db)
			switch {
			case tt.want == "" && err != nil:
				t.Errorf("Finish: %v", err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Errorf("Finish: %v, want %q", err, tt.want)
			}
		})
	}
}
