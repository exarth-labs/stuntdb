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
	"slices"
	"strings"
	"sync"

	"github.com/exarth-labs/stuntdb/dialect"
	"github.com/exarth-labs/stuntdb/internal/outfile"
	"github.com/exarth-labs/stuntdb/internal/schema"
)

// rowsPerInsert is the most rows one INSERT statement writes. 100-row
// statements are clearly faster than single rows; beyond that the gain is
// small.
const rowsPerInsert = 100

// cacheKiB is the page cache of the staging file, which mostly speeds up
// building indexes after the load.
const cacheKiB = 64 * 1024

type loadState uint8

const (
	stateOpen loadState = iota
	stateCreated
	stateFinished
	stateCommitted
	stateFailed
	stateAborted
)

// loader builds the copy in a staging file next to the target, named
// ".<target>.partial-<run ID>" like every other partial output, so that
// cleanup after a crash finds it. Commit renames it over the target.
type loader struct {
	target  string
	staging string
	opts    dialect.LoadOptions
	logger  *slog.Logger

	// mu makes writes take turns: SQLite has one writer at a time.
	mu        sync.Mutex
	db        *sql.DB
	state     loadState
	namespace schema.Ident
	tables    map[schema.Ident]*schema.Table
}

func newLoader(ctx context.Context, target string, opts dialect.LoadOptions, logger *slog.Logger) (*loader, error) {
	if opts.RunID == "" || strings.ContainsAny(opts.RunID, `/\:`) || opts.RunID != filepath.Base(opts.RunID) {
		return nil, errors.New("sqlite: loading needs a run ID that is a plain name")
	}
	staging := filepath.Join(filepath.Dir(target), "."+filepath.Base(target)+".partial-"+opts.RunID)

	// Create the file exclusively, so that two runs can never share one.
	// An empty file is a valid, empty SQLite database.
	f, err := os.OpenFile(staging, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // next to the target the user named
	if errors.Is(err, fs.ErrExist) {
		return nil, errors.New("sqlite: a staging file for this run ID already exists")
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: create the staging file: %w", pathless(err))
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(staging)
		return nil, fmt.Errorf("sqlite: create the staging file: %w", pathless(err))
	}

	l := &loader{target: target, staging: staging, opts: opts, logger: logger}
	// The staging file is thrown away if anything fails, so it needs no
	// rollback journal and no syncing while it loads. Commit syncs it once,
	// at the end.
	dsn, err := fileDSN(staging, url.Values{"_pragma": {
		"journal_mode(OFF)",
		"synchronous(OFF)",
		"locking_mode(EXCLUSIVE)",
		fmt.Sprintf("cache_size(-%d)", cacheKiB),
	}})
	if err == nil {
		l.db, err = sql.Open("sqlite", dsn)
	}
	if err == nil {
		l.db.SetMaxOpenConns(1)
		err = l.db.PingContext(ctx)
	}
	if err != nil {
		_ = l.Abort(ctx)
		return nil, fmt.Errorf("sqlite: open the staging file: %w", hide(err, staging, dsn))
	}
	return l, nil
}

func (l *loader) CreateTables(ctx context.Context, db *schema.Database) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state != stateOpen {
		return errors.New("sqlite: CreateTables must be the first step of a load")
	}
	ns, err := onlyNamespace(db)
	if err != nil {
		return l.failed(err)
	}
	native := db.Engine == "sqlite"
	l.namespace = ns.Name
	l.tables = map[schema.Ident]*schema.Table{}
	for _, t := range ns.Tables {
		stmt := t.Native
		if !native || stmt == "" {
			if stmt, err = createTableSQL(t); err != nil {
				return l.failed(err)
			}
		}
		if _, err := l.db.ExecContext(ctx, stmt); err != nil {
			return l.failed(fmt.Errorf("sqlite: create table %s: %w", t.Name, err))
		}
		l.tables[t.Name] = t
	}
	l.state = stateCreated
	return nil
}

// onlyNamespace returns the one namespace that has tables: a SQLite file
// holds a single namespace, so they all go into it.
func onlyNamespace(db *schema.Database) (*schema.Namespace, error) {
	var found *schema.Namespace
	for _, ns := range db.Namespaces {
		if len(ns.Tables) == 0 && len(ns.Objects) == 0 {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("sqlite: a SQLite database holds one namespace, but the copy has %q and %q", found.Name, ns.Name)
		}
		found = ns
	}
	if found == nil {
		found = &schema.Namespace{Name: mainNamespace}
	}
	return found, nil
}

func (l *loader) Load(ctx context.Context, ref schema.TableRef, columns []schema.Ident, rows dialect.RowIter) (int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state != stateCreated {
		return 0, errors.New("sqlite: Load needs CreateTables first, and must come before Finish")
	}
	if ref.Namespace != l.namespace || l.tables[ref.Name] == nil {
		return 0, fmt.Errorf("sqlite: table %s is not part of this load", ref)
	}
	if len(columns) == 0 {
		return 0, fmt.Errorf("sqlite: load %s: no columns", ref)
	}
	n, err := l.insert(ctx, schema.TableRef{Namespace: mainNamespace, Name: ref.Name}, columns, rows)
	if err != nil {
		return n, l.failed(fmt.Errorf("sqlite: load %s: %w", ref, err))
	}
	return n, nil
}

func (l *loader) insert(ctx context.Context, table schema.TableRef, columns []schema.Ident, rows dialect.RowIter) (int64, error) {
	batch := min(rowsPerInsert, maxParams/len(columns))
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	full, err := tx.PrepareContext(ctx, insertSQL(table, columns, batch))
	if err != nil {
		return 0, err
	}
	defer func() { _ = full.Close() }()

	args := make([]any, 0, batch*len(columns))
	var written, pending int64
	for rows.Next() {
		row := rows.Row()
		if len(row) != len(columns) {
			return written, fmt.Errorf("row %d has %d values for %d columns", written+pending+1, len(row), len(columns))
		}
		for i, v := range row {
			a, err := toStorage(v)
			if err != nil {
				return written, fmt.Errorf("row %d, column %q: %w", written+pending+1, columns[i], err)
			}
			args = append(args, a)
		}
		pending++
		if pending == int64(batch) {
			if _, err := full.ExecContext(ctx, args...); err != nil {
				return written, err
			}
			written += pending
			pending, args = 0, args[:0]
		}
	}
	if err := rows.Err(); err != nil {
		return written, err
	}
	if pending > 0 {
		if _, err := tx.ExecContext(ctx, insertSQL(table, columns, int(pending)), args...); err != nil {
			return written, err
		}
		written += pending
	}
	return written, tx.Commit()
}

func (l *loader) Finish(ctx context.Context, db *schema.Database) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state != stateCreated {
		return errors.New("sqlite: Finish needs CreateTables first, and runs once")
	}
	ns, err := onlyNamespace(db)
	if err != nil {
		return l.failed(err)
	}
	native := db.Engine == "sqlite"

	// Indexes are built now, after the data, which is many times faster.
	for _, t := range ns.Tables {
		for _, ix := range t.Indexes {
			stmt := ix.Native
			if !native || stmt == "" {
				if stmt, err = createIndexSQL(t, ix); err != nil {
					return l.failed(err)
				}
			}
			if _, err := l.db.ExecContext(ctx, stmt); err != nil {
				return l.failed(fmt.Errorf("sqlite: create index %s on %s: %w", ix.Name, t.Name, err))
			}
		}
	}

	// Views, triggers and virtual tables come last, in the order they were
	// first created, so that triggers never fire while the data loads.
	// Another engine's are written in its own SQL, so they are skipped.
	for _, obj := range ns.Objects {
		if !native {
			l.logger.Warn("sqlite: an object from another engine is skipped", "object", string(obj.Name))
			continue
		}
		if _, err := l.db.ExecContext(ctx, obj.Native); err != nil {
			return l.failed(fmt.Errorf("sqlite: create %s: %w", obj.Name, err))
		}
	}

	if err := l.checkForeignKeys(ctx); err != nil {
		return l.failed(err)
	}
	// AUTOINCREMENT counters (sqlite_sequence) already moved past the loaded
	// values as rows were inserted; plain rowids need nothing.
	l.state = stateFinished
	return nil
}

// checkForeignKeys fails if any row references a parent row that doesn't
// exist. The error counts the broken rows per table, and never shows them.
func (l *loader) checkForeignKeys(ctx context.Context) error {
	rows, err := l.db.QueryContext(ctx, "SELECT \"table\", parent FROM pragma_foreign_key_check")
	if err != nil {
		return fmt.Errorf("sqlite: check foreign keys: %w", err)
	}
	defer func() { _ = rows.Close() }()
	type edge struct{ child, parent string }
	counts := map[edge]int{}
	for rows.Next() {
		var e edge
		if err := rows.Scan(&e.child, &e.parent); err != nil {
			return fmt.Errorf("sqlite: check foreign keys: %w", err)
		}
		counts[e]++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("sqlite: check foreign keys: %w", err)
	}
	if len(counts) == 0 {
		return nil
	}
	var parts []string
	total := 0
	for e, n := range counts {
		parts = append(parts, fmt.Sprintf("%d in %s (referencing %s)", n, e.child, e.parent))
		total += n
	}
	slices.Sort(parts)
	return fmt.Errorf("sqlite: %d rows reference rows that don't exist: %s", total, strings.Join(parts, ", "))
}

func (l *loader) Commit(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state != stateFinished {
		return errors.New("sqlite: Commit needs a finished load")
	}
	if info, err := os.Stat(l.target); err == nil && info.Size() > 0 && !l.opts.Replace {
		return errors.New("sqlite: the target database already exists; replace it explicitly")
	}
	// A WAL or rollback journal next to the target belongs to it: renaming a
	// new file over the target while one exists would corrupt the new file.
	for _, suffix := range []string{"-wal", "-journal"} {
		if info, err := os.Stat(l.target + suffix); err == nil && info.Size() > 0 {
			return errors.New("sqlite: the target database is open in another program, or was not closed cleanly; close it and try again")
		}
	}
	if err := l.db.Close(); err != nil {
		return fmt.Errorf("sqlite: close the staging file: %w", err)
	}
	l.db = nil
	if err := outfile.Commit(ctx, l.staging, l.target); err != nil {
		return fmt.Errorf("sqlite: commit: %w", err)
	}
	l.state = stateCommitted
	return nil
}

func (l *loader) Abort(context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state == stateCommitted || l.state == stateAborted {
		return nil
	}
	if l.db != nil {
		_ = l.db.Close()
		l.db = nil
	}
	l.state = stateAborted
	var problems []error
	for _, suffix := range []string{"", "-journal", "-wal", "-shm"} {
		if err := os.Remove(l.staging + suffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
			problems = append(problems, pathless(err))
		}
	}
	if err := errors.Join(problems...); err != nil {
		return fmt.Errorf("sqlite: remove the staging file: %w", err)
	}
	return nil
}

// failed marks the load as failed, so that it can only be aborted, and
// returns err.
func (l *loader) failed(err error) error {
	l.state = stateFailed
	return err
}
