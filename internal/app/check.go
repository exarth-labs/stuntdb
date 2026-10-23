package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/exarth-labs/stuntdb/internal/buildinfo"
	"github.com/exarth-labs/stuntdb/internal/classify"
	"github.com/exarth-labs/stuntdb/internal/config"
	"github.com/exarth-labs/stuntdb/internal/errs"
	"github.com/exarth-labs/stuntdb/internal/schema"
)

// CheckRequest asks to check the configuration against the source
// database and the lockfile.
type CheckRequest struct {
	Env

	// ConfigPath is the project configuration. Empty means
	// config.DefaultPath. The lockfile is next to it.
	ConfigPath string

	// Update writes the lockfile from the current decisions, instead of
	// comparing them with it. It writes nothing if the configuration has
	// mistakes.
	Update bool
}

// CheckResult is what a check found.
type CheckResult struct {
	Config          string    `json:"config"`
	Lockfile        string    `json:"lockfile"`
	LockfileWritten bool      `json:"lockfile_written"`
	Tables          int       `json:"tables"`
	Columns         int       `json:"columns"`
	Decisions       Tally     `json:"decisions"`
	Problems        []Problem `json:"problems"`
}

// Tally counts the columns by the reason for their decision.
type Tally struct {
	Configured int `json:"configured"` // a rule in stuntdb.yaml
	Safe       int `json:"safe"`       // a safe classification keeps it
	References int `json:"references"` // a foreign key, masked like what it references
	Policy     int `json:"policy"`     // masking.unclassified
	Generated  int `json:"generated"`  // the target computes it again
	Unreviewed int `json:"unreviewed"` // needs a decision
	Waiting    int `json:"waiting"`    // a foreign key that waits for the column it references
}

func (t *Tally) add(dec *Decision) {
	switch dec.Reason {
	case Configured:
		t.Configured++
	case SafeRule:
		t.Safe++
	case Reference:
		t.References++
	case Policy:
		t.Policy++
	case Computed:
		t.Generated++
	default:
		if dec.Follows != nil {
			t.Waiting++
		} else {
			t.Unreviewed++
		}
	}
}

// Check compares the configuration with the source database's structure
// and with the lockfile. It fails closed: a column without a decision, a
// rule that is not allowed, and any difference from the lockfile are all
// problems. When it finds any, it returns the result together with an
// error of kind errs.CheckFailed.
func Check(ctx context.Context, req CheckRequest) (*CheckResult, error) {
	cfg, err := req.loadConfig(config.LoadOptions{Path: req.ConfigPath})
	if err != nil {
		return nil, err
	}
	src, err := req.readSource(ctx, cfg)
	if err != nil {
		return nil, err
	}
	rules, problems := resolveRules(src, cfg.Columns)
	decisions, mistakes := decide(src, classify.Default().Database(src.db), rules, cfg.Masking.Unclassified)
	problems = append(problems, mistakes...)

	res := &CheckResult{
		Config:   cfg.Path,
		Lockfile: filepath.Join(filepath.Dir(cfg.Path), config.LockPath),
		Columns:  len(decisions),
	}
	for _, ns := range src.db.Namespaces {
		res.Tables += len(ns.Tables)
	}
	for _, dec := range decisions {
		res.Decisions.add(dec)
	}

	lock := newLock(src.db, decisions)
	switch {
	case req.Update && len(problems) == 0:
		if err := lock.Write(ctx, res.Lockfile); err != nil {
			return nil, err
		}
		res.LockfileWritten = true
	case !req.Update:
		drift, err := lockProblems(res.Lockfile, lock)
		if err != nil {
			return nil, err
		}
		problems = append(problems, drift...)
	}
	problems = append(problems, unreviewedProblems(decisions)...)

	res.Problems = append([]Problem{}, problems...)
	if len(problems) > 0 {
		return res, errs.E(errs.CheckFailed, fmt.Errorf("check failed: %s", plural(len(problems), "problem")))
	}
	return res, nil
}

// newLock builds the lockfile for the decisions.
func newLock(db *schema.Database, decisions []*Decision) *config.Lock {
	lock := &config.Lock{
		GeneratedBy:       "stuntdb " + buildinfo.Get().Version,
		GeneratorVersion:  GeneratorVersion,
		SchemaFingerprint: db.Fingerprint(),
	}
	var table *config.LockTable
	for _, dec := range decisions {
		name := tableName(dec.Column.Table)
		if table == nil || table.Table != name {
			table = &config.LockTable{Table: name}
			lock.Tables = append(lock.Tables, table)
		}
		c := &config.LockColumn{Column: string(dec.Column.Column), Decision: dec.Use}
		if dec.Use != config.Unreviewed && dec.Use != config.Generated {
			c.Source = dec.Source
		}
		table.Columns = append(table.Columns, c)
	}
	return lock
}

// lockProblems compares the decisions with the lockfile at path. Every
// difference is a problem: a change must go through "stuntdb check
// --update", so that it shows up in review.
func lockProblems(path string, cur *config.Lock) ([]Problem, error) {
	old, err := config.ReadLock(path)
	if errors.Is(err, config.ErrNoLock) {
		return []Problem{{At: path, Message: `the lockfile does not exist; run "stuntdb check --update", review it, and commit it`}}, nil
	}
	if err != nil {
		return nil, err
	}

	var out []Problem
	add := func(at, format string, args ...any) {
		out = append(out, Problem{At: at, Message: fmt.Sprintf(format, args...)})
	}
	const update = `run "stuntdb check --update" and review the change`
	if old.GeneratorVersion != cur.GeneratorVersion {
		add(path, "generator_version is %d, but this stuntdb masks with version %d, which gives different values; %s", old.GeneratorVersion, cur.GeneratorVersion, update)
	}
	if old.SchemaFingerprint != cur.SchemaFingerprint {
		add(path, "the database's structure changed since the lockfile was written; %s", update)
	}

	before := map[schema.ColumnRef]*config.LockColumn{}
	var order []schema.ColumnRef
	for _, t := range old.Tables {
		for _, c := range t.Columns {
			ref := columnRef(t.Table, c.Column)
			before[ref] = c
			order = append(order, ref)
		}
	}
	now := map[schema.ColumnRef]bool{}
	for _, t := range cur.Tables {
		for _, c := range t.Columns {
			ref := columnRef(t.Table, c.Column)
			now[ref] = true
			switch o := before[ref]; {
			case o == nil:
				add(path, "%s is new: %s; %s", ref, describe(c), update)
			case o.Decision != c.Decision || o.Source != c.Source:
				add(o.At.String(), "%s changed: %s, was %s; %s", ref, describe(c), describe(o), update)
			}
		}
	}
	for _, ref := range order {
		if !now[ref] {
			add(before[ref].At.String(), "%s is no longer in the database; %s", ref, update)
		}
	}
	return out, nil
}

// describe writes a lockfile decision for a message.
func describe(c *config.LockColumn) string {
	if c.Source == "" {
		return c.Decision
	}
	return c.Decision + " (" + c.Source + ")"
}

func tableName(ref schema.TableRef) config.TableName {
	return config.TableName{Namespace: string(ref.Namespace), Name: string(ref.Name)}
}

func columnRef(t config.TableName, column string) schema.ColumnRef {
	return schema.ColumnRef{Table: schema.TableRef{Namespace: schema.Ident(t.Namespace), Name: schema.Ident(t.Name)}, Column: schema.Ident(column)}
}

// plural writes a count and a noun, such as "1 problem" or "3 problems".
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
