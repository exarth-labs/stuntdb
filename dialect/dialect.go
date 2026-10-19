package dialect

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/exarth-labs/stuntdb/internal/schema"
	"github.com/exarth-labs/stuntdb/internal/value"
)

// ErrNotSupported is returned for an operation the engine, or the server
// connected to, does not support.
var ErrNotSupported = errors.New("dialect: not supported by this database")

// Dialect is a database engine adapter.
type Dialect interface {
	// Name is the adapter's name, such as "sqlite" or "postgres".
	Name() string

	// Schemes are the URL schemes the adapter handles, such as "postgres"
	// and "postgresql".
	Schemes() []string

	// Open connects to the database at url. The error must not contain url,
	// which may hold a password.
	Open(ctx context.Context, url string, opts OpenOptions) (Conn, error)
}

// Role says what stuntdb will do with a connection.
type Role uint8

const (
	// Source connections only read, and are opened read-only wherever the
	// engine allows it.
	Source Role = iota

	// Target connections create tables and load data.
	Target
)

// OpenOptions configures a connection.
type OpenOptions struct {
	Role Role

	// AppName identifies stuntdb in the server's list of sessions, where the
	// engine supports it.
	AppName string

	// MaxConns caps the connections the adapter may open. 0 means the
	// adapter's default.
	MaxConns int

	// StatementTimeout bounds each statement on the server, where the engine
	// supports it. 0 means no limit.
	StatementTimeout time.Duration

	// Logger receives diagnostic messages, which must never include row
	// values. Nil discards them.
	Logger *slog.Logger
}

// Conn is an open connection to one database.
type Conn interface {
	// Info describes the server.
	Info() ServerInfo

	// Capabilities describes what this server supports, as detected when
	// connecting.
	Capabilities() Capabilities

	// Introspect reads the structure of the namespaces that f selects.
	Introspect(ctx context.Context, f Filter) (*schema.Database, error)

	// Snapshot starts a consistent, read-only view of the database.
	// Source connections only.
	Snapshot(ctx context.Context) (Snapshot, error)

	// Loader prepares a staging area to load data into. Target connections only.
	Loader(ctx context.Context, opts LoadOptions) (Loader, error)

	// Close releases the connection.
	Close() error
}

// ServerInfo describes a database server.
type ServerInfo struct {
	Engine  string // e.g. "postgres", "mysql", "mariadb", "sqlite"
	Variant string // e.g. "aurora", "cockroachdb", "tidb"; empty for the standard engine
	Version string // as the server reports it, e.g. "16.4"
}

// Filter selects the namespaces to introspect.
type Filter struct {
	// Namespaces to include. Empty means the namespaces the connection points
	// at: every user schema for PostgreSQL and SQL Server, the URL's database
	// for MySQL and MariaDB, the user's schema for Oracle, and "main" for
	// SQLite. System namespaces are never included.
	Namespaces []schema.Ident
}

// Capabilities describes the engine differences that change what the rest of
// stuntdb does. Everything else stays inside the adapter.
type Capabilities struct {
	// Snapshot says how the engine gives a consistent view for reading.
	Snapshot SnapshotMode

	// EmptyStringIsNull is true when the engine stores '' as NULL, as Oracle
	// does, so that masking never writes '' into a NOT NULL column.
	EmptyStringIsNull bool

	// TableNames and ColumnNames say how the engine matches a name written
	// without quotes, so that names in stuntdb's configuration find the
	// right objects. They are separate because MySQL can match table names
	// case-sensitively while it never does for column names.
	TableNames  IdentifierCase
	ColumnNames IdentifierCase
}

// SnapshotMode says how an engine gives a consistent view for reading.
type SnapshotMode uint8

const (
	// SnapshotNone means the engine can't guarantee a consistent view, so
	// stuntdb warns, and verification afterwards catches broken references.
	// It is the zero value, so an adapter that forgets to set the mode never
	// claims more than it delivers.
	SnapshotNone SnapshotMode = iota

	// SnapshotSingle means one connection holds a consistent view, so reads
	// can't run in parallel: MySQL by default, SQL Server with snapshot
	// isolation, and SQLite.
	SnapshotSingle

	// SnapshotShared means extra connections can join the same view, for
	// parallel reads: PostgreSQL exported snapshots, Oracle flashback queries.
	SnapshotShared
)

func (m SnapshotMode) String() string {
	switch m {
	case SnapshotSingle:
		return "single"
	case SnapshotShared:
		return "shared"
	default:
		return "none"
	}
}

// IdentifierCase says how an engine matches a name written without quotes.
type IdentifierCase uint8

const (
	CaseSensitive   IdentifierCase = iota // only the exact spelling matches
	FoldLower                             // unquoted names become lower case (PostgreSQL)
	FoldUpper                             // unquoted names become upper case (Oracle)
	CaseInsensitive                       // any spelling matches (SQLite; SQL Server's usual collations)
)

// Match reports whether name, written by a user without quotes, refers to the
// catalog name ident. The exact spelling always matches.
func (c IdentifierCase) Match(name string, ident schema.Ident) bool {
	if name == string(ident) {
		return true
	}
	switch c {
	case FoldLower:
		return strings.ToLower(name) == string(ident)
	case FoldUpper:
		return strings.ToUpper(name) == string(ident)
	case CaseInsensitive:
		return strings.EqualFold(name, string(ident))
	default:
		return false
	}
}

// Snapshot is a consistent, read-only view of a source database.
type Snapshot interface {
	// Point identifies the moment the view shows, for the run manifest: a
	// PostgreSQL WAL position, a MySQL GTID set, an Oracle SCN. It may be empty.
	Point() string

	// Join opens another reader on the same view, for a parallel worker.
	// It returns ErrNotSupported unless the snapshot mode is SnapshotShared.
	Join(ctx context.Context) (Snapshot, error)

	// Read streams the rows that q selects.
	Read(ctx context.Context, q ReadQuery) (RowIter, error)

	// Close ends this reader's view. Joined readers are closed separately.
	Close() error
}

// RowIter streams rows. Call Next until it returns false, then check Err.
// Always call Close.
type RowIter interface {
	// Next moves to the next row, and reports whether there is one.
	Next() bool

	// Row returns the current row's values, in the order of the query's
	// columns. The slice is only valid until the next call to Next.
	Row() []value.Value

	// Err returns the error that stopped iteration, if any.
	Err() error

	// Close releases the iterator.
	Close() error
}

// LoadOptions configures loading into a target.
type LoadOptions struct {
	// RunID names the staging area, so that leftovers from an interrupted run
	// can be found and removed.
	RunID string

	// Replace lets Commit replace tables that already exist in the target.
	Replace bool
}

// Loader loads data into a staging area of a target database, and swaps it
// into place on Commit, so that a failed or interrupted run never leaves a
// half-loaded target. The order of calls is: CreateTables, Load for each
// table, Finish, then Commit or Abort.
type Loader interface {
	// CreateTables creates db's tables in the staging area. When db comes from
	// the same engine, the adapter may replay native definitions; otherwise
	// it builds them from the canonical model. Indexes and constraints that
	// can be added later wait for Finish.
	CreateTables(ctx context.Context, db *schema.Database) error

	// Load writes rows into a table, with values in the order of columns, and
	// returns how many rows it wrote. Generated columns are never among the
	// columns. Load may be called for several tables at once; adapters that
	// can only write one table at a time take turns internally.
	Load(ctx context.Context, table schema.TableRef, columns []schema.Ident, rows RowIter) (int64, error)

	// Finish adds the remaining indexes and constraints, checks every foreign
	// key, and resets identity counters past the loaded values.
	Finish(ctx context.Context, db *schema.Database) error

	// Commit swaps the staging area into place, atomically where the engine
	// allows it.
	Commit(ctx context.Context) error

	// Abort removes the staging area. It is safe to call after Commit and
	// more than once.
	Abort(ctx context.Context) error
}
