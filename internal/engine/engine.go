// Package engine copies tables from a source snapshot to a sink, masking
// every row on the way, in memory that stays flat whatever the tables' size.
//
// Each table runs as a pipeline: one reader, several mask workers, and the
// sink, which receives the masked rows in the order they were read.
package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"golang.org/x/sync/errgroup"

	"github.com/exarth-labs/stuntdb/dialect"
	"github.com/exarth-labs/stuntdb/internal/errs"
	"github.com/exarth-labs/stuntdb/internal/mask"
	"github.com/exarth-labs/stuntdb/internal/schema"
)

// Sink receives the masked rows of each table. A dialect.Loader is a Sink.
type Sink interface {
	Load(ctx context.Context, table schema.TableRef, columns []schema.Ident, rows dialect.RowIter) (int64, error)
}

// Table is one table to copy.
type Table struct {
	Ref    schema.TableRef
	Schema *schema.Table
	Mask   *mask.Table
}

// Job describes a copy. Copy uses Source alone until it returns; closing
// it is left to the caller.
type Job struct {
	Source   dialect.Snapshot
	Snapshot dialect.SnapshotMode
	Keys     *mask.Keys
	Sink     Sink
	Tables   []Table

	// Budget is the memory the copy may use, in bytes: at least 1 MiB, and
	// more for many Workers or TableJobs.
	Budget int64

	// Workers is the number of mask workers per table. 0 shares
	// GOMAXPROCS among the tables copied at once, as far as Budget allows.
	Workers int

	// TableJobs is the most tables copied at once from a shared snapshot.
	// 0 means DefaultTableJobs, or fewer if Budget can't hold them. Other
	// snapshots copy one table at a time.
	TableJobs int

	// ChunkBytes is the size of one read of a table with a key. 0 means
	// DefaultChunkBytes.
	ChunkBytes int64

	// Logger receives warnings and progress, never row values. Nil
	// discards them.
	Logger *slog.Logger
}

// Report says what a copy wrote.
type Report struct {
	Tables []TableReport // in the job's order
}

// TableReport is the number of rows copied into one table.
type TableReport struct {
	Ref  schema.TableRef
	Rows int64
}

// Copy copies every table of the job, and returns how many rows each got.
// The first failure stops the copy. If ctx is canceled, Copy returns its
// cause, whatever else failed.
func Copy(ctx context.Context, job Job) (Report, error) {
	if err := job.validate(); err != nil {
		return Report{}, err
	}
	plans := make([]*plan, len(job.Tables))
	for i, t := range job.Tables {
		p, err := newPlan(t)
		if err != nil {
			return Report{}, err
		}
		plans[i] = p
	}
	tn, err := tune(job, len(plans))
	if err != nil {
		return Report{}, err
	}
	return run(ctx, job, plans, tn)
}

func (j Job) validate() error {
	switch {
	case j.Source == nil:
		return errors.New("engine: the job has no source")
	case j.Sink == nil:
		return errors.New("engine: the job has no sink")
	case j.Keys == nil:
		return errors.New("engine: the job has no masking keys")
	case j.Snapshot > dialect.SnapshotShared:
		return fmt.Errorf("engine: unknown snapshot mode %d", j.Snapshot)
	case j.Budget <= 0:
		return errors.New("engine: the job needs a memory budget")
	case j.Workers < 0 || j.TableJobs < 0 || j.ChunkBytes < 0:
		return errors.New("engine: workers, table jobs and chunk bytes can't be negative")
	}
	seen := make(map[schema.TableRef]bool, len(j.Tables))
	for _, t := range j.Tables {
		if t.Schema == nil || t.Mask == nil {
			return fmt.Errorf("engine: %s needs its structure and its masking", t.Ref)
		}
		if seen[t.Ref] {
			return fmt.Errorf("engine: %s is in the job twice", t.Ref)
		}
		seen[t.Ref] = true
	}
	return nil
}

func (j Job) logger() *slog.Logger {
	if j.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return j.Logger
}

// plan is what one table's pipeline reads and writes.
type plan struct {
	ref       schema.TableRef
	mask      *mask.Table
	write     []schema.Ident // the masker's columns, in its order
	read      []schema.Ident // write, then the key's other columns
	key       []schema.Ident // nil: one unordered read
	keyAt     []int          // the key's positions in read
	estimated int64
}

func newPlan(t Table) (*plan, error) {
	if len(t.Schema.Columns) == 0 {
		return nil, errs.E(errs.CheckFailed, fmt.Errorf("engine: %s has no columns, and a table without columns can't be read; leave it out of the copy", t.Ref))
	}
	p := &plan{
		ref:       t.Ref,
		mask:      t.Mask,
		write:     t.Mask.Columns(),
		key:       t.Schema.IdentityKey(),
		estimated: t.Schema.EstimatedRows,
	}
	p.read = slices.Clone(p.write)
	for _, k := range p.key {
		if !slices.Contains(p.read, k) {
			p.read = append(p.read, k)
		}
	}
	// A table whose columns are all left to their defaults is still read,
	// one column, to count its rows.
	if len(p.read) == 0 {
		p.read = []schema.Ident{t.Schema.Columns[0].Name}
	}
	for _, k := range p.key {
		p.keyAt = append(p.keyAt, slices.Index(p.read, k))
	}
	return p, nil
}

func run(ctx context.Context, job Job, plans []*plan, tn tuning) (Report, error) {
	log := job.logger()
	if job.Snapshot == dialect.SnapshotNone {
		log.Warn("engine: the source has no consistent snapshot, so rows that change during the copy can break references; verification reports them")
	}
	readers, err := openPool(ctx, job.Source, tn.tables)
	defer readers.close(log)
	if err != nil {
		if ctx.Err() != nil {
			return Report{}, context.Cause(ctx)
		}
		return Report{}, errs.E(errs.Connect, fmt.Errorf("engine: open another reader on the source snapshot: %w", err))
	}

	report := Report{Tables: make([]TableReport, len(plans))}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(tn.tables)
	for _, i := range largestFirst(plans) {
		p := plans[i]
		g.Go(func() error {
			if gctx.Err() != nil {
				return context.Cause(gctx)
			}
			src, err := readers.get(gctx)
			if err != nil {
				return err
			}
			defer readers.put(src)
			n, err := copyTable(gctx, src, job, p, tn)
			if err != nil {
				return err
			}
			report.Tables[i] = TableReport{Ref: p.ref, Rows: n}
			log.Debug("engine: table copied", "table", p.ref.String(), "rows", n)
			return nil
		})
	}
	err = g.Wait()
	if ctx.Err() != nil {
		return Report{}, context.Cause(ctx)
	}
	if err != nil {
		return Report{}, err
	}
	return report, nil
}

// largestFirst returns the plans' positions, the largest table first, so
// that the longest table doesn't start last. Unknown sizes go last; ties
// go by name, so the order is the same every run.
func largestFirst(plans []*plan) []int {
	order := make([]int, len(plans))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int {
		pa, pb := plans[a], plans[b]
		if c := cmp.Compare(pb.estimated, pa.estimated); c != 0 {
			return c
		}
		return cmp.Compare(pa.ref.String(), pb.ref.String())
	})
	return order
}

// pool lends readers to the tables copied at once: the job's own snapshot
// and one joined reader per other table at once. All are opened before any
// table starts, as pg_dump -j opens its connections: joining uses the
// snapshot, which no other goroutine may use meanwhile.
type pool struct {
	free   chan dialect.Snapshot
	joined []dialect.Snapshot
}

func openPool(ctx context.Context, source dialect.Snapshot, n int) (*pool, error) {
	p := &pool{free: make(chan dialect.Snapshot, n)}
	p.free <- source
	for range n - 1 {
		s, err := source.Join(ctx)
		if err != nil {
			return p, err
		}
		p.joined = append(p.joined, s)
		p.free <- s
	}
	return p, nil
}

// get takes a free reader. One is always free while no more tables run at
// once than the pool has readers.
func (p *pool) get(ctx context.Context) (dialect.Snapshot, error) {
	select {
	case s := <-p.free:
		return s, nil
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	}
}

func (p *pool) put(s dialect.Snapshot) { p.free <- s }

// close closes the joined readers. A reader that fails to close only
// read, so the copy stands; the failure is logged.
func (p *pool) close(log *slog.Logger) {
	for _, s := range p.joined {
		if err := s.Close(); err != nil {
			log.Warn("engine: a source reader did not close cleanly", "error", err)
		}
	}
}
