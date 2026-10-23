package app

import (
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/exarth-labs/stuntdb/dialect"
	"github.com/exarth-labs/stuntdb/internal/config"
	"github.com/exarth-labs/stuntdb/internal/errs"
	"github.com/exarth-labs/stuntdb/internal/fakedb"
	"github.com/exarth-labs/stuntdb/internal/redact"
	"github.com/exarth-labs/stuntdb/internal/schema"
	"github.com/exarth-labs/stuntdb/internal/value"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata")

var (
	intType  = value.Type{Kind: value.TypeInt, Bits: 64}
	textType = value.Type{Kind: value.TypeText}
	strType  = value.Type{Kind: value.TypeString, MaxLen: 200}
	timeType = value.Type{Kind: value.TypeTimestamp, Precision: 6}
	decType  = value.Type{Kind: value.TypeDecimal, Precision: 10, Scale: 2}
)

// shop is a small database in the style of the fake engine, which writes
// names in upper case, as Oracle does. It has the columns every kind of
// decision needs: a key the database assigns, personal data, a secret, a
// generated column, free text, a column no rule knows, record timestamps,
// and foreign keys to a surrogate key and to a natural key.
func shop() *schema.Database {
	return &schema.Database{Namespaces: []*schema.Namespace{{Name: "APP", Tables: []*schema.Table{
		{
			Name: "CUSTOMERS",
			Columns: []*schema.Column{
				{Name: "ID", Type: intType, Identity: &schema.Identity{}},
				{Name: "EMAIL", Type: strType},
				{Name: "FIRST_NAME", Type: strType, Nullable: true},
				{Name: "API_TOKEN", Type: strType, Nullable: true},
				{Name: "NOTES", Type: textType, Nullable: true},
				{Name: "TIER", Type: strType},
				{Name: "SEARCH_KEY", Type: strType, Nullable: true, Generated: &schema.Generated{Expr: "lower(email)", Stored: true}},
				{Name: "CREATED_AT", Type: timeType},
			},
			PrimaryKey: &schema.Key{Columns: []schema.Ident{"ID"}},
			Uniques:    []*schema.Key{{Columns: []schema.Ident{"EMAIL"}}},
		},
		{
			Name: "ORDERS",
			Columns: []*schema.Column{
				{Name: "ID", Type: intType, Identity: &schema.Identity{}},
				{Name: "CUSTOMER_ID", Type: intType},
				{Name: "BILLING_EMAIL", Type: strType},
				{Name: "TOTAL", Type: decType},
			},
			PrimaryKey: &schema.Key{Columns: []schema.Ident{"ID"}},
			ForeignKeys: []*schema.ForeignKey{
				{Columns: []schema.Ident{"CUSTOMER_ID"}, Ref: schema.TableRef{Namespace: "APP", Name: "CUSTOMERS"}, RefColumns: []schema.Ident{"ID"}},
				{Columns: []schema.Ident{"BILLING_EMAIL"}, Ref: schema.TableRef{Namespace: "APP", Name: "CUSTOMERS"}, RefColumns: []schema.Ident{"EMAIL"}},
			},
		},
	}}}}
}

// shopRules are the rules a reviewer writes for shop: every column that
// needs a decision has one.
const shopRules = `
columns:
  app.customers:
    email: email
    first_name: first_name
    api_token: random_token
    notes: synthesize_text
    tier: keep
  app.orders:
    total: keep
`

// fixture is a project folder with a configuration, and a fake engine
// holding a database.
type fixture struct {
	dir    string
	engine *fakedb.Dialect
	env    map[string]string
	redact *redact.Redactor
	stub   *stubDialect // when set, used instead of the fake engine
}

func newFixture(t *testing.T, db *schema.Database) *fixture {
	t.Helper()
	f := &fixture{dir: t.TempDir(), engine: fakedb.New(), env: map[string]string{}, redact: redact.New()}
	f.engine.Create("shop", db)
	return f
}

func (f *fixture) Env() Env {
	return Env{
		Lookup: func(url string) (dialect.Dialect, error) {
			if !strings.HasPrefix(url, "fake:") {
				return nil, dialect.ErrUnknownScheme
			}
			if f.stub != nil {
				return f.stub, nil
			}
			return f.engine, nil
		},
		LookupEnv: func(name string) (string, bool) {
			v, ok := f.env[name]
			return v, ok
		},
		UserConfig: "-",
		Redactor:   f.redact,
	}
}

func (f *fixture) path(name string) string { return filepath.Join(f.dir, name) }

func (f *fixture) write(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(f.path(name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) read(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(f.path(name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func (f *fixture) check(t *testing.T, update bool) (*CheckResult, error) {
	t.Helper()
	return Check(context.Background(), CheckRequest{Env: f.Env(), ConfigPath: f.path(config.DefaultPath), Update: update})
}

// stubDialect is an adapter that fails in a chosen way: it cannot connect,
// cannot read the structure, or reads db.
type stubDialect struct {
	openErr       error
	introspectErr error
	db            *schema.Database
}

func (*stubDialect) Name() string      { return "stub" }
func (*stubDialect) Schemes() []string { return []string{"fake"} }
func (s *stubDialect) Open(context.Context, string, dialect.OpenOptions) (dialect.Conn, error) {
	if s.openErr != nil {
		return nil, s.openErr
	}
	return stubConn{s: s}, nil
}

// stubConn implements only what reading a source's structure calls; the
// embedded interface is nil, so anything else panics.
type stubConn struct {
	dialect.Conn
	s *stubDialect
}

func (c stubConn) Introspect(context.Context, dialect.Filter) (*schema.Database, error) {
	return c.s.db, c.s.introspectErr
}
func (stubConn) Capabilities() dialect.Capabilities { return dialect.Capabilities{} }
func (stubConn) Close() error                       { return nil }

// problemText joins the problems, one per line.
func problemText(ps []Problem) string {
	lines := make([]string, len(ps))
	for i, p := range ps {
		lines[i] = p.String()
	}
	return strings.Join(lines, "\n")
}

func TestProblemString(t *testing.T) {
	if got := (Problem{At: "stuntdb.yaml:3:5", Message: "bad"}).String(); got != "stuntdb.yaml:3:5: bad" {
		t.Errorf("String() = %q", got)
	}
	if got := (Problem{Message: "bad"}).String(); got != "bad" {
		t.Errorf("String() = %q", got)
	}
}

func TestSourceErrors(t *testing.T) {
	const fake = "version: 1\nsource:\n  url: fake:shop\n"
	invalid := shop()
	invalid.Namespaces[0].Tables[1].ForeignKeys[0].RefColumns = nil // references no column
	tests := []struct {
		name    string
		config  string
		stub    *stubDialect
		want    errs.Kind
		is      error
		message string
	}{
		{"no source", "version: 1\n", nil, errs.Usage, ErrNoSource, "set STUNTDB_SOURCE_URL"},
		{"unset variable", "version: 1\nsource:\n  url: ${NOPE}\n", nil, errs.Usage, nil, "${NOPE}, which is not set"},
		{"unknown scheme", "version: 1\nsource:\n  url: nope:x\n", nil, errs.Usage, dialect.ErrUnknownScheme, "source database"},
		{"cannot connect", fake, &stubDialect{openErr: errors.New("refused")}, errs.Connect, nil, "connect to the source database: refused"},
		{"cannot read the structure", fake, &stubDialect{introspectErr: errors.New("denied")}, errs.Connect, nil, "read the source database's structure: denied"},
		{"an adapter bug", fake, &stubDialect{db: invalid}, errs.Internal, nil, "the stub adapter read an invalid structure"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, shop())
			f.stub = tt.stub
			f.write(t, config.DefaultPath, tt.config)
			_, err := f.check(t, false)
			if errs.KindOf(err) != tt.want || (tt.is != nil && !errors.Is(err, tt.is)) || !strings.Contains(err.Error(), tt.message) {
				t.Errorf("Check = %v (%v), want %v containing %q", err, errs.KindOf(err), tt.want, tt.message)
			}
		})
	}
}

func TestSourceURLReachesTheRedactor(t *testing.T) {
	f := newFixture(t, shop())
	f.env["STUNTDB_SOURCE_URL"] = "fake:shop"
	f.write(t, config.DefaultPath, "version: 1\n")
	if _, err := f.check(t, false); errs.KindOf(err) != errs.CheckFailed {
		t.Fatalf("Check = %v", err)
	}
	if got := f.redact.String("connect to fake:shop failed"); strings.Contains(got, "fake:shop") {
		t.Errorf("the redactor does not know the source URL: %q", got)
	}
}
