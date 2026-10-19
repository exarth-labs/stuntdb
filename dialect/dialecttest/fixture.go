package dialecttest

import (
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
