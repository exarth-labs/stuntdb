package engine

import (
	"cmp"
	"math"
	"runtime"

	"github.com/exarth-labs/stuntdb/dialect"
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
// heap grow to about twice what is live, and for drivers. Each table has
// queue + 2 batches in flight (the queue, one being read, one being
// written), each held twice while it is masked.
func tune(job Job, tables int) tuning {
	tn := tuning{tables: 1, batchRows: maxBatchRows, chunkBytes: cmp.Or(job.ChunkBytes, DefaultChunkBytes)}
	if job.Snapshot == dialect.SnapshotShared {
		tn.tables = max(1, min(cmp.Or(job.TableJobs, DefaultTableJobs), tables))
	}
	tn.workers = cmp.Or(job.Workers, max(1, runtime.GOMAXPROCS(0)/tn.tables))
	tn.queue = 2 * tn.workers
	inFlight := int64(tn.tables) * int64(tn.queue+2) * 2
	tn.batchBytes = int(min(max(job.Budget/2/inFlight, minBatchBytes), maxBatchBytes))
	return tn
}
