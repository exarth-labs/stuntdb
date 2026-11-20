package engine

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/exarth-labs/stuntdb/dialect"
	"github.com/exarth-labs/stuntdb/internal/errs"
	"github.com/exarth-labs/stuntdb/internal/mask"
	"github.com/exarth-labs/stuntdb/internal/schema"
	"github.com/exarth-labs/stuntdb/internal/value"
)

// canary is in every generated row. No error or log line may contain it.
const canary = "canary-7f3a"

var peopleColumns = ids("ID", "NAME", "BORN")

// people is a generated table: ID, a NAME holding the canary, and BORN, a
// date as text, which is the canary instead at row refuseAt (if > 0), so
// that date_shift refuses it.
func people(rows, refuseAt int64) (*schema.Table, *genTable) {
	tbl := &schema.Table{
		Columns: []*schema.Column{
			{Name: "ID", Type: tInt64},
			{Name: "NAME", Type: tText},
			{Name: "BORN", Type: tText},
		},
		PrimaryKey:    pk("ID"),
		EstimatedRows: rows,
	}
	gen := &genTable{rows: rows, columns: peopleColumns, value: func(c int, i int64) value.Value {
		switch {
		case c == 0:
			return value.Int(i)
		case c == 1:
			return value.String(fmt.Sprintf("%s %d", canary, i))
		case refuseAt > 0 && i == refuseAt:
			return value.String(canary)
		default:
			y, m, d := date(int(i % 9000)).Date()
			return value.String(fmt.Sprintf("%04d-%02d-%02d", y, m, d))
		}
	}}
	return tbl, gen
}

var peopleRules = map[schema.Ident]mask.Rule{"ID": {Use: "keep"}, "NAME": {Use: "hash"}, "BORN": {Use: "date_shift", Per: "ID"}}

// genJob is a job over generated tables named T0, T1, … of rows each.
func genJob(t testing.TB, mode dialect.SnapshotMode, tables int, rows, refuseAt int64, sink Sink) (Job, *genSource) {
	t.Helper()
	gens := map[schema.TableRef]*genTable{}
	job := Job{Snapshot: mode, Keys: testKeys, Sink: sink, Budget: 16 << 20}
	for i := range tables {
		r := ref(schema.Ident(fmt.Sprintf("T%d", i)))
		tbl, gen := people(rows, refuseAt)
		tbl.Name = r.Name
		gens[r] = gen
		job.Tables = append(job.Tables, Table{Ref: r, Schema: tbl, Mask: maskTable(t, r, tbl, peopleRules)})
	}
	src := newGenSource(mode, gens)
	job.Source = src
	return job, src
}

// small makes many batches and chunks out of few rows.
func small(tn *tuning) { tn.batchRows, tn.chunkBytes, tn.workers, tn.queue = 4, 300, 3, 2 }

func TestErrorKinds(t *testing.T) {
	disk := errors.New("disk full")
	reset := errors.New("connection reset")
	cases := []struct {
		name     string
		tables   int
		mode     dialect.SnapshotMode
		refuseAt int64
		source   func(*genSource)
		adjust   func(*tuning)
		sink     Sink
		kind     errs.Kind
		want     string
	}{
		{name: "read fails", source: func(s *genSource) { s.failRead = reset }, kind: errs.Connect, want: "read APP.T0: connection reset"},
		{name: "read fails midway", source: func(s *genSource) { s.failAt, s.failIter = 3, reset }, kind: errs.Connect, want: "read APP.T0: connection reset"},
		{name: "closing a read fails", source: func(s *genSource) { s.failClose = reset }, kind: errs.Connect, want: "read APP.T0: connection reset"},
		{name: "rows have a value too many", source: func(s *genSource) { s.extraValue = true }, kind: errs.Internal, want: "a row has 4 values for 3 columns"},
		{name: "chunks don't advance", source: func(s *genSource) { s.stuck = true }, kind: errs.Integrity, want: "did not move past the previous one"},
		{name: "a chunk repeats the last row", source: func(s *genSource) { s.inclusive = true }, kind: errs.Integrity, want: "did not move past the previous one"},
		{
			// Only the repeated first row shows it: without the check the
			// copy would succeed, with that row twice and every count equal.
			name: "a long chunk repeats the last row", source: func(s *genSource) { s.inclusive = true }, kind: errs.Integrity, want: "did not move past the previous one",
			adjust: func(tn *tuning) { tn.batchRows, tn.chunkBytes, tn.workers, tn.queue = 4, 1<<40, 2, 2 },
		},
		{name: "joining a reader fails", tables: 3, mode: dialect.SnapshotShared, source: func(s *genSource) { s.failJoin = reset }, kind: errs.Connect, want: "open another reader on the source snapshot: connection reset"},
		{name: "masking refuses a value", refuseAt: 21, kind: errs.CheckFailed, want: "mask APP.T0.BORN with date_shift"},
		{
			name: "the sink returns the masking refusal", refuseAt: 21, kind: errs.CheckFailed, want: "mask APP.T0.BORN with date_shift",
			sink: sinkFunc(func(_ context.Context, _ schema.TableRef, _ []schema.Ident, rows dialect.RowIter) (int64, error) {
				n, err := drain(rows)
				return n, fmt.Errorf("loader: %w", err)
			}),
		},
		{
			name: "the sink fails", kind: errs.Output, want: "write APP.T0: disk full",
			sink: sinkFunc(func(context.Context, schema.TableRef, []schema.Ident, dialect.RowIter) (int64, error) { return 0, disk }),
		},
		{
			name: "the sink drops a row", kind: errs.Integrity, want: "37 rows read, 37 passed on, 36 written",
			sink: sinkFunc(func(_ context.Context, _ schema.TableRef, _ []schema.Ident, rows dialect.RowIter) (int64, error) {
				n, err := drain(rows)
				return n - 1, err
			}),
		},
		{
			name: "the sink counts a row too many", kind: errs.Integrity, want: "37 rows read, 37 passed on, 38 written",
			sink: sinkFunc(func(_ context.Context, _ schema.TableRef, _ []schema.Ident, rows dialect.RowIter) (int64, error) {
				n, err := drain(rows)
				return n + 1, err
			}),
		},
		{
			// Every count matches; only the missing end shows the problem.
			name: "the sink never asks past the last row", kind: errs.Integrity, want: "stopped taking rows after 37 of them",
			sink: sinkFunc(func(_ context.Context, _ schema.TableRef, _ []schema.Ident, rows dialect.RowIter) (int64, error) {
				var n int64
				for n < 37 && rows.Next() {
					n++
				}
				return n, nil
			}),
		},
		{
			name: "the sink stops early", kind: errs.Integrity, want: "stopped taking rows after 5 of them",
			sink: sinkFunc(func(_ context.Context, _ schema.TableRef, _ []schema.Ident, rows dialect.RowIter) (int64, error) {
				var n int64
				for n < 5 && rows.Next() {
					n++
				}
				return n, nil
			}),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			sink := tc.sink
			if sink == nil {
				sink = discard
			}
			job, src := genJob(t, cmp.Or(tc.mode, dialect.SnapshotSingle), max(1, tc.tables), 37, tc.refuseAt, sink)
			job.Logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
			if tc.source != nil {
				tc.source(src)
			}
			adjust := tc.adjust
			if adjust == nil {
				adjust = small
			}
			_, err := copyTuned(t.Context(), job, adjust)
			if err == nil {
				t.Fatal("Copy succeeded")
			}
			if got := errs.KindOf(err); got != tc.kind {
				t.Errorf("kind %s, want %s (%v)", got, tc.kind, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q, want it to contain %q", err, tc.want)
			}
			if strings.Contains(err.Error(), canary) || strings.Contains(logs.String(), canary) {
				t.Error("a row value reached the error or the logs")
			}
		})
	}
}

func TestTableWithoutColumns(t *testing.T) {
	r := ref("EMPTY")
	tbl := &schema.Table{Name: "EMPTY"}
	_, err := Copy(t.Context(), Job{
		Source: newGenSource(dialect.SnapshotSingle, nil), Snapshot: dialect.SnapshotSingle, Keys: testKeys, Sink: discard, Budget: 1 << 20,
		Tables: []Table{{Ref: r, Schema: tbl, Mask: maskTable(t, r, tbl, nil)}},
	})
	if errs.KindOf(err) != errs.CheckFailed || !strings.Contains(fmt.Sprint(err), "APP.EMPTY has no columns") {
		t.Errorf("Copy = %v (%s), want CHECK_FAILED naming the table", err, errs.KindOf(err))
	}
}

func TestJobValidation(t *testing.T) {
	valid := func() Job {
		job, _ := genJob(t, dialect.SnapshotSingle, 1, 1, 0, discard)
		return job
	}
	cases := []struct {
		name   string
		change func(*Job)
		want   string
	}{
		{"no source", func(j *Job) { j.Source = nil }, "no source"},
		{"no sink", func(j *Job) { j.Sink = nil }, "no sink"},
		{"no keys", func(j *Job) { j.Keys = nil }, "no masking keys"},
		{"no budget", func(j *Job) { j.Budget = 0 }, "memory budget"},
		{"a budget too small", func(j *Job) { j.Budget = 1 << 19 }, "is too small for the rows in flight"},
		{"negative workers", func(j *Job) { j.Workers = -1 }, "can't be negative"},
		{"negative table jobs", func(j *Job) { j.TableJobs = -1 }, "can't be negative"},
		{"negative chunk", func(j *Job) { j.ChunkBytes = -1 }, "can't be negative"},
		{"unknown snapshot mode", func(j *Job) { j.Snapshot = 9 }, "unknown snapshot mode"},
		{"no structure", func(j *Job) { j.Tables[0].Schema = nil }, "needs its structure"},
		{"no masking", func(j *Job) { j.Tables[0].Mask = nil }, "needs its structure"},
		{"a table twice", func(j *Job) { j.Tables = append(j.Tables, j.Tables[0]) }, "APP.T0 is in the job twice"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			job := valid()
			tc.change(&job)
			if _, err := Copy(t.Context(), job); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Copy = %v, want an error containing %q", err, tc.want)
			}
		})
	}
	if _, err := Copy(t.Context(), valid()); err != nil {
		t.Errorf("a valid job: %v", err)
	}
}

func TestNoSnapshotWarns(t *testing.T) {
	var logs bytes.Buffer
	job, _ := genJob(t, dialect.SnapshotNone, 3, 10, 0, discard)
	job.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	if _, err := Copy(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(logs.String(), "no consistent snapshot"); n != 1 {
		t.Errorf("%d warnings, want 1:\n%s", n, logs.String())
	}

	logs.Reset()
	job, _ = genJob(t, dialect.SnapshotSingle, 1, 10, 0, discard)
	job.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	if _, err := Copy(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	if logs.Len() != 0 {
		t.Errorf("unexpected logs:\n%s", logs.String())
	}
}
