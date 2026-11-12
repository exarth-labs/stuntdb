package mask

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/exarth-labs/stuntdb/internal/schema"
	"github.com/exarth-labs/stuntdb/internal/value"
)

// Guarantee says whether a masking function keeps distinct inputs distinct.
type Guarantee uint8

const (
	// None: equal inputs give equal outputs, but distinct inputs may collide.
	None Guarantee = iota

	// CollisionResistant: the output carries at least 96 bits of a keyed
	// hash of the input, so a billion distinct inputs collide with a
	// probability below 10^-11, the argument that makes random UUIDs unique.
	CollisionResistant

	// Bijective: a keyed permutation of the value's own format, so distinct
	// inputs never collide.
	Bijective
)

// collisionBits is the fewest keyed bits a CollisionResistant output carries.
const collisionBits = 96

func (g Guarantee) String() string {
	switch g {
	case CollisionResistant:
		return "collision-resistant"
	case Bijective:
		return "bijective"
	default:
		return "none"
	}
}

// Column describes the column a transformer masks.
type Column struct {
	Ref      schema.ColumnRef
	Type     value.Type
	Nullable bool

	// Unique is true when the column is part of a primary key, a unique
	// constraint or a unique index, so that its masked values must stay
	// distinct.
	Unique bool

	// HasDefault is true when the database fills the column when a row
	// leaves it out.
	HasDefault bool

	// EmptyStringIsNull is the engine's Capabilities.EmptyStringIsNull: an
	// empty string written there becomes NULL.
	EmptyStringIsNull bool
}

// Context is what a masking function gets besides the value.
type Context struct {
	// Rand is the value's deterministic random stream, for seeded functions.
	Rand *Rand

	// Keys hold the masking keys, for keyed permutations.
	Keys *Keys

	// Domain is the column's consistency domain. Keyed permutations use it
	// as their tweak, so that each domain has its own permutation.
	Domain string
}

// Func masks the values of one column. NULL never reaches it: NULL always
// stays NULL.
type Func interface {
	// Guarantee says whether distinct inputs stay distinct.
	Guarantee() Guarantee

	// Seeded reports whether Mask uses Context.Rand, which costs a keyed
	// hash per value.
	Seeded() bool

	// Mask returns the masked value. Its errors must not contain any value.
	Mask(c *Context, in value.Value) (value.Value, error)
}

// target is all a transformer knows about a column when it decides its
// outputs: what every column of a domain shares. It never sees a column's
// length, nullability or integer width, so a value gets the same output in
// every column of its domain, such as a key and the foreign keys that
// reference it. Whether a column can hold the outputs is checked
// afterwards, by fn.fit.
type target struct {
	// kind is the column's type kind. A string column appears as text: the
	// two differ only in their length limit.
	kind value.TypeKind

	// distinct is true when the domain holds a column of a unique key:
	// every column of the domain then uses the forms that keep values
	// distinct.
	distinct bool
}

// Transformer is one entry of the catalog.
type Transformer struct {
	Name    string
	Summary string

	// Types are the column types the transformer can mask.
	Types []value.TypeKind

	// Options are the options it takes, besides the generic domain and per.
	Options []string

	// build decides the masking function from the options and the target.
	build func(t target, o *Options) (*fn, error)

	// alike names the class of a column type: the transformer masks equal
	// values alike in columns whose types have one class, so that those
	// columns can share a domain. nil means familyClass, the seed's value
	// family; a transformer sets it when its outputs follow other lines.
	alike func(value.Type) string

	// entityOptions are the options that may differ between the columns of
	// one per group (the same per key, domain and transformer), because
	// each column holds another part of one entity, such as address's part.
	entityOptions []string

	// readsText is set when the transformer masks text only in one format,
	// such as ISO 8601 dates or UUIDs: it is never suggested for a text
	// column without knowing what the column holds.
	readsText bool

	// domainFree is set when the outputs never depend on the domain: the
	// transformer is neither seeded nor keyed by it.
	domainFree bool
}

// UsesDomain reports whether the transformer's outputs depend on the
// domain: seeded transformers draw from a seed that includes it, and keyed
// permutations (uuid, fpe) take it as their tweak. Columns that must hold
// equal values need one domain only under such a transformer; keep, for
// one, copies the value whatever the domain.
func (t *Transformer) UsesDomain() bool { return !t.domainFree }

// SameOptions reports whether two sets of options build and are equal once
// defaults are filled in, so { use: hash } equals { use: hash, length: 32 }.
// Distinct is left out: it never changes which options are read
// (TestDistinctReadsTheSameOptions).
func (t *Transformer) SameOptions(kind value.TypeKind, a, b map[string]any) bool {
	_, ra, errA := t.configure(kind, a, false)
	_, rb, errB := t.configure(kind, b, false)
	return errA == nil && errB == nil && maps.Equal(ra, rb)
}

// Accepts reports whether the transformer can mask a column of type t.
func (t *Transformer) Accepts(k value.TypeKind) bool { return slices.Contains(t.Types, k) }

// MasksAlike reports whether the transformer accepts columns of types a and
// b, and gives equal values in them equal outputs: whether such columns can
// share a domain. Values are never converted to find out: the text "00123"
// and the number 123 may be different rows.
func (t *Transformer) MasksAlike(a, b value.Type) bool {
	if !t.Accepts(a.Kind) || !t.Accepts(b.Kind) {
		return false
	}
	c := t.class(a)
	return c != noClass && c == t.class(b)
}

// noClass is the class of a transformer whose outputs don't come from the
// value, so that equal values in two columns never stay equal: default,
// which leaves each column to its own database default.
const noClass = ""

// class returns the class of a column type under the transformer.
func (t *Transformer) class(typ value.Type) string {
	if t.alike != nil {
		return t.alike(typ)
	}
	return familyClass(typ)
}

// familyClass is the class of a column type for most transformers: the
// family its values are seeded in (see appendCanonical), so that the
// integer 5 and the decimal 5.00 are alike, and the number 5 and the text
// "5" are not. Times and timestamps with a time zone are a class of their
// own: the seed reads them as instants, the others as wall-clock times.
func familyClass(typ value.Type) string {
	c := "family " + strconv.Itoa(int(typeFamily(typ.Kind)))
	if typ.TZ && (typ.Kind == value.TypeTime || typ.Kind == value.TypeTimestamp) {
		c += " with time zone"
	}
	return c
}

// anyClass is the class function of a transformer that masks every type
// alike: equal values always give equal outputs.
func anyClass(value.Type) string { return "any" }

// Configure returns the masking function of a column under a rule. It checks
// the transformer, its options, whether the column can hold every output,
// the column's uniqueness and the use of per. It needs no masking key, so
// "stuntdb check" can run it without one. Its errors explain what to change
// and name no value. Whether per names a column of the table is for
// NewTable to check.
func Configure(col Column, rule Rule) (Func, error) {
	t, ok := Lookup(rule.Use)
	if !ok {
		if s := Suggest(rule.Use); s != "" {
			return nil, fmt.Errorf("there is no transformer %s; did you mean %s? (stuntdb transformers lists them)", rule.Use, s)
		}
		return nil, fmt.Errorf("there is no transformer %s (stuntdb transformers lists them)", rule.Use)
	}
	if !t.Accepts(col.Type.Kind) {
		return nil, fmt.Errorf("%s cannot mask a %s column; it masks %s", t.Name, col.Type, typeList(t.Types))
	}
	f, _, err := t.configure(col.Type.Kind, rule.Options, rule.Distinct || col.Unique)
	if err != nil {
		return nil, err
	}
	if col.Unique && f.guarantee == None {
		return nil, fmt.Errorf("the column is part of a unique key, so its masked values must stay distinct, and %s cannot promise that%s", t.Name, distinctAlternatives(col, t.Name))
	}
	if rule.Per != "" {
		switch {
		case !f.seeded:
			return nil, fmt.Errorf("%s takes no seed, so per has no effect", t.Name)
		case col.Unique:
			return nil, errors.New("per seeds the column from another column, so rows that are equal there would get equal masked values; a column in a unique key can't use per")
		}
	}
	if f.fit != nil {
		if err := f.fit(col); err != nil {
			var short *TooShortError
			if errors.As(err, &short) && short.others {
				short.msg += fitAlternatives(col, rule.Distinct || col.Unique, t.Name)
			}
			return nil, err
		}
	}
	return &checked{fn: f, col: col, name: t.Name}, nil
}

// configure builds the masking function for a column kind, and reports the
// mistakes in the options together. It also returns the options the build
// used, with their defaults filled in (see Options.resolve).
func (t *Transformer) configure(kind value.TypeKind, options map[string]any, distinct bool) (*fn, map[string]string, error) {
	if kind == value.TypeString {
		kind = value.TypeText
	}
	o := newOptions(t, options)
	f, err := t.build(target{kind: kind, distinct: distinct}, o)
	if err := errors.Join(err, o.err()); err != nil {
		return nil, nil, err
	}
	return f, o.resolved, nil
}

// distinctAlternatives tells the user, for an error message, which other
// transformers keep the values of a unique column distinct.
func distinctAlternatives(col Column, except string) string {
	names := alternatives(col, true, except)
	if len(names) == 0 {
		return fmt.Sprintf("; in a %s column, no transformer can, so keep the values or change the key", col.Type)
	}
	return "; these can: " + strings.Join(names, ", ")
}

// fitAlternatives tells the user, for an error message, which other
// transformers fit a column that is too short for the one refused.
func fitAlternatives(col Column, distinct bool, except string) string {
	names := alternatives(col, distinct, except)
	if len(names) == 0 {
		return "; no other transformer fits the column"
	}
	return "; these fit: " + strings.Join(names, ", ")
}

// alternatives returns the transformers that can mask a column with their
// default options: they take its type, fit it, and keep its values
// distinct where the domain needs it. It leaves out except, the transformer
// just refused, those that mask nothing (keep, null and default), and in a
// text column, those that read text in one format only.
func alternatives(col Column, distinct bool, except string) []string {
	text := col.Type.Kind == value.TypeString || col.Type.Kind == value.TypeText
	var names []string
	for _, t := range Catalog() {
		if t.Name == except || t.Name == "keep" || t.Name == "null" || t.Name == "default" || !t.Accepts(col.Type.Kind) || text && t.readsText {
			continue
		}
		f, _, err := t.configure(col.Type.Kind, nil, distinct || col.Unique)
		if err != nil || (col.Unique || distinct) && f.guarantee == None || (f.fit != nil && f.fit(col) != nil) {
			continue
		}
		names = append(names, t.Name)
	}
	return names
}

// checked checks every output against the column, so that a value that
// would not fit never leaves the masker.
type checked struct {
	*fn
	col  Column
	name string
}

func (c *checked) Mask(ctx *Context, in value.Value) (value.Value, error) {
	out, err := c.fn.Mask(ctx, in)
	if err != nil {
		return value.Value{}, err
	}
	if out.IsNull() {
		if !c.col.Nullable {
			return value.Value{}, fmt.Errorf("%s wrote NULL into a NOT NULL column", c.name)
		}
		return out, nil
	}
	// A value of another kind than the column's type, such as text in a
	// SQLite INTEGER column, passes only when the input had that kind too:
	// the source engine already holds such values, and the transformer
	// kept their kind. A transformer never introduces a mismatch.
	sourceKind := !natural(c.col.Type, out.Kind()) && out.Kind() == in.Kind()
	if err := c.col.Type.Check(out); err != nil && !sourceKind {
		if natural(c.col.Type, in.Kind()) && c.col.Type.Check(in) != nil {
			return value.Value{}, fmt.Errorf("the database holds a value that does not fit the column's type (%s), so %s cannot mask it", c.col.Type, c.name)
		}
		return value.Value{}, fmt.Errorf("%s wrote a value that does not fit the column: %w", c.name, err)
	}
	if c.col.EmptyStringIsNull && !c.col.Nullable && out.Kind() == value.KindString && out.Text() == "" {
		return value.Value{}, fmt.Errorf("%s wrote an empty string, which this database stores as NULL, into a NOT NULL column", c.name)
	}
	return out, nil
}

// natural reports whether a column of type t normally holds values of kind
// k. Integer columns hold signed and unsigned integers alike.
func natural(t value.Type, k value.Kind) bool {
	if t.Kind == value.TypeInt {
		return k == value.KindInt || k == value.KindUint
	}
	return k == t.NaturalKind()
}

func typeList(kinds []value.TypeKind) string {
	names := make([]string, len(kinds))
	for i, k := range kinds {
		names[i] = k.String()
	}
	return strings.Join(names, ", ")
}

// Options reads a transformer's options. Every reader records the option
// as used and collects mistakes, which Configure reports together with any
// option the transformer does not take.
type Options struct {
	t    *Transformer
	raw  map[string]any
	used map[string]bool
	errs []error

	// resolved holds every option the build read, with the value it used
	// in a canonical form: the default when the option is not set. Columns
	// of one domain compare these, so that { use: hash } equals
	// { use: hash, length: 32 }. They are compared, never shown.
	resolved map[string]string
}

func newOptions(t *Transformer, raw map[string]any) *Options {
	return &Options{t: t, raw: raw, used: map[string]bool{}, resolved: map[string]string{}}
}

// resolve records the value a build uses for an option, in a canonical
// form.
func (o *Options) resolve(name, canonical string) { o.resolved[name] = canonical }

func (o *Options) fail(name, format string, args ...any) {
	o.errs = append(o.errs, fmt.Errorf("option %s: %s", name, fmt.Sprintf(format, args...)))
}

// err reports the mistakes, and the options the transformer does not take.
func (o *Options) err() error {
	for _, name := range slices.Sorted(maps.Keys(o.raw)) {
		if !o.used[name] {
			if len(o.t.Options) == 0 {
				o.errs = append(o.errs, fmt.Errorf("%s takes no option %s", o.t.Name, name))
			} else {
				o.errs = append(o.errs, fmt.Errorf("%s takes no option %s; its options are %s", o.t.Name, name, strings.Join(o.t.Options, ", ")))
			}
		}
	}
	return errors.Join(o.errs...)
}

// text returns an option as text: YAML text, a number, or true or false.
func (o *Options) text(name string) (string, bool) {
	v, ok := o.raw[name]
	if !ok {
		return "", false
	}
	o.used[name] = true
	switch v := v.(type) {
	case string:
		return v, true
	case fmt.Stringer: // json.Number
		return v.String(), true
	case bool:
		if v {
			return "true", true
		}
		return "false", true
	case nil:
		o.fail(name, "has no value")
	default:
		o.fail(name, "must be a single value, not a list or a set of settings")
	}
	return "", false
}

// Int returns a whole-number option from lo to hi, or def when it is not set.
func (o *Options) Int(name string, def, lo, hi int) int {
	n := def
	if s, ok := o.text(name); ok {
		if v, err := parseInt(s); err != nil || v < lo || v > hi {
			o.fail(name, "must be a whole number from %d to %d", lo, hi)
		} else {
			n = v
		}
	}
	o.resolve(name, strconv.Itoa(n))
	return n
}

// OneOf returns an option that must be one of the allowed words, or def
// when it is not set.
func (o *Options) OneOf(name, def string, allowed ...string) string {
	s, ok := o.text(name)
	switch {
	case !ok:
		s = def
	case !slices.Contains(allowed, s):
		o.fail(name, "must be one of: %s", strings.Join(allowed, ", "))
		s = def
	}
	o.resolve(name, s)
	return s
}

// Text returns a text option, and whether it is set.
func (o *Options) Text(name string) (string, bool) {
	s, ok := o.text(name)
	if ok {
		o.resolve(name, s)
	}
	return s, ok
}

// textOr returns a text option, or def when it is not set.
func (o *Options) textOr(name, def string) string {
	s, ok := o.text(name)
	if !ok {
		s = def
	}
	o.resolve(name, s)
	return s
}

// refuse reports an option that the transformer takes, but not for this
// column, when it is set.
func (o *Options) refuse(name, why string) {
	if _, set := o.raw[name]; set {
		o.used[name] = true
		o.fail(name, "%s", why)
	}
}

func parseInt(s string) (int, error) {
	n := 0
	if s == "" || len(s) > 9 {
		return 0, errors.New("not a small whole number")
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return 0, errors.New("not a whole number")
		}
		n = n*10 + int(s[i]-'0')
	}
	return n, nil
}

// errKind is the error for a value whose kind a transformer cannot mask,
// such as text in a SQLite INTEGER column. It names kinds, never the value.
func errKind(name string, in value.Value) error {
	return fmt.Errorf("%s cannot mask a %s value", name, in.Kind())
}

// catalog holds the built-in transformers, by name. init fills it once, at
// start-up, and nothing changes it afterwards. It is filled in init, not
// where it is declared, because transformers read the catalog to explain a
// refusal (distinctAlternatives), which Go's ordering of package variables
// would report as an initialization cycle.
var catalog map[string]*Transformer

func init() {
	catalog = map[string]*Transformer{}
	for _, t := range builtins() {
		if catalog[t.Name] != nil {
			panic("mask: transformer " + t.Name + " is defined twice")
		}
		catalog[t.Name] = t
	}
}

// Lookup returns the transformer with the given name.
func Lookup(name string) (*Transformer, bool) {
	t, ok := catalog[name]
	return t, ok
}

// Catalog returns every built-in transformer, sorted by name.
func Catalog() []*Transformer {
	out := slices.Collect(maps.Values(catalog))
	slices.SortFunc(out, func(a, b *Transformer) int { return cmp.Compare(a.Name, b.Name) })
	return out
}

// Suggest returns the transformer name closest to a misspelled one, or "".
func Suggest(name string) string {
	best, bestDist := "", 3
	for _, t := range Catalog() {
		if d := editDistance(name, t.Name); d < bestDist {
			best, bestDist = t.Name, d
		}
	}
	return best
}

// editDistance is the Levenshtein distance between two short names.
func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}
