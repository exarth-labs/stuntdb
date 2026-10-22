// Package config loads stuntdb's configuration: the project's stuntdb.yaml,
// the user's own defaults, STUNTDB_* environment variables and command-line
// flags, in that order of increasing precedence. It also reads and writes
// the lockfile, stuntdb.lock.
//
// Every file is checked against the published JSON Schema
// (schemas/stuntdb.schema.json), so that editors and stuntdb agree on what
// is valid. Mistakes are reported all at once, each with its file, line and
// column, and never with the value, which may be a secret.
//
// Secrets never live in a file. ${NAME} takes a value from an environment
// variable in the four settings that may hold one (source.url, output.to,
// masking.key and masking.key_file); $$ stands for a single $ there. A
// masking key, or a URL with a password, written straight into a file is an
// error. A variable that is not set fails only when the setting is used, so
// that "stuntdb check" can run in CI without the masking key.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/exarth-labs/stuntdb/internal/errs"
	"github.com/exarth-labs/stuntdb/internal/redact"
)

// DefaultPath is the project configuration file, in the working folder.
const DefaultPath = "stuntdb.yaml"

// MinKeyLength is the shortest masking key accepted.
const MinKeyLength = 32

// ErrNoConfig is returned when the project configuration file does not exist.
var ErrNoConfig = errors.New("config: no configuration file")

// Policy says what happens to a column that no rule covers.
type Policy string

const (
	PolicyFail Policy = "fail" // stop until someone reviews the column (the default)
	PolicyNull Policy = "null" // write NULL
	PolicyKeep Policy = "keep" // copy the value as it is
)

// Config is the loaded configuration.
type Config struct {
	// Path is the project configuration file.
	Path string

	Source  Source
	Output  Output
	Masking Masking

	// Columns holds the masking rules, in the order the file lists them.
	Columns []*TableRules

	redactor *redact.Redactor
}

// Source is the database to copy.
type Source struct {
	URL              Setting
	MaxConnections   int           // 0 means the adapter's default
	StatementTimeout time.Duration // 0 means no limit
}

// Output is where the copy goes.
type Output struct {
	To Setting // a database URL
}

// Masking holds the masking settings.
type Masking struct {
	key, keyFile Setting
	dir          string // the folder key_file is relative to
	redactor     *redact.Redactor

	Unclassified Policy
}

// TableRules are the masking rules for one table.
type TableRules struct {
	Table   TableName
	At      Position
	Columns []*ColumnRule
}

// ColumnRule is the masking rule for one column. The check step validates
// the transformer name and its options against the transformer catalog.
type ColumnRule struct {
	Column  string
	At      Position
	Use     string         // keep, null, or a transformer name
	Domain  string         // empty means namespace.table.column
	Options map[string]any // the transformer's own options, as written
}

// Setting is a text setting that may come from an environment variable.
type Setting struct {
	value string
	from  Position
	set   bool
	err   error // an environment variable it names is not set
}

// Get returns the setting's value, or an error if it is not set or names an
// environment variable that is not set.
func (s Setting) Get() (string, error) {
	if s.err != nil {
		return "", s.err
	}
	if !s.set {
		return "", errs.E(errs.Usage, errors.New("config: the setting is not set"))
	}
	return s.value, nil
}

// IsSet reports whether any file, variable or flag gives the setting.
func (s Setting) IsSet() bool { return s.set }

// From returns where the setting's value comes from, such as
// "stuntdb.yaml:4:8" or "environment variable STUNTDB_SOURCE_URL".
func (s Setting) From() Position { return s.from }

// LoadOptions says where to find the configuration.
type LoadOptions struct {
	// Path is the project configuration file. Empty means DefaultPath.
	Path string

	// UserPath is the user's own defaults. Empty means config.yaml in
	// stuntdb's folder of the user's configuration folder; "-" means none.
	UserPath string

	// LookupEnv reads environment variables. Nil means os.LookupEnv.
	LookupEnv func(string) (string, bool)

	// Flags are values from command-line flags, by setting name, such as
	// "output.to". They take precedence over everything else.
	Flags map[string]string

	// Redactor learns every secret the configuration holds, so that no error
	// or log can show it. Nil means none.
	Redactor *redact.Redactor
}

// settings are the single-value settings, which every layer can give. Each
// can also come from an environment variable, named after it:
// source.url is STUNTDB_SOURCE_URL.
var settings = []struct {
	name        string
	interpolate bool // may use ${NAME}
	secret      bool // must never be written in a file
	url         bool // a URL, which must not carry a password in a file or flag
}{
	{name: "source.url", interpolate: true, url: true},
	{name: "source.max_connections"},
	{name: "source.statement_timeout"},
	{name: "output.to", interpolate: true, url: true},
	{name: "masking.key", interpolate: true, secret: true},
	{name: "masking.key_file", interpolate: true},
	{name: "masking.unclassified"},
}

// EnvName returns the environment variable for a setting.
func EnvName(setting string) string {
	return "STUNTDB_" + strings.ToUpper(strings.ReplaceAll(setting, ".", "_"))
}

// raw is one setting's value as a layer gives it, before it is checked.
type raw struct {
	text   string
	from   Position
	inFile bool // from a file, so ${NAME} applies and secrets are refused
	flag   bool
}

// Load reads the configuration. Its errors carry errs.Usage.
func Load(opts LoadOptions) (*Config, error) {
	cfg, err := load(opts)
	return cfg, errs.E(errs.Usage, err)
}

func load(opts LoadOptions) (*Config, error) {
	lookup := opts.LookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}
	path := opts.Path
	if path == "" {
		path = DefaultPath
	}
	cfg := &Config{Path: path, redactor: opts.Redactor, Masking: Masking{dir: filepath.Dir(path), redactor: opts.Redactor}}
	values := map[string]raw{}

	if user := userPath(opts.UserPath); user != "" {
		f, err := readFile(user, false)
		if err != nil && !errors.Is(err, ErrNoConfig) {
			return nil, err
		}
		if f != nil {
			f.collect(values)
		}
	}
	f, err := readFile(path, true)
	if err != nil {
		return nil, err
	}
	f.collect(values)
	if cfg.Columns, err = f.columns(); err != nil {
		return nil, err
	}

	for _, s := range settings {
		env := EnvName(s.name)
		if v, ok := lookup(env); ok {
			override(values, s.name, raw{text: v, from: Position{File: "environment variable " + env}})
		}
	}
	for name, v := range opts.Flags {
		if !knownSetting(name) {
			return nil, fmt.Errorf("config: unknown setting %s", name)
		}
		override(values, name, raw{text: v, from: Position{File: "flag for " + name}, flag: true})
	}
	return cfg, cfg.apply(values, lookup)
}

// override sets a value, and drops the other way of giving the masking key
// from lower layers, so that the key and key_file never conflict.
func override(values map[string]raw, name string, v raw) {
	switch name {
	case "masking.key":
		delete(values, "masking.key_file")
	case "masking.key_file":
		delete(values, "masking.key")
	}
	values[name] = v
}

func knownSetting(name string) bool {
	for _, s := range settings {
		if s.name == name {
			return true
		}
	}
	return false
}

func userPath(p string) string {
	switch p {
	case "-":
		return ""
	case "":
		dir, err := os.UserConfigDir()
		if err != nil {
			return ""
		}
		return filepath.Join(dir, "stuntdb", "config.yaml")
	default:
		return p
	}
}

// readFile reads, parses and validates one file. A missing file is
// ErrNoConfig.
func readFile(path string, project bool) (*file, error) {
	data, err := os.ReadFile(path) //nolint:gosec // the user's own configuration file
	if errors.Is(err, fs.ErrNotExist) {
		if project {
			return nil, fmt.Errorf("%w: %s does not exist; create one with \"stuntdb init\", or name another with --config", ErrNoConfig, path)
		}
		return nil, ErrNoConfig
	}
	if err != nil {
		return nil, fmt.Errorf("config: cannot read %s: %w", path, err)
	}
	f, err := parseFile(path, data)
	if err != nil {
		return nil, err
	}
	// The schema's problems and the secrets in the file are reported
	// together, so that a password written in the file is never hidden
	// behind a typo.
	ps, err := f.schemaProblems()
	if err != nil {
		return nil, err
	}
	ps = append(ps, f.secretProblems()...)
	if err := ps.err(); err != nil {
		return nil, err
	}
	if !project {
		if n := f.lookup("columns"); n != nil {
			return nil, fmt.Errorf("%s: masking rules belong in the project's %s, not in the user's defaults", f.pos(n), DefaultPath)
		}
	}
	return f, nil
}

// secretProblems reports secrets written straight into the file: a masking
// key, or a URL with a password, that doesn't come from ${NAME}.
func (f *file) secretProblems() problems {
	var ps problems
	for _, s := range settings {
		n := f.lookup(s.name)
		if n == nil || n.Kind != yaml.ScalarNode || strings.Contains(n.Value, "${") {
			continue
		}
		switch {
		case s.secret && n.Value != "":
			ps.add(f.pos(n), "%s is written in the file; take it from an environment variable, as in ${%s}, or use masking.key_file", s.name, EnvName(s.name))
		case s.url && hasPassword(n.Value):
			ps.add(f.pos(n), "%s holds a password; take the URL from an environment variable, as in ${%s}", s.name, EnvName(s.name))
		}
	}
	return ps
}

// lookup returns the node at a dotted setting name, or nil.
func (f *file) lookup(name string) *yaml.Node {
	n := f.root
	for part := range strings.SplitSeq(name, ".") {
		var next *yaml.Node
		for i := 0; n.Kind == yaml.MappingNode && i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == part {
				next = n.Content[i+1]
				break
			}
		}
		if next == nil {
			return nil
		}
		n = next
	}
	return n
}

// collect adds the file's single-value settings to values. An empty value
// (key: with nothing after it) sets nothing; the word null is a value.
func (f *file) collect(values map[string]raw) {
	for _, s := range settings {
		n := f.lookup(s.name)
		if n == nil || scalar(n) == nil {
			continue
		}
		override(values, s.name, raw{text: n.Value, from: f.pos(n), inFile: true})
	}
}

// apply checks every collected value and stores it in cfg.
func (cfg *Config) apply(values map[string]raw, lookup func(string) (string, bool)) error {
	var ps problems
	for _, s := range settings {
		v, ok := values[s.name]
		if !ok {
			continue
		}
		text, missing := v.text, ""
		if s.interpolate && v.inFile {
			var err error
			if text, missing, err = interpolate(v.text, lookup); err != nil {
				ps.add(v.from, "%s: %v", s.name, err)
				continue
			}
		}
		var err error
		if missing != "" {
			err = errs.E(errs.Usage, fmt.Errorf("%s: %s uses ${%s}, which is not set", v.from, s.name, missing))
		}
		// Secrets written in files were refused when the file was read.
		switch {
		case s.secret && v.flag:
			ps.add(v.from, "%s must never be given as a flag; set %s instead", s.name, EnvName(s.name))
			continue
		case s.url && v.flag && hasPassword(text):
			ps.add(v.from, "%s holds a password; take the URL from an environment variable, %s, instead of a flag", s.name, EnvName(s.name))
			continue
		}
		setting := Setting{value: text, from: v.from, set: true, err: err}
		if err == nil && cfg.redactor != nil && (s.url || s.secret) {
			if s.url {
				cfg.redactor.AddURL(text)
			} else {
				cfg.redactor.Add(text)
			}
		}
		if problem := cfg.store(s.name, setting); problem != "" {
			ps.add(v.from, "%s %s", s.name, problem)
		}
	}
	if cfg.Masking.Unclassified == "" {
		cfg.Masking.Unclassified = PolicyFail
	}
	return ps.err()
}

// store puts one checked setting in place, and returns what is wrong with
// it, if anything. Files were checked by the schema already; environment
// variables and flags are checked here.
func (cfg *Config) store(name string, s Setting) string {
	switch name {
	case "source.url":
		cfg.Source.URL = s
	case "output.to":
		cfg.Output.To = s
	case "masking.key":
		cfg.Masking.key = s
	case "masking.key_file":
		cfg.Masking.keyFile = s
	case "source.max_connections":
		n, err := strconv.Atoi(s.value)
		if err != nil || n < 1 || n > 64 {
			return "must be a whole number from 1 to 64"
		}
		cfg.Source.MaxConnections = n
	case "source.statement_timeout":
		d, err := time.ParseDuration(s.value)
		if err != nil || d <= 0 {
			return "must be a duration, such as 500ms, 30s, 5m or 1h30m"
		}
		cfg.Source.StatementTimeout = d
	case "masking.unclassified":
		switch p := Policy(s.value); p {
		case PolicyFail, PolicyNull, PolicyKeep:
			cfg.Masking.Unclassified = p
		default:
			return "must be one of: fail, null, keep"
		}
	}
	return ""
}

// hasPassword reports whether a URL carries a password.
func hasPassword(s string) bool {
	u, err := url.Parse(s)
	if err != nil || u.User == nil {
		return false
	}
	_, ok := u.User.Password()
	return ok
}

// Key returns the masking key, from masking.key or the file masking.key_file
// names. It is needed only to mask, so a missing key fails here and not when
// the configuration loads.
func (m Masking) Key() ([]byte, error) {
	var key string
	switch {
	case m.key.IsSet():
		v, err := m.key.Get()
		if err != nil {
			return nil, err
		}
		key = v
	case m.keyFile.IsSet():
		path, err := m.keyFile.Get()
		if err != nil {
			return nil, err
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(m.dir, path)
		}
		data, err := os.ReadFile(path) //nolint:gosec // the key file the user named
		if err != nil {
			return nil, errs.E(errs.Usage, fmt.Errorf("config: cannot read the masking key file (from %s): %w", m.keyFile.From(), pathless(err)))
		}
		key = strings.TrimRight(string(data), "\r\n")
	default:
		return nil, errs.E(errs.Usage, fmt.Errorf("config: no masking key; set %s, or masking.key_file", EnvName("masking.key")))
	}
	if m.redactor != nil {
		m.redactor.Add(key)
	}
	if len(key) < MinKeyLength {
		return nil, errs.E(errs.Usage, fmt.Errorf("config: the masking key must have at least %d characters; make one with: openssl rand -base64 32", MinKeyLength))
	}
	return []byte(key), nil
}

func pathless(err error) error {
	if e, ok := errors.AsType[*fs.PathError](err); ok {
		return e.Err
	}
	return err
}
