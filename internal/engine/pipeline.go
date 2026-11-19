package engine

import (
	"context"
	"errors"
	"fmt"

	"golang.org/x/sync/errgroup"

	"github.com/exarth-labs/stuntdb/dialect"
	"github.com/exarth-labs/stuntdb/internal/errs"
	"github.com/exarth-labs/stuntdb/internal/mask"
	"github.com/exarth-labs/stuntdb/internal/value"
)

// batch is a run of consecutive rows, in one flat slice each: src as read,
// out as masked. The reader fills src; one mask worker fills out, drops
// src and closes done.
type batch struct {
	rows  int
	bytes int
	src   []value.Value
	out   []value.Value
	err   error
	done  chan struct{}
}

// errSinkReturned stops a table's pipeline once the sink has returned.
var errSinkReturned = errors.New("engine: the sink returned")

// copyTable runs one table's pipeline: a reader, the mask workers, and the
// sink on this goroutine. The reader puts each batch on pending, in read
// order, then on work; the sink takes pending in order and waits for each
// batch to be masked. Workers never wait on anyone, so nothing can
// deadlock, and pending's capacity bounds the batches in flight.
func copyTable(ctx context.Context, src dialect.Snapshot, job Job, p *plan, tn tuning) (int64, error) {
	tctx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	g, gctx := errgroup.WithContext(tctx)

	pending := make(chan *batch, tn.queue)
	work := make(chan *batch, tn.workers)
	r := &reader{src: src, plan: p, tn: tn, pending: pending, work: work}
	g.Go(func() error { return r.run(gctx) })
	for range tn.workers {
		m := p.mask.Bind(job.Keys)
		g.Go(func() error { return maskBatches(gctx, m, work, len(p.read), len(p.write)) })
	}

	rows := &ordered{ctx: gctx, pending: pending, width: len(p.write)}
	written, loadErr := job.Sink.Load(gctx, p.ref, p.write, rows)
	stop(errSinkReturned)
	pipeErr := g.Wait()

	// The pipeline's error comes first: the sink may have returned a
	// wrapped copy of it, which would hide its kind.
	switch {
	case ctx.Err() != nil:
		return 0, context.Cause(ctx)
	case pipeErr != nil:
		return 0, pipeErr
	case loadErr != nil:
		return 0, errs.E(errs.Output, fmt.Errorf("engine: write %s: %w", p.ref, loadErr))
	case !rows.end:
		return 0, errs.E(errs.Integrity, fmt.Errorf("engine: %s: the target stopped taking rows after %d of them", p.ref, rows.delivered))
	case written != rows.delivered || r.read != rows.delivered:
		return 0, errs.E(errs.Integrity, fmt.Errorf("engine: %s: %d rows read, %d passed on, %d written", p.ref, r.read, rows.delivered, written))
	}
	return written, nil
}

// consequence drops an error that comes after ctx ended: the cancel
// caused it, and the cancel's cause is the error to report.
func consequence(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return nil
	}
	return err
}

// reader reads one table into batches. With a key, it reads in chunks
// (keyset pagination), each continuing after the last key of the one
// before, and closes each read before the next.
type reader struct {
	src     dialect.Snapshot
	plan    *plan
	tn      tuning
	pending chan<- *batch
	work    chan<- *batch

	read  int64 // rows read
	bytes int64 // their size, for sizing chunks and batches
}

func (r *reader) run(ctx context.Context) error {
	defer close(r.work)
	b := r.newBatch()
	keyed := r.plan.key != nil
	limit := 0
	if keyed {
		limit = r.tn.batchRows
	}
	var after []value.Value
	for {
		q := dialect.ReadQuery{Table: r.plan.ref, Columns: r.plan.read}
		if keyed {
			q.OrderBy, q.Limit = r.plan.key, limit
			if after != nil {
				q.Where = dialect.After{Columns: r.plan.key, Key: after}
			}
		}
		c, err := r.chunk(ctx, q, &b)
		if err != nil {
			return consequence(ctx, r.readErr(err))
		}
		if !keyed || c.rows == 0 {
			break
		}
		// A chunk that starts at or ends on the key it was asked to pass
		// would read the same rows forever, or copy them twice.
		if after != nil && (keyEqual(c.first, after) || keyEqual(c.last, after)) {
			return consequence(ctx, errs.E(errs.Integrity, fmt.Errorf("engine: %s: a chunk read did not move past the previous one, so the key's values may not round-trip through the database", r.plan.ref)))
		}
		if c.rows < limit {
			break
		}
		after = c.last
		limit = r.chunkRows()
	}
	if b.rows > 0 && !r.send(ctx, b) {
		return nil
	}
	close(r.pending)
	return nil
}

type chunk struct {
	rows        int
	first, last []value.Value // the key of the chunk's first and last rows
}

func (r *reader) chunk(ctx context.Context, q dialect.ReadQuery, b **batch) (c chunk, err error) {
	it, err := r.src.Read(ctx, q)
	if err != nil {
		return c, err
	}
	defer func() {
		if cerr := it.Close(); err == nil {
			err = cerr
		}
	}()
	width := len(q.Columns)
	keyed := len(q.OrderBy) > 0
	if keyed {
		c.last = make([]value.Value, len(r.plan.keyAt))
	}
	for it.Next() {
		row := it.Row()
		if len(row) != width {
			return c, errs.E(errs.Internal, fmt.Errorf("engine: read %s: a row has %d values for %d columns", r.plan.ref, len(row), width))
		}
		cur := *b
		size := 0
		for _, v := range row {
			size += v.Size()
		}
		// Values are immutable, so copying them out of the reused row is
		// enough.
		cur.src = append(cur.src, row...)
		cur.rows++
		cur.bytes += size
		r.read++
		r.bytes += int64(size)
		c.rows++
		if keyed {
			for i, at := range r.plan.keyAt {
				c.last[i] = row[at]
			}
			if c.first == nil {
				c.first = append([]value.Value(nil), c.last...)
			}
		}
		if cur.rows >= r.tn.batchRows || cur.bytes >= r.tn.batchBytes {
			if !r.send(ctx, cur) {
				return c, ctx.Err()
			}
			*b = r.newBatch()
		}
	}
	return c, it.Err()
}

func (r *reader) readErr(err error) error {
	if _, ok := errors.AsType[*errs.Error](err); ok {
		return err
	}
	return errs.E(errs.Connect, fmt.Errorf("engine: read %s: %w", r.plan.ref, err))
}

// send puts b on pending, then on work. Once on pending, the sink waits
// for it; once on work, a worker masks it.
func (r *reader) send(ctx context.Context, b *batch) bool {
	for _, ch := range [...]chan<- *batch{r.pending, r.work} {
		select {
		case ch <- b:
		case <-ctx.Done():
			return false
		}
	}
	return true
}

// newBatch sizes a batch's slice from the rows read so far, so that it
// doesn't grow far past its byte limit.
func (r *reader) newBatch() *batch {
	rows := r.tn.batchRows
	if r.read > 0 {
		rows = min(rows, int(int64(r.tn.batchBytes)/max(1, r.bytes/r.read))+1)
	} else {
		rows = min(rows, 64)
	}
	return &batch{src: make([]value.Value, 0, rows*len(r.plan.read)), done: make(chan struct{})}
}

// chunkRows is the next chunk's row count: the chunk size over the average
// row size so far. Rows come in key order, so where chunks end never
// changes the output.
func (r *reader) chunkRows() int {
	avg := max(1, r.bytes/max(1, r.read))
	return int(min(max(1, r.tn.chunkBytes/avg), int64(maxChunkRows)))
}

func keyEqual(a, b []value.Value) bool {
	for i := range a {
		if !a[i].Equal(b[i]) {
			return false
		}
	}
	return true
}

// maskBatches masks batches until work is closed or ctx ends.
func maskBatches(ctx context.Context, m *mask.RowMasker, work <-chan *batch, in, out int) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case b, ok := <-work:
			if !ok {
				return nil
			}
			if err := b.mask(m, in, out); err != nil {
				return err
			}
		}
	}
}

// mask masks each row's first out values of in into b.out. The rest are
// key columns read only to page through the table.
func (b *batch) mask(m *mask.RowMasker, in, out int) error {
	defer close(b.done)
	masked := make([]value.Value, b.rows*out)
	for i := range b.rows {
		if err := m.Mask(masked[i*out:(i+1)*out], b.src[i*in:i*in+out]); err != nil {
			b.err = errs.E(errs.CheckFailed, err)
			return b.err
		}
	}
	b.out, b.src = masked, nil
	return nil
}

// ordered is the RowIter the sink reads: the masked rows, batch by batch
// in read order. It may be used from any goroutine, one at a time, and
// stops when ctx ends.
type ordered struct {
	ctx     context.Context
	pending <-chan *batch
	width   int

	cur  *batch
	next int
	row  []value.Value
	err  error

	delivered int64
	end       bool // every row was passed on
}

func (o *ordered) Next() bool {
	for o.cur == nil || o.next == o.cur.rows {
		o.cur = nil
		if o.err != nil || o.end || !o.receive() {
			return false
		}
	}
	i := o.next
	o.row = o.cur.out[i*o.width : (i+1)*o.width : (i+1)*o.width]
	o.next++
	o.delivered++
	return true
}

func (o *ordered) receive() bool {
	var b *batch
	select {
	case <-o.ctx.Done():
		o.err = context.Cause(o.ctx)
		return false
	case got, ok := <-o.pending:
		if !ok {
			o.end = true
			return false
		}
		b = got
	}
	select {
	case <-o.ctx.Done():
		o.err = context.Cause(o.ctx)
		return false
	case <-b.done:
	}
	if b.err != nil {
		o.err = b.err
		return false
	}
	o.cur, o.next = b, 0
	return true
}

func (o *ordered) Row() []value.Value { return o.row }
func (o *ordered) Err() error         { return o.err }
func (o *ordered) Close() error       { return nil }
