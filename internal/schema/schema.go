// Package schema is stuntdb's engine-neutral description of a database's
// structure: namespaces, tables, columns, keys, foreign keys and indexes.
//
// Every database adapter's introspection produces a *Database, and the rest
// of stuntdb works only with it. Each object keeps both a canonical
// description, used for core logic and cross-engine restores, and the
// engine's own native definition, replayed as-is for same-engine copies.
package schema

import "github.com/exarth-labs/stuntdb/internal/value"

// Ident is a name exactly as the database's catalog stores it, never
// case-folded: "users" in PostgreSQL, "USERS" in Oracle. Quoting and
// case-folding rules belong to the dialect.
type Ident string

// TableRef names a table within a database.
type TableRef struct {
	Namespace Ident
	Name      Ident
}

// String returns "namespace.table". Names are structure, not row data, so
// printing them is safe.
func (r TableRef) String() string { return string(r.Namespace) + "." + string(r.Name) }

// ColumnRef names a column within a database.
type ColumnRef struct {
	Table  TableRef
	Column Ident
}

// String returns "namespace.table.column".
func (r ColumnRef) String() string { return r.Table.String() + "." + string(r.Column) }

// Database is the structure of one database.
type Database struct {
	Engine     string // the adapter that produced it, e.g. "sqlite"
	Version    string // the server or library version
	Namespaces []*Namespace
}

// Namespace is a schema (PostgreSQL, SQL Server, Oracle), a database
// (MySQL, MariaDB), or "main" or an attached database (SQLite).
type Namespace struct {
	Name    Ident
	Tables  []*Table
	Objects []*Object // views, triggers, sequences and routines
}

// Table is one table.
type Table struct {
	Name        Ident
	Columns     []*Column
	PrimaryKey  *Key // nil when the table has none
	Uniques     []*Key
	ForeignKeys []*ForeignKey
	Indexes     []*Index
	Checks      []*Check

	// Native is the engine's own CREATE TABLE statement, when it has one
	// (SQLite, MySQL, Oracle). PostgreSQL and SQL Server have no single
	// statement, so their adapters leave it empty and rebuild the table from
	// the native fragments kept on columns, constraints and indexes.
	Native string

	// EstimatedRows comes from the engine's statistics; -1 means unknown.
	EstimatedRows int64
}

// Column is one table column.
type Column struct {
	Name       Ident
	Type       value.Type
	NativeType string // the type as the engine writes it, e.g. "varchar(255)"
	Nullable   bool
	Default    *Default   // nil when the column has no default
	Generated  *Generated // nil unless the engine computes the column
	Identity   *Identity  // nil unless the engine numbers the column automatically
	Collation  string     // empty means the engine's default
}

// Key is a primary key or a unique constraint.
type Key struct {
	Name    Ident // empty when the engine does not name it
	Columns []Ident
}

// Action is what a foreign key does when the row it references is deleted or updated.
type Action uint8

const (
	NoAction Action = iota
	Restrict
	Cascade
	SetNull
	SetDefault
)

var actionNames = [...]string{"NO ACTION", "RESTRICT", "CASCADE", "SET NULL", "SET DEFAULT"}

func (a Action) String() string {
	if int(a) < len(actionNames) {
		return actionNames[a]
	}
	return "invalid"
}

// ForeignKey is a reference from some columns of a table to a key of a
// table, possibly in another namespace.
type ForeignKey struct {
	Name              Ident
	Columns           []Ident
	Ref               TableRef
	RefColumns        []Ident
	OnDelete          Action
	OnUpdate          Action
	Deferrable        bool
	InitiallyDeferred bool

	// Virtual is true for a relationship declared in stuntdb's configuration
	// that the database itself does not enforce.
	Virtual bool
}

// Index is an index. Unique indexes are indexes too; unique constraints are Keys.
type Index struct {
	Name       Ident
	Columns    []Ident // the plain columns, in order; empty for a pure expression index
	Unique     bool
	Partial    bool   // the index has a WHERE clause
	Expression bool   // the index uses expressions, not only plain columns
	Native     string // the engine's own CREATE INDEX statement
}

// Check is a check constraint, kept as the engine's own expression.
type Check struct {
	Name Ident
	Expr string
}

// DefaultKind classifies a column default, so that common defaults can be
// recreated on other engines.
type DefaultKind uint8

const (
	DefaultExpr    DefaultKind = iota // any other expression, kept as native text
	DefaultLiteral                    // a constant value
	DefaultNow                        // the current date or time
	DefaultUUID                       // a newly generated UUID
)

// Default is a column default.
type Default struct {
	Kind DefaultKind
	Expr string // the engine's own expression
}

// Generated describes a column whose value the engine computes. Such a
// column is never written; the target computes it again.
type Generated struct {
	Expr   string // the engine's own expression
	Stored bool   // stored, as opposed to computed when read
}

// Identity describes a column the engine numbers automatically.
type Identity struct {
	// Always is true when the engine refuses explicit values unless told
	// otherwise, as with PostgreSQL's GENERATED ALWAYS AS IDENTITY.
	Always bool
}

// ObjectKind is the kind of a non-table object.
type ObjectKind uint8

const (
	ObjectOther ObjectKind = iota
	ObjectView
	ObjectMaterializedView
	ObjectTrigger
	ObjectSequence
	ObjectRoutine // a function or stored procedure
)

// Object is a view, trigger, sequence or routine. Objects are kept as the
// engine's own definition and copied only between instances of the same engine.
type Object struct {
	Kind   ObjectKind
	Name   Ident
	Native string
}

// Namespace returns the namespace with the given name, or nil.
func (d *Database) Namespace(name Ident) *Namespace {
	for _, ns := range d.Namespaces {
		if ns.Name == name {
			return ns
		}
	}
	return nil
}

// Table returns the table that ref names, or nil.
func (d *Database) Table(ref TableRef) *Table {
	if ns := d.Namespace(ref.Namespace); ns != nil {
		return ns.Table(ref.Name)
	}
	return nil
}

// Table returns the table with the given name, or nil.
func (ns *Namespace) Table(name Ident) *Table {
	for _, t := range ns.Tables {
		if t.Name == name {
			return t
		}
	}
	return nil
}

// Column returns the column with the given name, or nil.
func (t *Table) Column(name Ident) *Column {
	for _, c := range t.Columns {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// IdentityKey returns the columns that identify a row: the primary key, or
// else the first unique constraint, or else the first plain unique index,
// whose columns are all NOT NULL. It returns nil when the table has none,
// in which case its rows cannot be tracked one by one.
func (t *Table) IdentityKey() []Ident {
	if t.PrimaryKey != nil && len(t.PrimaryKey.Columns) > 0 {
		return t.PrimaryKey.Columns
	}
	for _, k := range t.Uniques {
		if t.allNotNull(k.Columns) {
			return k.Columns
		}
	}
	for _, ix := range t.Indexes {
		if ix.Unique && !ix.Partial && !ix.Expression && t.allNotNull(ix.Columns) {
			return ix.Columns
		}
	}
	return nil
}

func (t *Table) allNotNull(names []Ident) bool {
	if len(names) == 0 {
		return false
	}
	for _, name := range names {
		if c := t.Column(name); c == nil || c.Nullable {
			return false
		}
	}
	return true
}
