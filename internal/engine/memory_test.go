package engine

import (
	"runtime"
	"runtime/metrics"
	"testing"
	"time"

	"github.com/exarth-labs/stuntdb/dialect"
	"github.com/exarth-labs/stuntdb/internal/mask"
	"github.com/exarth-labs/stuntdb/internal/schema"
)

// TestMemoryIsFlat copies a table and one four times its size, and checks
// that the live heap stays within the pipeline's half of the budget for
// both. Each peak is a sample of how full the queues happened to be, so the
// two peaks are compared with the bound, not with each other.
func TestMemoryIsFlat(t *testing.T) {
	small, large := int64(1_000_000), int64(4_000_000)
	if raceEnabled {
		small, large = 100_000, 400_000
	}
	const budget = 16 << 20
	for _, rows := range []int64{small, large} {
		peak := peakLiveHeap(t, rows, budget)
		t.Logf("%d rows: live heap peaked %d KiB above the baseline", rows, peak>>10)
		if peak > budget/2 {
			t.Errorf("%d rows: the live heap peaked at %d KiB, over the pipeline's %d KiB", rows, peak>>10, budget/2>>10)
		}
	}
}

// peakLiveHeap copies rows and returns the most live heap seen above what
// was live before. Live heap is what the last collection marked, so
// garbage not yet collected doesn't blur the comparison.
func peakLiveHeap(t *testing.T, rows int64, budget int64) uint64 {
	t.Helper()
	r := ref("BIG")
	tbl, gen := people(rows, 0)
	tbl.Name = r.Name
	keep := map[schema.Ident]mask.Rule{"ID": {Use: "keep"}, "NAME": {Use: "keep"}, "BORN": {Use: "keep"}}
	job := Job{
		Source:   newGenSource(dialect.SnapshotSingle, map[schema.TableRef]*genTable{r: gen}),
		Snapshot: dialect.SnapshotSingle,
		Keys:     testKeys,
		Sink:     discard,
		Budget:   budget,
		Tables:   []Table{{Ref: r, Schema: tbl, Mask: maskTable(t, r, tbl, keep)}},
	}

	sample := []metrics.Sample{{Name: "/gc/heap/live:bytes"}}
	live := func() uint64 { metrics.Read(sample); return sample[0].Value.Uint64() }
	runtime.GC()
	runtime.GC()
	base := live()

	stop, done := make(chan struct{}), make(chan uint64)
	go func() {
		var peak uint64
		tick := time.NewTicker(time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				done <- peak
				return
			case <-tick.C:
				peak = max(peak, live())
			}
		}
	}()
	report, err := Copy(t.Context(), job)
	close(stop)
	peak := <-done
	if err != nil {
		t.Fatal(err)
	}
	if report.Tables[0].Rows != rows {
		t.Fatalf("%d rows copied, want %d", report.Tables[0].Rows, rows)
	}
	if peak < base {
		return 0
	}
	return peak - base
}
