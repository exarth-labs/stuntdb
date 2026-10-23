package app

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/exarth-labs/stuntdb/internal/classify"
	"github.com/exarth-labs/stuntdb/internal/config"
	"github.com/exarth-labs/stuntdb/internal/errs"
	"github.com/exarth-labs/stuntdb/internal/outfile"
	"github.com/exarth-labs/stuntdb/schemas"
)

// InitRequest asks for a starter configuration.
type InitRequest struct {
	Env

	// ConfigPath is the file to create. Empty means config.DefaultPath. It
	// must not exist yet.
	ConfigPath string

	// SourceURL is the source database, given on the command line. It must
	// not hold a password. Empty means STUNTDB_SOURCE_URL, or the user's
	// defaults.
	SourceURL string
}

// InitResult says what Init wrote.
type InitResult struct {
	Path         string `json:"path"`
	Tables       int    `json:"tables"`
	Columns      int    `json:"columns"`
	Suggested    int    `json:"suggested"`     // columns written with a suggested rule, to review
	NoSuggestion int    `json:"no_suggestion"` // columns written without one
	NoRuleNeeded int    `json:"no_rule_needed"`
}

// Init connects to the source database, classifies every column, and
// writes a starter configuration. Each suggested rule is written as a
// comment, so that none is used until someone reviews it. Init never
// replaces an existing file.
func Init(ctx context.Context, req InitRequest) (*InitResult, error) {
	path := cmp.Or(req.ConfigPath, config.DefaultPath)
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		return nil, errs.E(errs.Output, existsError(path, err))
	}
	opts := config.LoadOptions{Path: path, NoProject: true}
	if req.SourceURL != "" {
		opts.Flags = map[string]string{"source.url": req.SourceURL}
	}
	cfg, err := req.loadConfig(opts)
	if err != nil {
		return nil, err
	}
	src, err := req.readSource(ctx, cfg)
	if err != nil {
		return nil, err
	}
	// Without rules there is nothing to check yet: "stuntdb check" reports
	// any problem once the rules are written.
	decisions, _ := decide(src, classify.Default().Database(src.db), nil, config.PolicyFail)

	w := starter{url: req.sourceURLSetting()}
	res := &InitResult{Path: path, Columns: len(decisions)}
	for _, ns := range src.db.Namespaces {
		res.Tables += len(ns.Tables)
	}
	for _, dec := range decisions {
		switch {
		case !needsRule(dec):
			res.NoRuleNeeded++
		case dec.Match.Class == classify.Unclassified:
			res.NoSuggestion++
		default:
			res.Suggested++
		}
	}
	if err := writeNew(ctx, path, w.render(decisions)); err != nil {
		return nil, err
	}
	return res, nil
}

// sourceURLSetting returns what the starter file says for source.url: the
// URL itself when the command line gave it, which then has no password; a
// reference to the environment variable when that gave it; and nothing when
// the user's own defaults give it.
func (r InitRequest) sourceURLSetting() string {
	env := config.EnvName("source.url")
	switch _, fromEnv := r.lookupEnv(env); {
	case r.SourceURL != "":
		// $ starts ${NAME} in this setting, and $$ stands for a single $.
		return yamlScalar(strings.ReplaceAll(r.SourceURL, "$", "$$"))
	case fromEnv:
		return "${" + env + "}"
	default:
		return ""
	}
}

func existsError(path string, statErr error) error {
	if statErr != nil {
		return fmt.Errorf("cannot check whether %s exists: %w", path, statErr)
	}
	return fmt.Errorf("%s already exists; edit it, or remove it to start over", path)
}

// writeNew writes a new file atomically. It never replaces an existing one,
// even one created while it writes.
func writeNew(ctx context.Context, path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".partial-*")
	if err != nil {
		return errs.E(errs.Output, fmt.Errorf("write %s: %w", path, err))
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmp.Name())
		}
	}()
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		return errs.E(errs.Output, fmt.Errorf("write %s: %w", path, err))
	}
	if err := outfile.CommitNew(ctx, tmp.Name(), path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return errs.E(errs.Output, existsError(path, nil))
		}
		return errs.E(errs.Output, fmt.Errorf("write %s: %w", path, err))
	}
	committed = true
	return nil
}

// starter writes the starter configuration.
type starter struct {
	url string // the value of source.url; empty to leave it to the user's defaults
	buf bytes.Buffer
}

func (w *starter) line(format string, args ...any) {
	fmt.Fprintf(&w.buf, format+"\n", args...)
}

func (w *starter) render(decisions []*Decision) []byte {
	w.line(`# stuntdb configuration, written by "stuntdb init". Editors with YAML`)
	w.line(`# support check it, and complete settings, with the schema below.`)
	w.line(`# yaml-language-server: $schema=%s`, schemas.ConfigURL)
	w.line(`#`)
	w.line(`# Next steps:`)
	w.line(`#   1. Review the masking rules under "columns".`)
	w.line(`#   2. Run "stuntdb check --update" to write stuntdb.lock.`)
	w.line(`#   3. Commit both files, and run "stuntdb check" in CI.`)
	w.line(``)
	w.line(`version: 1`)
	w.line(``)
	if w.url != "" {
		w.line(`source:`)
		w.line(`  url: %s`, w.url)
	} else {
		w.line(`# source.url comes from your own defaults, in stuntdb's folder of your`)
		w.line(`# user configuration folder.`)
	}
	w.line(``)
	w.line(`# Where "stuntdb run" writes the masked copy. The flag --to overrides it.`)
	w.line(`# output:`)
	w.line(`#   to: sqlite:./dev.db`)
	w.line(``)
	w.line(`masking:`)
	w.line(`  # The masking key: 32 or more random characters from your secret store,`)
	w.line(`  # such as the output of "openssl rand -base64 32". Only "stuntdb run"`)
	w.line(`  # needs it.`)
	w.line(`  key: ${%s}`, config.EnvName("masking.key"))
	w.line(`  # A column without a rule stops stuntdb until someone reviews it.`)
	w.line(`  unclassified: fail`)
	w.line(``)
	w.columns(decisions)
	return w.buf.Bytes()
}

// suggestedRule is one commented-out rule of the starter configuration.
type suggestedRule struct {
	key     string // the column, as YAML writes it
	value   string // the suggested rule; empty when there is none
	comment string
}

// short writes the rule in the short form, "key: value".
func (r suggestedRule) short() string {
	if r.value == "" {
		return r.key + ":"
	}
	return r.key + ": " + r.value
}

// columns writes a commented-out rule for every column that needs one.
func (w *starter) columns(decisions []*Decision) {
	type table struct {
		key   string // the table, as YAML writes it
		rules []suggestedRule
	}
	var tables []*table
	for _, dec := range decisions {
		if !needsRule(dec) {
			continue
		}
		key := yamlScalar(tableName(dec.Column.Table).String())
		if len(tables) == 0 || tables[len(tables)-1].key != key {
			tables = append(tables, &table{key: key})
		}
		t := tables[len(tables)-1]
		r := suggestedRule{key: yamlScalar(string(dec.Column.Column)), comment: "no suggestion: write keep, null or a transformer"}
		if m := dec.Match; m.Class != classify.Unclassified {
			r.value = ruleText(m)
			r.comment = fmt.Sprintf("%s, rule %s (%s)", m.Class, m.Rule, strconv.FormatFloat(m.Confidence, 'f', 2, 64))
			if m.Class == classify.Secret {
				r.comment += "; secrets are always replaced"
			}
		}
		t.rules = append(t.rules, r)
	}

	if len(tables) == 0 {
		w.line(`# Every column is decided without a rule, so there are no masking rules`)
		w.line(`# to review. Add them under "columns:" to mask a column differently.`)
		return
	}
	w.line(`# Masking rules, by table and column. stuntdb suggested each rule below`)
	w.line(`# from the column's name and type, and uses none of them until you accept`)
	w.line(`# it: review each line, change it if needed, and remove the "# " in front`)
	w.line(`# of it. Write keep to copy a column as it is, or null to empty it.`)
	w.line(`#`)
	w.line(`# Columns that are not listed need no rule: keys the database assigns,`)
	w.line(`# record timestamps and other safe columns are kept, foreign keys are`)
	w.line(`# masked like the columns they reference, and generated columns are`)
	w.line(`# computed again.`)
	w.line(`columns:`)
	for _, t := range tables {
		if shortKey(t.key) {
			w.line(`  %s:`, t.key)
		} else {
			w.line(`  ? %s`, t.key)
			w.line(`  :`)
		}
		// The comments line up, except after a key too long for one line.
		width := 0
		for _, r := range t.rules {
			if shortKey(r.key) {
				width = max(width, utf8.RuneCountInString(r.short()))
			}
		}
		for _, r := range t.rules {
			if shortKey(r.key) {
				pad := strings.Repeat(" ", width-utf8.RuneCountInString(r.short()))
				w.line(`    # %s%s  # %s`, r.short(), pad, r.comment)
				continue
			}
			value := ""
			if r.value != "" {
				value = " " + r.value
			}
			w.line(`    # ? %s`, r.key)
			w.line(`    # :%s  # %s`, value, r.comment)
		}
	}
}
