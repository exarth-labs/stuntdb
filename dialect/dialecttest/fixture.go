package dialecttest

import (
	"time"

	"github.com/exarth-labs/stuntdb/internal/schema"
	"github.com/exarth-labs/stuntdb/internal/value"
)

var (
	intType  = value.Type{Kind: value.TypeInt, Bits: 64}
	textType = value.Type{Kind: value.TypeText}
)

// shop is the suite's sample database: 12 users, 20 orders and 30 order
// items, with a NOT NULL foreign key, a composite primary key and nullable
// text. It uses only types that every engine stores exactly. The data is
// generated, never real.
func shop(ns schema.Ident) (*schema.Database, map[schema.TableRef][][]value.Value) {
	usersRef := schema.TableRef{Namespace: ns, Name: "users"}
	ordersRef := schema.TableRef{Namespace: ns, Name: "orders"}
	itemsRef := schema.TableRef{Namespace: ns, Name: "order_items"}

	db := &schema.Database{Namespaces: []*schema.Namespace{{
		Name: ns,
		Tables: []*schema.Table{
			{
				Name: "users",
				Columns: []*schema.Column{
					{Name: "id", Type: intType},
					{Name: "email", Type: textType},
					{Name: "nickname", Type: textType, Nullable: true},
				},
				PrimaryKey: &schema.Key{Columns: []schema.Ident{"id"}},
			},
			{
				Name: "orders",
				Columns: []*schema.Column{
					{Name: "id", Type: intType},
					{Name: "user_id", Type: intType},
					{Name: "note", Type: textType, Nullable: true},
				},
				PrimaryKey: &schema.Key{Columns: []schema.Ident{"id"}},
				ForeignKeys: []*schema.ForeignKey{{
					Name: "orders_user", Columns: []schema.Ident{"user_id"},
					Ref: usersRef, RefColumns: []schema.Ident{"id"},
				}},
			},
			{
				Name: "order_items",
				Columns: []*schema.Column{
					{Name: "order_id", Type: intType},
					{Name: "line", Type: intType},
					{Name: "quantity", Type: intType},
				},
				PrimaryKey: &schema.Key{Columns: []schema.Ident{"order_id", "line"}},
				ForeignKeys: []*schema.ForeignKey{{
					Name: "items_order", Columns: []schema.Ident{"order_id"},
					Ref: ordersRef, RefColumns: []schema.Ident{"id"}, OnDelete: schema.Cascade,
				}},
			},
		},
	}}}

	data := map[schema.TableRef][][]value.Value{}
	for id := int64(1); id <= 12; id++ {
		nickname := value.Null()
		if id%3 != 0 {
			nickname = value.String("user " + string(rune('a'+id-1)))
		}
		data[usersRef] = append(data[usersRef], []value.Value{
			value.Int(id), value.String("user" + string(rune('a'+id-1)) + "@example.com"), nickname,
		})
	}
	for id := int64(1); id <= 20; id++ {
		note := value.Null()
		if id%2 == 0 {
			note = value.String("gift wrap")
		}
		data[ordersRef] = append(data[ordersRef], []value.Value{value.Int(id), value.Int((id-1)%12 + 1), note})
	}
	for order := int64(1); order <= 15; order++ {
		for line := int64(1); line <= 2; line++ {
			data[itemsRef] = append(data[itemsRef], []value.Value{value.Int(order), value.Int(line), value.Int(order * line)})
		}
	}
	return db, data
}

// keyed is a table with one NOT NULL unique key of each common type, for
// reading in chunks along each of them. The values sort differently as text
// and as what they are: "10" before "9", 10.00 after 9.99, and dates and
// timestamps across months, years and microseconds.
func keyed(ns schema.Ident) (*schema.Database, schema.TableRef, [][]value.Value) {
	ref := schema.TableRef{Namespace: ns, Name: "keyed"}
	db := &schema.Database{Namespaces: []*schema.Namespace{{
		Name: ns,
		Tables: []*schema.Table{{
			Name: "keyed",
			Columns: []*schema.Column{
				{Name: "id", Type: intType},
				{Name: "code", Type: value.Type{Kind: value.TypeString, MaxLen: 20}},
				{Name: "amount", Type: value.Type{Kind: value.TypeDecimal, Precision: 12, Scale: 2}},
				{Name: "day", Type: value.Type{Kind: value.TypeDate}},
				{Name: "at", Type: value.Type{Kind: value.TypeTimestamp, Precision: 6}},
			},
			PrimaryKey: &schema.Key{Columns: []schema.Ident{"id"}},
			Uniques: []*schema.Key{
				{Columns: []schema.Ident{"code"}},
				{Columns: []schema.Ident{"amount"}},
				{Columns: []schema.Ident{"day"}},
				{Columns: []schema.Ident{"at"}},
			},
		}},
	}}}
	codes := []string{"10", "9", "B", "a", "a1", "Z", "m", "_"}
	amounts := []string{"-1.50", "0.00", "0.10", "2.25", "10.00", "9.99", "100.00", "-0.01"}
	days := [][3]int{{2024, 1, 2}, {2023, 12, 31}, {2024, 2, 29}, {1999, 1, 1}, {2024, 10, 1}, {2024, 9, 30}, {2000, 1, 1}, {2024, 1, 10}}
	base := time.Date(2024, 1, 2, 10, 0, 0, 0, time.UTC)
	offsets := []time.Duration{0, time.Microsecond, time.Hour, -time.Hour, 24 * time.Hour, 31 * 24 * time.Hour, -366 * 24 * time.Hour, 2 * time.Microsecond}
	var rows [][]value.Value
	for i := range codes {
		amount, err := value.Decimal(amounts[i])
		if err != nil {
			panic(err)
		}
		day, err := value.Date(days[i][0], time.Month(days[i][1]), days[i][2])
		if err != nil {
			panic(err)
		}
		rows = append(rows, []value.Value{
			value.Int(int64(i + 1)), value.String(codes[i]), amount, day, value.Timestamp(base.Add(offsets[i])),
		})
	}
	return db, ref, rows
}

// canaryDB is the database testErrorsHideValues breaks: people with a text
// primary key, a NOT NULL unique email and a short code, and notes that
// must reference a person.
func canaryDB(ns schema.Ident) (db *schema.Database, people, notes schema.TableRef) {
	people = schema.TableRef{Namespace: ns, Name: "people"}
	notes = schema.TableRef{Namespace: ns, Name: "notes"}
	idType := value.Type{Kind: value.TypeString, MaxLen: 40}
	db = &schema.Database{Namespaces: []*schema.Namespace{{
		Name: ns,
		Tables: []*schema.Table{
			{
				Name: "people",
				Columns: []*schema.Column{
					{Name: "id", Type: idType},
					{Name: "email", Type: textType},
					{Name: "code", Type: value.Type{Kind: value.TypeString, MaxLen: 8}, Nullable: true},
				},
				PrimaryKey: &schema.Key{Columns: []schema.Ident{"id"}},
				Uniques:    []*schema.Key{{Columns: []schema.Ident{"email"}}},
			},
			{
				Name: "notes",
				Columns: []*schema.Column{
					{Name: "id", Type: intType},
					{Name: "person_id", Type: idType},
				},
				PrimaryKey: &schema.Key{Columns: []schema.Ident{"id"}},
				ForeignKeys: []*schema.ForeignKey{{
					Name: "notes_person", Columns: []schema.Ident{"person_id"},
					Ref: people, RefColumns: []schema.Ident{"id"},
				}},
			},
		},
	}}}
	return db, people, notes
}
