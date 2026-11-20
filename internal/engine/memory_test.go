package engine

import (
	"context"
	"runtime"
	"runtime/metrics"
	"testing"
	"testing/synctest"

	"github.com/exarth-labs/stuntdb/dialect"
	"github.com/exarth-labs/stuntdb/internal/schema"
)

// TestMemoryIsFlat copies a table and one four times its size, and checks
// that the live heap stays within the pipeline's half of the budget for
// both, measured at pauses of the target.
func TestMemoryIsFlat(t *testing.T) {
	small, large := int64(1_000_000), int64(4_000_000)
	if raceEnabled {
		small, large = 100_000, 400_000
	}
	const budget = 16 << 20
	for _, rows := range []int64{small, large} {
		peak := pausedLiveHeap(t, rows, budget)
		t.Logf("%d rows: live heap at most %d KiB above the baseline", rows, peak>>10)
		if peak > budget/2 {
			t.Errorf("%d rows: the live heap reached %d KiB, over the pipeline's %d KiB", rows, peak>>10, budget/2>>10)
		}
	}
}

// TestSmallBudgetOnManyCPUs fills every queue of a copy into a target that
// takes no rows, with 64 CPUs and a 4 MiB budget, and checks that the live
// heap stays within the pipeline's half of the budget. A worker per CPU
// with batches of at least 64 KiB would hold about 10 MiB.
func TestSmallBudgetOnManyCPUs(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(64))
	const budget = 4 << 20
	synctest.Test(t, func(t *testing.T) {
		job, src := genJob(t, dialect.SnapshotSingle, 1, 10_000_000, 0, waitForCancel(nil))
		job.Budget = budget
		runtime.GC()
		base := liveHeap()
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = Copy(ctx, job)
		}()
		synctest.Wait() // the queues are full and the reader waits
		runtime.GC()
		held := int64(liveHeap()) - int64(base)
		read := src.stats.rows.Load()
		cancel()
		<-done
		t.Logf("%d rows read, live heap %d KiB above the baseline", read, held>>10)
		if read == 0 {
			t.Fatal("nothing was read")
		}
		if held > budget/2 {
			t.Errorf("the live heap held %d KiB, over the pipeline's %d KiB", held>>10, budget/2>>10)
		}
	})
}

// liveHeap is the heap the last collection marked live.
func liveHeap() uint64 {
	sample := []metrics.Sample{{Name: "/gc/heap/live:bytes"}}
	metrics.Read(sample)
	return sample[0].Value.Uint64()
}

// pauses is how many times pausedLiveHeap stops a copy to measure it.
const pauses = 20

// pausedLiveHeap copies rows into a target that pauses every rows/pauses
// rows, and returns the most live heap above the baseline seen at a pause.
// It measures only while every goroutine is blocked: while a collection
// marks, the runtime counts each new object as live, so a sample taken
// while the copy runs grows with the machine's load, not with what the
// copy holds.
func pausedLiveHeap(t *testing.T, rows, budget int64) (peak int64) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		r := ref("BIG")
		tbl, gen := people(rows, 0)
		tbl.Name = r.Name
		pause, resume := make(chan struct{}), make(chan struct{})
		sink := sinkFunc(func(_ context.Context, _ schema.TableRef, _ []schema.Ident, it dialect.RowIter) (int64, error) {
			var n int64
			for it.Next() {
				if n++; n%(rows/pauses) == 0 {
					pause <- struct{}{}
					<-resume
				}
			}
			return n, it.Err()
		})
		job := Job{
			Source:   newGenSource(dialect.SnapshotSingle, map[schema.TableRef]*genTable{r: gen}),
			Snapshot: dialect.SnapshotSingle,
			Keys:     testKeys,
			Sink:     sink,
			Budget:   budget,
			// Masked text is new, as for most real columns: under keep the
			// masked rows would share the source's text, and source rows
			// kept after masking would cost almost nothing.
			Tables: []Table{{Ref: r, Schema: tbl, Mask: maskTable(t, r, tbl, peopleRules)}},
		}

		runtime.GC()
		base := int64(liveHeap())
		var (
			report Report
			err    error
		)
		done := make(chan struct{})
		go func() {
			defer close(done)
			report, err = Copy(t.Context(), job)
		}()
		measured := 0
	copying:
		for {
			select {
			case <-pause:
				synctest.Wait() // nothing runs, so nothing is allocated
				runtime.GC()
				peak = max(peak, int64(liveHeap())-base)
				measured++
				resume <- struct{}{}
			case <-done:
				break copying
			}
		}
		if err != nil {
			t.Fatal(err)
		}
		if report.Tables[0].Rows != rows || measured != pauses {
			t.Fatalf("%d rows copied and %d pauses, want %d and %d", report.Tables[0].Rows, measured, rows, pauses)
		}
	})
	return peak
}
