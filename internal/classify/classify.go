// Package classify suggests what each column of a database holds, such as an
// email address, a password hash or free text, from the column's name and
// type alone. It never reads data, so it is safe to run against any database.
//
// Rules come in packs, YAML files embedded in stuntdb (see the rules
// package): core for personal data, and secrets for credentials. Each rule
// puts a column in a class and suggests a transformer. A human confirms every
// suggestion in stuntdb.yaml, with two exceptions:
//
//   - A column of class Safe, such as a key the database numbers itself, is
//     kept without review. The lockfile records that decision, so that it is
//     reviewed in pull requests like any other change.
//   - A column of class Secret is always replaced: a production token copied
//     into a development database still works. Match.Allows refuses keep for
//     it.
//
// A foreign key holds the values of the key it references, so it gets the
// same classification, unless its own is more protective. A reference to a
// key the database numbers itself is then safe, and a reference to an email
// is an email, masked the same way so that joins still work.
package classify

import (
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/exarth-labs/stuntdb/internal/errs"
	"github.com/exarth-labs/stuntdb/internal/schema"
	"github.com/exarth-labs/stuntdb/rules"
)

// keep is the transformer that copies a value as it is.
const keep = "keep"

// Class is what kind of data a column holds.
type Class uint8

const (
	Unclassified Class = iota // no rule matched: someone must review the column
	Direct                    // identifies a person on its own: a name, an email, a phone number
	Quasi                     // identifies a person in combination: a postcode, a birth date, a gender
	Sensitive                 // harmful if disclosed: health, salary
	Secret                    // a credential: a password hash, an API key, a token
	FreeText                  // text people write, which may contain anything
	Safe                      // copied without review: a key the database numbers itself
)

var classNames = [...]string{
	Unclassified: "unclassified",
	Direct:       "direct",
	Quasi:        "quasi",
	Sensitive:    "sensitive",
	Secret:       "secret",
	FreeText:     "free_text",
	Safe:         "safe",
}

// String returns the class's name, as rule packs and reports write it.
func (c Class) String() string {
	if int(c) < len(classNames) {
		return classNames[c]
	}
	return "invalid"
}

func parseClass(s string) (Class, bool) {
	for c, name := range classNames {
		if name == s {
			return Class(c), true
		}
	}
	return Unclassified, false
}

// protection orders the classes by how much protection they call for. An
// unclassified column is blocked until someone reviews it, so it ranks above
// a safe one.
func (c Class) protection() int {
	switch c {
	case Safe:
		return 0
	case FreeText:
		return 2
	case Quasi:
		return 3
	case Sensitive:
		return 4
	case Direct:
		return 5
	case Secret:
		return 6
	default:
		return 1
	}
}

// Match is the classification of one column.
type Match struct {
	Class      Class
	Rule       string            // the rule that matched, such as "core.email"; empty when Unclassified
	Use        string            // the suggested transformer; keep for a safe column
	Options    map[string]string // the suggested transformer's options, if any
	Confidence float64           // how likely the match is right, from 0 to 1

	// Via is the column this one references, when the classification comes
	// from it. Nil when the column's own name and type decided.
	Via *schema.ColumnRef
}

// Source says where the classification comes from, for the lockfile:
// "rule core.email", or "rule core.surrogate_key, references main.users.id".
// It is empty for an unclassified column.
func (m Match) Source() string {
	if m.Class == Unclassified {
		return ""
	}
	if m.Via != nil {
		return "rule " + m.Rule + ", references " + m.Via.String()
	}
	return "rule " + m.Rule
}

// ErrKeepSecret is returned when a secret column is marked keep.
var ErrKeepSecret = errors.New("secrets are always replaced")

// Allows reports whether a column with this classification may use a
// transformer. Every transformer is allowed, except keep for a secret. The
// error carries errs.CheckFailed and wraps ErrKeepSecret.
func (m Match) Allows(use string) error {
	if m.Class != Secret || use != keep {
		return nil
	}
	return errs.E(errs.CheckFailed, fmt.Errorf("%w: the column looks like a secret (rule %s), so it cannot be kept; use %s, null or another transformer",
		ErrKeepSecret, m.Rule, m.Use))
}

// Classifier classifies columns with a set of rule packs. It is safe for
// concurrent use.
type Classifier struct {
	rules []*rule
}

// New returns a classifier that uses the given rule packs. When two rules
// match a column equally well, the one from the earlier pack wins.
func New(packs ...rules.Pack) (*Classifier, error) {
	c := &Classifier{}
	seen := map[string]bool{}
	for _, p := range packs {
		if seen[p.Name] {
			return nil, fmt.Errorf("classify: rule pack %s is loaded twice", p.Name)
		}
		seen[p.Name] = true
		rs, err := parsePack(p)
		if err != nil {
			return nil, fmt.Errorf("classify: %w", err)
		}
		c.rules = append(c.rules, rs...)
	}
	return c, nil
}

// Default returns a classifier that uses the packs built into stuntdb.
func Default() (*Classifier, error) {
	return New(rules.Builtin()...)
}

// Database classifies every column of db.
func (c *Classifier) Database(db *schema.Database) map[schema.ColumnRef]Match {
	r := resolver{
		own:      map[schema.ColumnRef]Match{},
		parents:  map[schema.ColumnRef][]schema.ColumnRef{},
		done:     map[schema.ColumnRef]Match{},
		visiting: map[schema.ColumnRef]bool{},
	}
	var order []schema.ColumnRef
	for _, ns := range db.Namespaces {
		for _, t := range ns.Tables {
			table := schema.TableRef{Namespace: ns.Name, Name: t.Name}
			tableWords := trimNumbers(words(string(t.Name)))
			for _, col := range t.Columns {
				ref := schema.ColumnRef{Table: table, Column: col.Name}
				r.own[ref] = c.column(tableWords, col)
				order = append(order, ref)
			}
			for _, fk := range t.ForeignKeys {
				// A virtual foreign key is not enforced by the database, so
				// its values may be anything.
				if fk.Virtual {
					continue
				}
				for i, name := range fk.Columns {
					if i < len(fk.RefColumns) {
						ref := schema.ColumnRef{Table: table, Column: name}
						r.parents[ref] = append(r.parents[ref], schema.ColumnRef{Table: fk.Ref, Column: fk.RefColumns[i]})
					}
				}
			}
		}
	}
	// Columns are resolved in the database's order, so that a cycle of
	// references always resolves the same way.
	out := make(map[schema.ColumnRef]Match, len(order))
	for _, ref := range order {
		out[ref] = r.resolve(ref)
	}
	return out
}

// column classifies one column by its own name and type.
func (c *Classifier) column(table []string, col *schema.Column) Match {
	name := trimNumbers(words(string(col.Name)))
	var best *rule
	bestLen := 0
	for _, r := range c.rules {
		n, ok := r.matches(table, name, col)
		if ok && (best == nil || better(r, n, best, bestLen)) {
			best, bestLen = r, n
		}
	}
	if best == nil {
		return Match{}
	}
	return Match{Class: best.class, Rule: best.id, Use: best.use, Options: maps.Clone(best.options), Confidence: best.confidence}
}

// matches reports whether the rule applies to a column, and how many words
// of the column's name it matched.
func (r *rule) matches(table, name []string, col *schema.Column) (int, bool) {
	if r.types&(1<<col.Type.Kind) == 0 {
		return 0, false
	}
	if r.identity && !assignedByDatabase(col) {
		return 0, false
	}
	if len(r.tables) > 0 && !slices.ContainsFunc(r.tables, func(p []string) bool { return contains(table, p) }) {
		return 0, false
	}
	if len(r.names) == 0 {
		return 0, true
	}
	longest := 0
	for _, p := range r.names {
		if len(p) > longest && endsWith(name, p) {
			longest = len(p)
		}
	}
	return longest, longest > 0
}

// assignedByDatabase reports whether the database assigns a column's values
// itself: an identity or auto-increment column, or a generated UUID.
func assignedByDatabase(col *schema.Column) bool {
	return col.Identity != nil || col.Default != nil && col.Default.Kind == schema.DefaultUUID
}

// better reports whether rule a, matching n words of a name, beats rule b,
// matching m. Personal data beats safe, so that a column that looks both
// personal and safe is reviewed. Then the longest match wins: email_address
// is an email, not an address. Then the more protective class, then the
// higher confidence; otherwise the earlier rule stays.
func better(a *rule, n int, b *rule, m int) bool {
	if (a.class == Safe) != (b.class == Safe) {
		return b.class == Safe
	}
	if n != m {
		return n > m
	}
	if pa, pb := a.class.protection(), b.class.protection(); pa != pb {
		return pa > pb
	}
	return a.confidence > b.confidence
}

// resolver gives foreign keys the classification of the columns they reference.
type resolver struct {
	own      map[schema.ColumnRef]Match
	parents  map[schema.ColumnRef][]schema.ColumnRef // the columns each column references
	done     map[schema.ColumnRef]Match
	visiting map[schema.ColumnRef]bool
}

func (r *resolver) resolve(ref schema.ColumnRef) Match {
	if m, ok := r.done[ref]; ok {
		return m
	}
	m := r.own[ref]
	if r.visiting[ref] {
		return m // a cycle of references: the column's own classification decides
	}
	r.visiting[ref] = true
	for _, parent := range r.parents[ref] {
		if _, ok := r.own[parent]; ok {
			m = inherit(m, r.resolve(parent), parent)
		}
	}
	delete(r.visiting, ref)
	r.done[ref] = m
	return m
}

// inherit combines a foreign key's classification with that of the column it
// references. The referenced column's wins, so that both ends are masked the
// same way, unless the foreign key's own is more protective. A reference to
// an unclassified column is unclassified, even if its own name looks safe.
func inherit(own, parent Match, via schema.ColumnRef) Match {
	if own.Class != Unclassified && own.Class.protection() > parent.Class.protection() {
		return own
	}
	if parent.Class == Unclassified {
		return Match{}
	}
	parent.Via = &via
	parent.Options = maps.Clone(parent.Options)
	return parent
}
