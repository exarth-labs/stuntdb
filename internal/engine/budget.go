package engine

import (
	"cmp"
	"fmt"
	"math"
	"runtime"

	"github.com/exarth-labs/stuntdb/dialect"
	"github.com/exarth-labs/stuntdb/internal/errs"
)

const (
	// DefaultTableJobs is the most tables copied at once from a shared
	// snapshot, each on its own reader.
	DefaultTableJobs = 4

	// DefaultChunkBytes is the size of one read of a table with a key.
	DefaultChunkBytes = 64 << 20

	minBatchBytes = 64 << 10
	maxBatchBytes = 8 << 20
	maxBatchRows  = 10_000
	maxChunkRows  = math.MaxInt32
)

// tuning is the shape of every table's pipeline.
type tuning struct {
	tables     int // tables copied at once
	workers    int // mask workers per table
	queue      int // batches waiting for the sink, per table
	batchBytes int
	batchRows  int
	chunkBytes int64
}

// tune sizes the pipelines from the budget. Half of it goes to the rows in
// flight; the other half is room for the garbage collector, which lets the
// heap grow to about twice what is live, and for drivers.
//
// Batches never shrink below minBatchBytes, so a small budget gets fewer
// workers, then fewer tables at once. Workers and TableJobs set by the job
// are never lowered: a budget too small for them is refused.
func tune(job Job, tables int) (tuning, error) {
	tn := tuning{tables: 1, workers: max(1, job.Workers), batchRows: maxBatchRows, chunkBytes: cmp.Or(job.ChunkBytes, DefaultChunkBytes)}
	room := job.Budget / 2 / minBatchBytes // the most batches in flight at their smallest
	fits := func(tables, workers int) bool {
		return int64(workers) <= room && inFlight(workers) <= room/int64(tables)
	}
	if job.Snapshot == dialect.SnapshotShared {
		tn.tables = max(1, min(cmp.Or(job.TableJobs, DefaultTableJobs), tables))
		for job.TableJobs == 0 && tn.tables > 1 && !fits(tn.tables, tn.workers) {
			tn.tables--
		}
	}
	if job.Workers == 0 {
		tn.workers = max(1, runtime.GOMAXPROCS(0)/tn.tables)
		for tn.workers > 1 && !fits(tn.tables, tn.workers) {
			tn.workers--
		}
	}
	if !fits(tn.tables, tn.workers) {
		// In floats: the job's Workers can be any int, which inFlight would overflow.
		need := float64(tn.tables) * (2*float64(tn.workers) + 2) * 2 * minBatchBytes * 2
		return tuning{}, errs.E(errs.Usage, fmt.Errorf(
			"engine: the memory budget, %s, is too small for the rows in flight (tables at once: %d, mask workers per table: %d), which need at least %s",
			mib(float64(job.Budget), math.Floor), tn.tables, tn.workers, mib(need, math.Ceil)))
	}
	tn.queue = 2 * tn.workers
	tn.batchBytes = int(min(max(job.Budget/2/(int64(tn.tables)*inFlight(tn.workers)), minBatchBytes), maxBatchBytes))
	return tn, nil
}

// inFlight is the most batches one table holds with w mask workers: its
// queue of 2w, one being read and one being written, each held twice
// while it is masked (source and masked rows).
func inFlight(w int) int64 { return (2*int64(w) + 2) * 2 }

// mib writes bytes in MiB to one decimal, rounded as round says: a budget
// down and a need up, so that "too small" never reads as "enough".
func mib(bytes float64, round func(float64) float64) string {
	return fmt.Sprintf("%.1f MiB", round(bytes/(1<<20)*10)/10)
}
