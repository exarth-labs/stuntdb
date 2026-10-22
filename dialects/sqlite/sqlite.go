// Package sqlite is stuntdb's adapter for SQLite 3. It uses the pure-Go
// driver modernc.org/sqlite, so that builds need no C compiler.
//
// A URL names a database file: "sqlite:./app.db", "sqlite:C:\data\app.db" or
// "sqlite:///var/data/app.db". The scheme "sqlite3:" works too. URLs take no
// options, and in-memory databases are not supported, because every
// connection would see a different one.
//
// How the adapter follows the SDK's rules on SQLite:
//
//   - Source connections open the file read-only and turn on PRAGMA
//     query_only, so not even a bug can write to it.
//   - A snapshot is one connection in a read transaction. In WAL mode,
//     writers carry on and the snapshot doesn't see their changes; in the
//     other journal modes, writers wait until the snapshot ends.
//   - Target connections never open the target file. A load builds a new file
//     next to it, and Commit renames that file into place, replacing the whole
//     database at once.
//   - SQLite stores each value as INTEGER, REAL, TEXT or BLOB, whatever the
//     column's declared type. Values come through with their stored kind, and
//     become the column's natural kind only when that is exactly reversible
//     (see types.go), so a copy is always byte for byte.
//   - Error messages never contain the file path or row values.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver

	"github.com/exarth-labs/stuntdb/dialect"
	"github.com/exarth-labs/stuntdb/internal/schema"
)

// mainNamespace is the namespace of the database file itself. Attached
// databases would add others, but a URL names a single file.
const mainNamespace schema.Ident = "main"

// busyTimeoutMillis is how long a source connection waits for a writer's lock
// before giving up, in rollback-journal mode.
const busyTimeoutMillis = 5000

// defaultMaxConns caps a source connection pool: one for the snapshot, one
// for introspection.
const defaultMaxConns = 2

func init() { dialect.Register(Dialect{}) }

// Dialect is the SQLite adapter.
type Dialect struct{}

// Name implements dialect.Dialect.
func (Dialect) Name() string { return "sqlite" }

// Schemes implements dialect.Dialect.
func (Dialect) Schemes() []string { return []string{"sqlite", "sqlite3"} }

// Open implements dialect.Dialect.
func (Dialect) Open(ctx context.Context, url string, opts dialect.OpenOptions) (dialect.Conn, error) {
	path, err := filePath(url)
	if err != nil {
		return nil, err
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	c := &conn{path: path, role: opts.Role, logger: logger}

	switch opts.Role {
	case dialect.Source:
		db, err := openSource(ctx, path, opts.MaxConns)
		if err != nil {
			return nil, err
		}
		c.db = db
		c.version, err = libraryVersion(ctx, db)
		if err != nil {
			_ = db.Close()
			return nil, err
		}
	case dialect.Target:
		if err := checkTargetPath(path); err != nil {
			return nil, err
		}
		db, err := sql.Open("sqlite", "file::memory:")
		if err != nil {
			return nil, errors.New("sqlite: cannot start the SQLite library")
		}
		c.version, err = libraryVersion(ctx, db)
		_ = db.Close()
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("sqlite: unknown connection role %d", opts.Role)
	}
	return c, nil
}

// filePath returns the file path that a URL names.
func filePath(rawURL string) (string, error) {
	scheme, rest, _ := strings.Cut(rawURL, ":")
	if s := strings.ToLower(scheme); s != "sqlite" && s != "sqlite3" {
		return "", errors.New("sqlite: the URL scheme must be sqlite or sqlite3")
	}
	// "sqlite:///var/app.db" and "sqlite://./app.db" carry an empty host.
	rest = strings.TrimPrefix(rest, "//")
	switch {
	case rest == "":
		return "", errors.New("sqlite: the URL names no database file")
	case rest == ":memory:" || strings.HasPrefix(rest, "file::memory:"):
		return "", errors.New("sqlite: in-memory databases are not supported; name a file")
	case strings.Contains(rest, "?"):
		return "", errors.New("sqlite: SQLite URLs take no options; remove the part from the question mark")
	}
	return rest, nil
}

// fileDSN returns the driver's data source name for a file: a SQLite URI
// filename, so that SQLite applies options such as mode=ro.
func fileDSN(path string, params url.Values) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", errors.New("sqlite: cannot resolve the database file path")
	}
	p := filepath.ToSlash(abs)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p // Windows: file:///C:/data/app.db
	}
	u := url.URL{Scheme: "file", Path: p, RawQuery: params.Encode()}
	return u.String(), nil
}

func openSource(ctx context.Context, path string, maxConns int) (*sql.DB, error) {
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, errors.New("sqlite: the source database file does not exist")
	case err != nil:
		return nil, fmt.Errorf("sqlite: cannot read the source database file: %w", pathless(err))
	case info.IsDir():
		return nil, errors.New("sqlite: the source database path is a folder, not a file")
	}
	dsn, err := fileDSN(path, url.Values{
		"mode":    {"ro"},
		"_pragma": {"query_only(1)", fmt.Sprintf("busy_timeout(%d)", busyTimeoutMillis)},
	})
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, errors.New("sqlite: cannot start the SQLite library")
	}
	if maxConns <= 0 {
		maxConns = defaultMaxConns
	}
	db.SetMaxOpenConns(maxConns)
	// Reading the header proves the file is a SQLite database.
	var schemaVersion int64
	if err := db.QueryRowContext(ctx, "PRAGMA schema_version").Scan(&schemaVersion); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite: cannot open the source database: %w", hide(err, path, dsn))
	}
	return db, nil
}

func checkTargetPath(path string) error {
	dir, err := os.Stat(filepath.Dir(path))
	if err != nil || !dir.IsDir() {
		return errors.New("sqlite: the target database's folder does not exist")
	}
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		return errors.New("sqlite: the target database path is a folder, not a file")
	}
	return nil
}

func libraryVersion(ctx context.Context, db *sql.DB) (string, error) {
	var version string
	if err := db.QueryRowContext(ctx, "SELECT sqlite_version()").Scan(&version); err != nil {
		return "", fmt.Errorf("sqlite: cannot read the SQLite version: %w", err)
	}
	return version, nil
}

// conn is a connection to one database file. A source connection holds a
// pool of read-only connections; a target connection holds nothing open.
type conn struct {
	path    string
	role    dialect.Role
	logger  *slog.Logger
	version string
	db      *sql.DB // source connections only
}

func (c *conn) Info() dialect.ServerInfo {
	return dialect.ServerInfo{Engine: "sqlite", Version: c.version}
}

func (c *conn) Capabilities() dialect.Capabilities {
	return dialect.Capabilities{
		Snapshot:          dialect.SnapshotSingle,
		EmptyStringIsNull: false,
		TableNames:        dialect.CaseInsensitive,
		ColumnNames:       dialect.CaseInsensitive,
		MaxKeyValues:      maxKeyValues,
	}
}

func (c *conn) Introspect(ctx context.Context, f dialect.Filter) (*schema.Database, error) {
	for _, ns := range f.Namespaces {
		if ns != mainNamespace {
			return nil, fmt.Errorf("sqlite: namespace %q does not exist; a SQLite database has only %q", ns, mainNamespace)
		}
	}
	if c.db != nil {
		return introspect(ctx, c.db, c.version, c.logger)
	}
	// A target: read the file if there is one, without keeping it open.
	info, err := os.Stat(c.path)
	if errors.Is(err, fs.ErrNotExist) || (err == nil && info.Size() == 0) {
		return &schema.Database{Engine: "sqlite", Version: c.version, Namespaces: []*schema.Namespace{{Name: mainNamespace}}}, nil
	}
	db, err := openSource(ctx, c.path, 1)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	return introspect(ctx, db, c.version, c.logger)
}

func (c *conn) Snapshot(ctx context.Context) (dialect.Snapshot, error) {
	if c.role != dialect.Source {
		return nil, errors.New("sqlite: a snapshot needs a source connection")
	}
	return newSnapshot(ctx, c.db)
}

func (c *conn) Loader(ctx context.Context, opts dialect.LoadOptions) (dialect.Loader, error) {
	if c.role != dialect.Target {
		return nil, errors.New("sqlite: loading needs a target connection")
	}
	return newLoader(ctx, c.path, opts, c.logger)
}

func (c *conn) Close() error {
	if c.db == nil {
		return nil
	}
	return c.db.Close()
}

// hide returns err with every given string removed from its message, so that
// a driver message can't reveal a file path. It still wraps err, so that
// errors.Is keeps working.
func hide(err error, secrets ...string) error {
	msg := err.Error()
	for _, s := range secrets {
		if s == "" {
			continue
		}
		msg = strings.ReplaceAll(msg, s, "…")
		if abs, e := filepath.Abs(s); e == nil {
			msg = strings.ReplaceAll(msg, abs, "…")
			msg = strings.ReplaceAll(msg, filepath.ToSlash(abs), "…")
		}
	}
	if msg == err.Error() {
		return err
	}
	return &hiddenError{msg: msg, err: err}
}

type hiddenError struct {
	msg string
	err error
}

func (e *hiddenError) Error() string { return e.msg }
func (e *hiddenError) Unwrap() error { return e.err }

// pathless returns the cause of a file-system error without the path that os
// adds to it.
func pathless(err error) error {
	if e, ok := errors.AsType[*fs.PathError](err); ok {
		return e.Err
	}
	if e, ok := errors.AsType[*os.LinkError](err); ok {
		return e.Err
	}
	return err
}
