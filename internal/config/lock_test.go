package config

import (
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/exarth-labs/stuntdb/internal/errs"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

func sampleLock() *Lock {
	return &Lock{
		GeneratedBy:       "stuntdb v0.1.0",
		GeneratorVersion:  1,
		SchemaFingerprint: "sha256:4f1c0000000000000000000000000000000000000000000000000000000000aa",
		Tables: []*LockTable{
			{Table: TableName{Namespace: "main", Name: "users"}, Columns: []*LockColumn{
				{Column: "referral_code", Decision: Unreviewed},
				{Column: "email", Decision: "email", Source: "config"},
				{Column: "nickname", Decision: "null", Source: "config"},
				{Column: "created_at", Decision: "keep", Source: "rule safe.timestamp"},
			}},
			{Table: TableName{Namespace: "main", Name: "order.items"}, Columns: []*LockColumn{
				{Column: "Order Id", Decision: "keep", Source: "rule safe.id"},
			}},
		},
	}
}

// The lockfile's text is pinned: tables and columns sorted, one line per
// column, so that reviews see small, clear diffs.
func TestLockGolden(t *testing.T) {
	got, err := sampleLock().Marshal()
	if err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "stuntdb.lock.golden")
	if *update {
		if err := os.WriteFile(golden, got, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (run go test -update to create it)", err)
	}
	if string(got) != string(want) {
		t.Errorf("Marshal =\n%s\nwant\n%s", got, want)
	}
}

func TestLockRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), LockPath)
	if err := sampleLock().Write(context.Background(), path); err != nil {
		t.Fatalf("Write: %v", err)
	}
	l, err := ReadLock(path)
	if err != nil {
		t.Fatalf("ReadLock: %v", err)
	}
	want := sampleLock()
	if l.GeneratedBy != want.GeneratedBy || l.GeneratorVersion != 1 || l.SchemaFingerprint != want.SchemaFingerprint {
		t.Errorf("header = %+v", l)
	}
	for _, wt := range want.Tables {
		for _, wc := range wt.Columns {
			c := l.Column(wt.Table, wc.Column)
			if c == nil || c.Decision != wc.Decision || c.Source != wc.Source {
				t.Errorf("%s.%s = %+v, want %+v", wt.Table, wc.Column, c, wc)
			}
		}
	}
	if l.Column(TableName{Name: "nope"}, "x") != nil {
		t.Error("Column found a table that isn't there")
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("Write left %d files behind, want only the lockfile", len(entries)-1)
	}
}

// Names that YAML must quote, or write in its long-key form, read back
// unchanged.
func TestLockAwkwardNames(t *testing.T) {
	names := []string{"<<", "a: b", "null", "#x", "it's", "- x", strings.Repeat("x", 2000)}
	lock := &Lock{GeneratedBy: "stuntdb test", GeneratorVersion: 1, SchemaFingerprint: "sha256:00"}
	table := &LockTable{Table: TableName{Namespace: "<<", Name: strings.Repeat("t", 2000)}}
	for _, name := range names {
		table.Columns = append(table.Columns, &LockColumn{Column: name, Decision: "keep", Source: "config"})
	}
	lock.Tables = []*LockTable{table}

	path := filepath.Join(t.TempDir(), LockPath)
	if err := lock.Write(context.Background(), path); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := ReadLock(path)
	if err != nil {
		t.Fatalf("ReadLock: %v", err)
	}
	for _, name := range names {
		if c := got.Column(table.Table, name); c == nil || c.Decision != "keep" {
			t.Errorf("column %.20q = %+v", name, c)
		}
	}
}

func TestReadLockErrors(t *testing.T) {
	if _, err := ReadLock(filepath.Join(t.TempDir(), LockPath)); !errors.Is(err, ErrNoLock) {
		t.Errorf("a missing lockfile = %v, want ErrNoLock", err)
	}
	tests := []struct {
		name, text, want string
	}{
		{"newer version", "lock_version: 2\n", "lock_version 2 is not supported"},
		{"no version", "generated_by: x\n", "lock_version is missing"},
		{"unknown entry", "lock_version: 1\nextra: 1\n", `:2:1: unknown lockfile entry "extra"`},
		{"no decision", "lock_version: 1\ncolumns:\n  users:\n    email: {source: config}\n", `:4:5: column "email" has no decision`},
		{"bad table", "lock_version: 1\ncolumns:\n  a.b.c:\n    x: {decision: keep}\n", ":3:3: a table is written as"},
		{"edited by hand", "lock_version: 1\ncolumns:\n  users:\n    email: keep\n", `column "email" must have a decision`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), LockPath)
			if err := os.WriteFile(path, []byte(tt.text), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := ReadLock(path)
			if err == nil || !strings.Contains(err.Error(), tt.want) || errs.KindOf(err) != errs.Usage {
				t.Errorf("ReadLock = %v, want an error with %q", err, tt.want)
			}
		})
	}
}
