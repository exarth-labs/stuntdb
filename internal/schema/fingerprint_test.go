package schema

import (
	"slices"
	"strings"
	"testing"

	"github.com/exarth-labs/stuntdb/internal/value"
)

func fingerprintDB() *Database {
	text := value.Type{Kind: value.TypeText}
	id := value.Type{Kind: value.TypeInt, Bits: 64}
	return &Database{Engine: "sqlite", Namespaces: []*Namespace{{
		Name: "main",
		Tables: []*Table{
			{
				Name:       "users",
				Columns:    []*Column{{Name: "id", Type: id}, {Name: "email", Type: text, NativeType: "TEXT"}},
				PrimaryKey: &Key{Columns: []Ident{"id"}},
				Native:     "CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT)",
			},
			{
				Name:        "orders",
				Columns:     []*Column{{Name: "id", Type: id}, {Name: "user_id", Type: id}},
				ForeignKeys: []*ForeignKey{{Columns: []Ident{"user_id"}, Ref: TableRef{"main", "users"}, RefColumns: []Ident{"id"}}},
			},
		},
	}}}
}

func TestFingerprint(t *testing.T) {
	base := fingerprintDB().Fingerprint()
	if !strings.HasPrefix(base, "sha256:") || len(base) != len("sha256:")+64 {
		t.Fatalf("Fingerprint = %q", base)
	}

	same := []struct {
		name   string
		change func(*Database)
	}{
		{"table order", func(d *Database) { slices.Reverse(d.Namespaces[0].Tables) }},
		{"column order", func(d *Database) { slices.Reverse(d.Namespaces[0].Tables[0].Columns) }},
		{"native DDL", func(d *Database) {
			d.Namespaces[0].Tables[0].Native = "CREATE TABLE users(id INTEGER PRIMARY KEY,email TEXT)"
		}},
		{"an index", func(d *Database) {
			d.Namespaces[0].Tables[0].Indexes = []*Index{{Name: "users_email", Columns: []Ident{"email"}}}
		}},
		{"statistics", func(d *Database) { d.Namespaces[0].Tables[0].EstimatedRows = 1000 }},
		{"a view", func(d *Database) { d.Namespaces[0].Objects = []*Object{{Kind: ObjectView, Name: "v"}} }},
	}
	for _, tt := range same {
		d := fingerprintDB()
		tt.change(d)
		if d.Fingerprint() != base {
			t.Errorf("changing the %s changed the fingerprint", tt.name)
		}
	}

	different := []struct {
		name   string
		change func(*Database)
	}{
		{"a new column", func(d *Database) {
			d.Namespaces[0].Tables[0].Columns = append(d.Namespaces[0].Tables[0].Columns, &Column{Name: "tax_id", Type: value.Type{Kind: value.TypeText}})
		}},
		{"a column type", func(d *Database) {
			d.Namespaces[0].Tables[0].Columns[1].Type = value.Type{Kind: value.TypeString, MaxLen: 50}
		}},
		{"a native type", func(d *Database) { d.Namespaces[0].Tables[0].Columns[1].NativeType = "VARCHAR(50)" }},
		{"nullability", func(d *Database) { d.Namespaces[0].Tables[0].Columns[1].Nullable = true }},
		{"a renamed column", func(d *Database) { d.Namespaces[0].Tables[0].Columns[1].Name = "mail" }},
		{"a unique key", func(d *Database) { d.Namespaces[0].Tables[0].Uniques = []*Key{{Columns: []Ident{"email"}}} }},
		{"a foreign key", func(d *Database) { d.Namespaces[0].Tables[1].ForeignKeys = nil }},
		{"a generated column", func(d *Database) { d.Namespaces[0].Tables[0].Columns[1].Generated = &Generated{} }},
	}
	for _, tt := range different {
		d := fingerprintDB()
		tt.change(d)
		if d.Fingerprint() == base {
			t.Errorf("%s did not change the fingerprint", tt.name)
		}
	}
}
