package dialect

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/exarth-labs/stuntdb/internal/schema"
	"github.com/exarth-labs/stuntdb/internal/value"
)

type stubDialect struct {
	name    string
	schemes []string
}

func (s stubDialect) Name() string      { return s.name }
func (s stubDialect) Schemes() []string { return s.schemes }
func (s stubDialect) Open(context.Context, string, OpenOptions) (Conn, error) {
	return nil, ErrNotSupported
}

func TestRegistryLookup(t *testing.T) {
	r := newRegistry()
	sqlite := stubDialect{"sqlite", []string{"sqlite", "sqlite3"}}
	postgres := stubDialect{"postgres", []string{"postgres", "postgresql"}}
	r.register(sqlite)
	r.register(postgres)

	tests := []struct {
		url  string
		want string
	}{
		{"sqlite:./app.db", "sqlite"},
		{"sqlite3:app.db", "sqlite"},
		{"postgres://user:secret@host/db", "postgres"},
		{"PostgreSQL://host/db", "postgres"},
	}
	for _, tt := range tests {
		d, err := r.lookup(tt.url)
		if err != nil {
			t.Errorf("lookup(%q): %v", tt.url, err)
			continue
		}
		if d.Name() != tt.want {
			t.Errorf("lookup(%q) = %s, want %s", tt.url, d.Name(), tt.want)
		}
	}
	if got := r.names(); strings.Join(got, ",") != "postgres,sqlite" {
		t.Errorf("names() = %v", got)
	}
}

func TestRegistryLookupErrorsNeverContainTheURL(t *testing.T) {
	r := newRegistry()
	r.register(stubDialect{"sqlite", []string{"sqlite"}})
	tests := []struct {
		name, url, wantHint string
	}{
		{"unknown scheme", "mysql://admin:hunter2@db.internal/app", `"mysql"`},
		{"no scheme", "hunter2-app.db", "starts with a scheme"},
		{"invalid scheme", "1abc://hunter2@host", "starts with a scheme"},
		{"Windows path", `C:\hunter2\app.db`, "sqlite:c:"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := r.lookup(tt.url)
			if !errors.Is(err, ErrUnknownScheme) {
				t.Fatalf("lookup() = %v, want ErrUnknownScheme", err)
			}
			if strings.Contains(err.Error(), "hunter2") {
				t.Errorf("error %q leaks part of the URL", err)
			}
			if !strings.Contains(err.Error(), tt.wantHint) {
				t.Errorf("error %q should contain %q", err, tt.wantHint)
			}
		})
	}
}

func TestRegisterPanics(t *testing.T) {
	tests := []struct {
		name string
		do   func(r *registry)
	}{
		{"duplicate scheme", func(r *registry) {
			r.register(stubDialect{"a", []string{"db"}})
			r.register(stubDialect{"b", []string{"DB"}})
		}},
		{"no schemes", func(r *registry) { r.register(stubDialect{"a", nil}) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("expected a panic")
				}
			}()
			tt.do(newRegistry())
		})
	}
}

func TestIdentifierCaseMatch(t *testing.T) {
	tests := []struct {
		c     IdentifierCase
		name  string
		ident schema.Ident
		want  bool
	}{
		{CaseSensitive, "users", "users", true},
		{CaseSensitive, "Users", "users", false},
		{FoldLower, "Users", "users", true},
		{FoldLower, "Users", "Users", true}, // a quoted mixed-case name still matches exactly
		{FoldLower, "users", "USERS", false},
		{FoldUpper, "users", "USERS", true},
		{FoldUpper, "Users", "users", false},
		{CaseInsensitive, "USERS", "users", true},
		{CaseInsensitive, "user", "users", false},
	}
	for _, tt := range tests {
		if got := tt.c.Match(tt.name, tt.ident); got != tt.want {
			t.Errorf("%d.Match(%q, %q) = %v, want %v", tt.c, tt.name, tt.ident, got, tt.want)
		}
	}
}

func TestReadQueryValidate(t *testing.T) {
	users := schema.TableRef{Namespace: "main", Name: "users"}
	id := []schema.Ident{"id"}
	tests := []struct {
		name    string
		q       ReadQuery
		wantErr string
	}{
		{"every row", ReadQuery{Table: users, Columns: id}, ""},
		{
			"keyset chunk",
			ReadQuery{Table: users, Columns: id, OrderBy: id, Limit: 1000, Where: After{Columns: id, Key: []value.Value{value.Int(5)}}},
			"",
		},
		{
			"composite keys",
			ReadQuery{Table: users, Columns: id, Where: KeysIn{Columns: []schema.Ident{"a", "b"}, Keys: [][]value.Value{{value.Int(1), value.Int(2)}}}},
			"",
		},
		{
			"seed filter and chunk",
			ReadQuery{Table: users, Columns: id, OrderBy: id, Limit: 10, Where: And{Native{SQL: "active = 1"}, After{Columns: id, Key: []value.Value{value.Int(1)}}}},
			"",
		},
		{"no table", ReadQuery{Columns: id}, "has no table"},
		{"no columns", ReadQuery{Table: users}, "has no columns"},
		{"negative limit", ReadQuery{Table: users, Columns: id, Limit: -1}, "negative limit"},
		{"limit without order", ReadQuery{Table: users, Columns: id, Limit: 10}, "limit but no order"},
		{
			"key with the wrong width",
			ReadQuery{Table: users, Columns: id, Where: KeysIn{Columns: []schema.Ident{"a", "b"}, Keys: [][]value.Value{{value.Int(1)}}}},
			"key 0 has 1 values for 2 columns",
		},
		{"KeysIn without columns", ReadQuery{Table: users, Columns: id, Where: KeysIn{}}, "KeysIn on main.users has no columns"},
		{"After with a short key", ReadQuery{Table: users, Columns: id, Where: After{Columns: []schema.Ident{"a", "b"}, Key: []value.Value{value.Int(1)}}}, "one key value per column"},
		{"empty native condition", ReadQuery{Table: users, Columns: id, Where: Native{}}, "is empty"},
		{"nil inside And", ReadQuery{Table: users, Columns: id, Where: And{nil}}, "nil condition"},
		{"bad predicate inside And", ReadQuery{Table: users, Columns: id, Where: And{Native{}}}, "is empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.q.Validate()
			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("Validate() = %v, want nil", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Errorf("Validate() = %v, want an error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestSnapshotModeZeroValueClaimsNothing(t *testing.T) {
	var c Capabilities
	if c.Snapshot != SnapshotNone || c.Snapshot.String() != "none" {
		t.Errorf("zero Capabilities.Snapshot = %v, want none", c.Snapshot)
	}
	if SnapshotSingle.String() != "single" || SnapshotShared.String() != "shared" {
		t.Error("SnapshotMode.String")
	}
}

func TestKeysPerRead(t *testing.T) {
	tests := []struct {
		name    string
		limit   int
		columns int
		want    int
	}{
		{"zero claims only the default", 0, 1, DefaultMaxKeyValues},
		{"composite keys share the limit", 32000, 3, 10666},
		{"a tiny limit", 5, 2, 2},
		{"never below one key", 5, 9, 1},
		{"no columns counts as one", 10, 0, 10},
	}
	for _, tt := range tests {
		if got := (Capabilities{MaxKeyValues: tt.limit}).KeysPerRead(tt.columns); got != tt.want {
			t.Errorf("%s: KeysPerRead(%d) with MaxKeyValues %d = %d, want %d", tt.name, tt.columns, tt.limit, got, tt.want)
		}
	}
}
