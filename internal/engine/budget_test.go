package engine

import (
	"fmt"
	"math"
	"runtime"
	"strings"
	"testing"

	"github.com/exarth-labs/stuntdb/dialect"
	"github.com/exarth-labs/stuntdb/internal/errs"
)

func TestTune(t *testing.T) {
	const MiB = 1 << 20
	cases := []struct {
		name   string
		procs  int
		job    Job
		tables int
		want   tuning
	}{
		{
			// 256 MiB for rows in flight, over 1 table · (16 + 2) batches · 2.
			name: "one table at a time", procs: 8, tables: 10,
			job:  Job{Snapshot: dialect.SnapshotSingle, Budget: 512 * MiB},
			want: tuning{tables: 1, workers: 8, queue: 16, batchBytes: 256 * MiB / 36, batchRows: maxBatchRows, chunkBytes: DefaultChunkBytes},
		},
		{
			name: "no snapshot is one table at a time", procs: 8, tables: 10,
			job:  Job{Snapshot: dialect.SnapshotNone, Budget: 512 * MiB, TableJobs: 6},
			want: tuning{tables: 1, workers: 8, queue: 16, batchBytes: 256 * MiB / 36, batchRows: maxBatchRows, chunkBytes: DefaultChunkBytes},
		},
		{
			name: "shared, the default tables at once", procs: 8, tables: 10,
			job:  Job{Snapshot: dialect.SnapshotShared, Budget: 512 * MiB},
			want: tuning{tables: 4, workers: 2, queue: 4, batchBytes: 256 * MiB / 48, batchRows: maxBatchRows, chunkBytes: DefaultChunkBytes},
		},
		{
			name: "shared, fewer tables than jobs", procs: 8, tables: 2,
			job:  Job{Snapshot: dialect.SnapshotShared, Budget: 512 * MiB},
			want: tuning{tables: 2, workers: 4, queue: 8, batchBytes: 256 * MiB / 40, batchRows: maxBatchRows, chunkBytes: DefaultChunkBytes},
		},
		{
			name: "shared, no tables", procs: 8, tables: 0,
			job:  Job{Snapshot: dialect.SnapshotShared, Budget: 512 * MiB},
			want: tuning{tables: 1, workers: 8, queue: 16, batchBytes: 256 * MiB / 36, batchRows: maxBatchRows, chunkBytes: DefaultChunkBytes},
		},
		{
			name: "fewer CPUs than tables", procs: 2, tables: 10,
			job:  Job{Snapshot: dialect.SnapshotShared, Budget: 512 * MiB},
			want: tuning{tables: 4, workers: 1, queue: 2, batchBytes: 256 * MiB / 32, batchRows: maxBatchRows, chunkBytes: DefaultChunkBytes},
		},
		{
			name: "overrides", procs: 8, tables: 10,
			job:  Job{Snapshot: dialect.SnapshotShared, Budget: 512 * MiB, Workers: 3, TableJobs: 6, ChunkBytes: 5 * MiB},
			want: tuning{tables: 6, workers: 3, queue: 6, batchBytes: 256 * MiB / 96, batchRows: maxBatchRows, chunkBytes: 5 * MiB},
		},
		{
			// 512 KiB holds 8 batches of 64 KiB: one table, one worker.
			name: "the smallest budget gets one worker", procs: 8, tables: 1,
			job:  Job{Snapshot: dialect.SnapshotSingle, Budget: 1 * MiB},
			want: tuning{tables: 1, workers: 1, queue: 2, batchBytes: minBatchBytes, batchRows: maxBatchRows, chunkBytes: DefaultChunkBytes},
		},
		{
			// 8 MiB holds 128 batches of 64 KiB: (2·31 + 2) · 2 = 128.
			name: "a small budget on many CPUs gets fewer workers", procs: 64, tables: 1,
			job:  Job{Snapshot: dialect.SnapshotSingle, Budget: 16 * MiB},
			want: tuning{tables: 1, workers: 31, queue: 62, batchBytes: minBatchBytes, batchRows: maxBatchRows, chunkBytes: DefaultChunkBytes},
		},
		{
			// 128 batches over 4 tables: (2·7 + 2) · 2 = 32 each.
			name: "shared, fewer workers per table", procs: 64, tables: 10,
			job:  Job{Snapshot: dialect.SnapshotShared, Budget: 16 * MiB},
			want: tuning{tables: 4, workers: 7, queue: 14, batchBytes: minBatchBytes, batchRows: maxBatchRows, chunkBytes: DefaultChunkBytes},
		},
		{
			// 16 batches: two tables of 8, with one worker each.
			name: "shared, fewer tables at once", procs: 8, tables: 10,
			job:  Job{Snapshot: dialect.SnapshotShared, Budget: 2 * MiB},
			want: tuning{tables: 2, workers: 1, queue: 2, batchBytes: minBatchBytes, batchRows: maxBatchRows, chunkBytes: DefaultChunkBytes},
		},
		{
			// 32 batches: the job's 3 workers take 16 per table, so 2 tables.
			name: "the job's workers are kept, tables at once are lowered", procs: 8, tables: 10,
			job:  Job{Snapshot: dialect.SnapshotShared, Budget: 4 * MiB, Workers: 3},
			want: tuning{tables: 2, workers: 3, queue: 6, batchBytes: minBatchBytes, batchRows: maxBatchRows, chunkBytes: DefaultChunkBytes},
		},
		{
			name: "a large budget keeps batches of 8 MiB", procs: 8, tables: 1,
			job:  Job{Snapshot: dialect.SnapshotSingle, Budget: 64 << 30},
			want: tuning{tables: 1, workers: 8, queue: 16, batchBytes: maxBatchBytes, batchRows: maxBatchRows, chunkBytes: DefaultChunkBytes},
		},
	}
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(0))
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runtime.GOMAXPROCS(tc.procs)
			got, err := tune(tc.job, tc.tables)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("tune = %+v\nwant   %+v", got, tc.want)
			}
		})
	}
}

// TestTuneRefusesATooSmallBudget checks that a budget is refused, never
// exceeded, when it can't hold one worker, or the job's own workers or
// tables at once, and that the message names the budget they need.
func TestTuneRefusesATooSmallBudget(t *testing.T) {
	const MiB = 1 << 20
	cases := []struct {
		name   string
		job    Job
		tables int
		want   string
	}{
		{"under 1 MiB", Job{Snapshot: dialect.SnapshotSingle, Budget: MiB - 1}, 1, "0.9 MiB, is too small for the rows in flight (tables at once: 1, mask workers per table: 1), which need at least 1.0 MiB"},
		{"the job's workers", Job{Snapshot: dialect.SnapshotSingle, Budget: 4 * MiB, Workers: 8}, 1, "4.0 MiB, is too small for the rows in flight (tables at once: 1, mask workers per table: 8), which need at least 4.5 MiB"},
		{"the job's tables at once", Job{Snapshot: dialect.SnapshotShared, Budget: 2 * MiB, TableJobs: 6}, 10, "(tables at once: 6, mask workers per table: 1), which need at least 6.0 MiB"},
		{"workers past any budget", Job{Snapshot: dialect.SnapshotSingle, Budget: 64 << 30, Workers: math.MaxInt}, 1, "mask workers per table: 9223372036854775807"},
	}
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(8))
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tune(tc.job, tc.tables)
			if errs.KindOf(err) != errs.Usage || !strings.Contains(fmt.Sprint(err), tc.want) {
				t.Errorf("tune = %v (%v), want a usage error with %q", err, errs.KindOf(err), tc.want)
			}
		})
	}
}

// TestTuneKeepsTheBudget checks every combination of CPUs, budget, snapshot
// mode, tables and overrides: the rows in flight fit in half the budget, or
// the budget is refused, and with the defaults it is refused only under
// 1 MiB.
func TestTuneKeepsTheBudget(t *testing.T) {
	const MiB = 1 << 20
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(0))
	for _, procs := range []int{1, 2, 8, 64, 256} {
		runtime.GOMAXPROCS(procs)
		for _, budget := range []int64{MiB - 1, MiB, 2 * MiB, 3 * MiB, 4 * MiB, 16 * MiB, 64 * MiB, 512 * MiB, 64 << 30} {
			for _, mode := range []dialect.SnapshotMode{dialect.SnapshotSingle, dialect.SnapshotShared} {
				for _, tables := range []int{1, 3, 10} {
					for _, workers := range []int{0, 3} {
						for _, jobs := range []int{0, 6} {
							job := Job{Snapshot: mode, Budget: budget, Workers: workers, TableJobs: jobs}
							tn, err := tune(job, tables)
							name := fmt.Sprintf("%d CPUs, %+v, %d tables", procs, job, tables)
							if err != nil {
								if workers == 0 && jobs == 0 && budget >= MiB {
									t.Errorf("%s: refused: %v", name, err)
								}
								continue
							}
							if budget < MiB {
								t.Errorf("%s: accepted a budget under 1 MiB", name)
							}
							inFlight := int64(tn.tables) * int64(tn.queue+2) * 2 * int64(tn.batchBytes)
							if inFlight > budget/2 || tn.batchBytes < minBatchBytes || tn.workers < 1 || tn.tables < 1 {
								t.Errorf("%s: %+v holds %d bytes in flight, over %d", name, tn, inFlight, budget/2)
							}
							if workers > 0 && tn.workers != workers || jobs > 0 && mode == dialect.SnapshotShared && tn.tables != min(jobs, tables) {
								t.Errorf("%s: %+v lowered the job's own workers or tables at once", name, tn)
							}
						}
					}
				}
			}
		}
	}
}

func TestChunkRows(t *testing.T) {
	cases := []struct {
		read, bytes, chunk int64
		want               int
	}{
		{read: 10, bytes: 1000, chunk: 64 << 20, want: (64 << 20) / 100},
		{read: 10, bytes: 1000, chunk: 1, want: 1},
		{read: 3, bytes: 1000, chunk: 1000, want: 3},            // average 333
		{read: 1, bytes: 0, chunk: 1 << 40, want: maxChunkRows}, // never past an int32
	}
	for _, tc := range cases {
		r := &reader{read: tc.read, bytes: tc.bytes, tn: tuning{chunkBytes: tc.chunk}}
		if got := r.chunkRows(); got != tc.want {
			t.Errorf("chunkRows after %d rows of %d bytes, chunk %d = %d, want %d", tc.read, tc.bytes, tc.chunk, got, tc.want)
		}
	}
}
