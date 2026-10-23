package classify

import (
	"errors"
	"flag"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"text/tabwriter"

	"github.com/exarth-labs/stuntdb/internal/errs"
	"github.com/exarth-labs/stuntdb/internal/schema"
	"github.com/exarth-labs/stuntdb/internal/value"
	"github.com/exarth-labs/stuntdb/rules"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

func defaultClassifier(t *testing.T) *Classifier {
	t.Helper()
	c, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// typeOf returns a column type by its kind's name.
func typeOf(t testing.TB, kind string) value.Type {
	t.Helper()
	k, ok := typeKinds[kind]
	if !ok {
		t.Fatalf("unknown type %q", kind)
	}
	typ := value.Type{Kind: k}
	switch k {
	case value.TypeInt, value.TypeFloat:
		typ.Bits = 64
	case value.TypeOpaque:
		typ.Native = "inet"
	}
	return typ
}

// TestColumnNames checks the built-in rules against the names real
// applications use, including those that must not match.
func TestColumnNames(t *testing.T) {
	tests := []struct {
		table, column, kind string
		want                string // the rule, or empty for none
	}{
		// Emails, in every spelling.
		{"users", "email", "string", "core.email"},
		{"users", "Email", "text", "core.email"},
		{"users", "EMAIL_ADDRESS", "string", "core.email"},
		{"users", "EmailAddress", "string", "core.email"},
		{"users", "emailaddress", "string", "core.email"},
		{"users", "e_mail", "string", "core.email"},
		{"users", "eMail", "string", "core.email"},
		{"users", "customer_email", "string", "core.email"},
		{"users", "emails", "string", "core.email"},
		{"users", "email2", "string", "core.email"},
		{"users", "correo_electrónico", "string", "core.email"},
		{"users", "email_verified", "bool", ""},
		{"users", "email_verified_at", "timestamp", ""},
		{"campaigns", "email_subject", "string", ""},
		{"campaigns", "email_template_id", "int", ""},

		// Phones, also stored as numbers.
		{"users", "phone_number", "string", "core.phone"},
		{"users", "PhoneNumber", "string", "core.phone"},
		{"users", "mobile", "int", "core.phone"},
		{"users", "phone1", "string", "core.phone"},
		{"users", "Fax", "string", "core.phone"},
		{"users", "is_mobile", "bool", ""},
		{"rooms", "hotel", "string", ""},

		// Names. A bare "name" is a person's only in a table of people.
		{"users", "FirstName", "string", "core.first_name"},
		{"users", "FIRSTNAME", "string", "core.first_name"},
		{"users", "prénom", "string", "core.first_name"},
		{"users", "LastName", "string", "core.last_name"},
		{"users", "surname", "string", "core.last_name"},
		{"users", "full_name", "string", "core.full_name"},
		{"customers", "name", "string", "core.person_name"},
		{"tbl_customers", "Name", "string", "core.person_name"},
		{"AspNetUsers", "Name", "string", "core.person_name"},
		{"Artist", "Name", "string", "core.person_name"},
		{"genres", "name", "string", ""},
		{"products", "product_name", "string", ""},
		{"files", "file_name", "string", ""},
		{"users", "username", "string", "core.username"},
		{"users", "user_name", "string", "core.username"},
		{"companies", "company_name", "string", "core.company"},

		// Addresses: the longest match wins.
		{"orders", "billing_address", "string", "core.street_address"},
		{"users", "address_line_2", "string", "core.street_address"},
		{"users", "Address1", "string", "core.street_address"},
		{"logins", "ip_address", "string", "core.ip_address"},
		{"logins", "remote_addr", "string", "core.ip_address"},
		{"logins", "last_sign_in_ip", "opaque", "core.ip_address"},
		{"devices", "mac_address", "string", "core.device_id"},
		{"users", "City", "string", "core.city"},
		{"users", "PostalCode", "string", "core.postcode"},
		{"users", "zip", "int", "core.postcode"},
		{"Invoice", "BillingState", "string", "core.region"},
		{"users", "State", "string", "core.state"},
		{"addresses", "state", "string", "core.state"},
		{"orders", "state", "string", ""},
		{"users", "Country", "string", "core.country"},
		{"users", "country_id", "int", ""},

		// Dates: birth dates are personal, record timestamps are safe.
		{"users", "BirthDate", "timestamp", "core.birth_date"},
		{"users", "dob", "date", "core.birth_date"},
		{"users", "date_of_birth", "string", "core.birth_date"},
		{"users", "created_at", "timestamp", "core.record_timestamp"},
		{"users", "updated_at", "int", "core.record_timestamp"},
		{"users", "created_at", "text", ""},
		{"users", "last_login", "timestamp", ""},
		{"users", "lock_version", "int", "core.lock_version"},

		// Identifiers and money.
		{"users", "ssn", "string", "core.government_id"},
		{"users", "tax_id", "int", "core.government_id"},
		{"users", "TaxID", "string", "core.government_id"},
		{"users", "passport_no", "string", "core.government_id"},
		{"payments", "card_number", "string", "core.card_number"},
		{"payments", "iban", "string", "core.bank_account"},
		{"employees", "salary", "decimal", "core.salary"},
		{"employees", "salary", "string", ""},
		{"patients", "diagnosis", "text", "core.health"},
		{"tickets", "notes", "text", "core.free_text"},
		{"tickets", "admin_notes", "text", "core.free_text"},
		{"posts", "comment_count", "int", ""},
		{"users", "avatar", "binary", "core.photo"},

		// Secrets.
		{"users", "password", "string", "secrets.password"},
		{"users", "encrypted_password", "string", "secrets.password"},
		{"users", "password_digest", "string", "secrets.password"},
		{"users", "password_hint", "string", ""},
		{"users", "password_changed_at", "timestamp", ""},
		{"users", "reset_password_token", "string", "secrets.token"},
		{"users", "remember_token", "uuid", "secrets.token"},
		{"users", "tokens", "json", "secrets.token"},
		{"users", "token_count", "int", ""},
		{"oauth", "token_type", "string", ""},
		{"accounts", "api_key", "string", "secrets.api_key"},
		{"accounts", "apiKey", "string", "secrets.api_key"},
		{"accounts", "stripe_secret_key", "string", "secrets.api_key"},
		{"accounts", "api_key_name", "string", ""},
		{"apps", "client_secret", "string", "secrets.secret"},
		{"hooks", "webhook_secret", "string", "secrets.secret"},
		{"users", "otp_secret", "string", "secrets.secret"},
		{"users", "secret_question", "string", ""},
		{"users", "is_secret", "bool", ""},
		{"users", "otp", "int", "secrets.one_time_code"},
		{"cards", "cvv2", "string", "secrets.one_time_code"},
		{"django_session", "session_key", "string", "secrets.session"},
		{"events", "session_id", "int", ""},
		{"settings", "database_url", "string", "secrets.credentials"},

		// Columns no rule covers are left for review.
		{"invoices", "total", "decimal", ""},
		{"tracks", "composer", "string", ""},
		{"", "", "string", ""},
	}
	c := defaultClassifier(t)
	for _, tt := range tests {
		col := &schema.Column{Name: schema.Ident(tt.column), Type: typeOf(t, tt.kind)}
		m := c.column(trimNumbers(words(tt.table)), col)
		if m.Rule != tt.want {
			t.Errorf("%s.%s (%s) matches %q, want %q", tt.table, tt.column, tt.kind, m.Rule, tt.want)
		}
	}
}

// TestPrecedence checks which rule wins when several match, with packs
// made for it: personal data beats safe, then the longest name, then the
// more protective class, then the higher confidence, then the earlier pack.
func TestPrecedence(t *testing.T) {
	pack := func(name string, rs ...string) rules.Pack {
		return rules.Pack{Name: name, Data: []byte("version: 1\npack: " + name + "\nrules:\n  - " + strings.Join(rs, "\n  - ") + "\n")}
	}
	rule := func(id, class, use, confidence, names string) string {
		return fmt.Sprintf("{id: %s, class: %s, use: %s, confidence: %s, types: [string], names: [%s]}", id, class, use, confidence, names)
	}
	c, err := New(
		pack("first",
			rule("safe_long", "safe", "keep", "0.9", "user email"),
			rule("short", "direct", "email", "0.9", "email"),
			rule("quasi_code", "quasi", "keep", "0.9", "code"),
			rule("low", "direct", "hash", "0.5", "number"),
			rule("tie", "direct", "hash", "0.7", "serial"),
		),
		pack("second",
			rule("secret_code", "secret", "redact", "0.5", "code"),
			rule("high", "direct", "hash", "0.9", "number"),
			rule("tie", "direct", "hash", "0.7", "serial"),
			rule("long", "quasi", "keep", "0.5", "other user email"),
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct{ column, want string }{
		{"user_email", "first.short"},       // personal beats safe, even with fewer words
		{"other_user_email", "second.long"}, // three words beat one
		{"code", "second.secret_code"},      // the more protective class
		{"number", "second.high"},           // the higher confidence
		{"serial", "first.tie"},             // otherwise the earlier pack
	}
	for _, tt := range tests {
		m := c.column(nil, &schema.Column{Name: schema.Ident(tt.column), Type: typeOf(t, "string")})
		if m.Rule != tt.want {
			t.Errorf("%s matches %q, want %q", tt.column, m.Rule, tt.want)
		}
	}
}

func ref(table, column string) schema.ColumnRef {
	ns, name, _ := strings.Cut(table, ".")
	return schema.ColumnRef{Table: schema.TableRef{Namespace: schema.Ident(ns), Name: schema.Ident(name)}, Column: schema.Ident(column)}
}

// TestReferences checks how foreign keys take the classification of the
// columns they reference.
func TestReferences(t *testing.T) {
	db := parseSchema(t, `
table main.people
  id         int pk identity
  email      string
  code       string
table main.orders
  id            int pk identity
  person_id     int ->main.people.id
  buyer         string ->main.people.email
  buyer_email   string ->main.people.email
  person_code   string ->main.people.code
  created_at    timestamp ->main.people.code
  phone         int ->main.people.id
  secret        string ->main.people.email
  maybe_person  int virtual->main.people.id
  missing       int ->main.nowhere.id
table main.lines
  order_id   int ->main.orders.id
  person_id  int ->main.orders.person_id
table main.a
  id  string pk ->main.b.id
table main.b
  id  string pk ->main.a.id
`)
	got := defaultClassifier(t).Database(db)
	tests := []struct {
		column      schema.ColumnRef
		rule        string
		via         string
		explanation string
	}{
		{ref("main.orders", "person_id"), "core.surrogate_key", "main.people.id", "a reference to a key the database numbers is safe"},
		{ref("main.orders", "buyer"), "core.email", "main.people.email", "a reference to an email is an email"},
		{ref("main.orders", "buyer_email"), "core.email", "main.people.email", "a tie goes to the referenced column, so both ends are masked the same way"},
		{ref("main.orders", "person_code"), "", "", "a reference to an unclassified column is unclassified"},
		{ref("main.orders", "created_at"), "", "", "even when its own name looks safe"},
		{ref("main.orders", "phone"), "core.phone", "", "its own name is more protective than a safe key"},
		{ref("main.orders", "secret"), "secrets.secret", "", "a secret stays a secret"},
		{ref("main.orders", "maybe_person"), "", "", "a virtual foreign key is not enforced, so it passes nothing on"},
		{ref("main.orders", "missing"), "", "", "a reference to a missing table is ignored"},
		{ref("main.lines", "order_id"), "core.surrogate_key", "main.orders.id", ""},
		{ref("main.lines", "person_id"), "core.surrogate_key", "main.orders.person_id", "references are followed through other references"},
		{ref("main.a", "id"), "", "", "a cycle of references ends"},
	}
	for _, tt := range tests {
		m := got[tt.column]
		via := ""
		if m.Via != nil {
			via = m.Via.String()
		}
		if m.Rule != tt.rule || via != tt.via {
			t.Errorf("%s: rule %q via %q, want rule %q via %q (%s)", tt.column, m.Rule, via, tt.rule, tt.via, tt.explanation)
		}
	}
	if len(got) != 17 {
		t.Errorf("%d columns classified, want 17", len(got))
	}
}

func TestCompositeReferences(t *testing.T) {
	db := &schema.Database{Namespaces: []*schema.Namespace{{
		Name: "main",
		Tables: []*schema.Table{
			{Name: "accounts", Columns: []*schema.Column{
				{Name: "region", Type: typeOf(t, "string")},
				{Name: "email", Type: typeOf(t, "string")},
			}, Uniques: []*schema.Key{{Columns: []schema.Ident{"region", "email"}}}},
			{Name: "logins", Columns: []*schema.Column{
				{Name: "account_region", Type: typeOf(t, "string")},
				{Name: "account_ref", Type: typeOf(t, "string")},
			}, ForeignKeys: []*schema.ForeignKey{{
				Columns:    []schema.Ident{"account_region", "account_ref"},
				Ref:        schema.TableRef{Namespace: "main", Name: "accounts"},
				RefColumns: []schema.Ident{"region", "email"},
			}}},
		},
	}}}
	got := defaultClassifier(t).Database(db)
	if m := got[ref("main.logins", "account_ref")]; m.Rule != "core.email" || m.Via == nil || *m.Via != ref("main.accounts", "email") {
		t.Errorf("account_ref = %+v, want the email it references, by position", m)
	}
	if m := got[ref("main.logins", "account_region")]; m.Class != Unclassified {
		t.Errorf("account_region = %+v, want unclassified", m)
	}
}

func TestMatchesAreIndependent(t *testing.T) {
	db := parseSchema(t, `
table main.users
  city  string
table main.orders
  city  string ->main.users.city
`)
	got := defaultClassifier(t).Database(db)
	got[ref("main.users", "city")].Options["part"] = "changed"
	if part := got[ref("main.orders", "city")].Options["part"]; part != "city" {
		t.Errorf("changing one match's options changed another's: part = %q", part)
	}
	again := defaultClassifier(t).Database(db)
	if part := again[ref("main.users", "city")].Options["part"]; part != "city" {
		t.Errorf("changing a match's options changed the rule: part = %q", part)
	}
}

func TestAllows(t *testing.T) {
	secret := Match{Class: Secret, Rule: "secrets.token", Use: "random_token"}
	err := secret.Allows("keep")
	if !errors.Is(err, ErrKeepSecret) || errs.KindOf(err) != errs.CheckFailed {
		t.Fatalf("Allows(keep) on a secret = %v, want ErrKeepSecret with CHECK_FAILED", err)
	}
	for _, want := range []string{"secrets.token", "random_token"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %s: %v", want, err)
		}
	}
	for _, use := range []string{"random_token", "null", "redact", "hash"} {
		if err := secret.Allows(use); err != nil {
			t.Errorf("Allows(%s) on a secret = %v", use, err)
		}
	}
	for _, m := range []Match{{}, {Class: Direct, Rule: "core.email"}, {Class: Safe, Rule: "core.surrogate_key"}} {
		if err := m.Allows("keep"); err != nil {
			t.Errorf("Allows(keep) on %s = %v", m.Class, err)
		}
	}
}

func TestSource(t *testing.T) {
	via := ref("main.users", "id")
	tests := []struct {
		m    Match
		want string
	}{
		{Match{}, ""},
		{Match{Class: Direct, Rule: "core.email"}, "rule core.email"},
		{Match{Class: Safe, Rule: "core.surrogate_key", Via: &via}, "rule core.surrogate_key, references main.users.id"},
	}
	for _, tt := range tests {
		if got := tt.m.Source(); got != tt.want {
			t.Errorf("Source() = %q, want %q", got, tt.want)
		}
	}
}

func TestClassNames(t *testing.T) {
	for c := Unclassified; c <= Safe; c++ {
		parsed, ok := parseClass(c.String())
		if !ok || parsed != c {
			t.Errorf("parseClass(%q) = %v, %t", c, parsed, ok)
		}
	}
	if Class(99).String() != "invalid" {
		t.Error("an unknown class must print as invalid")
	}
	if _, ok := parseClass("personal"); ok {
		t.Error("parseClass accepts an unknown class")
	}
}

func TestProtectionOrder(t *testing.T) {
	order := []Class{Safe, Unclassified, FreeText, Quasi, Sensitive, Direct, Secret}
	for i := 1; i < len(order); i++ {
		if order[i-1].protection() >= order[i].protection() {
			t.Errorf("%s must call for less protection than %s", order[i-1], order[i])
		}
	}
}

// TestGolden classifies the schemas of real applications (Chinook, Rails
// with Devise, Django, Laravel) and some tricky shapes, and compares the
// result with testdata/applications.golden. A change to the rule packs
// shows up there as a diff to review. Run "go test -update" to rewrite it.
func TestGolden(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("testdata", "applications.schema"))
	if err != nil {
		t.Fatal(err)
	}
	db := parseSchema(t, string(src))
	if err := db.Validate(); err != nil {
		t.Fatalf("the test schema is not valid: %v", err)
	}
	got := report(db, defaultClassifier(t).Database(db))

	golden := filepath.Join("testdata", "applications.golden")
	if *update {
		if err := os.WriteFile(golden, got, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (run go test -update to create it)", err)
	}
	if string(got) != strings.ReplaceAll(string(want), "\r\n", "\n") {
		t.Errorf("the classification changed; review the diff, then run go test -update:\n%s", got)
	}
}

// report writes one line per column, in the database's order.
func report(db *schema.Database, matches map[schema.ColumnRef]Match) []byte {
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "COLUMN\tCLASS\tSUGGESTION\tCONFIDENCE\tSOURCE")
	for _, ns := range db.Namespaces {
		for _, t := range ns.Tables {
			for _, c := range t.Columns {
				r := schema.ColumnRef{Table: schema.TableRef{Namespace: ns.Name, Name: t.Name}, Column: c.Name}
				m := matches[r]
				if m.Class == Unclassified {
					fmt.Fprintf(w, "%s\t%s\t-\t-\t-\n", r, m.Class)
					continue
				}
				use := m.Use
				for _, k := range slices.Sorted(maps.Keys(m.Options)) {
					use += " " + k + "=" + m.Options[k]
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%.2f\t%s\n", r, m.Class, use, m.Confidence, m.Source())
			}
		}
	}
	_ = w.Flush()
	return []byte(b.String())
}

// parseSchema reads a schema written as text, one table at a time:
//
//	table namespace.name
//	  column type [pk] [identity] [uuid] [->ns.table.column] [virtual->ns.table.column]
//
// uuid gives the column a generated UUID default. A # starts a comment.
func parseSchema(t testing.TB, src string) *schema.Database {
	t.Helper()
	db := &schema.Database{Engine: "test"}
	var table *schema.Table
	for n, line := range strings.Split(src, "\n") {
		line, _, _ = strings.Cut(line, "#")
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if fields[0] == "table" {
			ns, name, _ := strings.Cut(fields[1], ".")
			nsp := db.Namespace(schema.Ident(ns))
			if nsp == nil {
				nsp = &schema.Namespace{Name: schema.Ident(ns)}
				db.Namespaces = append(db.Namespaces, nsp)
			}
			table = &schema.Table{Name: schema.Ident(name), EstimatedRows: -1}
			nsp.Tables = append(nsp.Tables, table)
			continue
		}
		if table == nil || len(fields) < 2 {
			t.Fatalf("line %d: %q is not a column", n+1, line)
		}
		col := &schema.Column{Name: schema.Ident(fields[0]), Type: typeOf(t, fields[1]), NativeType: fields[1], Nullable: true}
		for _, flag := range fields[2:] {
			switch {
			case flag == "pk":
				if table.PrimaryKey == nil {
					table.PrimaryKey = &schema.Key{}
				}
				table.PrimaryKey.Columns = append(table.PrimaryKey.Columns, col.Name)
				col.Nullable = false
			case flag == "identity":
				col.Identity = &schema.Identity{}
			case flag == "uuid":
				col.Default = &schema.Default{Kind: schema.DefaultUUID, Expr: "uuid()"}
			case strings.Contains(flag, "->"):
				virtual, target, _ := strings.Cut(flag, "->")
				r := ref(target[:strings.LastIndex(target, ".")], target[strings.LastIndex(target, ".")+1:])
				table.ForeignKeys = append(table.ForeignKeys, &schema.ForeignKey{
					Columns: []schema.Ident{col.Name}, Ref: r.Table, RefColumns: []schema.Ident{r.Column}, Virtual: virtual == "virtual",
				})
			default:
				t.Fatalf("line %d: unknown flag %q", n+1, flag)
			}
		}
		table.Columns = append(table.Columns, col)
	}
	return db
}
