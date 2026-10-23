package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/exarth-labs/stuntdb/dialect"
	"github.com/exarth-labs/stuntdb/internal/config"
	"github.com/exarth-labs/stuntdb/internal/errs"
	"github.com/exarth-labs/stuntdb/internal/schema"
)

const shopConfig = "version: 1\nsource:\n  url: fake:shop\n" + shopRules

// TestCheckLifecycle follows a project from its first check to drift:
// there is no lockfile yet, --update writes it, the check passes, and then
// every kind of change makes it fail until the lockfile is updated.
func TestCheckLifecycle(t *testing.T) {
	f := newFixture(t, shop())
	f.write(t, config.DefaultPath, shopConfig)

	res, err := f.check(t, false)
	if errs.KindOf(err) != errs.CheckFailed || !strings.Contains(problemText(res.Problems), "the lockfile does not exist") {
		t.Fatalf("first check = %v, %v", err, res)
	}

	res, err = f.check(t, true)
	if err != nil || !res.LockfileWritten {
		t.Fatalf("check --update = %v, %+v", err, res)
	}
	assertGoldenLock(t, f.read(t, config.LockPath))

	res, err = f.check(t, false)
	if err != nil {
		t.Fatalf("check after --update = %v:\n%s", err, problemText(res.Problems))
	}
	want := Tally{Configured: 6, Safe: 3, References: 2, Generated: 1}
	if res.Decisions != want || res.Tables != 2 || res.Columns != 12 || len(res.Problems) != 0 || res.LockfileWritten {
		t.Errorf("result = %+v, want decisions %+v", res, want)
	}
	if res.Config != f.path(config.DefaultPath) || res.Lockfile != f.path(config.LockPath) {
		t.Errorf("paths = %q, %q", res.Config, res.Lockfile)
	}

	lock := f.read(t, config.LockPath)
	tests := []struct {
		name   string
		change func(t *testing.T)
		want   []string
	}{
		{
			name: "a new column",
			change: func(*testing.T) {
				db := shop()
				customers := db.Table(schema.TableRef{Namespace: "APP", Name: "CUSTOMERS"})
				customers.Columns = append(customers.Columns, &schema.Column{Name: "PHONE", Type: strType, Nullable: true})
				f.engine.Create("shop", db)
			},
			want: []string{
				"stuntdb.lock: the database's structure changed since the lockfile was written",
				"stuntdb.lock: APP.CUSTOMERS.PHONE is new: UNREVIEWED",
				"APP.CUSTOMERS.PHONE needs a decision; suggested: phone (direct, rule core.phone, 0.90)",
			},
		},
		{
			name: "a column removed",
			change: func(*testing.T) {
				db := shop()
				customers := db.Table(schema.TableRef{Namespace: "APP", Name: "CUSTOMERS"})
				customers.Columns = customers.Columns[:len(customers.Columns)-1]
				f.engine.Create("shop", db)
			},
			want: []string{"APP.CUSTOMERS.CREATED_AT is no longer in the database"},
		},
		{
			name: "a decision changed",
			change: func(t *testing.T) {
				f.write(t, config.DefaultPath, strings.Replace(shopConfig, "tier: keep", "tier: hash", 1))
			},
			want: []string{"stuntdb.lock:16:5: APP.CUSTOMERS.TIER changed: hash (config), was keep (config)"},
		},
		{
			name: "another generator version",
			change: func(t *testing.T) {
				f.write(t, config.LockPath, strings.Replace(lock, "generator_version: 1", "generator_version: 7", 1))
			},
			want: []string{"generator_version is 7, but this stuntdb masks with version 1"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f.engine.Create("shop", shop())
			f.write(t, config.DefaultPath, shopConfig)
			f.write(t, config.LockPath, lock)
			tt.change(t)

			res, err := f.check(t, false)
			if errs.KindOf(err) != errs.CheckFailed {
				t.Fatalf("check = %v, want CHECK_FAILED", err)
			}
			text := problemText(res.Problems)
			for _, w := range tt.want {
				if !strings.Contains(text, w) {
					t.Errorf("the problems do not contain %q:\n%s", w, text)
				}
			}
			if !strings.Contains(err.Error(), "check failed: ") {
				t.Errorf("error = %v", err)
			}
		})
	}
}

// assertGoldenLock compares a lockfile with testdata/shop.lock.golden. The
// stuntdb version that wrote it differs from build to build.
func assertGoldenLock(t *testing.T, got string) {
	t.Helper()
	got = regexp.MustCompile(`(?m)^generated_by: .*$`).ReplaceAllString(got, "generated_by: stuntdb (version)")
	golden := filepath.Join("testdata", "shop.lock.golden")
	if *update {
		if err := os.WriteFile(golden, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (run go test -update to create it)", err)
	}
	if got != strings.ReplaceAll(string(want), "\r\n", "\n") {
		t.Errorf("the lockfile changed; review it, then run go test -update:\n%s", got)
	}
}

func TestCheckLockfileErrors(t *testing.T) {
	f := newFixture(t, shop())
	f.write(t, config.DefaultPath, shopConfig)

	f.write(t, config.LockPath, "lock_version: 99\n")
	if _, err := f.check(t, false); errs.KindOf(err) != errs.Usage || !strings.Contains(err.Error(), "lock_version 99 is not supported") {
		t.Errorf("check with an unreadable lockfile = %v", err)
	}

	if err := os.Remove(f.path(config.LockPath)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(f.path(config.LockPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := f.check(t, true); errs.KindOf(err) != errs.Output {
		t.Errorf("check --update where the lockfile cannot be written = %v", err)
	}
}

// With nothing given, the environment uses the registered adapters and the
// process's environment variables.
func TestDefaultEnvironment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.DefaultPath)
	if err := os.WriteFile(path, []byte("version: 1\nsource:\n  url: unregistered:x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STUNTDB_SOURCE_STATEMENT_TIMEOUT", "not a duration")
	_, err := Check(context.Background(), CheckRequest{Env: Env{UserConfig: "-"}, ConfigPath: path})
	if errs.KindOf(err) != errs.Usage || !strings.Contains(err.Error(), "source.statement_timeout must be a duration") {
		t.Errorf("Check read no environment variable: %v", err)
	}
	t.Setenv("STUNTDB_SOURCE_STATEMENT_TIMEOUT", "5s")
	_, err = Check(context.Background(), CheckRequest{Env: Env{UserConfig: "-"}, ConfigPath: path})
	if !errors.Is(err, dialect.ErrUnknownScheme) {
		t.Errorf("Check did not look up the registered adapters: %v", err)
	}
}

// Two columns that differ only in case cannot be told apart by a name that
// matches both.
func TestColumnsThatDifferOnlyInCase(t *testing.T) {
	table := &schema.Table{Columns: []*schema.Column{{Name: "Email"}, {Name: "EMAIL"}}}
	if name, err := findColumn(dialect.CaseInsensitive, table, "EMAIL"); err != nil || name != "EMAIL" {
		t.Errorf("the exact spelling = %q, %v", name, err)
	}
	if _, err := findColumn(dialect.CaseInsensitive, table, "email"); err == nil || !strings.Contains(err.Error(), "differ only in case") {
		t.Errorf("an ambiguous name = %v", err)
	}
}

func TestCheckFindsMistakes(t *testing.T) {
	tests := []struct {
		name   string
		config string
		want   []string
	}{
		{
			"a table that does not exist",
			"columns:\n  app.nope:\n    a: keep\n",
			[]string{"stuntdb.yaml:5:3: table app.nope is not in the database"},
		},
		{
			"a column that does not exist",
			"columns:\n  app.customers:\n    nope: keep\n",
			[]string{`stuntdb.yaml:6:5: APP.CUSTOMERS: column "nope" is not in the table`},
		},
		{
			"two rules for one table",
			"columns:\n  app.customers:\n    tier: keep\n  customers:\n    notes: keep\n",
			[]string{"stuntdb.yaml:7:3: APP.CUSTOMERS already has rules, at "},
		},
		{
			"two rules for one column",
			"columns:\n  app.customers:\n    email: email\n    EMAIL: email\n",
			[]string{"stuntdb.yaml:7:5: APP.CUSTOMERS.EMAIL already has a rule, at "},
		},
		{
			"a rule for a generated column",
			"columns:\n  app.customers:\n    search_key: keep\n",
			[]string{"stuntdb.yaml:6:5: APP.CUSTOMERS.SEARCH_KEY is a generated column"},
		},
		{
			"keep on a secret",
			"columns:\n  app.customers:\n    api_token: keep\n",
			[]string{"stuntdb.yaml:6:5: APP.CUSTOMERS.API_TOKEN: secrets are always replaced: the column looks like a secret (rule secrets.token), so it cannot be kept; use random_token"},
		},
		{
			"null in a NOT NULL column",
			"columns:\n  app.customers:\n    tier: null\n",
			[]string{"stuntdb.yaml:6:5: APP.CUSTOMERS.TIER is NOT NULL, so it cannot be null"},
		},
		{
			"a foreign key masked unlike what it references",
			"columns:\n  app.customers:\n    email: email\n  app.orders:\n    billing_email: hash\n",
			[]string{"stuntdb.yaml:8:5: APP.ORDERS.BILLING_EMAIL references APP.CUSTOMERS.EMAIL, so it must be masked the same way (email); remove its rule"},
		},
		{
			"columns without a decision",
			"",
			[]string{
				"APP.CUSTOMERS.EMAIL needs a decision; suggested: email (direct, rule core.email, 0.95)",
				"APP.CUSTOMERS.TIER needs a decision; no rule suggests one",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, shop())
			f.write(t, config.DefaultPath, "version: 1\nsource:\n  url: fake:shop\n"+tt.config)
			res, err := f.check(t, true)
			if errs.KindOf(err) != errs.CheckFailed {
				t.Fatalf("check = %v, want CHECK_FAILED", err)
			}
			text := problemText(res.Problems)
			for _, w := range tt.want {
				if !strings.Contains(text, w) {
					t.Errorf("the problems do not contain %q:\n%s", w, text)
				}
			}
			// Mistakes in the configuration keep --update from writing the
			// lockfile; columns that only need a decision do not.
			mistakes := tt.config != ""
			if _, err := os.Stat(f.path(config.LockPath)); mistakes == (err == nil) || res.LockfileWritten == mistakes {
				t.Errorf("lockfile written = %t (stat: %v), but the configuration has mistakes = %t", res.LockfileWritten, err, mistakes)
			}
		})
	}
}

// A foreign key that waits for an unreviewed column is not listed: the
// column it references is, and deciding it decides both.
func TestWaitingReferencesAreNotListed(t *testing.T) {
	f := newFixture(t, shop())
	f.write(t, config.DefaultPath, "version: 1\nsource:\n  url: fake:shop\n")
	res, _ := f.check(t, true)
	text := problemText(res.Problems)
	if strings.Contains(text, "BILLING_EMAIL") || res.Decisions.Unreviewed != 6 || res.Decisions.Waiting != 1 {
		t.Errorf("decisions = %+v, problems:\n%s", res.Decisions, text)
	}
	if !strings.Contains(f.read(t, config.LockPath), "BILLING_EMAIL: {decision: UNREVIEWED}") {
		t.Errorf("the lockfile does not record the waiting reference:\n%s", f.read(t, config.LockPath))
	}
}

func TestPolicies(t *testing.T) {
	tests := []struct {
		policy string
		want   []string // lockfile lines
	}{
		{"null", []string{
			"NOTES: {decision: \"null\", source: masking.unclassified}",
			"TIER: {decision: UNREVIEWED}", // NOT NULL
			"BILLING_EMAIL: {decision: UNREVIEWED}",
		}},
		{"keep", []string{
			"NOTES: {decision: keep, source: masking.unclassified}",
			"API_TOKEN: {decision: random_token, source: rule secrets.token; secrets are always replaced}",
			"BILLING_EMAIL: {decision: keep, source: follows APP.CUSTOMERS.EMAIL}",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.policy, func(t *testing.T) {
			f := newFixture(t, shop())
			f.write(t, config.DefaultPath, "version: 1\nsource:\n  url: fake:shop\nmasking:\n  unclassified: "+tt.policy+"\n")
			res, _ := f.check(t, true)
			lock := f.read(t, config.LockPath)
			for _, w := range tt.want {
				if !strings.Contains(lock, w) {
					t.Errorf("the lockfile does not contain %q:\n%s", w, lock)
				}
			}
			if tt.policy == "null" && !strings.Contains(problemText(res.Problems), "TIER needs a decision; no rule suggests one; masking.unclassified is null, but the column is NOT NULL") {
				t.Errorf("problems:\n%s", problemText(res.Problems))
			}
		})
	}
}

func TestTableNames(t *testing.T) {
	db := shop()
	db.Namespaces = append(db.Namespaces, &schema.Namespace{Name: "ARCHIVE", Tables: []*schema.Table{
		{Name: "CUSTOMERS", Columns: []*schema.Column{{Name: "ID", Type: intType, Identity: &schema.Identity{}}}},
		{Name: "customers", Columns: []*schema.Column{{Name: "ID", Type: intType, Identity: &schema.Identity{}}}},
	}})
	tests := []struct {
		name  string
		table string
		want  string // a problem, or empty
	}{
		{"qualified", "app.customers", ""},
		{"the exact spelling wins over folded names", `archive.customers`, `column "tier" is not in the table`},
		{"unqualified, in two namespaces", "CUSTOMERS", "table CUSTOMERS could be any of APP.CUSTOMERS, ARCHIVE.CUSTOMERS; write namespace.table"},
		{"an unknown namespace", "nope.customers", "table nope.customers is not in the database"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, db)
			f.write(t, config.DefaultPath, "version: 1\nsource:\n  url: fake:shop\ncolumns:\n  "+tt.table+":\n    tier: keep\n")
			res, _ := f.check(t, true)
			text := problemText(res.Problems)
			if tt.want == "" && strings.Contains(text, "stuntdb.yaml") || !strings.Contains(text, tt.want) {
				t.Errorf("problems:\n%s\nwant %q", text, tt.want)
			}
		})
	}
}
