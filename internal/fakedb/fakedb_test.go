package fakedb

import (
	"context"
	"errors"
	"testing"

	"github.com/exarth-labs/stuntdb/dialect"
	"github.com/exarth-labs/stuntdb/internal/schema"
	"github.com/exarth-labs/stuntdb/internal/value"
)

var (
	intType = value.Type{Kind: value.TypeInt, Bits: 64}
	people  = schema.TableRef{Namespace: "APP", Name: "PEOPLE"}
)

func seed(t *testing.T, d *Dialect, url string, ids ...int64) {
	t.Helper()
	ctx := context.Background()
	conn, err := d.Open(ctx, url, dialect.OpenOptions{Role: dialect.Target})
	if err != nil {
		t.Fatal(err)
	}
	l, err := conn.Loader(ctx, dialect.LoadOptions{RunID: "seed", Replace: true})
	if err != nil {
		t.Fatal(err)
	}
	db := &schema.Database{Namespaces: []*schema.Namespace{{Name: "APP", Tables: []*schema.Table{{
		Name:       "PEOPLE",
		Columns:    []*schema.Column{{Name: "ID", Type: intType}},
		PrimaryKey: &schema.Key{Columns: []schema.Ident{"ID"}},
	}}}}}
	rows := make([][]value.Value, len(ids))
	for i, id := range ids {
		rows[i] = []value.Value{value.Int(id)}
	}
	if err := l.CreateTables(ctx, db); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Load(ctx, people, []schema.Ident{"ID"}, &rowIter{rows: rows, buf: make([]value.Value, 1)}); err != nil {
		t.Fatal(err)
	}
	if err := l.Finish(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := l.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func source(t *testing.T, d *Dialect, url string) dialect.Snapshot {
	t.Helper()
	conn, err := d.Open(context.Background(), url, dialect.OpenOptions{Role: dialect.Source})
	if err != nil {
		t.Fatal(err)
	}
	s, err := conn.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestLongKeyListsAreSplit(t *testing.T) {
	d := New()
	seed(t, d, "fake:split", 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12)
	var keys [][]value.Value
	for id := int64(1); id <= 12; id++ {
		keys = append(keys, []value.Value{value.Int(id)})
	}
	before := d.Statements()
	it, err := source(t, d, "fake:split").Read(context.Background(), dialect.ReadQuery{
		Table: people, Columns: []schema.Ident{"ID"}, Where: dialect.KeysIn{Columns: []schema.Ident{"ID"}, Keys: keys},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = it.Close()
	if got := d.Statements() - before; got != 3 {
		t.Errorf("12 keys with a limit of %d parameters took %d statements, want 3", MaxParams, got)
	}
}

func TestSnapshotsIgnoreLaterWrites(t *testing.T) {
	d := New()
	seed(t, d, "fake:frozen", 1, 2)
	snap := source(t, d, "fake:frozen")
	seed(t, d, "fake:frozen", 1, 2, 3, 4)

	it, err := snap.Read(context.Background(), dialect.ReadQuery{Table: people, Columns: []schema.Ident{"ID"}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = it.Close() }()
	n := 0
	for it.Next() {
		n++
	}
	if n != 2 {
		t.Errorf("the snapshot saw %d rows, want the 2 that existed when it started", n)
	}
}

func TestRowsReuseTheirSlice(t *testing.T) {
	d := New()
	seed(t, d, "fake:reuse", 1, 2)
	it, err := source(t, d, "fake:reuse").Read(context.Background(), dialect.ReadQuery{
		Table: people, Columns: []schema.Ident{"ID"}, OrderBy: []schema.Ident{"ID"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = it.Close() }()
	it.Next()
	first := it.Row()
	it.Next()
	if first[0].Int() != 2 {
		t.Error("Row should reuse its slice, so that callers who keep rows without copying them break in tests")
	}
}

func TestUnsupportedReads(t *testing.T) {
	d := New()
	seed(t, d, "fake:unsupported", 1)
	snap := source(t, d, "fake:unsupported")
	tests := []struct {
		name string
		q    dialect.ReadQuery
	}{
		{"native SQL", dialect.ReadQuery{Table: people, Columns: []schema.Ident{"ID"}, Where: dialect.Native{SQL: "ID = 1"}}},
		{"key wider than the parameter limit", dialect.ReadQuery{Table: people, Columns: []schema.Ident{"ID"}, Where: dialect.KeysIn{
			Columns: []schema.Ident{"ID", "ID", "ID", "ID", "ID", "ID"},
			Keys:    [][]value.Value{{value.Int(1), value.Int(1), value.Int(1), value.Int(1), value.Int(1), value.Int(1)}},
		}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := snap.Read(context.Background(), tt.q); !errors.Is(err, dialect.ErrNotSupported) {
				t.Errorf("Read = %v, want ErrNotSupported", err)
			}
		})
	}
}

func TestLoaderRejectsMisuse(t *testing.T) {
	d := New()
	ctx := context.Background()
	conn, err := d.Open(ctx, "fake:misuse", dialect.OpenOptions{Role: dialect.Target})
	if err != nil {
		t.Fatal(err)
	}
	l, err := conn.Loader(ctx, dialect.LoadOptions{RunID: "misuse"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Load(ctx, people, []schema.Ident{"ID"}, &rowIter{}); err == nil {
		t.Error("Load before CreateTables should fail")
	}
	if err := l.Commit(ctx); err == nil {
		t.Error("Commit before Finish should fail")
	}
	if _, err := d.Open(ctx, "postgres://x", dialect.OpenOptions{}); err == nil {
		t.Error("a non-fake URL should fail to open")
	}
}
