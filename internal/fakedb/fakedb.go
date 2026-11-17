// Package fakedb is an in-memory database adapter for tests.
//
// It deliberately behaves like the "hard" engines, the opposite of SQLite
// wherever SQLite is unusual, so that neither the core nor the conformance
// suite can quietly come to depend on SQLite behavior:
//
//   - several namespaces, and shared snapshots that parallel readers can join
//     (WithSnapshot models the single-snapshot engines instead);
//   - one open read per reader, as MySQL allows one result per connection,
//     and one load per table, as COPY streams a table in one statement;
//   - an empty string is stored as NULL, as in Oracle;
//   - unquoted names fold to upper case, as in Oracle;
//   - foreign keys and unique keys are checked only in Finish, after loading;
//   - at most MaxParams key values per read, so callers must send long key
//     lists in batches, and Statements counts the reads;
//   - rows come back in a reused slice, so callers that keep references break;
//   - reads and loads stop when their context is canceled;
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
	"math"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/exarth-labs/stuntdb/dialect"
	"github.com/exarth-labs/stuntdb/internal/schema"
	"github.com/exarth-labs/stuntdb/internal/value"
)

// MaxParams is the limit on key values per read, reported as
// Capabilities.MaxKeyValues.
const MaxParams = 5

// Dialect is the fake engine. Use New to make one.
type Dialect struct {
	mode       dialect.SnapshotMode
	mu         sync.Mutex
	databases  map[string]*store
	statements atomic.Int64
}

// Option configures a Dialect.
type Option func(*Dialect)

// WithSnapshot sets the snapshot mode the engine reports. Only
// SnapshotShared readers can Join. The default is SnapshotShared.
func WithSnapshot(mode dialect.SnapshotMode) Option {
	return func(d *Dialect) { d.mode = mode }
}

// store is one database: its structure and the rows of each table, in the
// table's column order.
type store struct {
	db      *schema.Database
	rows    map[schema.TableRef][][]value.Value
	version int64
}

// New returns a fake engine with an empty store.
func New(opts ...Option) *Dialect {
	d := &Dialect{mode: dialect.SnapshotShared, databases: make(map[string]*store)}
	for _, o := range opts {
		o(d)
	}
	return d
}

// Create stores the structure of a database, without rows, under name, so
// that "fake:<name>" opens it. It replaces any database of that name.
func (d *Dialect) Create(name string, db *schema.Database) {
	c := &schema.Database{Engine: "fake", Version: "1.0"}
	for _, ns := range db.Namespaces {
		copied := &schema.Namespace{Name: ns.Name}
		for _, t := range ns.Tables {
			copied.Tables = append(copied.Tables, cloneTable(t))
		}
		c.Namespaces = append(c.Namespaces, copied)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.databases[name] = &store{db: c, rows: map[schema.TableRef][][]value.Value{}}
}

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
		Snapshot:          c.d.mode,
		EmptyStringIsNull: true,
		TableNames:        dialect.FoldUpper,
		ColumnNames:       dialect.FoldUpper,
		MaxKeyValues:      MaxParams,
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
	d    *Dialect
	s    *store
	open *rowIter // the read not yet closed, if any
}

func (s *snapshot) Point() string { return strconv.FormatInt(s.s.version, 10) }

func (s *snapshot) Join(context.Context) (dialect.Snapshot, error) {
	if s.d.mode != dialect.SnapshotShared {
		return nil, dialect.ErrNotSupported
	}
	return &snapshot{d: s.d, s: s.s}, nil
}

func (s *snapshot) Close() error { return nil }

func (s *snapshot) Read(ctx context.Context, q dialect.ReadQuery) (dialect.RowIter, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.open != nil && !s.open.closed {
		return nil, errors.New("fakedb: a read is still open on this snapshot; close it before the next one")
	}
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
	if n := keyValues(q.Where); n > MaxParams {
		return nil, fmt.Errorf("%w: %d key values, at most %d", dialect.ErrTooManyKeys, n, MaxParams)
	}
	s.d.statements.Add(1)

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
	s.open = &rowIter{ctx: ctx, rows: out, buf: make([]value.Value, len(q.Columns))}
	return s.open, nil
}

// keyValues counts the KeysIn values in a condition.
func keyValues(p dialect.Predicate) int {
	switch p := p.(type) {
	case dialect.KeysIn:
		return len(p.Columns) * len(p.Keys)
	case dialect.And:
		n := 0
		for _, sub := range p {
			n += keyValues(sub)
		}
		return n
	default:
		return 0
	}
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
	case value.KindBool:
		return cmp.Compare(boolRank(a.Bool()), boolRank(b.Bool())), nil
	case value.KindInt:
		return cmp.Compare(a.Int(), b.Int()), nil
	case value.KindUint:
		return cmp.Compare(a.Uint(), b.Uint()), nil
	case value.KindFloat:
		// NaN last, as for decimals; cmp.Compare puts it first.
		x, y := a.Float(), b.Float()
		if nx, ny := math.IsNaN(x), math.IsNaN(y); nx || ny {
			return cmp.Compare(boolRank(nx), boolRank(ny)), nil
		}
		return cmp.Compare(x, y), nil
	case value.KindString:
		return strings.Compare(a.Text(), b.Text()), nil
	case value.KindBytes:
		return bytes.Compare(a.Bytes(), b.Bytes()), nil
	case value.KindUUID:
		x, y := a.UUID(), b.UUID()
		return bytes.Compare(x[:], y[:]), nil
	case value.KindDecimal, value.KindDate, value.KindTimestamp:
		sa, sb := a.Special(), b.Special()
		switch {
		case sa == value.ZeroDate && sb == value.ZeroDate:
			return strings.Compare(a.ZeroText(), b.ZeroText()), nil
		case sa != value.Normal || sb != value.Normal:
			return cmp.Compare(specialRank(sa), specialRank(sb)), nil
		case a.Kind() == value.KindDecimal:
			x, _ := new(big.Rat).SetString(a.DecimalText())
			y, _ := new(big.Rat).SetString(b.DecimalText())
			return x.Cmp(y), nil
		case a.Kind() == value.KindDate:
			return dateOf(a).Compare(dateOf(b)), nil
		default:
			return a.Timestamp().Compare(b.Timestamp()), nil
		}
	case value.KindTime:
		return cmp.Compare(a.TimeOfDay(), b.TimeOfDay()), nil
	default:
		return 0, fmt.Errorf("%w: fakedb cannot order %s values", dialect.ErrNotSupported, a.Kind())
	}
}

func boolRank(b bool) int {
	if b {
		return 1
	}
	return 0
}

// specialRank orders special values as PostgreSQL does, with MySQL's zero
// dates before every real date: -infinity, zero dates, the rest, infinity,
// then NaN.
func specialRank(s value.Special) int {
	switch s {
	case value.NegInfinity:
		return 0
	case value.ZeroDate:
		return 1
	case value.Normal:
		return 2
	case value.PosInfinity:
		return 3
	default:
		return 4
	}
}

func dateOf(v value.Value) time.Time {
	y, m, d := v.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

type rowIter struct {
	ctx    context.Context
	rows   [][]value.Value
	next   int
	buf    []value.Value
	err    error
	closed bool
}

func (r *rowIter) Next() bool {
	if r.closed || r.err != nil || r.next >= len(r.rows) {
		return false
	}
	if err := r.ctx.Err(); err != nil {
		r.err = err
		return false
	}
	copy(r.buf, r.rows[r.next]) // the same slice every time, like real drivers
	r.next++
	return true
}

func (r *rowIter) Row() []value.Value { return r.buf }
func (r *rowIter) Err() error         { return r.err }
func (r *rowIter) Close() error       { r.closed = true; return nil }

// loader loads into a private staging store, and swaps it in on Commit.
type loader struct {
	d        *Dialect
	name     string
	opts     dialect.LoadOptions
	mu       sync.Mutex
	staging  *store
	loaded   map[schema.TableRef]bool
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
	l.loaded = map[schema.TableRef]bool{}
	return nil
}

// Load writes NULL where a column is left to its default or identity: the
// fake can't evaluate SQL expressions.
func (l *loader) Load(ctx context.Context, ref schema.TableRef, columns []schema.Ident, rows dialect.RowIter) (int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.staging == nil {
		return 0, errors.New("fakedb: Load before CreateTables")
	}
	t := l.staging.table(ref)
	if t == nil {
		return 0, fmt.Errorf("fakedb: table %s does not exist", ref)
	}
	if l.loaded[ref] {
		return 0, fmt.Errorf("fakedb: %s is already loaded; a table is loaded in one call", ref)
	}
	l.loaded[ref] = true
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
		if err := ctx.Err(); err != nil {
			return n, err
		}
		in := rows.Row()
		if len(in) != len(idx) {
			return n, fmt.Errorf("fakedb: row %d of %s has %d values for %d columns", n+1, ref, len(in), len(idx))
		}
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
			filled := col.Nullable || col.Generated != nil || col.Default != nil || col.Identity != nil
			if !filled && !slices.Contains(idx, i) {
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
