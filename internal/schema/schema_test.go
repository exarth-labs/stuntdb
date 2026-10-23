package schema

import (
	"strings"
	"testing"

	"github.com/exarth-labs/stuntdb/internal/value"
)

var (
	intType  = value.Type{Kind: value.TypeInt, Bits: 64}
	textType = value.Type{Kind: value.TypeText}
)

func col(name Ident, nullable bool) *Column {
	return &Column{Name: name, Type: intType, NativeType: "INTEGER", Nullable: nullable}
}

// shop is a small schema with the shapes real databases have: a composite
// key, a self-reference, a cycle between two tables, and a reference across
// namespaces.
func shop() *Database {
	return &Database{
		Engine: "test",
		Namespaces: []*Namespace{
			{
				Name: "public",
				Tables: []*Table{
					{
						Name:       "users",
						Columns:    []*Column{col("id", false), col("referred_by", true), col("team_id", true)},
						PrimaryKey: &Key{Columns: []Ident{"id"}},
						ForeignKeys: []*ForeignKey{
							{Name: "users_referred_by", Columns: []Ident{"referred_by"}, Ref: TableRef{"public", "users"}, RefColumns: []Ident{"id"}, OnDelete: SetNull},
							{Name: "users_team", Columns: []Ident{"team_id"}, Ref: TableRef{"public", "teams"}, RefColumns: []Ident{"id"}},
						},
					},
					{
						Name:       "teams",
						Columns:    []*Column{col("id", false), col("owner_id", true)},
						PrimaryKey: &Key{Columns: []Ident{"id"}},
						ForeignKeys: []*ForeignKey{
							{Name: "teams_owner", Columns: []Ident{"owner_id"}, Ref: TableRef{"public", "users"}, RefColumns: []Ident{"id"}, Deferrable: true},
						},
					},
					{
						Name:       "order_items",
						Columns:    []*Column{col("order_id", false), col("line", false), {Name: "note", Type: textType, Nullable: true}},
						PrimaryKey: &Key{Columns: []Ident{"order_id", "line"}},
						ForeignKeys: []*ForeignKey{
							{Name: "items_order", Columns: []Ident{"order_id"}, Ref: TableRef{"billing", "orders"}, RefColumns: []Ident{"id"}, OnDelete: Cascade},
						},
					},
				},
			},
			{
				Name: "billing",
				Tables: []*Table{{
					Name:       "orders",
					Columns:    []*Column{col("id", false), col("user_id", false)},
					PrimaryKey: &Key{Columns: []Ident{"id"}},
					ForeignKeys: []*ForeignKey{
						{Name: "orders_user", Columns: []Ident{"user_id"}, Ref: TableRef{"public", "users"}, RefColumns: []Ident{"id"}},
					},
				}},
			},
		},
	}
}

func TestValidateAcceptsRealShapes(t *testing.T) {
	if err := shop().Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
}

func TestValidateFindsProblems(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(d *Database)
		want   string
	}{
		{
			"duplicate namespace",
			func(d *Database) { d.Namespaces = append(d.Namespaces, &Namespace{Name: "public"}) },
			`namespace "public" appears twice`,
		},
		{
			"duplicate table",
			func(d *Database) { ns := d.Namespaces[1]; ns.Tables = append(ns.Tables, &Table{Name: "orders"}) },
			"table billing.orders appears twice",
		},
		{
			"table without a name",
			func(d *Database) { d.Namespaces[1].Tables = append(d.Namespaces[1].Tables, &Table{}) },
			"has a table without a name",
		},
		{
			"duplicate column",
			func(d *Database) {
				tb := d.Table(TableRef{"public", "teams"})
				tb.Columns = append(tb.Columns, col("id", false))
			},
			`column "id" of public.teams appears twice`,
		},
		{
			"invalid column type",
			func(d *Database) {
				d.Table(TableRef{"public", "teams"}).Columns[1].Type = value.Type{Kind: value.TypeInt, Bits: 7}
			},
			`column "owner_id" of public.teams: an int type needs 8, 16, 32 or 64 bits`,
		},
		{
			"generated identity column",
			func(d *Database) {
				c := d.Table(TableRef{"public", "teams"}).Columns[0]
				c.Generated, c.Identity = &Generated{Expr: "1"}, &Identity{}
			},
			"cannot be both generated and an identity",
		},
		{
			"primary key on a missing column",
			func(d *Database) { d.Table(TableRef{"public", "teams"}).PrimaryKey.Columns = []Ident{"uuid"} },
			`the primary key of public.teams uses column "uuid", which does not exist`,
		},
		{
			"empty unique constraint",
			func(d *Database) { tb := d.Table(TableRef{"public", "teams"}); tb.Uniques = []*Key{{Name: "u"}} },
			`unique constraint "u" of public.teams has no columns`,
		},
		{
			"index on a missing column",
			func(d *Database) {
				tb := d.Table(TableRef{"public", "teams"})
				tb.Indexes = []*Index{{Name: "ix", Columns: []Ident{"nope"}}}
			},
			`index "ix" of public.teams uses column "nope"`,
		},
		{
			"foreign key to a missing table",
			func(d *Database) {
				d.Table(TableRef{"billing", "orders"}).ForeignKeys[0].Ref = TableRef{"public", "people"}
			},
			"references table public.people, which does not exist",
		},
		{
			"foreign key to a missing column",
			func(d *Database) { d.Table(TableRef{"billing", "orders"}).ForeignKeys[0].RefColumns = []Ident{"uid"} },
			`references column "uid" of public.users`,
		},
		{
			"foreign key column count mismatch",
			func(d *Database) {
				d.Table(TableRef{"billing", "orders"}).ForeignKeys[0].RefColumns = []Ident{"id", "team_id"}
			},
			"has 1 columns but references 2",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := shop()
			tt.mutate(d)
			err := d.Validate()
			if err == nil {
				t.Fatal("Validate() = nil, want an error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Validate() = %q, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestValidateReportsEveryProblem(t *testing.T) {
	d := shop()
	d.Table(TableRef{"public", "teams"}).PrimaryKey.Columns = []Ident{"a"}
	d.Table(TableRef{"billing", "orders"}).ForeignKeys[0].Ref = TableRef{"x", "y"}
	err := d.Validate()
	if err == nil || strings.Count(err.Error(), "\n") != 1 {
		t.Errorf("want exactly two problems, got: %v", err)
	}
}

func TestValidateAllowsExpressionIndexWithoutColumns(t *testing.T) {
	d := shop()
	d.Table(TableRef{"public", "users"}).Indexes = []*Index{{Name: "lower_email", Expression: true, Native: "CREATE INDEX …"}}
	if err := d.Validate(); err != nil {
		t.Errorf("Validate() = %v", err)
	}
}

func TestLookups(t *testing.T) {
	d := shop()
	if d.Namespace("billing") == nil || d.Namespace("nope") != nil {
		t.Error("Namespace lookup")
	}
	if d.Table(TableRef{"billing", "orders"}) == nil || d.Table(TableRef{"public", "orders"}) != nil || d.Table(TableRef{"x", "orders"}) != nil {
		t.Error("Table lookup")
	}
	users := d.Table(TableRef{"public", "users"})
	if users.Column("team_id") == nil || users.Column("TEAM_ID") != nil {
		t.Error("Column lookup must be exact, never case-folded")
	}
	if got := (TableRef{"public", "users"}).String(); got != "public.users" {
		t.Errorf("TableRef.String() = %q", got)
	}
	if got := (ColumnRef{TableRef{"public", "users"}, "team_id"}).String(); got != "public.users.team_id" {
		t.Errorf("ColumnRef.String() = %q", got)
	}
}

func TestIdentityKey(t *testing.T) {
	tests := []struct {
		name  string
		table *Table
		want  []Ident
	}{
		{
			"primary key",
			&Table{Columns: []*Column{col("a", false), col("b", false)}, PrimaryKey: &Key{Columns: []Ident{"a", "b"}}},
			[]Ident{"a", "b"},
		},
		{
			"unique constraint on NOT NULL columns",
			&Table{Columns: []*Column{col("email", false)}, Uniques: []*Key{{Columns: []Ident{"email"}}}},
			[]Ident{"email"},
		},
		{
			"nullable unique constraint is skipped",
			&Table{
				Columns: []*Column{col("email", true), col("code", false)},
				Uniques: []*Key{{Columns: []Ident{"email"}}},
				Indexes: []*Index{{Unique: true, Columns: []Ident{"code"}}},
			},
			[]Ident{"code"},
		},
		{
			"partial and expression indexes are skipped",
			&Table{
				Columns: []*Column{col("a", false)},
				Indexes: []*Index{
					{Unique: true, Partial: true, Columns: []Ident{"a"}},
					{Unique: true, Expression: true, Columns: []Ident{"a"}},
					{Unique: false, Columns: []Ident{"a"}},
				},
			},
			nil,
		},
		{"no key at all", &Table{Columns: []*Column{col("a", false)}}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.table.IdentityKey()
			if strings.Join(idents(got), ",") != strings.Join(idents(tt.want), ",") || (got == nil) != (tt.want == nil) {
				t.Errorf("IdentityKey() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestActionString(t *testing.T) {
	if Cascade.String() != "CASCADE" || SetNull.String() != "SET NULL" || Action(99).String() != "invalid" {
		t.Error("Action.String")
	}
}

func idents(ids []Ident) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = string(id)
	}
	return out
}
