package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/exarth-labs/stuntdb/internal/config"
	"github.com/exarth-labs/stuntdb/internal/errs"
	"github.com/exarth-labs/stuntdb/internal/schema"
)

func (f *fixture) init(t *testing.T, sourceURL string) (*InitResult, error) {
	t.Helper()
	return Init(context.Background(), InitRequest{Env: f.Env(), ConfigPath: f.path(config.DefaultPath), SourceURL: sourceURL})
}

func TestInitGolden(t *testing.T) {
	f := newFixture(t, shop())
	res, err := f.init(t, "fake:shop")
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	want := InitResult{Path: f.path(config.DefaultPath), Tables: 2, Columns: 12, Suggested: 4, NoSuggestion: 2, NoRuleNeeded: 6}
	if *res != want {
		t.Errorf("result = %+v, want %+v", *res, want)
	}

	got := f.read(t, config.DefaultPath)
	golden := filepath.Join("testdata", "shop.stuntdb.yaml.golden")
	if *update {
		if err := os.WriteFile(golden, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	wantText, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (run go test -update to create it)", err)
	}
	if got != strings.ReplaceAll(string(wantText), "\r\n", "\n") {
		t.Errorf("the starter configuration changed; review it, then run go test -update:\n%s", got)
	}
}

// accept does what a reviewer does with a starter configuration: it
// accepts every suggested rule, and keeps every column without one.
func accept(starter string) string {
	accepted := regexp.MustCompile(`(?m)^    # (.*?): +# no suggestion.*$`).ReplaceAllString(starter, "    $1: keep")
	return regexp.MustCompile(`(?m)^    # `).ReplaceAllString(accepted, "    ")
}

// TestInitThenCheck checks that the starter configuration is valid as
// written, and that accepting its suggestions decides every column.
func TestInitThenCheck(t *testing.T) {
	db := shop()
	// Names that YAML must quote, and names too long for a key on one line,
	// which SQLite allows.
	long := schema.Ident(strings.Repeat("x", maxShortKey))
	customers := db.Table(schema.TableRef{Namespace: "APP", Name: "CUSTOMERS"})
	for _, name := range []schema.Ident{"a: b", "#hash", "null", "it's", "<<", long, long + "_email"} {
		customers.Columns = append(customers.Columns, &schema.Column{Name: name, Type: strType})
	}
	db.Namespaces[0].Tables = append(db.Namespaces[0].Tables, &schema.Table{
		Name:    long,
		Columns: []*schema.Column{{Name: "NOTE", Type: textType}, {Name: "LABEL", Type: strType}},
	})
	f := newFixture(t, db)
	if _, err := f.init(t, "fake:shop"); err != nil {
		t.Fatalf("Init: %v", err)
	}

	res, err := f.check(t, true)
	if errs.KindOf(err) != errs.CheckFailed {
		t.Fatalf("check of the starter = %v", err)
	}
	for _, p := range res.Problems {
		if !strings.Contains(p.Message, "needs a decision") {
			t.Errorf("the starter configuration has a mistake: %s", p)
		}
	}

	f.write(t, config.DefaultPath, accept(f.read(t, config.DefaultPath)))
	if res, err := f.check(t, true); err != nil {
		t.Fatalf("check after accepting every suggestion = %v%s", err, details(res))
	}
	if res, err := f.check(t, false); err != nil {
		t.Fatalf("check against the new lockfile = %v%s", err, details(res))
	}
}

// details lists a failed check's problems, if it got as far as a result.
func details(res *CheckResult) string {
	if res == nil {
		return ""
	}
	return ":\n" + problemText(res.Problems)
}

func TestInitNeverReplacesAFile(t *testing.T) {
	f := newFixture(t, shop())
	f.write(t, config.DefaultPath, "mine")
	f.stub = &stubDialect{openErr: errors.New("init must not connect when the file exists")}
	_, err := f.init(t, "fake:shop")
	if errs.KindOf(err) != errs.Output || !strings.Contains(err.Error(), "already exists; edit it, or remove it to start over") {
		t.Errorf("Init over an existing file = %v", err)
	}
	if got := f.read(t, config.DefaultPath); got != "mine" {
		t.Errorf("the file was replaced: %q", got)
	}
}

func TestInitCannotWrite(t *testing.T) {
	f := newFixture(t, shop())
	f.write(t, "file", "")
	for _, path := range []string{
		f.path(filepath.Join("missing", config.DefaultPath)), // a folder that does not exist
		f.path(filepath.Join("file", config.DefaultPath)),    // a folder that is a file
	} {
		_, err := Init(context.Background(), InitRequest{Env: f.Env(), ConfigPath: path, SourceURL: "fake:shop"})
		if errs.KindOf(err) != errs.Output {
			t.Errorf("Init to %s = %v, want an output error", path, err)
		}
	}
	entries, err := os.ReadDir(f.dir)
	if err != nil || len(entries) != 1 {
		t.Errorf("Init left files behind: %v, %v", entries, err)
	}
}

// writeNew never replaces a file, even one that appears after Init looked.
func TestWriteNewNeverReplaces(t *testing.T) {
	f := newFixture(t, shop())
	f.write(t, config.DefaultPath, "mine")
	err := writeNew(context.Background(), f.path(config.DefaultPath), []byte("new"))
	if errs.KindOf(err) != errs.Output || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("writeNew over a file = %v", err)
	}
	if got := f.read(t, config.DefaultPath); got != "mine" {
		t.Errorf("the file was replaced: %q", got)
	}
	if entries, _ := os.ReadDir(f.dir); len(entries) != 1 {
		t.Errorf("writeNew left its partial file behind: %v", entries)
	}
}

func TestInitSourceURL(t *testing.T) {
	tests := []struct {
		name     string
		flag     string
		env      string
		userFile string
		want     string // in the starter file
		wantErr  string
	}{
		{name: "from the command line", flag: "fake:sh$op", want: "source:\n  url: fake:sh$$op\n"},
		{name: "from the environment", env: "fake:sh$op", want: "source:\n  url: ${STUNTDB_SOURCE_URL}\n"},
		{name: "from the user's defaults", userFile: "version: 1\nsource:\n  url: fake:sh$op\n", want: "# source.url comes from your own defaults"},
		{name: "a password on the command line", flag: "fake://user:pw@host/shop", wantErr: "source.url holds a password"},
		{name: "nowhere", wantErr: "no source database URL: set STUNTDB_SOURCE_URL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, shop())
			f.engine.Create("sh$op", shop())
			env := f.Env()
			if tt.env != "" {
				f.env["STUNTDB_SOURCE_URL"] = tt.env
			}
			if tt.userFile != "" {
				env.UserConfig = f.path("user.yaml")
				f.write(t, "user.yaml", tt.userFile)
			}
			_, err := Init(context.Background(), InitRequest{Env: env, ConfigPath: f.path(config.DefaultPath), SourceURL: tt.flag})
			if tt.wantErr != "" {
				if errs.KindOf(err) != errs.Usage || !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("Init = %v, want a usage error containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Init: %v", err)
			}
			if got := f.read(t, config.DefaultPath); !strings.Contains(got, tt.want) {
				t.Errorf("the starter does not contain %q:\n%s", tt.want, got)
			}
			// The file reads back as the same URL.
			res, _ := Check(context.Background(), CheckRequest{Env: env, ConfigPath: f.path(config.DefaultPath)})
			if res == nil || res.Columns != 12 {
				t.Errorf("check of the starter = %+v", res)
			}
		})
	}
}

func TestInitWithNothingToReview(t *testing.T) {
	f := newFixture(t, &schema.Database{Namespaces: []*schema.Namespace{{Name: "APP", Tables: []*schema.Table{{
		Name:    "EVENTS",
		Columns: []*schema.Column{{Name: "ID", Type: intType, Identity: &schema.Identity{}}, {Name: "CREATED_AT", Type: timeType}},
	}}}}})
	res, err := f.init(t, "fake:shop")
	if err != nil || res.NoRuleNeeded != 2 || res.Suggested+res.NoSuggestion != 0 {
		t.Fatalf("Init = %+v, %v", res, err)
	}
	got := f.read(t, config.DefaultPath)
	if strings.Contains(got, "\ncolumns:") || !strings.Contains(got, "# Every column is decided without a rule") {
		t.Errorf("starter:\n%s", got)
	}
}
