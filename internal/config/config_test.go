package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/exarth-labs/stuntdb/internal/errs"
	"github.com/exarth-labs/stuntdb/internal/redact"
	"github.com/exarth-labs/stuntdb/schemas"
)

// canary stands for a secret; no error may show it.
const canary = "canary-7f3a-0123456789-abcdefghijklmnop"

// env is a fake environment.
type env map[string]string

func (e env) lookup(name string) (string, bool) {
	v, ok := e[name]
	return v, ok
}

// write puts a project configuration in a new folder and returns its path.
func write(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), DefaultPath)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func loadTest(t *testing.T, path string, e env, flags map[string]string) (*Config, error) {
	t.Helper()
	return Load(LoadOptions{Path: path, UserPath: "-", LookupEnv: e.lookup, Flags: flags})
}

const example = `# A project configuration.
version: 1

source:
  url: ${APP_DATABASE_URL}
  max_connections: 4
  statement_timeout: 5m

output:
  to: sqlite:./dev.db

masking:
  key: ${STUNTDB_MASKING_KEY}
  unclassified: fail

columns:
  main.users:
    email: { use: email, domain: person.email }
    nickname: null
    created_at: keep
    phone: { use: phone, region: PK }
  orders:
    note: synthesize_text
  main."order.items":
    sku: keep
`

func TestLoadExample(t *testing.T) {
	path := write(t, example)
	cfg, err := loadTest(t, path, env{"APP_DATABASE_URL": "sqlite:./app.db", "STUNTDB_MASKING_KEY": canary}, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if url, err := cfg.Source.URL.Get(); err != nil || url != "sqlite:./app.db" {
		t.Errorf("source.url = %q, %v", url, err)
	}
	if from := cfg.Source.URL.From().String(); from != path+":5:8" {
		t.Errorf("source.url comes from %s, want %s:5:8", from, path)
	}
	if cfg.Source.MaxConnections != 4 || cfg.Source.StatementTimeout != 5*time.Minute {
		t.Errorf("source = %d connections, %v timeout", cfg.Source.MaxConnections, cfg.Source.StatementTimeout)
	}
	if to, _ := cfg.Output.To.Get(); to != "sqlite:./dev.db" {
		t.Errorf("output.to = %q", to)
	}
	if cfg.Masking.Unclassified != PolicyFail {
		t.Errorf("masking.unclassified = %q", cfg.Masking.Unclassified)
	}
	if key, err := cfg.Masking.Key(); err != nil || string(key) != canary {
		t.Errorf("Key() = %v", err)
	}

	var tables []string
	for _, tr := range cfg.Columns {
		tables = append(tables, tr.Table.String())
	}
	if want := []string{"main.users", "orders", `main."order.items"`}; !slices.Equal(tables, want) {
		t.Errorf("tables = %v, want %v, in file order", tables, want)
	}
	users := cfg.Columns[0]
	if users.Table != (TableName{Namespace: "main", Name: "users"}) {
		t.Errorf("table = %+v", users.Table)
	}
	got := map[string]*ColumnRule{}
	for _, r := range users.Columns {
		got[r.Column] = r
	}
	if r := got["email"]; r.Use != "email" || r.Domain != "person.email" || r.At.Line != 18 {
		t.Errorf("email rule = %+v", r)
	}
	if r := got["nickname"]; r.Use != "null" {
		t.Errorf("the bare word null should name the null transformer, got %q", r.Use)
	}
	if r := got["phone"]; r.Use != "phone" || r.Options["region"] != "PK" {
		t.Errorf("phone rule = %+v", r)
	}
	if cfg.Columns[2].Table != (TableName{Namespace: "main", Name: "order.items"}) {
		t.Errorf("quoted table = %+v", cfg.Columns[2].Table)
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want []string // substrings, each with its position
	}{
		{"empty file", "", []string{"the file is empty"}},
		{"not settings", "- a\n- b\n", []string{"must hold settings"}},
		{"YAML syntax", "version: 1\nsource: [\n", []string{"stuntdb.yaml"}},
		{"two documents", "version: 1\n---\nversion: 1\n", []string{"more than one YAML document"}},
		{"duplicate key", "version: 1\nversion: 1\n", []string{`:2:1: "version" appears twice`}},
		{"anchors", "version: 1\nsource: &s {url: sqlite:a.db}\noutput: *s\n", []string{"anchors", "aliases"}},
		{"missing version", "source: {url: sqlite:a.db}\n", []string{":1:1: version is missing"}},
		{"wrong version", "version: 2\n", []string{":1:10: version must be 1"}},
		{"unknown setting", "version: 1\nsource:\n  uri: x\n", []string{":3:3: unknown setting source.uri"}},
		{"a later feature", "version: 1\nsubset: {}\n", []string{":2:1: unknown setting subset (subsetting arrives in a later release)"}},
		{"wrong type", "version: 1\nsource:\n  max_connections: four\n", []string{":3:20: source.max_connections must be a whole number"}},
		{"out of range", "version: 1\nsource:\n  max_connections: 0\n", []string{"source.max_connections must be at least 1"}},
		{"bad duration", "version: 1\nsource:\n  statement_timeout: 5 minutes\n", []string{":3:22: source.statement_timeout must be a duration"}},
		{"bad policy", "version: 1\nmasking:\n  unclassified: maybe\n", []string{":3:17: masking.unclassified must be one of: fail, null, keep"}},
		{"key and key file", "version: 1\nmasking:\n  key: ${K}\n  key_file: k.txt\n", []string{"use key or key_file, not both"}},
		{"rule without use", "version: 1\ncolumns:\n  users:\n    email: {domain: x}\n", []string{`:4:12: columns.users.email.use is missing`}},
		{"bad transformer name", "version: 1\ncolumns:\n  users:\n    email: Email\n", []string{":4:12: columns.users.email must be a transformer name"}},
		{"rule as a list", "version: 1\ncolumns:\n  users:\n    email: [a]\n", []string{"columns.users.email must be a string or a set of settings"}},
		{"bad table name", "version: 1\ncolumns:\n  a.b.c:\n    x: keep\n", []string{":3:3: columns: a table is written as table or namespace.table"}},
		{"duplicate table", "version: 1\ncolumns:\n  users: {a: keep}\n  '\"users\"': {b: keep}\n", []string{"already has rules"}},
		{"dotted table name in messages", "version: 1\ncolumns:\n  main.users:\n    email: Email\n", []string{`columns["main.users"].email must be a transformer name`}},
		{"bad interpolation", "version: 1\nsource:\n  url: ${NOT CLOSED\n", []string{":3:8: source.url: a ${ is not closed"}},
		{"bad variable name", "version: 1\nsource:\n  url: ${1X}\n", []string{"must hold an environment variable name"}},
		{"key written in the file", "version: 1\nmasking:\n  key: " + canary + "\n", []string{":3:8: masking.key is written in the file"}},
		{"password in a URL", "version: 1\nsource:\n  url: postgres://admin:" + canary + "@db/app\n", []string{":3:8: source.url holds a password"}},
		{"every problem at once", "version: 1\nsource:\n  uri: x\nmasking:\n  unclassified: maybe\n", []string{"unknown setting source.uri", "masking.unclassified must be one of"}},
		{"a secret behind a typo", "version: 1\nsourcee: {}\nmasking:\n  key: " + canary + "\n", []string{":2:1: unknown setting sourcee", ":4:8: masking.key is written in the file"}},
		{"problems in line order", "version: 1\ncolumns:\n  users:\n    a: keep\n    b: keep\n    c: keep\n    d: keep\n    e: keep\n    f: keep\n    g: Bad\nsource:\n  uri: x\n", []string{":10:8: columns.users.g must be a transformer name", ":12:3: unknown setting source.uri"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := write(t, tt.yaml)
			_, err := loadTest(t, path, env{"K": canary}, nil)
			if err == nil {
				t.Fatal("Load succeeded")
			}
			if errs.KindOf(err) != errs.Usage {
				t.Errorf("kind = %v, want USAGE", errs.KindOf(err))
			}
			msg := err.Error()
			for _, w := range tt.want {
				if !strings.Contains(msg, w) {
					t.Errorf("the error lacks %q:\n%s", w, msg)
				}
			}
			if strings.Contains(msg, canary) {
				t.Errorf("the error shows a secret:\n%s", msg)
			}
		})
	}
}

// Problems come in the order of the file, by line number, not as text
// (where line 12 would sort before line 4).
func TestProblemsInLineOrder(t *testing.T) {
	path := write(t, "version: 1\nmasking:\n  unclassified: maybe\ncolumns:\n  users:\n    a: keep\n    b: keep\n    c: keep\n    d: keep\n    e: keep\n    f: keep\n    g: Bad\n")
	_, err := loadTest(t, path, env{}, nil)
	if err == nil {
		t.Fatal("Load succeeded")
	}
	msg := err.Error()
	first, second := strings.Index(msg, ":3:17:"), strings.Index(msg, ":12:8:")
	if first < 0 || second < 0 || first > second {
		t.Errorf("problems are not in line order:\n%s", msg)
	}
}

func TestMissingProjectConfig(t *testing.T) {
	_, err := loadTest(t, filepath.Join(t.TempDir(), DefaultPath), nil, nil)
	if !errors.Is(err, ErrNoConfig) || errs.KindOf(err) != errs.Usage || !strings.Contains(err.Error(), "stuntdb init") {
		t.Errorf("Load of a missing file = %v", err)
	}
}

func TestNullAndEmptyValues(t *testing.T) {
	cfg, err := loadTest(t, write(t, "version: 1\nmasking:\n  unclassified: null\n"), nil, nil)
	if err != nil || cfg.Masking.Unclassified != PolicyNull {
		t.Errorf("unclassified: null = %q, %v; want the null policy", cfg.Masking.Unclassified, err)
	}
	cfg, err = loadTest(t, write(t, "version: 1\nmasking:\n  unclassified:\n"), nil, nil)
	if err == nil {
		t.Errorf("an empty unclassified was accepted as %q", cfg.Masking.Unclassified)
	}
	cfg, err = loadTest(t, write(t, "version: 1\n"), nil, nil)
	if err != nil || cfg.Masking.Unclassified != PolicyFail || cfg.Source.URL.IsSet() {
		t.Errorf("defaults: %+v, %v", cfg, err)
	}
}

// Settings apply in order: user defaults, the project, environment
// variables, then flags.
func TestPrecedence(t *testing.T) {
	dir := t.TempDir()
	user := filepath.Join(dir, "user.yaml")
	if err := os.WriteFile(user, []byte("version: 1\nsource:\n  max_connections: 2\n  statement_timeout: 1m\noutput:\n  to: sqlite:user.db\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	project := write(t, "version: 1\nsource:\n  statement_timeout: 2m\noutput:\n  to: sqlite:project.db\n")
	get := func(cfg *Config) string { v, _ := cfg.Output.To.Get(); return v }

	opts := LoadOptions{Path: project, UserPath: user, LookupEnv: env{}.lookup}
	cfg, err := Load(opts)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Source.MaxConnections != 2 || cfg.Source.StatementTimeout != 2*time.Minute || get(cfg) != "sqlite:project.db" {
		t.Errorf("files: %d, %v, %q", cfg.Source.MaxConnections, cfg.Source.StatementTimeout, get(cfg))
	}

	opts.LookupEnv = env{"STUNTDB_OUTPUT_TO": "sqlite:env.db", "STUNTDB_SOURCE_MAX_CONNECTIONS": "8"}.lookup
	cfg, err = Load(opts)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Source.MaxConnections != 8 || get(cfg) != "sqlite:env.db" || cfg.Output.To.From().String() != "environment variable STUNTDB_OUTPUT_TO" {
		t.Errorf("environment: %d, %q from %s", cfg.Source.MaxConnections, get(cfg), cfg.Output.To.From())
	}

	opts.Flags = map[string]string{"output.to": "sqlite:flag.db"}
	if cfg, err = Load(opts); err != nil || get(cfg) != "sqlite:flag.db" {
		t.Errorf("flag: %q, %v", get(cfg), err)
	}

	opts.LookupEnv = env{"STUNTDB_SOURCE_MAX_CONNECTIONS": "lots"}.lookup
	if _, err := Load(opts); err == nil || !strings.Contains(err.Error(), "environment variable STUNTDB_SOURCE_MAX_CONNECTIONS: source.max_connections must be a whole number") {
		t.Errorf("a bad environment value = %v", err)
	}
}

func TestUserConfigHasNoColumns(t *testing.T) {
	user := filepath.Join(t.TempDir(), "user.yaml")
	if err := os.WriteFile(user, []byte("version: 1\ncolumns:\n  users: {email: keep}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(LoadOptions{Path: write(t, "version: 1\n"), UserPath: user, LookupEnv: env{}.lookup})
	if err == nil || !strings.Contains(err.Error(), "masking rules belong in the project") {
		t.Errorf("Load = %v", err)
	}
}

// A variable that is not set fails only when its setting is used, so that
// stuntdb check can run without the masking key.
func TestMissingVariablesFailLate(t *testing.T) {
	cfg, err := loadTest(t, write(t, "version: 1\nsource:\n  url: ${NO_SUCH_URL}\nmasking:\n  key: ${NO_SUCH_KEY}\n"), env{}, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := cfg.Source.URL.Get(); err == nil || !strings.Contains(err.Error(), ":3:8: source.url uses ${NO_SUCH_URL}, which is not set") {
		t.Errorf("source.url = %v", err)
	}
	if _, err := cfg.Masking.Key(); err == nil || !strings.Contains(err.Error(), "${NO_SUCH_KEY}") || errs.KindOf(err) != errs.Usage {
		t.Errorf("Key() = %v", err)
	}
	cfg, err = loadTest(t, write(t, "version: 1\n"), env{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.Masking.Key(); err == nil || !strings.Contains(err.Error(), "STUNTDB_MASKING_KEY") {
		t.Errorf("Key() without a key = %v", err)
	}
}

func TestKeyFile(t *testing.T) {
	path := write(t, "version: 1\nmasking:\n  key_file: secrets/key.txt\n")
	dir := filepath.Join(filepath.Dir(path), "secrets")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	r := redact.New()
	keyPath := filepath.Join(dir, "key.txt")
	for _, tt := range []struct {
		content string
		ok      bool
	}{
		{canary + "\n", true},
		{"short\n", false},
	} {
		if err := os.WriteFile(keyPath, []byte(tt.content), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(LoadOptions{Path: path, UserPath: "-", LookupEnv: env{}.lookup, Redactor: r})
		if err != nil {
			t.Fatal(err)
		}
		key, err := cfg.Masking.Key()
		switch {
		case tt.ok && (err != nil || string(key) != canary):
			t.Errorf("Key() = %v; want the file's key without its newline", err)
		case !tt.ok && (err == nil || !strings.Contains(err.Error(), "at least 32 characters")):
			t.Errorf("a short key = %v", err)
		}
	}
	if got := r.String("key " + canary); strings.Contains(got, canary) {
		t.Error("the redactor did not learn the key from the file")
	}

	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	cfg, _ := Load(LoadOptions{Path: path, UserPath: "-", LookupEnv: env{}.lookup})
	if _, err := cfg.Masking.Key(); err == nil || strings.Contains(err.Error(), "secrets") {
		t.Errorf("a missing key file = %v; the error must not show the path", err)
	}

	// A key from the environment replaces key_file from the file.
	cfg, err := loadTest(t, path, env{"STUNTDB_MASKING_KEY": canary}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if key, err := cfg.Masking.Key(); err != nil || string(key) != canary {
		t.Errorf("Key() with STUNTDB_MASKING_KEY = %v", err)
	}
}

func TestSecretsReachTheRedactor(t *testing.T) {
	r := redact.New()
	_, err := Load(LoadOptions{
		Path: write(t, "version: 1\nsource:\n  url: ${DB}\nmasking:\n  key: ${KEY}\n"), UserPath: "-", Redactor: r,
		LookupEnv: env{"DB": "postgres://admin:" + canary + "@db/app", "KEY": "k-" + canary}.lookup,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"login failed: " + canary, "bad key k-" + canary} {
		if got := r.String(s); strings.Contains(got, canary) {
			t.Errorf("the redactor let a secret through: %q", got)
		}
	}
}

func TestFlagsNeverCarrySecrets(t *testing.T) {
	path := write(t, "version: 1\n")
	for _, flags := range []map[string]string{
		{"masking.key": canary},
		{"output.to": "postgres://admin:" + canary + "@db/app"},
	} {
		_, err := loadTest(t, path, env{}, flags)
		if err == nil {
			t.Errorf("flags %v were accepted", slices.Collect(mapKeys(flags)))
			continue
		}
		if strings.Contains(err.Error(), canary) {
			t.Errorf("the error shows the secret: %v", err)
		}
	}
	if _, err := loadTest(t, path, env{}, map[string]string{"output.nope": "x"}); err == nil {
		t.Error("an unknown flag setting was accepted")
	}
}

func mapKeys(m map[string]string) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// The schema and the code list the same settings, so that what editors
// offer is exactly what stuntdb reads.
func TestSchemaMatchesTheCode(t *testing.T) {
	var doc struct {
		Properties map[string]struct {
			Properties map[string]any `json:"properties"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(schemas.Config(), &doc); err != nil {
		t.Fatal(err)
	}
	var inSchema []string
	for section, p := range doc.Properties {
		for name := range p.Properties {
			inSchema = append(inSchema, section+"."+name)
		}
	}
	var inCode []string
	for _, s := range settings {
		inCode = append(inCode, s.name)
	}
	slices.Sort(inSchema)
	slices.Sort(inCode)
	if !slices.Equal(inSchema, inCode) {
		t.Errorf("schema settings %v, code settings %v", inSchema, inCode)
	}
}

func TestInterpolate(t *testing.T) {
	e := env{"A": "x", "EMPTY": ""}
	tests := []struct {
		in, want, missing string
	}{
		{"plain", "plain", ""},
		{"${A}", "x", ""},
		{"pre-${A}-${A}-post", "pre-x-x-post", ""},
		{"${EMPTY}", "", ""},
		{"cost $$5 and $x and $", "cost $5 and $x and $", ""},
		{"${A}${MISSING}${OTHER}", "x", "MISSING"},
	}
	for _, tt := range tests {
		got, missing, err := interpolate(tt.in, e.lookup)
		if err != nil || got != tt.want || missing != tt.missing {
			t.Errorf("interpolate(%q) = %q, %q, %v; want %q, %q", tt.in, got, missing, err, tt.want, tt.missing)
		}
	}
	for _, bad := range []string{"${", "${A", "${}", "${A B}", "${9A}"} {
		if _, _, err := interpolate(bad, e.lookup); err == nil {
			t.Errorf("interpolate(%q) succeeded", bad)
		}
	}
}

func TestParseTableName(t *testing.T) {
	tests := []struct {
		in   string
		want TableName
	}{
		{"users", TableName{Name: "users"}},
		{"main.users", TableName{Namespace: "main", Name: "users"}},
		{`main."order.items"`, TableName{Namespace: "main", Name: "order.items"}},
		{`"my ""quoted"" ns"."t"`, TableName{Namespace: `my "quoted" ns`, Name: "t"}},
		{"Users", TableName{Name: "Users"}},
	}
	for _, tt := range tests {
		got, err := ParseTableName(tt.in)
		if err != nil || got != tt.want {
			t.Errorf("ParseTableName(%q) = %+v, %v", tt.in, got, err)
			continue
		}
		if back, err := ParseTableName(got.String()); err != nil || back != got {
			t.Errorf("%+v does not round-trip through %q", got, got.String())
		}
	}
	for _, bad := range []string{"", ".", "a.", ".b", "a.b.c", `"a`, `a"b`, `"a"b`, `"".t`} {
		if got, err := ParseTableName(bad); err == nil {
			t.Errorf("ParseTableName(%q) = %+v", bad, got)
		}
	}
}

func FuzzLoad(f *testing.F) {
	f.Add([]byte(example))
	f.Add([]byte("version: 1\ncolumns:\n  a: {b: {use: c, d: [1, {e: f}]}}\n"))
	f.Add([]byte("version: 1\nsource: &a {url: x}\nb: *a\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		// Parsing, validating and reading the rules must never panic, and a
		// file that passes must give every rule a transformer.
		file, err := parseFile(DefaultPath, data)
		if err != nil || file.validate() != nil {
			return
		}
		rules, err := file.columns()
		if err != nil {
			return
		}
		for _, tr := range rules {
			for _, r := range tr.Columns {
				if r.Use == "" {
					t.Errorf("a rule for %s.%s has no transformer", tr.Table, r.Column)
				}
			}
		}
	})
}
