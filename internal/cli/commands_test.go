package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/exarth-labs/stuntdb/dialect"
	"github.com/exarth-labs/stuntdb/internal/app"
	"github.com/exarth-labs/stuntdb/internal/errs"
	"github.com/exarth-labs/stuntdb/internal/fakedb"
	"github.com/exarth-labs/stuntdb/internal/schema"
	"github.com/exarth-labs/stuntdb/internal/value"
)

// fakeEnv is an environment with a fake engine that holds one database,
// "fake:shop", and no environment variables or user defaults.
func fakeEnv() app.Env {
	engine := fakedb.New()
	engine.Create("shop", &schema.Database{Namespaces: []*schema.Namespace{{Name: "APP", Tables: []*schema.Table{{
		Name: "USERS",
		Columns: []*schema.Column{
			{Name: "ID", Type: value.Type{Kind: value.TypeInt, Bits: 64}, Identity: &schema.Identity{}},
			{Name: "EMAIL", Type: value.Type{Kind: value.TypeText}},
			{Name: "PLAN", Type: value.Type{Kind: value.TypeText}},
		},
	}}}}})
	return app.Env{
		Lookup:     func(string) (dialect.Dialect, error) { return engine, nil },
		LookupEnv:  func(string) (string, bool) { return "", false },
		UserConfig: "-",
	}
}

type outcome struct {
	stdout, stderr string
	err            error
}

func runWith(env app.Env, args ...string) outcome {
	var stdout, stderr bytes.Buffer
	err := execute(context.Background(), args, &stdout, &stderr, env)
	return outcome{stdout.String(), stderr.String(), err}
}

func (o outcome) expect(t *testing.T, kind errs.Kind, stdout, stderr []string) {
	t.Helper()
	switch {
	case kind == errs.Internal && o.err != nil:
		t.Errorf("unexpected error: %v", o.err)
	case kind != errs.Internal && errs.KindOf(o.err) != kind:
		t.Errorf("error = %v (%v), want kind %v", o.err, errs.KindOf(o.err), kind)
	}
	for _, s := range stdout {
		if !strings.Contains(o.stdout, s) {
			t.Errorf("stdout does not contain %q:\n%s", s, o.stdout)
		}
	}
	for _, s := range stderr {
		if !strings.Contains(o.stderr, s) {
			t.Errorf("stderr does not contain %q:\n%s", s, o.stderr)
		}
	}
	if len(stderr) == 0 && o.stderr != "" {
		t.Errorf("stderr should be empty, got:\n%s", o.stderr)
	}
}

// TestInitThenCheck runs the commands the way a user does: init, a failing
// check, check --update after the review, and a passing check.
func TestInitThenCheck(t *testing.T) {
	env := fakeEnv()
	cfg := filepath.Join(t.TempDir(), "stuntdb.yaml")
	lock := filepath.Join(filepath.Dir(cfg), "stuntdb.lock")

	runWith(env, "init", "--source", "fake:shop", "-c", cfg).expect(t, errs.Internal,
		[]string{"Wrote " + cfg + " for 3 columns in 1 table:", "1 with a suggested rule to review", "1 without a suggestion", "1 that need no rule", `then run "stuntdb check --update"`},
		nil)
	runWith(env, "init", "--source", "fake:shop", "-c", cfg).expect(t, errs.Output, nil,
		[]string{"stuntdb: " + cfg + " already exists"})

	// Findings go to stdout; only the verdict goes to stderr.
	o := runWith(env, "check", "--config", cfg)
	o.expect(t, errs.CheckFailed,
		[]string{"Checked 3 columns in 1 table:", "1 kept as safe", "2 without a decision", "3 problems:", "APP.USERS.EMAIL needs a decision; suggested: email", "the lockfile does not exist"},
		[]string{"stuntdb: check failed: 3 problems"})
	if strings.Contains(o.stderr, "--help") {
		t.Errorf("a failed check is not a usage error:\n%s", o.stderr)
	}

	runWith(env, "check", "--update", "-c", cfg).expect(t, errs.CheckFailed,
		[]string{"Wrote " + lock + ". Review the change, and commit it."},
		[]string{"check failed: 2 problems"})

	// The review: accept the suggestion, and keep the column without one.
	starter, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	reviewed := strings.NewReplacer("    # EMAIL: email", "    EMAIL: email", "    # PLAN:", "    PLAN: keep #").Replace(string(starter))
	if err := os.WriteFile(cfg, []byte(reviewed), 0o600); err != nil {
		t.Fatal(err)
	}
	runWith(env, "check", "-c", cfg).expect(t, errs.CheckFailed, []string{"APP.USERS.EMAIL changed: email (config), was UNREVIEWED"}, []string{"check failed"})
	runWith(env, "check", "--update", "-c", cfg).expect(t, errs.Internal, []string{"2 decided by a rule in " + cfg, "Wrote " + lock}, nil)
	runWith(env, "check", "-c", cfg).expect(t, errs.Internal, []string{"Checked 3 columns in 1 table:"}, nil)
}

func TestCheckMistakesKeepTheLockfile(t *testing.T) {
	env := fakeEnv()
	cfg := filepath.Join(t.TempDir(), "stuntdb.yaml")
	if err := os.WriteFile(cfg, []byte("version: 1\nsource:\n  url: fake:shop\ncolumns:\n  app.users:\n    nope: keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runWith(env, "check", "--update", "-c", cfg).expect(t, errs.CheckFailed,
		[]string{`APP.USERS: column "nope" is not in the table`, "stuntdb.lock was not written: fix the mistakes above first."},
		[]string{"check failed"})
}

func TestCommandsAsJSON(t *testing.T) {
	env := fakeEnv()
	cfg := filepath.Join(t.TempDir(), "stuntdb.yaml")

	var initRes app.InitResult
	o := runWith(env, "init", "--json", "--source", "fake:shop", "-c", cfg)
	if err := json.Unmarshal([]byte(o.stdout), &initRes); err != nil || o.err != nil {
		t.Fatalf("init --json = %v, %v:\n%s", o.err, err, o.stdout)
	}
	if initRes.Path != cfg || initRes.Columns != 3 || initRes.Suggested != 1 {
		t.Errorf("init --json = %+v", initRes)
	}

	var checkRes app.CheckResult
	o = runWith(env, "check", "--json", "-c", cfg)
	if err := json.Unmarshal([]byte(o.stdout), &checkRes); err != nil || errs.KindOf(o.err) != errs.CheckFailed {
		t.Fatalf("check --json = %v, %v:\n%s", o.err, err, o.stdout)
	}
	if checkRes.Columns != 3 || checkRes.Decisions.Unreviewed != 2 || len(checkRes.Problems) != 3 || checkRes.Problems[0].Message == "" {
		t.Errorf("check --json = %+v", checkRes)
	}
}

func TestInitNeedsASource(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "stuntdb.yaml")
	runWith(fakeEnv(), "init", "-c", cfg).expect(t, errs.Usage, nil,
		[]string{"stuntdb: no source database URL: set STUNTDB_SOURCE_URL, or use --source", "Run 'stuntdb --help'"})
	if _, err := os.Stat(cfg); !os.IsNotExist(err) {
		t.Errorf("init without a source wrote a file: %v", err)
	}
}
