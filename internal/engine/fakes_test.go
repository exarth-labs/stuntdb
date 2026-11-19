package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/exarth-labs/stuntdb/dialect"
	"github.com/exarth-labs/stuntdb/internal/schema"
	"github.com/exarth-labs/stuntdb/internal/value"
)

// genTable is a table whose rows are computed from their number, so a
// source can hold millions of them without storing any. Its key is its
// first column, an integer equal to the row number, so rows come in key
// order.
type genTable struct {
	rows    int64
	columns []schema.Ident
	value   func(column int, i int64) value.Value
}

// genSource is a streaming source. It honors OrderBy by the key, After and
// Limit, allows one open read per reader like the strict engines, and stops
// on cancel. Its knobs make it misbehave for the error and cancel tests.
type genSource struct {
	tables map[schema.TableRef]*genTable
	mode   dialect.SnapshotMode
	stats  *genStats

	// Misbehavior, for tests.
	stuck      bool  // ignore After, so chunks never advance
	inclusive  bool  // treat After as >=, returning the last row again
	blockAt    int64 // >0: block at this row until canceled
	failRead   error // returned by Read
	failAt     int64 // >0: Err returns failIter after this many rows of a read
	failIter   error
	failClose  error
	failJoin   error
	blockJoin  bool  // Join waits until canceled
	failClosed error // returned when a joined reader closes
	extraValue bool  // rows have one value too many

	open *genIter
}

type genStats struct {
	reads   atomic.Int64 // statements
	rows    atomic.Int64 // rows returned
	joined  atomic.Int64
	closed  atomic.Int64 // joined readers closed
	reading atomic.Int64 // readers with an open read now
	maxRead atomic.Int64 // the most readers with an open read at once
}

// maxGenReads is far above what any test reads.
const maxGenReads = 100_000

func newGenSource(mode dialect.SnapshotMode, tables map[schema.TableRef]*genTable) *genSource {
	return &genSource{tables: tables, mode: mode, stats: &genStats{}}
}

func (s *genSource) Point() string { return "" }

func (s *genSource) Join(ctx context.Context) (dialect.Snapshot, error) {
	if s.mode != dialect.SnapshotShared {
		return nil, dialect.ErrNotSupported
	}
	if s.blockJoin {
		<-ctx.Done()
		return nil, errors.New("gen: join interrupted")
	}
	if s.failJoin != nil {
		return nil, s.failJoin
	}
	if s.open != nil && !s.open.closed {
		return nil, errors.New("gen: joined while a read is open on this reader")
	}
	s.stats.joined.Add(1)
	j := *s
	j.open = nil
	return &joinedGen{genSource: &j}, nil
}

func (s *genSource) Close() error { return nil }

type joinedGen struct{ *genSource }

func (j *joinedGen) Close() error { j.stats.closed.Add(1); return j.failClosed }

func (s *genSource) Read(ctx context.Context, q dialect.ReadQuery) (dialect.RowIter, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.failRead != nil {
		return nil, s.failRead
	}
	if s.open != nil && !s.open.closed {
		return nil, errors.New("gen: a read is still open on this reader")
	}
	if err := q.Validate(); err != nil {
		return nil, err
	}
	t := s.tables[q.Table]
	if t == nil {
		return nil, fmt.Errorf("gen: no table %s", q.Table)
	}
	if len(q.OrderBy) > 0 && !slices.Equal(q.OrderBy, t.columns[:1]) {
		return nil, errors.New("gen: rows can only be ordered by the first column")
	}
	cols := make([]int, len(q.Columns))
	for i, c := range q.Columns {
		if cols[i] = slices.Index(t.columns, c); cols[i] < 0 {
			return nil, fmt.Errorf("gen: no column %s", c)
		}
	}
	from := int64(0)
	if a, ok := q.Where.(dialect.After); ok && !s.stuck {
		from = a.Key[0].Int() + 1
		if s.inclusive {
			from--
		}
	}
	to := t.rows
	if q.Limit > 0 {
		to = min(to, from+int64(q.Limit))
	}
	width := len(cols)
	if s.extraValue {
		width++
	}
	// A reader that never stops would hang the test; fail it instead.
	if s.stats.reads.Add(1) > maxGenReads {
		return nil, errors.New("gen: too many reads; the reader doesn't stop")
	}
	raise(&s.stats.maxRead, s.stats.reading.Add(1))
	s.open = &genIter{ctx: ctx, s: s, t: t, cols: cols, next: from, to: to, buf: make([]value.Value, width)}
	return s.open, nil
}

type genIter struct {
	ctx      context.Context
	s        *genSource
	t        *genTable
	cols     []int
	next, to int64
	read     int64
	buf      []value.Value
	err      error
	closed   bool
}

func (it *genIter) Next() bool {
	if it.err != nil || it.closed || it.next >= it.to {
		return false
	}
	if err := it.ctx.Err(); err != nil {
		it.err = err
		return false
	}
	if it.s.blockAt > 0 && it.next == it.s.blockAt {
		<-it.ctx.Done()
		it.err = it.ctx.Err()
		return false
	}
	if it.s.failAt > 0 && it.read == it.s.failAt {
		it.err = it.s.failIter
		return false
	}
	for i, c := range it.cols {
		it.buf[i] = it.t.value(c, it.next)
	}
	it.next++
	it.read++
	it.s.stats.rows.Add(1)
	return true
}

func (it *genIter) Row() []value.Value { return it.buf }
func (it *genIter) Err() error         { return it.err }

func (it *genIter) Close() error {
	if !it.closed {
		it.closed = true
		it.s.stats.reading.Add(-1)
	}
	return it.s.failClose
}

// raise sets m to n if n is larger.
func raise(m *atomic.Int64, n int64) {
	for {
		old := m.Load()
		if n <= old || m.CompareAndSwap(old, n) {
			return
		}
	}
}

// sinkFunc makes a function a Sink.
type sinkFunc func(ctx context.Context, table schema.TableRef, columns []schema.Ident, rows dialect.RowIter) (int64, error)

func (f sinkFunc) Load(ctx context.Context, table schema.TableRef, columns []schema.Ident, rows dialect.RowIter) (int64, error) {
	return f(ctx, table, columns, rows)
}

// drain reads rows to the end, as a well-behaved sink does.
func drain(rows dialect.RowIter) (int64, error) {
	var n int64
	for rows.Next() {
		n++
	}
	return n, rows.Err()
}

// discard is a sink that reads every row and keeps none.
var discard = sinkFunc(func(_ context.Context, _ schema.TableRef, _ []schema.Ident, rows dialect.RowIter) (int64, error) {
	return drain(rows)
})

// recorder is a sink that keeps every row it gets, per table.
type recorder struct {
	mu      sync.Mutex
	columns map[schema.TableRef][]schema.Ident
	rows    map[schema.TableRef][][]value.Value
	loading atomic.Int64
	maxLoad atomic.Int64 // the most tables loading at once
}

func newRecorder() *recorder {
	return &recorder{columns: map[schema.TableRef][]schema.Ident{}, rows: map[schema.TableRef][][]value.Value{}}
}

func (r *recorder) Load(_ context.Context, table schema.TableRef, columns []schema.Ident, rows dialect.RowIter) (int64, error) {
	raise(&r.maxLoad, r.loading.Add(1))
	defer r.loading.Add(-1)
	var got [][]value.Value
	for rows.Next() {
		got = append(got, slices.Clone(rows.Row()))
	}
	r.mu.Lock()
	r.columns[table] = slices.Clone(columns)
	r.rows[table] = got
	r.mu.Unlock()
	return int64(len(got)), rows.Err()
}

// serialSink writes one table at a time, as SQLite's loader does: a table
// waits for the token before reading any row.
func serialSink(next Sink) Sink {
	token := make(chan struct{}, 1)
	return sinkFunc(func(ctx context.Context, table schema.TableRef, columns []schema.Ident, rows dialect.RowIter) (int64, error) {
		select {
		case token <- struct{}{}:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
		defer func() { <-token }()
		return next.Load(ctx, table, columns, rows)
	})
}
