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
				Indexes: []*Index{{
					Name: "orders_user", Columns: []Ident{"user_id"}, Unique: true,
					Native: "CREATE UNIQUE INDEX orders_user ON orders (user_id)",
				}},
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
		{"a unique index's name", func(d *Database) { d.Namespaces[0].Tables[1].Indexes[0].Name = "orders_user_key" }},
		{"a unique index's native DDL", func(d *Database) {
			d.Namespaces[0].Tables[1].Indexes[0].Native = "CREATE UNIQUE INDEX orders_user ON orders(user_id)"
		}},
		{"keys that compare bytes by name", func(d *Database) {
			d.Namespaces[0].Tables[0].PrimaryKey.Collations = []Collation{{Name: "BINARY"}}
			d.Namespaces[0].Tables[1].Indexes[0].Collations = []Collation{{Name: "BINARY"}}
		}},
		{"a collation's name, of the same kind", func(d *Database) {
			d.Namespaces[0].Tables[0].Uniques = []*Key{{Columns: []Ident{"email"}, Collations: []Collation{{Name: "NOCASE", Equality: EqualIgnoringASCIICase}}}}
			base := d.Fingerprint()
			d.Namespaces[0].Tables[0].Uniques[0].Collations[0].Name = "nocase"
			if d.Fingerprint() != base {
				t.Error("a collation's name changed the fingerprint")
			}
			d.Namespaces[0].Tables[0].Uniques = nil
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
		{"a unique index", func(d *Database) {
			d.Namespaces[0].Tables[0].Indexes = []*Index{{Name: "users_email", Columns: []Ident{"email"}, Unique: true}}
		}},
		{"a unique index dropped", func(d *Database) { d.Namespaces[0].Tables[1].Indexes = nil }},
		{"a unique index made not unique", func(d *Database) { d.Namespaces[0].Tables[1].Indexes[0].Unique = false }},
		{"a unique index's columns", func(d *Database) { d.Namespaces[0].Tables[1].Indexes[0].Columns = []Ident{"id", "user_id"} }},
		{"a partial unique index", func(d *Database) { d.Namespaces[0].Tables[1].Indexes[0].Partial = true }},
		{"a unique index on an expression", func(d *Database) { d.Namespaces[0].Tables[1].Indexes[0].Expression = true }},
		{"how a primary key compares", func(d *Database) {
			d.Namespaces[0].Tables[0].PrimaryKey.Collations = []Collation{{Name: "NOCASE", Equality: EqualIgnoringASCIICase}}
		}},
		{"how a unique key compares", func(d *Database) {
			d.Namespaces[0].Tables[0].Uniques = []*Key{{Columns: []Ident{"email"}, Collations: []Collation{{Equality: EqualIgnoringTrailingSpaces}}}}
			base := d.Fingerprint()
			d.Namespaces[0].Tables[0].Uniques[0].Collations[0].Equality = EqualIgnoringASCIICase
			if d.Fingerprint() == base {
				t.Error("changing a unique key's kind of equality did not change the fingerprint")
			}
		}},
		{"how a unique index compares", func(d *Database) {
			d.Namespaces[0].Tables[1].Indexes[0].Collations = []Collation{{Name: "reverse", Equality: EqualOther}}
		}},
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
