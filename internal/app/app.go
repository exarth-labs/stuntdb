// Package app holds stuntdb's use cases: init, check, and later plan, run
// and restore. The command line, the Go API and the MCP server all call
// them, so that they can never drift apart. Each use case takes a request
// and returns a result, which the caller renders as text or JSON.
//
// Every error carries an errs.Kind, which decides the exit code.
package app

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/exarth-labs/stuntdb/dialect"
	"github.com/exarth-labs/stuntdb/internal/config"
	"github.com/exarth-labs/stuntdb/internal/errs"
	"github.com/exarth-labs/stuntdb/internal/redact"
	"github.com/exarth-labs/stuntdb/internal/schema"
)

// GeneratorVersion is the version of the fake-value generators, which
// decides the masked values. The lockfile records it, so that an upgrade of
// stuntdb never changes masked values without a reviewed lockfile change.
// It moves to the masking package with the generators themselves.
const GeneratorVersion = 1

// appName identifies stuntdb in the database server's list of sessions.
const appName = "stuntdb"

// ErrNoSource is returned when no setting gives the source database URL.
var ErrNoSource = errors.New("no source database URL")

// Env is what the use cases need from outside: where adapters and settings
// come from, and the redactor that learns every secret they handle.
type Env struct {
	// Lookup finds the adapter for a database URL. Nil means dialect.Lookup,
	// which knows the adapters that cmd/stuntdb registers.
	Lookup func(url string) (dialect.Dialect, error)

	// LookupEnv reads environment variables. Nil means os.LookupEnv.
	LookupEnv func(name string) (string, bool)

	// UserConfig is the user's own defaults. Empty means config.yaml in
	// stuntdb's folder of the user's configuration folder; "-" means none.
	UserConfig string

	// Redactor learns every secret the use case reads, such as the source
	// URL, so that no error or log can show it. Nil means none.
	Redactor *redact.Redactor
}

func (e Env) lookup(url string) (dialect.Dialect, error) {
	if e.Lookup != nil {
		return e.Lookup(url)
	}
	return dialect.Lookup(url)
}

func (e Env) lookupEnv(name string) (string, bool) {
	if e.LookupEnv != nil {
		return e.LookupEnv(name)
	}
	return os.LookupEnv(name)
}

// loadConfig reads the configuration with the environment's settings.
func (e Env) loadConfig(opts config.LoadOptions) (*config.Config, error) {
	opts.UserPath = e.UserConfig
	opts.LookupEnv = e.lookupEnv
	opts.Redactor = e.Redactor
	return config.Load(opts)
}

// source is what the use cases learn from the source database.
type source struct {
	db   *schema.Database
	caps dialect.Capabilities
}

// readSource connects to the source database, read-only, and reads its
// structure. It reads no rows.
func (e Env) readSource(ctx context.Context, cfg *config.Config) (*source, error) {
	url, err := cfg.Source.URL.Get()
	if err != nil {
		if !cfg.Source.URL.IsSet() {
			return nil, errs.E(errs.Usage, fmt.Errorf("%w: set %s", ErrNoSource, config.EnvName("source.url")))
		}
		return nil, err
	}
	d, err := e.lookup(url)
	if err != nil {
		return nil, errs.E(errs.Usage, fmt.Errorf("source database: %w", err))
	}
	conn, err := d.Open(ctx, url, dialect.OpenOptions{
		Role:             dialect.Source,
		AppName:          appName,
		MaxConns:         1,
		StatementTimeout: cfg.Source.StatementTimeout,
	})
	if err != nil {
		return nil, errs.E(errs.Connect, fmt.Errorf("connect to the source database: %w", err))
	}
	defer func() { _ = conn.Close() }()
	db, err := conn.Introspect(ctx, dialect.Filter{})
	if err != nil {
		return nil, errs.E(errs.Connect, fmt.Errorf("read the source database's structure: %w", err))
	}
	// The rest of stuntdb relies on a valid structure, such as foreign keys
	// with as many columns as they reference. An invalid one is a bug in
	// the adapter.
	if err := db.Validate(); err != nil {
		return nil, fmt.Errorf("the %s adapter read an invalid structure: %w", d.Name(), err)
	}
	return &source{db: db, caps: conn.Capabilities()}, nil
}

// Problem is one reason a check fails. It names tables and columns, which
// are structure, and never shows a row value.
type Problem struct {
	// At is where the problem is, such as "stuntdb.yaml:12:5" or
	// "stuntdb.lock"; empty when it concerns the database itself.
	At      string `json:"at,omitempty"`
	Message string `json:"message"`
}

func (p Problem) String() string {
	if p.At == "" {
		return p.Message
	}
	return p.At + ": " + p.Message
}
