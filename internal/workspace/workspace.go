// Package workspace gives each stuntdb run a private scratch folder, and finds
// and removes what a run left behind after a crash.
//
// A run's folder holds temporary files, such as the list of row keys to copy,
// and a journal: the run ID, the process ID, and everything the run created
// outside the folder, such as partial output files and staging schemas. The
// folder is removed when the run ends, whether it succeeded or failed. If the
// process dies instead (a hard kill, a power cut), the next run finds the
// journal of a process that no longer exists, and cleans up.
//
// Folders are private to the user, because temporary files can hold row
// keys. Journals never contain connection URLs, which can hold passwords.
package workspace

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	journalName    = "journal.json"
	journalVersion = 1
	dirPerm        = 0o700
	filePerm       = 0o600

	// orphanAge is how old a folder without a readable journal must be before
	// it counts as a leftover, so that a run that is still starting is never
	// mistaken for one.
	orphanAge = time.Hour
)

// Options configures where workspaces live.
type Options struct {
	// Root is the folder that holds every run's folder. Empty means
	// stuntdb/runs in the user's cache folder, which survives reboots so that
	// leftovers can still be found after a power cut.
	Root string
}

func (o Options) root() string {
	if o.Root != "" {
		return o.Root
	}
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	return filepath.Join(base, "stuntdb", "runs")
}

// ItemKind is the kind of thing a run created outside its folder.
type ItemKind string

const (
	// ItemFile is a partial output file or folder, written under a temporary
	// name until the run finishes.
	ItemFile ItemKind = "file"

	// ItemStaging is a staging schema, database or set of tables in a target
	// database.
	ItemStaging ItemKind = "staging"
)

// Item is something a run created outside its folder.
type Item struct {
	Kind   ItemKind `json:"kind"`
	Path   string   `json:"path,omitempty"`   // ItemFile: the partial output's path
	Engine string   `json:"engine,omitempty"` // ItemStaging: the adapter, e.g. "postgres"
	Name   string   `json:"name,omitempty"`   // ItemStaging: the staging object's name
}

func (it Item) validate() error {
	switch {
	case it.Kind == ItemFile && it.Path != "":
	case it.Kind == ItemStaging && it.Engine != "" && it.Name != "":
	default:
		return errors.New("workspace: an item needs a file path, or an engine and a staging name")
	}
	return nil
}

// Journal is what a run writes down so that its leftovers can be found
// after a crash.
type Journal struct {
	Version int       `json:"version"`
	RunID   string    `json:"run_id"`
	PID     int       `json:"pid"`
	Host    string    `json:"host"`
	Started time.Time `json:"started"`
	Items   []Item    `json:"items"`
}

// Workspace is one run's private folder. Create it at the start of a run and
// defer Close.
type Workspace struct {
	id  string
	dir string

	mu      sync.Mutex
	journal Journal
	closed  bool
}

// Create makes a folder for a new run, with a fresh run ID, and writes its journal.
func Create(opts Options) (*Workspace, error) {
	root := opts.root()
	if err := os.MkdirAll(root, dirPerm); err != nil {
		return nil, fmt.Errorf("workspace: %w", err)
	}
	now := time.Now().UTC()
	id, err := newRunID(now)
	if err != nil {
		return nil, fmt.Errorf("workspace: %w", err)
	}
	dir := filepath.Join(root, id)
	if err := os.Mkdir(dir, dirPerm); err != nil {
		return nil, fmt.Errorf("workspace: %w", err)
	}
	host, _ := os.Hostname()
	w := &Workspace{id: id, dir: dir, journal: Journal{
		Version: journalVersion, RunID: id, PID: os.Getpid(), Host: host, Started: now, Items: []Item{},
	}}
	if err := w.writeJournal(); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	return w, nil
}

// ID returns the run ID, such as "20261020-101500-a1b2c3". IDs sort by start time.
func (w *Workspace) ID() string { return w.id }

// Dir returns the workspace folder.
func (w *Workspace) Dir() string { return w.dir }

// Path returns the path of a temporary file in the workspace. name must be a
// plain file name.
func (w *Workspace) Path(name string) (string, error) {
	if name == "" || name == "." || name == ".." || strings.HasPrefix(name, journalName) ||
		strings.ContainsAny(name, `/\`) || filepath.Base(name) != name {
		return "", errors.New("workspace: a temporary file needs a plain name")
	}
	return filepath.Join(w.dir, name), nil
}

// PartialPath returns the name to write an output to until it is complete:
// ".<name>.partial-<run ID>" in the same folder as final, so that renaming it
// into place at the end is atomic.
func (w *Workspace) PartialPath(final string) string {
	return filepath.Join(filepath.Dir(final), "."+filepath.Base(final)+".partial-"+w.id)
}

// Record adds an item to the journal. Record it before creating it, so that a
// crash right after creating it still leaves a record.
func (w *Workspace) Record(it Item) error {
	if err := it.validate(); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return errors.New("workspace: already closed")
	}
	if slices.Contains(w.journal.Items, it) {
		return nil
	}
	w.journal.Items = append(w.journal.Items, it)
	return w.writeJournal()
}

// Forget removes an item from the journal once it is gone or committed.
func (w *Workspace) Forget(it Item) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return errors.New("workspace: already closed")
	}
	w.journal.Items = slices.DeleteFunc(w.journal.Items, func(x Item) bool { return x == it })
	return w.writeJournal()
}

// Close removes the workspace folder and its journal. Defer it as soon as
// Create succeeds, so that it runs on success, on an error, on a panic, and
// when a signal cancels the run's context. It is safe to call more than once.
func (w *Workspace) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	if err := os.RemoveAll(w.dir); err != nil {
		return fmt.Errorf("workspace: %w", err)
	}
	return nil
}

func (w *Workspace) writeJournal() error {
	return writeJournal(w.dir, w.journal)
}

// writeJournal replaces the journal atomically, so that a crash mid-write
// never leaves half a journal.
func writeJournal(dir string, j Journal) error {
	data, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return fmt.Errorf("workspace: %w", err)
	}
	tmp := filepath.Join(dir, journalName+".tmp")
	if err := os.WriteFile(tmp, data, filePerm); err != nil {
		return fmt.Errorf("workspace: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, journalName)); err != nil {
		return fmt.Errorf("workspace: %w", err)
	}
	return nil
}

func readJournal(dir string) (Journal, error) {
	var j Journal
	data, err := os.ReadFile(filepath.Join(filepath.Clean(dir), journalName))
	if err != nil {
		return j, err
	}
	if err := json.Unmarshal(data, &j); err != nil {
		return j, err
	}
	if j.Version != journalVersion || j.RunID == "" {
		return j, errors.New("workspace: unknown journal format")
	}
	return j, nil
}

func newRunID(now time.Time) (string, error) {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return now.Format("20060102-150405") + "-" + hex.EncodeToString(b[:]), nil
}

// Leftover is the folder of a run whose process ended without removing it.
type Leftover struct {
	Dir string

	// Journal is the zero value when the folder had no readable journal.
	Journal Journal
}

// FindLeftovers returns the folders of runs on this machine whose process no
// longer exists. Runs that are still going are never returned. A reused
// process ID can make a dead run look alive, which only delays its cleanup.
func FindLeftovers(opts Options) ([]Leftover, error) {
	root := opts.root()
	entries, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("workspace: %w", err)
	}
	host, _ := os.Hostname()
	var leftovers []Leftover
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		j, err := readJournal(dir)
		if err != nil {
			// No readable journal: a run that is still starting, or one that
			// died before writing it. Only an old folder is a leftover.
			if info, statErr := e.Info(); statErr == nil && time.Since(info.ModTime()) > orphanAge {
				leftovers = append(leftovers, Leftover{Dir: dir})
			}
			continue
		}
		if j.Host != host || processAlive(j.PID) {
			continue
		}
		leftovers = append(leftovers, Leftover{Dir: dir, Journal: j})
	}
	return leftovers, nil
}

// Clean removes the leftover's partial output files. For safety, it removes a
// recorded file only if its name is a partial output of that same run, so a
// damaged or edited journal cannot make it delete anything else.
//
// Staging objects live in databases, and removing them needs the target and
// the user's confirmation. If there are any, Clean keeps the journal with
// just those and returns them; call Discard once they are dealt with.
// Otherwise it removes the whole folder.
func (l Leftover) Clean() (staging []Item, err error) {
	var problems []error
	for _, it := range l.Journal.Items {
		switch it.Kind {
		case ItemFile:
			if !isPartialOf(it.Path, l.Journal.RunID) {
				problems = append(problems, fmt.Errorf("workspace: not removing %s: it is not a partial output of run %s", it.Path, l.Journal.RunID))
				continue
			}
			if err := os.RemoveAll(it.Path); err != nil {
				problems = append(problems, fmt.Errorf("workspace: %w", err))
			}
		case ItemStaging:
			staging = append(staging, it)
		}
	}
	if len(staging) > 0 {
		j := l.Journal
		j.Version, j.Items = journalVersion, staging
		if err := writeJournal(l.Dir, j); err != nil {
			problems = append(problems, err)
		}
		return staging, errors.Join(problems...)
	}
	if err := l.Discard(); err != nil {
		problems = append(problems, err)
	}
	return nil, errors.Join(problems...)
}

// Discard removes the leftover's folder, journal included.
func (l Leftover) Discard() error {
	if err := os.RemoveAll(l.Dir); err != nil {
		return fmt.Errorf("workspace: %w", err)
	}
	return nil
}

func isPartialOf(path, runID string) bool {
	base := filepath.Base(path)
	return runID != "" && strings.HasPrefix(base, ".") && strings.HasSuffix(base, ".partial-"+runID) && len(base) > len(".partial-"+runID)+1
}
