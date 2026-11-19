package engine

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/exarth-labs/stuntdb/dialect"
	"github.com/exarth-labs/stuntdb/internal/mask"
	"github.com/exarth-labs/stuntdb/internal/schema"
	"github.com/exarth-labs/stuntdb/internal/value"
)

// waitForCancel is a sink that takes no rows and waits for the cancel,
// then returns err (or the context's error).
func waitForCancel(err error) Sink {
	return sinkFunc(func(ctx context.Context, _ schema.TableRef, _ []schema.Ident, _ dialect.RowIter) (int64, error) {
		<-ctx.Done()
		if err != nil {
			return 0, err
		}
		return 0, ctx.Err()
	})
}

// TestCancel cancels a copy wherever it waits, and checks that Copy
// returns the cancel's cause and leaves no goroutine behind (synctest
// fails a test whose goroutines are still blocked when it ends).
func TestCancel(t *testing.T) {
	stop := errors.New("stopped by the user")
	cases := []struct {
		name   string
		mode   dialect.SnapshotMode
		tables int
		rows   int64 // 0: 100,000
		adjust func(*tuning)
		source func(*genSource)
		sink   func() Sink // made in the bubble, so that its channels are too
	}{
		{name: "while reading", source: func(s *genSource) { s.blockAt = 50 }, sink: func() Sink { return discard }},
		{name: "while reading several tables", mode: dialect.SnapshotShared, tables: 5, source: func(s *genSource) { s.blockAt = 50 }, sink: func() Sink { return discard }},
		{name: "while the reader waits for the sink", sink: func() Sink { return waitForCancel(nil) }},
		{name: "when the sink reports the cancel as its own error", sink: func() Sink { return waitForCancel(errors.New("interrupted")) }},
		{name: "while joining a reader", mode: dialect.SnapshotShared, tables: 3, source: func(s *genSource) { s.blockJoin = true }, sink: func() Sink { return waitForCancel(nil) }},
		{
			// 5 rows in batches of 4 behind a queue of 1: the last row's batch
			// finds the queue full.
			name: "while the last batch waits", rows: 5, sink: func() Sink { return waitForCancel(nil) },
			adjust: func(tn *tuning) { tn.batchRows, tn.chunkBytes, tn.workers, tn.queue = 4, 1<<40, 1, 1 },
		},
		{name: "while tables wait for a serial sink", mode: dialect.SnapshotShared, tables: 4, sink: func() Sink { return serialSink(waitForCancel(nil)) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				job, src := genJob(t, cmp.Or(tc.mode, dialect.SnapshotSingle), max(1, tc.tables), cmp.Or(tc.rows, 100_000), 0, tc.sink())
				if tc.source != nil {
					tc.source(src)
				}
				ctx, cancel := context.WithCancelCause(t.Context())
				var err error
				done := make(chan struct{})
				go func() {
					defer close(done)
					adjust := tc.adjust
					if adjust == nil {
						adjust = small
					}
					_, err = copyTuned(ctx, job, adjust)
				}()
				synctest.Wait()
				select {
				case <-done:
					t.Fatalf("Copy returned before the cancel: %v", err)
				default:
				}
				cancel(stop)
				<-done
				if !errors.Is(err, stop) {
					t.Errorf("Copy = %v, want the cancel's cause", err)
				}
			})
		})
	}
}

// TestNoDeadlock copies several tables into a sink that writes one table
// at a time, which deadlocks if tables are read in parallel on one
// connection, or if a pipeline waits on a table it blocks.
func TestNoDeadlock(t *testing.T) {
	for _, mode := range []dialect.SnapshotMode{dialect.SnapshotNone, dialect.SnapshotSingle, dialect.SnapshotShared} {
		t.Run(mode.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				rec := newRecorder()
				job, src := genJob(t, mode, 5, 500, 0, serialSink(rec))
				if _, err := copyTuned(t.Context(), job, small); err != nil {
					t.Fatal(err)
				}
				if got := len(rec.rows); got != 5 {
					t.Errorf("%d tables written, want 5", got)
				}
				if mode != dialect.SnapshotShared && src.stats.maxRead.Load() > 1 {
					t.Errorf("%d reads open at once on one snapshot", src.stats.maxRead.Load())
				}
			})
		})
	}
}

// TestRowsInFlightAreBounded stops the sink and checks that the reader
// stops too, after filling the queue: memory doesn't grow with the table.
func TestRowsInFlightAreBounded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		job, src := genJob(t, dialect.SnapshotSingle, 1, 1_000_000, 0, waitForCancel(nil))
		adjust := func(tn *tuning) { tn.batchRows, tn.chunkBytes, tn.workers, tn.queue = 50, 1<<40, 4, 3 }
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = copyTuned(ctx, job, adjust)
		}()
		synctest.Wait()
		// The queue's batches, and the one the reader can't hand over.
		if got, want := src.stats.rows.Load(), int64((3+1)*50); got != want {
			t.Errorf("%d rows read while the sink takes none, want %d", got, want)
		}
		cancel()
		<-done
	})
}

// TestBatchesReachTheSinkInOrder masks batches in the reverse of their
// order, and checks that the sink still gets them in read order.
func TestBatchesReachTheSinkInOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const batches, rows = 5, 3
		pending := make(chan *batch, batches)
		all := make([]*batch, batches)
		for i := range all {
			b := &batch{rows: rows, done: make(chan struct{})}
			for j := range rows {
				b.out = append(b.out, value.Int(int64(i*rows+j)))
			}
			all[i] = b
			pending <- b
		}
		close(pending)
		go func() {
			for i := batches - 1; i >= 0; i-- {
				close(all[i].done)
				synctest.Wait() // the sink is still waiting for batch 0
			}
		}()
		it := &ordered{ctx: t.Context(), pending: pending, width: 1}
		var got []int64
		for it.Next() {
			got = append(got, it.Row()[0].Int())
		}
		if it.Err() != nil || it.Close() != nil || !it.end || len(got) != batches*rows {
			t.Fatalf("%d rows, end %v, error %v", len(got), it.end, it.Err())
		}
		for i, v := range got {
			if v != int64(i) {
				t.Fatalf("row %d is row %d of the source", i, v)
			}
		}
	})
}

// TestReaderPool checks that a shared snapshot copies several tables at
// once on one reader per table at once, opened up front and reused, closes
// the readers it joined, and leaves the job's own reader open. The first
// three loads wait for each other, so three tables surely overlap.
func TestReaderPool(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rec := newRecorder()
		var started atomic.Int64
		together := make(chan struct{})
		sink := sinkFunc(func(ctx context.Context, table schema.TableRef, columns []schema.Ident, rows dialect.RowIter) (int64, error) {
			switch n := started.Add(1); {
			case n == 3:
				close(together)
			case n < 3:
				<-together
			}
			return rec.Load(ctx, table, columns, rows)
		})
		job, src := genJob(t, dialect.SnapshotShared, 7, 300, 0, sink)
		job.TableJobs = 3
		report, err := copyTuned(t.Context(), job, func(tn *tuning) {
			tn.batchRows, tn.chunkBytes, tn.workers, tn.queue = 4, 300, 1, 1
		})
		if err != nil {
			t.Fatal(err)
		}
		if joined := src.stats.joined.Load(); joined != 2 {
			t.Errorf("%d readers joined, want 2 besides the job's own", joined)
		}
		if src.stats.closed.Load() != src.stats.joined.Load() {
			t.Errorf("%d of %d joined readers closed", src.stats.closed.Load(), src.stats.joined.Load())
		}
		if n := rec.maxLoad.Load(); n != 3 {
			t.Errorf("%d tables loading at once, want 3", n)
		}
		for i, tr := range report.Tables {
			if want := ref(schema.Ident(fmt.Sprintf("T%d", i))); tr.Ref != want || tr.Rows != 300 {
				t.Errorf("report %d: %+v", i, tr)
			}
		}
	})
}

// TestLargestFirst checks the order tables start in: the largest first,
// unknown sizes last, ties by name.
func TestLargestFirst(t *testing.T) {
	sized := func(name string, rows int64) *plan { return &plan{ref: ref(schema.Ident(name)), estimated: rows} }
	plans := []*plan{sized("B", 10), sized("UNKNOWN", -1), sized("A", 10), sized("HUGE", 1e9), sized("EMPTY", 0)}
	var got []string
	for _, i := range largestFirst(plans) {
		got = append(got, string(plans[i].ref.Name))
	}
	if want := []string{"HUGE", "A", "B", "EMPTY", "UNKNOWN"}; !slices.Equal(got, want) {
		t.Errorf("order %v, want %v", got, want)
	}
}

// TestJoinedReaderFailsToClose checks that a joined reader that fails to
// close is logged, and doesn't fail a copy that is complete.
func TestJoinedReaderFailsToClose(t *testing.T) {
	var logs bytes.Buffer
	job, src := genJob(t, dialect.SnapshotShared, 3, 50, 0, discard)
	job.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	src.failClosed = errors.New("rollback failed")
	if _, err := Copy(t.Context(), job); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if n := src.stats.joined.Load(); n != 2 {
		t.Fatalf("%d readers joined, want 2", n)
	}
	if strings.Count(logs.String(), "did not close cleanly") != 2 || !strings.Contains(logs.String(), "rollback failed") {
		t.Errorf("no warning about each reader:\n%s", logs.String())
	}
}

// TestPoolStopsOnCancel checks that a table waiting for a reader stops
// when the copy is canceled.
func TestPoolStopsOnCancel(t *testing.T) {
	stop := errors.New("stopped")
	p, err := openPool(t.Context(), newGenSource(dialect.SnapshotSingle, nil), 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.get(t.Context()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancelCause(t.Context())
	cancel(stop)
	if _, err := p.get(ctx); !errors.Is(err, stop) {
		t.Errorf("get on an empty pool = %v, want the cancel's cause", err)
	}
}

// TestSinkSeesTheBatchError checks the sink's side when a batch's masking
// failed before the pipeline was canceled: its error stops the rows.
func TestSinkSeesTheBatchError(t *testing.T) {
	refused := errors.New("refused")
	pending := make(chan *batch, 1)
	b := &batch{rows: 1, out: []value.Value{value.Int(1)}, err: refused, done: make(chan struct{})}
	close(b.done)
	pending <- b
	it := &ordered{ctx: t.Context(), pending: pending, width: 1}
	if it.Next() || !errors.Is(it.Err(), refused) || it.end || it.delivered != 0 {
		t.Errorf("Next passed a row of a failed batch on, or lost its error (%v)", it.Err())
	}
}

// TestOrderKeptWhenMaskingFinishesOutOfOrder makes every 40th row slow to
// mask (a 4 MB value under hash, alone in its batch), so that the small
// batches after it finish first, and checks the sink still gets every row
// in read order.
func TestOrderKeptWhenMaskingFinishesOutOfOrder(t *testing.T) {
	const rows = 400
	huge := strings.Repeat("x", 4<<20)
	r := ref("SLOW")
	tbl := &schema.Table{Name: r.Name, Columns: []*schema.Column{{Name: "ID", Type: tInt64}, {Name: "NAME", Type: tText}}, PrimaryKey: pk("ID")}
	gen := &genTable{rows: rows, columns: ids("ID", "NAME"), value: func(c int, i int64) value.Value {
		switch {
		case c == 0:
			return value.Int(i)
		case i%40 == 0:
			return value.String(huge)
		default:
			return value.String(fmt.Sprintf("name %d", i))
		}
	}}
	m := maskTable(t, r, tbl, map[schema.Ident]mask.Rule{"ID": {Use: "keep"}, "NAME": {Use: "hash"}})
	rec := newRecorder()
	job := Job{
		Source: newGenSource(dialect.SnapshotSingle, map[schema.TableRef]*genTable{r: gen}), Snapshot: dialect.SnapshotSingle,
		Keys: testKeys, Sink: rec, Budget: 64 << 20, Tables: []Table{{Ref: r, Schema: tbl, Mask: m}},
	}
	if _, err := copyTuned(t.Context(), job, func(tn *tuning) {
		tn.batchRows, tn.batchBytes, tn.chunkBytes, tn.workers, tn.queue = 8, 64<<10, 1<<40, 4, 8
	}); err != nil {
		t.Fatal(err)
	}
	source := make([][]value.Value, rows)
	for i := range source {
		source[i] = []value.Value{gen.value(0, int64(i)), gen.value(1, int64(i))}
	}
	sameRows(t, "SLOW", rec.rows[r], maskRows(t, m, source))
}
