package mask

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/exarth-labs/stuntdb/dialect"
	"github.com/exarth-labs/stuntdb/internal/schema"
	"github.com/exarth-labs/stuntdb/internal/value"
)

// shop is a parent table with a unique natural key (email) and a child
// whose foreign key references it with another declared type.
func shop() (users, orders *schema.Table) {
	users = &schema.Table{
		Name: "users",
		Columns: []*schema.Column{
			{Name: "id", Type: tInt64, Identity: &schema.Identity{}},
			{Name: "email", Type: tString(100)},
			{Name: "born", Type: tDate, Nullable: true},
			{Name: "joined", Type: tDate, Nullable: true},
			{Name: "status", Type: tText, Default: &schema.Default{Kind: schema.DefaultLiteral, Expr: "'new'"}},
			{Name: "slug", Type: tText, Generated: &schema.Generated{Expr: "lower(email)"}},
		},
		PrimaryKey: &schema.Key{Columns: []schema.Ident{"id"}},
		Uniques:    []*schema.Key{{Columns: []schema.Ident{"email"}}},
	}
	orders = &schema.Table{
		Name: "orders",
		Columns: []*schema.Column{
			{Name: "id", Type: tInt64},
			{Name: "user_email", Type: tText},
		},
		PrimaryKey: &schema.Key{Columns: []schema.Ident{"id"}},
		ForeignKeys: []*schema.ForeignKey{
			{Columns: []schema.Ident{"user_email"}, Ref: schema.TableRef{Namespace: "main", Name: "users"}, RefColumns: []schema.Ident{"email"}},
		},
	}
	return users, orders
}

var (
	usersRef  = schema.TableRef{Namespace: "main", Name: "users"}
	ordersRef = schema.TableRef{Namespace: "main", Name: "orders"}
)

func usersRules() map[schema.Ident]Rule {
	return map[schema.Ident]Rule{
		"id":     {Use: "keep"},
		"email":  {Use: "email"},
		"born":   {Use: "date_shift", Per: "id"},
		"joined": {Use: "date_shift", Per: "id"},
		"status": {Use: "default"},
	}
}

func TestNewTable(t *testing.T) {
	users, _ := shop()
	tbl, err := NewTable(usersRef, users, usersRules(), dialect.Capabilities{})
	if err != nil {
		t.Fatal(err)
	}
	// The generated column is never copied, and status is left to its default.
	if got := fmt.Sprint(tbl.Columns()); got != "[id email born joined]" {
		t.Errorf("Columns = %s", got)
	}

	mistakes := []struct {
		name  string
		rules func(map[schema.Ident]Rule)
		want  string
	}{
		{"no rule", func(r map[schema.Ident]Rule) { delete(r, "born") }, "main.users.born has no rule"},
		{"rule for a generated column", func(r map[schema.Ident]Rule) { r["slug"] = Rule{Use: "keep"} }, "slug, which is not a column it copies"},
		{"rule for a missing column", func(r map[schema.Ident]Rule) { r["nope"] = Rule{Use: "keep"} }, "nope, which is not a column it copies"},
		{"weak transformer on a unique column", func(r map[schema.Ident]Rule) { r["email"] = Rule{Use: "first_name"} }, "main.users.email: the column is part of a unique key"},
		{"per on its own column", func(r map[schema.Ident]Rule) { r["born"] = Rule{Use: "date_shift", Per: "born"} }, "not another column"},
		{"per on a missing column", func(r map[schema.Ident]Rule) { r["born"] = Rule{Use: "date_shift", Per: "nope"} }, "not another column"},
		{"per on a defaulted column", func(r map[schema.Ident]Rule) { r["born"] = Rule{Use: "date_shift", Per: "status"} }, "not another column"},
		{"per without a seed", func(r map[schema.Ident]Rule) { r["born"] = Rule{Use: "keep", Per: "id"} }, "keep takes no seed"},
		{"per on a unique column", func(r map[schema.Ident]Rule) { r["email"] = Rule{Use: "email", Per: "id"} }, "main.users.email: per seeds the column from another column"},
	}
	for _, m := range mistakes {
		t.Run(m.name, func(t *testing.T) {
			rules := usersRules()
			m.rules(rules)
			_, err := NewTable(usersRef, users, rules, dialect.Capabilities{})
			if err == nil || !strings.Contains(err.Error(), m.want) {
				t.Errorf("error = %v, want one containing %q", err, m.want)
			}
		})
	}
}

func TestMaskRows(t *testing.T) {
	users, orders := shop()
	keys := testKeys(t)
	ut, err := NewTable(usersRef, users, usersRules(), dialect.Capabilities{})
	if err != nil {
		t.Fatal(err)
	}
	// The foreign key follows its root: the same transformer, options and
	// domain, and Distinct, because the domain holds the unique users.email.
	// orders.user_email is neither unique nor as long, yet equal emails must
	// give equal fake emails in both tables.
	ot, err := NewTable(ordersRef, orders, map[schema.Ident]Rule{
		"id":         {Use: "keep"},
		"user_email": {Use: "email", Domain: DefaultDomain(schema.ColumnRef{Table: usersRef, Column: "email"}), Distinct: true},
	}, dialect.Capabilities{})
	if err != nil {
		t.Fatal(err)
	}
	um, om := ut.Bind(keys), ot.Bind(keys)

	born, _ := value.Date(1990, 3, 15)
	joined, _ := value.Date(2020, 1, 1)
	const canary = "canary.person@private.test"
	user := []value.Value{value.Int(7), value.String(canary), born, joined}
	maskedUser := make([]value.Value, 4)
	if err := um.Mask(maskedUser, user); err != nil {
		t.Fatal(err)
	}
	order := []value.Value{value.Int(1), value.String(canary)}
	maskedOrder := make([]value.Value, 2)
	if err := om.Mask(maskedOrder, order); err != nil {
		t.Fatal(err)
	}
	if !maskedOrder[1].Equal(maskedUser[1]) {
		t.Error("the foreign key's fake email differs from its parent's: the reference is broken")
	}
	if maskedUser[1].Text() == canary || !maskedUser[0].Equal(user[0]) {
		t.Error("email kept, or id changed")
	}

	// per: id, without a domain: both dates move by the same number of
	// days, so the gap between them stays.
	if daysBetween(user[2], user[3]) != daysBetween(maskedUser[2], maskedUser[3]) {
		t.Error("per: the dates of one person moved by different amounts")
	}

	// NULL stays NULL.
	nulls := []value.Value{value.Int(8), value.String("x@y.test"), value.Null(), value.Null()}
	if err := um.Mask(maskedUser, nulls); err != nil || !maskedUser[2].IsNull() || !maskedUser[3].IsNull() {
		t.Errorf("NULL dates: %v", err)
	}

	// Mistakes in the call, and errors that never show the value.
	if err := um.Mask(maskedUser[:2], user); err == nil {
		t.Error("a short row is accepted")
	}
	if err := um.Mask(user, user); err == nil {
		t.Error("masking a row into itself is accepted")
	}
	bad := []value.Value{value.Int(9), value.String(canary), value.String(canary), joined}
	err = um.Mask(maskedUser, bad)
	if err == nil || !strings.Contains(err.Error(), "main.users.born") || strings.Contains(err.Error(), canary) {
		t.Errorf("an unreadable date: %v", err)
	}
}

// TestPerGroupsAnEntity checks per-entity consistency without any domain:
// the columns seeded from the same key with the same transformer give one
// key one result. All of a person's dates move by the same number of days,
// and the parts of an address describe one address.
func TestPerGroupsAnEntity(t *testing.T) {
	people := &schema.Table{
		Name: "people",
		Columns: []*schema.Column{
			{Name: "id", Type: tInt64},
			{Name: "born", Type: tDate},
			{Name: "hired", Type: tDate},
			{Name: "address", Type: tText},
			{Name: "street", Type: tText},
			{Name: "city", Type: tText},
			{Name: "first", Type: tText},
			{Name: "zip", Type: tInt32},
		},
		PrimaryKey: &schema.Key{Columns: []schema.Ident{"id"}},
	}
	ref := schema.TableRef{Namespace: "main", Name: "people"}
	rules := map[schema.Ident]Rule{
		"id":      {Use: "keep"},
		"born":    {Use: "date_shift", Per: "id"},
		"hired":   {Use: "date_shift", Per: "id"},
		"address": {Use: "address", Per: "id"},
		"street":  {Use: "address", Options: map[string]any{"part": "street"}, Per: "id"},
		"city":    {Use: "address", Options: map[string]any{"part": "city"}, Per: "id"},
		"first":   {Use: "first_name", Per: "id"},
		"zip":     {Use: "address", Options: map[string]any{"part": "postcode"}, Per: "id"},
	}
	var cols []ColumnRule
	for _, c := range Describe(ref, people, dialect.Capabilities{}) {
		cols = append(cols, ColumnRule{c, rules[c.Ref.Column]})
	}
	if _, err := ResolveDomains(cols); err != nil {
		t.Fatalf("the parts of one entity are refused: %v", err)
	}
	tbl, err := NewTable(ref, people, rules, dialect.Capabilities{})
	if err != nil {
		t.Fatal(err)
	}
	m := tbl.Bind(testKeys(t))
	born, _ := value.Date(1990, 3, 15)
	hired, _ := value.Date(2020, 1, 1)
	out := make([]value.Value, 8)
	shifts := map[int]bool{}
	for id := range int64(40) {
		in := []value.Value{value.Int(id), born, hired, value.String("x"), value.String("y"), value.String("z"), value.String("w"), value.Int(75001)}
		if err := m.Mask(out, in); err != nil {
			t.Fatal(err)
		}
		shift := daysBetween(born, out[1])
		if daysBetween(hired, out[2]) != shift {
			t.Fatalf("id %d: the two dates moved by different amounts", id)
		}
		shifts[shift] = true
		address, street, city, zip := out[3].Text(), out[4].Text(), out[5].Text(), out[7].Int()
		if !strings.HasPrefix(address, street+", "+city+", ") {
			t.Fatalf("id %d: the street and city columns don't belong to the address: %q, %q, %q", id, street, city, address)
		}
		if !strings.HasSuffix(address, " "+intText(zip)) {
			t.Fatalf("id %d: the number column's postcode doesn't belong to the address", id)
		}
	}
	if len(shifts) < 20 {
		t.Errorf("40 people got only %d different shifts: the key doesn't seed the shift", len(shifts))
	}
}

func daysBetween(a, b value.Value) int {
	ya, ma, da := a.Date()
	yb, mb, db := b.Date()
	return int(dateOf(yb, int(mb), db).Sub(dateOf(ya, int(ma), da)).Hours() / 24)
}

// jsonNumber stands for json.Number, the type of numbers in options read
// from YAML: a fmt.Stringer.
type jsonNumber string

func (n jsonNumber) String() string { return string(n) }

func TestDescribeUniqueness(t *testing.T) {
	tbl := &schema.Table{
		Columns: []*schema.Column{{Name: "a", Type: tText}, {Name: "b", Type: tText}, {Name: "c", Type: tText}, {Name: "d", Type: tText}},
		Uniques: []*schema.Key{{Columns: []schema.Ident{"a", "b"}}},
		Indexes: []*schema.Index{{Columns: []schema.Ident{"c"}, Unique: true, Partial: true}, {Columns: []schema.Ident{"d"}}},
	}
	caps := dialect.Capabilities{EmptyStringIsNull: true}
	cols := Describe(usersRef, tbl, caps)
	if len(cols) != len(tbl.Columns) {
		t.Fatalf("Describe returned %d columns for %d", len(cols), len(tbl.Columns))
	}
	for i, col := range cols {
		name := tbl.Columns[i].Name
		if col.Ref != (schema.ColumnRef{Table: usersRef, Column: name}) {
			t.Errorf("column %d describes %s", i, col.Ref)
		}
		if want := name != "d"; col.Unique != want {
			t.Errorf("%s: unique = %v", name, col.Unique)
		}
		if !col.EmptyStringIsNull {
			t.Error("EmptyStringIsNull is lost")
		}
	}
}

// TestUnseededFunctionsNeverUseRand masks with every function that is not
// Seeded, for every type its transformer accepts, without a random stream.
// One that read Context.Rand would panic here; in a RowMasker it would read
// the stream of the column before it.
func TestUnseededFunctionsNeverUseRand(t *testing.T) {
	ctx := &Context{Keys: testKeys(t), Domain: "test.domain"}
	ran := map[string]bool{}
	for _, tr := range Catalog() {
		for _, kind := range tr.Types {
			col := column(sampleType(kind))
			col.HasDefault = true
			f, err := Configure(col, Rule{Use: tr.Name, Options: sampleOptions(tr.Name, kind)})
			if err != nil || f.Seeded() || Omits(f) {
				continue
			}
			for _, in := range sampleValues(t, kind) {
				func() {
					defer func() {
						if r := recover(); r != nil {
							t.Errorf("%s (column type %s, value kind %s): it reads Context.Rand but is not Seeded (%v)", tr.Name, kind, in.Kind(), r)
						}
					}()
					_, _ = f.Mask(ctx, in) // an error is fine here; reading Rand is not
				}()
			}
			ran[tr.Name] = true
		}
	}
	for _, name := range []string{"constant", "fpe", "keep", "null", "password_hash", "redact", "uuid"} {
		if !ran[name] {
			t.Errorf("%s was not tried: it is no longer unseeded, or the samples don't fit it", name)
		}
	}
}

// sampleType returns a column type of each kind.
func sampleType(k value.TypeKind) value.Type {
	switch k {
	case value.TypeInt:
		return tInt64
	case value.TypeFloat:
		return tFloat
	case value.TypeDecimal:
		return tDecimal0
	case value.TypeString:
		return tString(100)
	case value.TypeTime:
		return value.Type{Kind: value.TypeTime, Precision: 6}
	case value.TypeTimestamp:
		return tTimestamp
	case value.TypeEnum:
		return value.Type{Kind: value.TypeEnum, Values: []string{"a", "b"}}
	case value.TypeArray:
		return value.Type{Kind: value.TypeArray, Elem: &tText}
	case value.TypeOpaque:
		return value.Type{Kind: value.TypeOpaque, Native: "point"}
	default:
		return value.Type{Kind: k}
	}
}

// sampleOptions returns the options a transformer needs for a column kind.
func sampleOptions(use string, k value.TypeKind) map[string]any {
	if use != "constant" {
		return nil
	}
	v := map[value.TypeKind]string{
		value.TypeBool: "true", value.TypeInt: "7", value.TypeFloat: "1.5", value.TypeDecimal: "7",
		value.TypeEnum: "a", value.TypeJSON: "{}", value.TypeUUID: "123e4567-e89b-42d3-a456-426614174000",
		value.TypeDate: "2026-01-31", value.TypeTime: "13:45:00", value.TypeTimestamp: "2026-01-31T13:45:00",
	}[k]
	return map[string]any{"value": v}
}

// sampleValues returns values a column of the kind holds.
func sampleValues(t *testing.T, k value.TypeKind) []value.Value {
	t.Helper()
	switch k {
	case value.TypeBool:
		return []value.Value{value.Bool(true)}
	case value.TypeInt:
		return []value.Value{value.Int(1234567), value.Int(-7)}
	case value.TypeFloat:
		return []value.Value{value.Float(1.5)}
	case value.TypeDecimal:
		return []value.Value{mustDecimal(t, "1234567"), mustDecimal(t, "1.5")}
	case value.TypeString, value.TypeText:
		return []value.Value{value.String("Order-123456"), value.String("123e4567-e89b-42d3-a456-426614174000")}
	case value.TypeBinary, value.TypeOpaque:
		return []value.Value{value.Bytes([]byte("0123456789abcdef"))}
	case value.TypeDate:
		d, _ := value.Date(2026, 1, 31)
		return []value.Value{d}
	case value.TypeTime:
		v, _ := value.TimeOfDay(time.Hour)
		return []value.Value{v}
	case value.TypeTimestamp:
		return []value.Value{value.Timestamp(time.Date(2026, 1, 31, 13, 45, 0, 0, time.UTC))}
	case value.TypeInterval:
		return []value.Value{value.Interval(1, 2, 3)}
	case value.TypeUUID:
		u, _ := value.ParseUUID("123e4567-e89b-42d3-a456-426614174000")
		return []value.Value{u}
	case value.TypeJSON:
		return []value.Value{value.String(`{"a":1}`)}
	case value.TypeEnum:
		return []value.Value{value.String("a")}
	case value.TypeArray:
		return []value.Value{value.Array([]value.Value{value.String("a")})}
	default:
		t.Fatalf("no sample values for %s", k)
		return nil
	}
}

// BenchmarkMaskRow measures masking one row of four columns, with a keyed
// hash per seeded value.
func BenchmarkMaskRow(b *testing.B) {
	users, _ := shop()
	tbl, err := NewTable(usersRef, users, usersRules(), dialect.Capabilities{})
	if err != nil {
		b.Fatal(err)
	}
	m := tbl.Bind(testKeys(b))
	born, _ := value.Date(1990, 3, 15)
	src := []value.Value{value.Int(7), value.String("someone@example.test"), born, born}
	dst := make([]value.Value, len(src))
	for b.Loop() {
		if err := m.Mask(dst, src); err != nil {
			b.Fatal(err)
		}
	}
}

func dateOf(y, m, d int) time.Time { return time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC) }
