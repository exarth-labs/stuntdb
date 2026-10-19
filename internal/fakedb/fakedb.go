// Package fakedb is an in-memory database adapter for tests.
//
// It deliberately behaves like the "hard" engines, the opposite of SQLite
// wherever SQLite is unusual, so that neither the core nor the conformance
// suite can quietly come to depend on SQLite behavior:
//
//   - several namespaces, and shared snapshots that parallel readers can join;
//   - an empty string is stored as NULL, as in Oracle;
//   - unquoted names fold to upper case, as in Oracle;
//   - foreign keys and unique keys are checked only in Finish, after loading;
//   - at most MaxParams bind parameters per statement, so long key lists are
//     split, and Statements counts the statements;
//   - rows come back in a reused slice, so callers that keep references break;
//   - native SQL conditions are not supported.
//
// URLs look like "fake:<name>". Each Dialect from New has its own store, so
// tests don't share data.
package fakedb

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/exarth-labs/stuntdb/dialect"
	"github.com/exarth-labs/stuntdb/internal/schema"
	"github.com/exarth-labs/stuntdb/internal/value"
)

// MaxParams is the bind-parameter limit per statement.
const MaxParams = 5

// Dialect is the fake engine. Use New to make one.
type Dialect struct {
	mu         sync.Mutex
	databases  map[string]*store
	statements atomic.Int64
}

// store is one database: its structure and the rows of each table, in the
// table's column order.
type store struct {
	db      *schema.Database
	rows    map[schema.TableRef][][]value.Value
	version int64
}

// New returns a fake engine with an empty store.
func New() *Dialect { return &Dialect{databases: make(map[string]*store)} }

// Statements returns the number of read statements run so far.
func (d *Dialect) Statements() int64 { return d.statements.Load() }

// Name implements dialect.Dialect.
func (d *Dialect) Name() string { return "fake" }

// Schemes implements dialect.Dialect.
func (d *Dialect) Schemes() []string { return []string{"fake"} }

// Open implements dialect.Dialect. URLs with credentials are rejected, which
// lets the conformance suite check that errors never contain the URL.
func (d *Dialect) Open(_ context.Context, url string, opts dialect.OpenOptions) (dialect.Conn, error) {
	name, ok := strings.CutPrefix(url, "fake:")
	if !ok || name == "" {
		return nil, errors.New("fakedb: a URL looks like fake:<name>")
	}
	if strings.ContainsAny(name, "@/") {
		return nil, errors.New("fakedb: the database name is invalid")
	}
	return &conn{d: d, name: name, role: opts.Role}, nil
}

type conn struct {
	d    *Dialect
	name string
	role dialect.Role
}

func (c *conn) Info() dialect.ServerInfo { return dialect.ServerInfo{Engine: "fake", Version: "1.0"} }

func (c *conn) Capabilities() dialect.Capabilities {
	return dialect.Capabilities{
		Snapshot:          dialect.SnapshotShared,
		EmptyStringIsNull: true,
		TableNames:        dialect.FoldUpper,
		ColumnNames:       dialect.FoldUpper,
	}
}

func (c *conn) Introspect(_ context.Context, f dialect.Filter) (*schema.Database, error) {
	c.d.mu.Lock()
	defer c.d.mu.Unlock()
	out := &schema.Database{Engine: "fake", Version: "1.0"}
	s := c.d.databases[c.name]
	if s == nil {
		return out, nil
	}
	for _, ns := range s.db.Namespaces {
		if len(f.Namespaces) > 0 && !slices.Contains(f.Namespaces, ns.Name) {
			continue
		}
		copied := &schema.Namespace{Name: ns.Name}
		for _, t := range ns.Tables {
			ct := cloneTable(t)
			ct.EstimatedRows = int64(len(s.rows[schema.TableRef{Namespace: ns.Name, Name: t.Name}]))
			copied.Tables = append(copied.Tables, ct)
		}
		out.Namespaces = append(out.Namespaces, copied)
	}
	return out, nil
}

func (c *conn) Snapshot(context.Context) (dialect.Snapshot, error) {
	if c.role != dialect.Source {
		return nil, errors.New("fakedb: a snapshot needs a source connection")
	}
	c.d.mu.Lock()
	defer c.d.mu.Unlock()
	s := c.d.databases[c.name]
	if s == nil {
		s = &store{db: &schema.Database{}, rows: map[schema.TableRef][][]value.Value{}}
	}
	return &snapshot{d: c.d, s: s.clone()}, nil
}

func (c *conn) Loader(_ context.Context, opts dialect.LoadOptions) (dialect.Loader, error) {
	if c.role != dialect.Target {
		return nil, errors.New("fakedb: loading needs a target connection")
	}
	return &loader{d: c.d, name: c.name, opts: opts}, nil
}

func (c *conn) Close() error { return nil }

// snapshot reads a frozen copy of a database, so that writes after it
// started stay invisible, and joined readers see exactly the same data.
type snapshot struct {
	d *Dialect
	s *store
}

func (s *snapshot) Point() string { return strconv.FormatInt(s.s.version, 10) }

func (s *snapshot) Join(context.Context) (dialect.Snapshot, error) {
	return &snapshot{d: s.d, s: s.s}, nil
}

func (s *snapshot) Close() error { return nil }

func (s *snapshot) Read(_ context.Context, q dialect.ReadQuery) (dialect.RowIter, error) {
	if err := q.Validate(); err != nil {
		return nil, err
	}
	t := s.s.table(q.Table)
	if t == nil {
		return nil, fmt.Errorf("fakedb: table %s does not exist", q.Table)
	}
	project, err := columnIndexes(t, q.Columns)
	if err != nil {
		return nil, err
	}
	statements, err := statementsFor(q.Where)
	if err != nil {
		return nil, err
	}
	s.d.statements.Add(statements)

	var matched [][]value.Value
	for _, row := range s.s.rows[q.Table] {
		ok, err := matches(t, row, q.Where)
		if err != nil {
			return nil, err
		}
		if ok {
			matched = append(matched, row)
		}
	}
	if len(q.OrderBy) > 0 {
		order, err := columnIndexes(t, q.OrderBy)
		if err != nil {
			return nil, err
		}
		var sortErr error
		slices.SortStableFunc(matched, func(a, b []value.Value) int {
			c, err := compareTuples(pick(a, order), pick(b, order))
			if err != nil && sortErr == nil {
				sortErr = err
			}
			return c
		})
		if sortErr != nil {
			return nil, sortErr
		}
	}
	if q.Limit > 0 && len(matched) > q.Limit {
		matched = matched[:q.Limit]
	}
	out := make([][]value.Value, len(matched))
	for i, row := range matched {
		out[i] = pick(row, project)
	}
	return &rowIter{rows: out, buf: make([]value.Value, len(q.Columns))}, nil
}

// statementsFor counts the statements a real engine with MaxParams would need.
func statementsFor(p dialect.Predicate) (int64, error) {
	keys, ok := p.(dialect.KeysIn)
	if !ok {
		return 1, nil
	}
	width := len(keys.Columns)
	if width > MaxParams {
		return 0, fmt.Errorf("%w: a key of %d columns exceeds %d parameters", dialect.ErrNotSupported, width, MaxParams)
	}
	perStatement := MaxParams / width
	return int64(max(1, (len(keys.Keys)+perStatement-1)/perStatement)), nil
}

func matches(t *schema.Table, row []value.Value, p dialect.Predicate) (bool, error) {
	switch p := p.(type) {
	case nil:
		return true, nil
	case dialect.KeysIn:
		idx, err := columnIndexes(t, p.Columns)
		if err != nil {
			return false, err
		}
		got := pick(row, idx)
		for _, key := range p.Keys {
			if equalNonNull(got, key) {
				return true, nil
			}
		}
		return false, nil
	case dialect.After:
		idx, err := columnIndexes(t, p.Columns)
		if err != nil {
			return false, err
		}
		c, err := compareTuples(pick(row, idx), p.Key)
		return c > 0, err
	case dialect.And:
		for _, sub := range p {
			ok, err := matches(t, row, sub)
			if err != nil || !ok {
				return false, err
			}
		}
		return true, nil
	case dialect.Native:
		return false, fmt.Errorf("%w: fakedb has no SQL", dialect.ErrNotSupported)
	default:
		return false, fmt.Errorf("%w: unknown predicate %T", dialect.ErrNotSupported, p)
	}
}

// equalNonNull follows SQL: NULL never equals anything, not even NULL.
func equalNonNull(a, b []value.Value) bool {
	for i := range a {
		if a[i].IsNull() || b[i].IsNull() || !a[i].Equal(b[i]) {
			return false
		}
	}
	return true
}

// compareTuples orders keys the way this "engine" does. The core never
// compares keys itself.
func compareTuples(a, b []value.Value) (int, error) {
	for i := range a {
		c, err := compareValues(a[i], b[i])
		if err != nil || c != 0 {
			return c, err
		}
	}
	return 0, nil
}

func compareValues(a, b value.Value) (int, error) {
	if a.Kind() != b.Kind() {
		return 0, fmt.Errorf("%w: cannot order a %s against a %s", dialect.ErrNotSupported, a.Kind(), b.Kind())
	}
	switch a.Kind() {
	case value.KindInt:
		return cmp.Compare(a.Int(), b.Int()), nil
	case value.KindUint:
		return cmp.Compare(a.Uint(), b.Uint()), nil
	case value.KindString:
		return strings.Compare(a.Text(), b.Text()), nil
	case value.KindBytes:
		return bytes.Compare(a.Bytes(), b.Bytes()), nil
	default:
		return 0, fmt.Errorf("%w: fakedb cannot order %s values", dialect.ErrNotSupported, a.Kind())
	}
}

type rowIter struct {
	rows   [][]value.Value
	next   int
	buf    []value.Value
	closed bool
}

func (r *rowIter) Next() bool {
	if r.closed || r.next >= len(r.rows) {
		return false
	}
	copy(r.buf, r.rows[r.next]) // the same slice every time, like real drivers
	r.next++
	return true
}

func (r *rowIter) Row() []value.Value { return r.buf }
func (r *rowIter) Err() error         { return nil }
func (r *rowIter) Close() error       { r.closed = true; return nil }

// loader loads into a private staging store, and swaps it in on Commit.
type loader struct {
	d        *Dialect
	name     string
	opts     dialect.LoadOptions
	mu       sync.Mutex
	staging  *store
	finished bool
}

func (l *loader) CreateTables(_ context.Context, db *schema.Database) error {
	if err := db.Validate(); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	s := &store{db: &schema.Database{Engine: "fake", Version: "1.0"}, rows: map[schema.TableRef][][]value.Value{}}
	for _, ns := range db.Namespaces {
		copied := &schema.Namespace{Name: ns.Name}
		for _, t := range ns.Tables {
			copied.Tables = append(copied.Tables, cloneTable(t))
		}
		s.db.Namespaces = append(s.db.Namespaces, copied)
	}
	l.staging = s
	return nil
}

func (l *loader) Load(_ context.Context, ref schema.TableRef, columns []schema.Ident, rows dialect.RowIter) (int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.staging == nil {
		return 0, errors.New("fakedb: Load before CreateTables")
	}
	t := l.staging.table(ref)
	if t == nil {
		return 0, fmt.Errorf("fakedb: table %s does not exist", ref)
	}
	idx, err := columnIndexes(t, columns)
	if err != nil {
		return 0, err
	}
	for _, i := range idx {
		if t.Columns[i].Generated != nil {
			return 0, fmt.Errorf("fakedb: column %q of %s is generated and cannot be written", t.Columns[i].Name, ref)
		}
	}
	var n int64
	for rows.Next() {
		in := rows.Row()
		full := make([]value.Value, len(t.Columns))
		for j, i := range idx {
			v := in[j]
			if v.Kind() == value.KindString && v.Text() == "" {
				v = value.Null() // '' is NULL here, as in Oracle
			}
			col := t.Columns[i]
			if v.IsNull() && !col.Nullable {
				return n, fmt.Errorf("fakedb: column %q of %s cannot be NULL", col.Name, ref)
			}
			if err := col.Type.Check(v); err != nil {
				return n, fmt.Errorf("fakedb: column %q of %s: %w", col.Name, ref, err)
			}
			full[i] = v
		}
		for i, col := range t.Columns {
			if !slices.Contains(idx, i) && !col.Nullable && col.Generated == nil {
				return n, fmt.Errorf("fakedb: column %q of %s cannot be NULL", col.Name, ref)
			}
		}
		l.staging.rows[ref] = append(l.staging.rows[ref], full)
		n++
	}
	return n, rows.Err()
}

func (l *loader) Finish(context.Context, *schema.Database) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.staging == nil {
		return errors.New("fakedb: Finish before CreateTables")
	}
	var problems []error
	for _, ns := range l.staging.db.Namespaces {
		for _, t := range ns.Tables {
			ref := schema.TableRef{Namespace: ns.Name, Name: t.Name}
			rows := l.staging.rows[ref]
			keys := append([]*schema.Key{}, t.Uniques...)
			if t.PrimaryKey != nil {
				keys = append(keys, t.PrimaryKey)
			}
			for _, k := range keys {
				if dup := l.duplicates(t, rows, k.Columns); dup > 0 {
					problems = append(problems, fmt.Errorf("fakedb: %d duplicate keys on (%s) of %s", dup, joinIdents(k.Columns), ref))
				}
			}
			for _, fk := range t.ForeignKeys {
				if missing := l.missingParents(t, rows, fk); missing > 0 {
					problems = append(problems, fmt.Errorf("fakedb: foreign key %q of %s: %d rows reference rows that do not exist", fk.Name, ref, missing))
				}
			}
		}
	}
	if len(problems) == 0 {
		l.finished = true
	}
	return errors.Join(problems...)
}

func (l *loader) duplicates(t *schema.Table, rows [][]value.Value, cols []schema.Ident) int {
	idx, _ := columnIndexes(t, cols)
	var dup int
	for i := range rows {
		for j := range i {
			if equalNonNull(pick(rows[i], idx), pick(rows[j], idx)) {
				dup++
				break
			}
		}
	}
	return dup
}

func (l *loader) missingParents(t *schema.Table, rows [][]value.Value, fk *schema.ForeignKey) int {
	parent := l.staging.table(fk.Ref)
	childIdx, _ := columnIndexes(t, fk.Columns)
	var missing int
	for _, row := range rows {
		key := pick(row, childIdx)
		if slices.ContainsFunc(key, value.Value.IsNull) {
			continue // a NULL reference points nowhere, which is allowed
		}
		found := false
		if parent != nil {
			parentIdx, _ := columnIndexes(parent, fk.RefColumns)
			for _, prow := range l.staging.rows[fk.Ref] {
				if equalNonNull(pick(prow, parentIdx), key) {
					found = true
					break
				}
			}
		}
		if !found {
			missing++
		}
	}
	return missing
}

func (l *loader) Commit(context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.finished {
		return errors.New("fakedb: Commit before a successful Finish")
	}
	l.d.mu.Lock()
	defer l.d.mu.Unlock()
	existing := l.d.databases[l.name]
	if existing != nil && existing.hasTables() && !l.opts.Replace {
		return errors.New("fakedb: the target already has tables, and Replace is not set")
	}
	if existing != nil {
		l.staging.version = existing.version
	}
	l.staging.version++
	l.d.databases[l.name] = l.staging
	l.staging, l.finished = nil, false
	return nil
}

func (l *loader) Abort(context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.staging, l.finished = nil, false
	return nil
}

func (s *store) table(ref schema.TableRef) *schema.Table {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Table(ref)
}

func (s *store) hasTables() bool {
	for _, ns := range s.db.Namespaces {
		if len(ns.Tables) > 0 {
			return true
		}
	}
	return false
}

// clone copies the row lists; values are immutable, so they can be shared.
func (s *store) clone() *store {
	c := &store{db: s.db, rows: make(map[schema.TableRef][][]value.Value, len(s.rows)), version: s.version}
	for ref, rows := range s.rows {
		c.rows[ref] = slices.Clone(rows)
	}
	return c
}

func cloneTable(t *schema.Table) *schema.Table {
	c := *t
	c.Columns = make([]*schema.Column, len(t.Columns))
	for i, col := range t.Columns {
		cc := *col
		c.Columns[i] = &cc
	}
	if t.PrimaryKey != nil {
		pk := *t.PrimaryKey
		c.PrimaryKey = &pk
	}
	c.Uniques = slices.Clone(t.Uniques)
	c.ForeignKeys = slices.Clone(t.ForeignKeys)
	c.Indexes = slices.Clone(t.Indexes)
	c.Checks = slices.Clone(t.Checks)
	return &c
}

func columnIndexes(t *schema.Table, names []schema.Ident) ([]int, error) {
	idx := make([]int, len(names))
	for i, name := range names {
		j := slices.IndexFunc(t.Columns, func(c *schema.Column) bool { return c.Name == name })
		if j < 0 {
			return nil, fmt.Errorf("fakedb: column %q does not exist", name)
		}
		idx[i] = j
	}
	return idx, nil
}

func pick(row []value.Value, idx []int) []value.Value {
	out := make([]value.Value, len(idx))
	for i, j := range idx {
		out[i] = row[j]
	}
	return out
}

func joinIdents(ids []schema.Ident) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = string(id)
	}
	return strings.Join(parts, ", ")
}
