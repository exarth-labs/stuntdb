package engine

import (
	"fmt"
	"runtime"
	"testing"

	"github.com/exarth-labs/stuntdb/dialect"
	"github.com/exarth-labs/stuntdb/internal/schema"
	"github.com/exarth-labs/stuntdb/internal/value"
)

// FuzzCopy copies generated tables under random pipeline shapes, and
// checks that each table's output always equals its rows masked one by
// one, in key order.
func FuzzCopy(f *testing.F) {
	f.Add(uint16(0), uint8(1), uint8(1), uint8(1), uint16(1), uint8(1), false)
	f.Add(uint16(1), uint8(1), uint8(1), uint8(1), uint16(1), uint8(1), false)
	f.Add(uint16(37), uint8(4), uint8(2), uint8(3), uint16(300), uint8(2), true)
	f.Add(uint16(64), uint8(8), uint8(1), uint8(8), uint16(1), uint8(3), true)
	f.Add(uint16(1000), uint8(250), uint8(7), uint8(5), uint16(9000), uint8(3), false)
	f.Fuzz(func(t *testing.T, rows uint16, batchRows, queue, workers uint8, chunkBytes uint16, tables uint8, shared bool) {
		n := int64(rows % 3000)
		mode := dialect.SnapshotSingle
		if shared {
			mode = dialect.SnapshotShared
		}
		rec := newRecorder()
		job, _ := genJob(t, mode, int(tables%4)+1, n, 0, rec)
		job.TableJobs = 3
		_, err := copyTuned(t.Context(), job, func(tn *tuning) {
			tn.batchRows = int(batchRows%64) + 1
			tn.queue = int(queue%8) + 1
			tn.workers = int(workers%8) + 1
			tn.chunkBytes = int64(chunkBytes) + 1
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, tbl := range job.Tables {
			_, gen := people(n, 0)
			source := make([][]value.Value, n)
			for i := range source {
				source[i] = []value.Value{gen.value(0, int64(i)), gen.value(1, int64(i)), gen.value(2, int64(i))}
			}
			sameRows(t, tbl.Ref.String(), rec.rows[tbl.Ref], maskRows(t, tbl.Mask, source))
		}
	})
}

// TestSameOutputWhateverTheShape checks that the copy doesn't depend on
// the number of workers or of tables at once.
func TestSameOutputWhateverTheShape(t *testing.T) {
	var first map[schema.TableRef][][]value.Value
	for _, shape := range []struct{ workers, tables int }{{1, 1}, {8, 1}, {1, 3}, {8, 3}, {runtime.GOMAXPROCS(0), 4}} {
		t.Run(fmt.Sprintf("%d workers, %d tables at once", shape.workers, shape.tables), func(t *testing.T) {
			rec := newRecorder()
			job, _ := genJob(t, dialect.SnapshotShared, 4, 2000, 0, rec)
			job.Workers, job.TableJobs = shape.workers, shape.tables
			if _, err := copyTuned(t.Context(), job, func(tn *tuning) { tn.batchRows, tn.chunkBytes = 16, 4096 }); err != nil {
				t.Fatal(err)
			}
			if first == nil {
				first = rec.rows
				return
			}
			for r, rows := range first {
				sameRows(t, r.String(), rec.rows[r], rows)
			}
		})
	}
}
