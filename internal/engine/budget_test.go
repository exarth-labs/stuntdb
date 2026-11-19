package engine

import (
	"runtime"
	"testing"

	"github.com/exarth-labs/stuntdb/dialect"
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
			name: "a small budget keeps batches of 64 KiB", procs: 8, tables: 1,
			job:  Job{Snapshot: dialect.SnapshotSingle, Budget: 1 * MiB},
			want: tuning{tables: 1, workers: 8, queue: 16, batchBytes: minBatchBytes, batchRows: maxBatchRows, chunkBytes: DefaultChunkBytes},
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
			if got := tune(tc.job, tc.tables); got != tc.want {
				t.Errorf("tune = %+v\nwant   %+v", got, tc.want)
			}
		})
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
