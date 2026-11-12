package mask

import (
	"fmt"
	"maps"
	"slices"

	"github.com/exarth-labs/stuntdb/dialect"
	"github.com/exarth-labs/stuntdb/internal/schema"
	"github.com/exarth-labs/stuntdb/internal/value"
)

// Rule says how one column is masked.
type Rule struct {
	// Use is the transformer's name.
	Use string

	// Options are the transformer's own options, as the configuration or
	// a rule pack gives them: text, numbers (json.Number) or true and false.
	Options map[string]any

	// Domain is the consistency domain. Empty means DefaultDomain of the
	// column, or with Per, of the Per column: the key's own domain, which
	// all the columns seeded from that key share.
	Domain string

	// Per names another column of the same row whose value seeds this one,
	// for per-entity consistency: the columns with the same Per and the
	// same transformer and options give one key the same result, such as
	// one shift for all of a person's dates. Empty means the column's own
	// value. Only seeded transformers take it, and a column in a unique key
	// can't use it: rows that are equal in the Per column would get equal
	// outputs.
	Per schema.Ident

	// Distinct is true when the rule's domain holds a column of a unique
	// key. Every column of the domain then uses the forms that keep values
	// distinct, such as email's tagged addresses, so that a foreign key
	// gets the same values as the key it references. ResolveDomains sets
	// it alike for every value column of a domain, and never for a Per
	// column; a unique column is distinct either way.
	Distinct bool
}

// DefaultDomain is a column's domain when its rule names none.
func DefaultDomain(ref schema.ColumnRef) string { return ref.String() }

// DomainFor returns the domain of a column under the rule: Domain when it
// is set, else the Per column's default domain, else the column's own.
func (r Rule) DomainFor(ref schema.ColumnRef) string {
	switch {
	case r.Domain != "":
		return r.Domain
	case r.Per != "":
		return DefaultDomain(schema.ColumnRef{Table: ref.Table, Column: r.Per})
	default:
		return DefaultDomain(ref)
	}
}

// Describe returns the masking description of every column of a table, in
// the table's order, from the table's keys and the engine's capabilities.
func Describe(ref schema.TableRef, t *schema.Table, caps dialect.Capabilities) []Column {
	unique := uniqueColumns(t)
	out := make([]Column, len(t.Columns))
	for i, col := range t.Columns {
		out[i] = Column{
			Ref:               schema.ColumnRef{Table: ref, Column: col.Name},
			Type:              col.Type,
			Nullable:          col.Nullable,
			Unique:            unique[col.Name],
			HasDefault:        col.Default != nil,
			EmptyStringIsNull: caps.EmptyStringIsNull,
		}
	}
	return out
}

// uniqueColumns returns the columns of a table that are part of a primary
// key, a unique constraint or a unique index, partial or not: their masked
// values must stay distinct. Composite keys count too, which may refuse a
// transformer that would have been safe for one column of the key: failing
// closed is the safe side.
//
// Uniqueness through an expression is not found here: an index on
// expressions, such as UNIQUE (lower(email)), names only its plain columns,
// and a unique generated column is recomputed by the target from the
// columns it reads. The target's unique index still refuses a collision at
// load time, so nothing broken is written, but the run fails late.
func uniqueColumns(t *schema.Table) map[schema.Ident]bool {
	out := map[schema.Ident]bool{}
	add := func(cols []schema.Ident) {
		for _, c := range cols {
			out[c] = true
		}
	}
	if t.PrimaryKey != nil {
		add(t.PrimaryKey.Columns)
	}
	for _, k := range t.Uniques {
		add(k.Columns)
	}
	for _, ix := range t.Indexes {
		if ix.Unique {
			add(ix.Columns)
		}
	}
	return out
}

// Table masks the rows of one table.
type Table struct {
	ref     schema.TableRef
	columns []schema.Ident
	plans   []plan
}

// plan is how one column of a row is masked.
type plan struct {
	ref    schema.ColumnRef
	use    string
	fn     Func
	domain string
	per    int // the row position of the value that seeds this column
}

// NewTable prepares the masking of a table's rows. Every column needs a
// rule, except generated columns, which are never copied and need none:
// there is no default, so a column nobody decided can never be copied.
func NewTable(ref schema.TableRef, t *schema.Table, rules map[schema.Ident]Rule, caps dialect.Capabilities) (*Table, error) {
	for _, name := range slices.Sorted(maps.Keys(rules)) {
		if c := t.Column(name); c == nil || c.Generated != nil {
			return nil, fmt.Errorf("mask: %s has a rule for %s, which is not a column it copies", ref, name)
		}
	}
	tbl := &Table{ref: ref}
	position := map[schema.Ident]int{}
	var pers []schema.Ident
	for i, col := range Describe(ref, t, caps) {
		if t.Columns[i].Generated != nil {
			continue
		}
		rule, ok := rules[col.Ref.Column]
		if !ok {
			return nil, fmt.Errorf("mask: %s has no rule", col.Ref)
		}
		f, err := Configure(col, rule)
		if err != nil {
			return nil, fmt.Errorf("mask: %s: %w", col.Ref, err)
		}
		if Omits(f) {
			continue
		}
		position[col.Ref.Column] = len(tbl.columns)
		tbl.columns = append(tbl.columns, col.Ref.Column)
		tbl.plans = append(tbl.plans, plan{ref: col.Ref, use: rule.Use, fn: f, domain: rule.DomainFor(col.Ref)})
		pers = append(pers, rule.Per)
	}
	for i, per := range pers {
		p := &tbl.plans[i]
		p.per = i
		if per == "" {
			continue
		}
		j, ok := position[per]
		if !ok || per == p.ref.Column {
			return nil, fmt.Errorf("mask: %s: per names %s, which is not another column this copy reads", p.ref, per)
		}
		p.per = j
	}
	return tbl, nil
}

// Columns returns the columns to read from the source and write to the
// target, in the order Mask takes them: every column but the generated
// ones and those the target fills with their default.
func (t *Table) Columns() []schema.Ident { return slices.Clone(t.columns) }

// Bind returns a RowMasker that masks with the given keys. Each worker
// needs its own.
func (t *Table) Bind(k *Keys) *RowMasker {
	return &RowMasker{t: t, seeder: newSeeder(k), rand: newRand(), ctx: Context{Keys: k}}
}

// RowMasker masks rows of one table. It reuses its state between values,
// so it is not safe for concurrent use.
type RowMasker struct {
	t      *Table
	seeder *seeder
	rand   *Rand
	ctx    Context
}

// Mask masks a row, src, into dst. Both hold one value per column, in the
// order of Columns. A per column is seeded from the unmasked value in src,
// so dst and src must not be the same slice.
func (m *RowMasker) Mask(dst, src []value.Value) error {
	if len(src) != len(m.t.plans) || len(dst) != len(src) {
		return fmt.Errorf("mask: a row of %s has %d values for %d columns", m.t.ref, len(src), len(m.t.plans))
	}
	if len(src) > 0 && &dst[0] == &src[0] {
		return fmt.Errorf("mask: a row of %s is masked into itself", m.t.ref)
	}
	for i := range m.t.plans {
		p := &m.t.plans[i]
		in := src[i]
		if in.IsNull() {
			dst[i] = in
			continue
		}
		m.ctx.Domain = p.domain
		m.ctx.Rand = nil // an unseeded function has no stream, not the last column's
		if p.fn.Seeded() {
			m.rand.reset(m.seeder.seed(p.domain, p.use, src[p.per]))
			m.ctx.Rand = m.rand
		}
		out, err := p.fn.Mask(&m.ctx, in)
		if err != nil {
			return fmt.Errorf("mask %s with %s: %w", p.ref, p.use, err)
		}
		dst[i] = out
	}
	return nil
}
