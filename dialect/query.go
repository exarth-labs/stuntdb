package dialect

import (
	"errors"
	"fmt"

	"github.com/exarth-labs/stuntdb/internal/schema"
	"github.com/exarth-labs/stuntdb/internal/value"
)

// ReadQuery selects rows from one table. It is a description, not SQL: each
// adapter renders it in its own dialect, within its engine's limits.
type ReadQuery struct {
	Table schema.TableRef

	// Columns to return, in this order.
	Columns []schema.Ident

	// Where selects rows. Nil selects every row.
	Where Predicate

	// OrderBy sorts the rows, usually by the identity key, so that output is
	// reproducible and After can continue where an earlier read stopped.
	// Rows are compared the way the engine compares them (its collation), so
	// stuntdb never compares key values itself; it only passes back the last
	// key it saw.
	OrderBy []schema.Ident

	// Limit caps the number of rows. 0 means no limit. A limit needs OrderBy.
	Limit int
}

// Predicate is a condition on rows. The set of predicates is fixed, and every
// adapter handles each one.
type Predicate interface{ predicate() }

// KeysIn matches rows whose Columns equal one of Keys, for fetching rows by
// primary key or children by foreign key. Each key has one value per
// column, and NULL never matches.
//
// One read carries at most Capabilities.MaxKeyValues values in all; callers
// send longer lists in batches of Capabilities.KeysPerRead keys. Adapters run
// every read as a single statement, and return ErrTooManyKeys rather than
// split one.
type KeysIn struct {
	Columns []schema.Ident
	Keys    [][]value.Value
}

// After matches rows that sort after Key in the order of Columns, for reading
// a large table in chunks (keyset pagination). Engines without row-value
// comparisons expand it, e.g. a > ? OR (a = ? AND b > ?).
type After struct {
	Columns []schema.Ident
	Key     []value.Value
}

// Native is a condition written in the source engine's own SQL, such as a
// seed filter from stuntdb's configuration. It is trusted, because it comes
// from the user's own configuration, and it runs on a read-only connection.
type Native struct {
	SQL string
}

// And matches rows that match every one of its predicates.
type And []Predicate

func (KeysIn) predicate() {}
func (After) predicate()  {}
func (Native) predicate() {}
func (And) predicate()    {}

// Validate reports mistakes in q that no adapter could run: missing columns,
// a limit without an order, or keys with the wrong number of values.
func (q ReadQuery) Validate() error {
	if q.Table.Name == "" {
		return errors.New("dialect: read query has no table")
	}
	if len(q.Columns) == 0 {
		return fmt.Errorf("dialect: read query on %s has no columns", q.Table)
	}
	if q.Limit < 0 {
		return fmt.Errorf("dialect: read query on %s has a negative limit", q.Table)
	}
	if q.Limit > 0 && len(q.OrderBy) == 0 {
		return fmt.Errorf("dialect: read query on %s has a limit but no order", q.Table)
	}
	if q.Where == nil {
		return nil
	}
	return validatePredicate(q.Table, q.Where)
}

func validatePredicate(table schema.TableRef, p Predicate) error {
	switch p := p.(type) {
	case KeysIn:
		if len(p.Columns) == 0 {
			return fmt.Errorf("dialect: KeysIn on %s has no columns", table)
		}
		for i, key := range p.Keys {
			if len(key) != len(p.Columns) {
				return fmt.Errorf("dialect: KeysIn on %s: key %d has %d values for %d columns", table, i, len(key), len(p.Columns))
			}
		}
	case After:
		if len(p.Columns) == 0 || len(p.Key) != len(p.Columns) {
			return fmt.Errorf("dialect: After on %s needs one key value per column", table)
		}
	case Native:
		if p.SQL == "" {
			return fmt.Errorf("dialect: Native condition on %s is empty", table)
		}
	case And:
		for _, sub := range p {
			if sub == nil {
				return fmt.Errorf("dialect: And on %s contains a nil condition", table)
			}
			if err := validatePredicate(table, sub); err != nil {
				return err
			}
		}
	}
	return nil
}
