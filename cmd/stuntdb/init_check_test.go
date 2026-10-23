package main

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/exarth-labs/stuntdb/internal/cli"
)

// canary is a value in the source database. stuntdb reads only the
// structure for init and check, so it must never appear anywhere.
const canary = "canary-7f3a@example.com"

// TestInitAndCheckOnSQLite runs init and check end to end on a real SQLite
// file, through the same code path and exit codes as the stuntdb binary.
func TestInitAndCheckOnSQLite(t *testing.T) {
	dir := t.TempDir()
	// No user defaults from the machine running the test.
	t.Setenv("AppData", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)

	dbPath := filepath.Join(dir, "app.db")
	execSQL(t, dbPath,
		`CREATE TABLE customers (
			id INTEGER PRIMARY KEY,
			email TEXT NOT NULL UNIQUE,
			full_name TEXT,
			password_hash TEXT,
			plan TEXT NOT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP)`,
		`CREATE TABLE orders (
			id INTEGER PRIMARY KEY,
			customer_id INTEGER NOT NULL REFERENCES customers(id),
			total NUMERIC(10,2),
			note TEXT)`,
		`INSERT INTO customers (email, full_name, password_hash, plan) VALUES ('`+canary+`', '`+canary+`', '`+canary+`', '`+canary+`')`,
	)
	cfg := filepath.Join(dir, "stuntdb.yaml")
	var outputs strings.Builder
	run := func(want int, args ...string) string {
		t.Helper()
		var stdout, stderr bytes.Buffer
		ctx := context.Background()
		code := exitCode(ctx, cli.Execute(ctx, args, &stdout, &stderr))
		outputs.WriteString(stdout.String() + stderr.String())
		if code != want {
			t.Fatalf("stuntdb %s: exit code %d, want %d\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), code, want, stdout.String(), stderr.String())
		}
		return stdout.String()
	}

	run(exitOK, "init", "--source", "sqlite:"+dbPath, "-c", cfg)
	out := run(exitCheckFailed, "check", "-c", cfg)
	for _, want := range []string{
		"main.customers.email needs a decision; suggested: email",
		"main.customers.password_hash needs a decision; suggested: password_hash (secret",
		"main.customers.plan needs a decision; no rule suggests one",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("check does not report %q:\n%s", want, out)
		}
	}

	// The review: accept every suggestion, and keep every column without one.
	writeFile(t, cfg, accept(readFile(t, cfg)))
	run(exitOK, "check", "--update", "-c", cfg)
	out = run(exitOK, "check", "-c", cfg)
	if !strings.Contains(out, "masked like the columns they reference") {
		t.Errorf("orders.customer_id is not masked like customers.id:\n%s", out)
	}

	// A new column in production fails the check until it is reviewed.
	execSQL(t, dbPath, `ALTER TABLE customers ADD COLUMN phone TEXT`)
	out = run(exitCheckFailed, "check", "-c", cfg)
	if !strings.Contains(out, "main.customers.phone is new: UNREVIEWED") {
		t.Errorf("check does not report the new column:\n%s", out)
	}

	for name, text := range map[string]string{
		"the output":        outputs.String(),
		"stuntdb.yaml":      readFile(t, cfg),
		"stuntdb.lock":      readFile(t, filepath.Join(dir, "stuntdb.lock")),
		"the folder's list": strings.Join(listDir(t, dir), "\n"),
	} {
		if strings.Contains(text, canary) {
			t.Errorf("%s contains a value from the database", name)
		}
	}
	for _, name := range listDir(t, dir) {
		if strings.Contains(name, ".partial-") {
			t.Errorf("a partial file was left behind: %s", name)
		}
	}
}

// accept accepts every suggested rule of a starter configuration, and keeps
// every column without one.
func accept(starter string) string {
	accepted := regexp.MustCompile(`(?m)^    # (.*?): +# no suggestion.*$`).ReplaceAllString(starter, "    $1: keep")
	return regexp.MustCompile(`(?m)^    # `).ReplaceAllString(accepted, "    ")
}

func execSQL(t *testing.T, path string, statements ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for _, s := range statements {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func listDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	return names
}
