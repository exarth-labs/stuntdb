package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"

	"github.com/exarth-labs/stuntdb/dialect"
	"github.com/exarth-labs/stuntdb/internal/schema"
	"github.com/exarth-labs/stuntdb/internal/value"
)

// snapshot is a read transaction on one connection. Every read in it sees
// the database as it was when the snapshot started.
type snapshot struct {
	tx *sql.Tx

	// maxKeys is the KeysIn limit per read; tests lower it.
	maxKeys int

	mu    sync.Mutex
	types map[schema.TableRef]map[schema.Ident]value.Type
	keys  map[schema.TableRef][]uniqueKey
}

func newSnapshot(ctx context.Context, db *sql.DB) (*snapshot, error) {
	// The transaction lives until Close, not until ctx ends: each read
	// takes its own context.
	tx, err := db.BeginTx(context.WithoutCancel(ctx), nil)
	if err != nil {
		return nil, fmt.Errorf("sqlite: start a snapshot: %w", err)
	}
	// SQLite starts a transaction's view at its first read, so read now: the
	// view then shows the database as it is at this moment.
	var n int64
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM main.sqlite_schema").Scan(&n); err != nil {
		_ = tx.Rollback()
		return nil, fmt.Errorf("sqlite: start a snapshot: %w", err)
	}
	return &snapshot{
		tx: tx, maxKeys: maxKeyValues,
		types: map[schema.TableRef]map[schema.Ident]value.Type{},
		keys:  map[schema.TableRef][]uniqueKey{},
	}, nil
}

// Point returns an empty string: SQLite has no log position that names the
// moment a snapshot shows.
func (s *snapshot) Point() string { return "" }

// Join returns ErrNotSupported. In WAL mode every connection has its own
// view, so a second reader can't be put on this one.
func (s *snapshot) Join(context.Context) (dialect.Snapshot, error) {
	return nil, dialect.ErrNotSupported
}

func (s *snapshot) Close() error {
	if err := s.tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		return fmt.Errorf("sqlite: end the snapshot: %w", err)
	}
	return nil
}

func (s *snapshot) Read(ctx context.Context, q dialect.ReadQuery) (dialect.RowIter, error) {
	var keys []uniqueKey
	if len(q.OrderBy) > 0 || hasAfter(q.Where) {
		var err error
		if keys, err = s.uniqueKeys(ctx, q.Table); err != nil {
			return nil, err
		}
	}
	stmt, err := renderRead(q, s.maxKeys, keys)
	if err != nil {
		return nil, err
	}
	columnTypes, err := s.columnTypes(ctx, q.Table)
	if err != nil {
		return nil, err
	}
	types := make([]value.Type, len(q.Columns))
	for i, c := range q.Columns {
		t, ok := columnTypes[c]
		if !ok {
			return nil, fmt.Errorf("sqlite: read %s: column %q does not exist", q.Table, c)
		}
		types[i] = t
	}
	cur, err := s.tx.QueryContext(ctx, stmt.sql, stmt.args...)
	if err != nil {
		return nil, fmt.Errorf("sqlite: read %s: %w", q.Table, err)
	}
	raw := make([]any, len(q.Columns))
	ptrs := make([]any, len(q.Columns))
	for i := range raw {
		ptrs[i] = &raw[i]
	}
	return &rows{cur: cur, table: q.Table, types: types, raw: raw, ptrs: ptrs, row: make([]value.Value, len(q.Columns))}, nil
}

// columnTypes returns the canonical type of each column of a table, to turn
// stored values into canonical ones. They are read inside the transaction,
// so they match the data.
func (s *snapshot) columnTypes(ctx context.Context, ref schema.TableRef) (map[schema.Ident]value.Type, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if types, ok := s.types[ref]; ok {
		return types, nil
	}
	rows, err := s.tx.QueryContext(ctx, `SELECT name, type FROM pragma_table_xinfo(?, ?)`, string(ref.Name), string(ref.Namespace))
	if err != nil {
		return nil, fmt.Errorf("sqlite: read %s: %w", ref, err)
	}
	defer func() { _ = rows.Close() }()
	types := map[schema.Ident]value.Type{}
	for rows.Next() {
		var name, declared string
		if err := rows.Scan(&name, &declared); err != nil {
			return nil, fmt.Errorf("sqlite: read %s: %w", ref, err)
		}
		types[schema.Ident(name)] = columnType(declared)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: read %s: %w", ref, err)
	}
	if len(types) == 0 {
		return nil, fmt.Errorf("sqlite: read %s: the table does not exist", ref)
	}
	s.types[ref] = types
	return types, nil
}

// uniqueKeys returns a table's unique keys over plain columns, without the
// partial ones (they don't cover every row): the primary key first, then
// constraints, then indexes. An INTEGER PRIMARY KEY is the rowid and has no
// index; it holds only integers, which no collation affects.
func (s *snapshot) uniqueKeys(ctx context.Context, ref schema.TableRef) ([]uniqueKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if keys, ok := s.keys[ref]; ok {
		return keys, nil
	}
	rows, err := s.tx.QueryContext(ctx, `
SELECT il.name, ix.cid, ix.name, ix.coll
FROM pragma_index_list(?, ?) AS il, pragma_index_xinfo(il.name, ?) AS ix
WHERE il."unique" = 1 AND il.partial = 0 AND ix.key = 1
ORDER BY CASE il.origin WHEN 'pk' THEN 0 WHEN 'u' THEN 1 ELSE 2 END, il.name, ix.seqno`,
		string(ref.Name), string(ref.Namespace), string(ref.Namespace))
	if err != nil {
		return nil, fmt.Errorf("sqlite: read %s: %w", ref, err)
	}
	defer func() { _ = rows.Close() }()
	var (
		keys    []uniqueKey
		index   string
		cur     uniqueKey
		plain   bool
		started bool
	)
	flush := func() {
		if started && plain {
			keys = append(keys, cur)
		}
	}
	for rows.Next() {
		var (
			name, coll string
			cid        int64
			column     sql.NullString
		)
		if err := rows.Scan(&name, &cid, &column, &coll); err != nil {
			return nil, fmt.Errorf("sqlite: read %s: %w", ref, err)
		}
		if !started || name != index {
			flush()
			index, cur, plain, started = name, uniqueKey{}, true, true
		}
		if cid < 0 || !column.Valid {
			plain = false // an expression or the rowid
			continue
		}
		cur.columns = append(cur.columns, schema.Ident(column.String))
		cur.collations = append(cur.collations, coll)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: read %s: %w", ref, err)
	}
	flush()
	s.keys[ref] = keys
	return keys, nil
}

// rows streams the rows of one read, converting each value.
type rows struct {
	cur   *sql.Rows
	table schema.TableRef
	types []value.Type
	raw   []any
	ptrs  []any
	row   []value.Value
	err   error
}

func (r *rows) Next() bool {
	if r.err != nil || !r.cur.Next() {
		return false
	}
	if err := r.cur.Scan(r.ptrs...); err != nil {
		return r.fail(err)
	}
	for i, raw := range r.raw {
		v, err := fromStorage(raw, r.types[i])
		if err != nil {
			return r.fail(err)
		}
		r.row[i] = v
	}
	return true
}

func (r *rows) fail(err error) bool {
	r.err = fmt.Errorf("sqlite: read %s: %w", r.table, err)
	return false
}

func (r *rows) Row() []value.Value { return r.row }

func (r *rows) Err() error {
	if r.err != nil {
		return r.err
	}
	if err := r.cur.Err(); err != nil {
		return fmt.Errorf("sqlite: read %s: %w", r.table, err)
	}
	return nil
}

func (r *rows) Close() error { return r.cur.Close() }
