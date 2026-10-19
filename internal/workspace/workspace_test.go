package workspace

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Some tests run this test binary again as a child process that plays the
// part of a stuntdb run, so that they can interrupt it or kill it.
const (
	helperEnv = "STUNTDB_WORKSPACE_HELPER"
	rootEnv   = "STUNTDB_WORKSPACE_ROOT"
)

func TestMain(m *testing.M) {
	switch os.Getenv(helperEnv) {
	case "wait-for-signal":
		helperWaitForSignal()
	case "wait-forever":
		helperWaitForever()
	default:
		m.Run() // since Go 1.15, returning from TestMain exits with m.Run's result
	}
}

// helperWaitForSignal works like cmd/stuntdb: a signal cancels the context,
// and the deferred Close removes the workspace. A problem is printed instead
// of the folder, which makes the parent test fail.
func helperWaitForSignal() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	w, err := Create(Options{Root: os.Getenv(rootEnv)})
	if err != nil {
		_, _ = fmt.Fprintln(os.Stdout, "error:", err)
		return
	}
	defer func() { _ = w.Close() }()
	_, _ = fmt.Fprintln(os.Stdout, w.Dir())
	<-ctx.Done()
}

// helperWaitForever creates a partial output and then hangs until killed.
func helperWaitForever() {
	root := os.Getenv(rootEnv)
	w, err := Create(Options{Root: root})
	if err != nil {
		_, _ = fmt.Fprintln(os.Stdout, "error:", err)
		return
	}
	partial := w.PartialPath(filepath.Join(root, "out.stuntdb"))
	if w.Record(Item{Kind: ItemFile, Path: partial}) != nil || os.WriteFile(partial, []byte("half"), 0o600) != nil {
		_, _ = fmt.Fprintln(os.Stdout, "error: could not create the partial output")
		return
	}
	_, _ = fmt.Fprintln(os.Stdout, w.Dir())
	_, _ = fmt.Fprintln(os.Stdout, partial)
	select {}
}

// startHelper starts a child run and returns the lines it prints at start-up.
func startHelper(t *testing.T, mode, root string, lines int) (*exec.Cmd, []string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), helperEnv+"="+mode, rootEnv+"="+root)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	scanner := bufio.NewScanner(stdout)
	var out []string
	for len(out) < lines && scanner.Scan() {
		out = append(out, scanner.Text())
	}
	if len(out) < lines {
		t.Fatalf("the child run printed %d lines, want %d", len(out), lines)
	}
	return cmd, out
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestCreateWritesAJournal(t *testing.T) {
	root := t.TempDir()
	w, err := Create(Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(w.Dir()) != root || filepath.Base(w.Dir()) != w.ID() {
		t.Errorf("Dir() = %s, want %s/<run ID>", w.Dir(), root)
	}
	if !regexp.MustCompile(`^\d{8}-\d{6}-[0-9a-f]{6}$`).MatchString(w.ID()) {
		t.Errorf("ID() = %q, want yyyymmdd-hhmmss-xxxxxx", w.ID())
	}
	j, err := readJournal(w.Dir())
	if err != nil {
		t.Fatal(err)
	}
	host, _ := os.Hostname()
	if j.RunID != w.ID() || j.PID != os.Getpid() || j.Host != host || j.Started.IsZero() {
		t.Errorf("journal = %+v", j)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if exists(w.Dir()) {
		t.Error("Close did not remove the workspace")
	}
	if err := w.Close(); err != nil {
		t.Errorf("a second Close must be safe: %v", err)
	}
}

func TestRunIDsAreUnique(t *testing.T) {
	root := t.TempDir()
	seen := map[string]bool{}
	for range 20 {
		w, err := Create(Options{Root: root})
		if err != nil {
			t.Fatal(err)
		}
		if seen[w.ID()] {
			t.Fatalf("run ID %s was handed out twice", w.ID())
		}
		seen[w.ID()] = true
	}
}

// A run defers Close, so the workspace disappears however the run ends.
func TestWorkspaceIsRemovedHoweverTheRunEnds(t *testing.T) {
	run := func(t *testing.T, body func(w *Workspace) error) (dir string, err error) {
		t.Helper()
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("panic: %v", r)
			}
		}()
		w, cerr := Create(Options{Root: t.TempDir()})
		if cerr != nil {
			t.Fatal(cerr)
		}
		dir = w.Dir()
		defer func() { _ = w.Close() }()
		path, perr := w.Path("keys.db")
		if perr != nil {
			t.Fatal(perr)
		}
		if werr := os.WriteFile(path, []byte("keys"), 0o600); werr != nil {
			t.Fatal(werr)
		}
		return dir, body(w)
	}
	tests := []struct {
		name    string
		body    func(w *Workspace) error
		wantErr bool
	}{
		{"success", func(*Workspace) error { return nil }, false},
		{"error", func(*Workspace) error { return errors.New("boom") }, true},
		{"panic", func(*Workspace) error { panic("boom") }, true},
		{"canceled context (what Ctrl+C does in cmd/stuntdb)", func(*Workspace) error {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			<-ctx.Done()
			return ctx.Err()
		}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, err := run(t, tt.body)
			if (err != nil) != tt.wantErr {
				t.Errorf("run error = %v, wantErr %v", err, tt.wantErr)
			}
			if exists(dir) {
				t.Error("the workspace is still there")
			}
		})
	}
}

// A real interrupt signal to a running child process removes its workspace.
func TestInterruptRemovesTheWorkspace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows cannot send Ctrl+C to one child process; the Linux and macOS runners cover this")
	}
	root := t.TempDir()
	cmd, out := startHelper(t, "wait-for-signal", root, 1)
	dir := out[0]
	if !exists(dir) {
		t.Fatal("the child run has no workspace")
	}
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("the child run exited badly: %v", err)
	}
	if exists(dir) {
		t.Error("the workspace is still there after the interrupt")
	}
}

// A hard kill can't be caught, so the workspace stays behind. The next run
// finds it and cleans it up, partial output included.
func TestHardKillLeavesALeftoverThatGetsCleaned(t *testing.T) {
	root := t.TempDir()
	cmd, out := startHelper(t, "wait-forever", root, 2)
	dir, partial := out[0], out[1]
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if !exists(dir) || !exists(partial) {
		t.Fatal("a hard kill should leave the workspace and the partial output behind")
	}

	leftovers, err := FindLeftovers(Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if len(leftovers) != 1 || leftovers[0].Dir != dir {
		t.Fatalf("FindLeftovers = %+v, want the killed run", leftovers)
	}
	staging, err := leftovers[0].Clean()
	if err != nil || len(staging) != 0 {
		t.Fatalf("Clean = %v, %v", staging, err)
	}
	if exists(dir) || exists(partial) {
		t.Error("Clean left the workspace or the partial output behind")
	}
}

// deadPID returns the ID of a process that has already exited.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

func writeLeftover(t *testing.T, root, runID string, j Journal) string {
	t.Helper()
	dir := filepath.Join(root, runID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	j.Version, j.RunID = journalVersion, runID
	if err := writeJournal(dir, j); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestFindLeftoversSkipsRunsThatAreNotLeftovers(t *testing.T) {
	root := t.TempDir()
	host, _ := os.Hostname()
	dead := deadPID(t)

	live, err := Create(Options{Root: root}) // this process: still running
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = live.Close() }()
	writeLeftover(t, root, "other-host", Journal{PID: dead, Host: host + "-elsewhere"})
	crashed := writeLeftover(t, root, "crashed", Journal{PID: dead, Host: host})
	young := filepath.Join(root, "starting")
	old := filepath.Join(root, "old-without-journal")
	for _, dir := range []string{young, old} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	twoHoursAgo := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(old, twoHoursAgo, twoHoursAgo); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "not-a-folder"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	leftovers, err := FindLeftovers(Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, l := range leftovers {
		got = append(got, filepath.Base(l.Dir))
	}
	if want := []string{filepath.Base(crashed), filepath.Base(old)}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("leftovers = %v, want %v", got, want)
	}
}

func TestFindLeftoversWithoutARootFolder(t *testing.T) {
	leftovers, err := FindLeftovers(Options{Root: filepath.Join(t.TempDir(), "missing")})
	if err != nil || leftovers != nil {
		t.Errorf("FindLeftovers = %v, %v; want nothing", leftovers, err)
	}
}

func TestCleanOnlyRemovesThatRunsOwnPartialFiles(t *testing.T) {
	root := t.TempDir()
	runID := "20261020-101500-abcdef"
	ownPartial := filepath.Join(root, ".report.stuntdb.partial-"+runID)
	otherRun := filepath.Join(root, ".report.stuntdb.partial-20261020-101500-000000")
	victim := filepath.Join(root, "important.txt")
	for _, f := range []string{ownPartial, otherRun, victim} {
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	dir := writeLeftover(t, root, runID, Journal{Items: []Item{
		{Kind: ItemFile, Path: ownPartial},
		{Kind: ItemFile, Path: otherRun},
		{Kind: ItemFile, Path: victim},
	}})
	_, err := Leftover{Dir: dir, Journal: Journal{RunID: runID, Items: []Item{
		{Kind: ItemFile, Path: ownPartial}, {Kind: ItemFile, Path: otherRun}, {Kind: ItemFile, Path: victim},
	}}}.Clean()
	if err == nil || strings.Count(err.Error(), "not removing") != 2 {
		t.Errorf("Clean error = %v, want two refusals", err)
	}
	if exists(ownPartial) {
		t.Error("the run's own partial output was not removed")
	}
	if !exists(otherRun) || !exists(victim) {
		t.Error("Clean removed a file that is not this run's partial output")
	}
}

func TestCleanKeepsStagingObjectsForConfirmation(t *testing.T) {
	root := t.TempDir()
	staging := Item{Kind: ItemStaging, Engine: "postgres", Name: "stuntdb_staging_abc"}
	dir := writeLeftover(t, root, "run-with-staging", Journal{Items: []Item{staging}})
	l := Leftover{Dir: dir, Journal: Journal{RunID: "run-with-staging", Items: []Item{staging}}}

	got, err := l.Clean()
	if err != nil || len(got) != 1 || got[0] != staging {
		t.Fatalf("Clean = %v, %v; want the staging object back", got, err)
	}
	j, err := readJournal(dir)
	if err != nil || len(j.Items) != 1 || j.Items[0] != staging {
		t.Fatalf("the journal should still list the staging object: %+v, %v", j, err)
	}
	if err := l.Discard(); err != nil || exists(dir) {
		t.Errorf("Discard = %v; the folder should be gone", err)
	}
}

func TestRecordAndForget(t *testing.T) {
	w, err := Create(Options{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	file := Item{Kind: ItemFile, Path: w.PartialPath("/tmp/out.stuntdb")}
	staging := Item{Kind: ItemStaging, Engine: "sqlite", Name: ".app.db.partial"}

	for _, it := range []Item{file, staging, file} {
		if err := w.Record(it); err != nil {
			t.Fatal(err)
		}
	}
	if j, _ := readJournal(w.Dir()); len(j.Items) != 2 {
		t.Errorf("journal has %d items, want 2 (no duplicates)", len(j.Items))
	}
	if err := w.Forget(file); err != nil {
		t.Fatal(err)
	}
	if j, _ := readJournal(w.Dir()); len(j.Items) != 1 || j.Items[0] != staging {
		t.Errorf("after Forget, journal items = %+v", j.Items)
	}
	for _, bad := range []Item{{Kind: ItemFile}, {Kind: ItemStaging, Name: "x"}, {Kind: "other", Path: "x"}} {
		if err := w.Record(bad); err == nil {
			t.Errorf("Record(%+v) should fail", bad)
		}
	}
	_ = w.Close()
	if err := w.Record(file); err == nil {
		t.Error("Record after Close should fail")
	}
	if err := w.Forget(file); err == nil {
		t.Error("Forget after Close should fail")
	}
}

func TestJournalNeverHoldsAURL(t *testing.T) {
	data, err := json.Marshal(Item{Kind: ItemStaging, Engine: "postgres", Name: "stuntdb_staging_x"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "url") {
		t.Errorf("journal items should have no URL field: %s", data)
	}
}

func TestPath(t *testing.T) {
	w, err := Create(Options{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	if p, err := w.Path("keys.db"); err != nil || p != filepath.Join(w.Dir(), "keys.db") {
		t.Errorf("Path(keys.db) = %q, %v", p, err)
	}
	for _, bad := range []string{"", ".", "..", "../escape", "a/b", `a\b`, journalName, journalName + ".tmp"} {
		if _, err := w.Path(bad); err == nil {
			t.Errorf("Path(%q) should fail", bad)
		}
	}
	want := filepath.Join("out", ".app.stuntdb.partial-"+w.ID())
	if got := w.PartialPath(filepath.Join("out", "app.stuntdb")); got != want {
		t.Errorf("PartialPath = %q, want %q", got, want)
	}
}

func TestFoldersArePrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows uses ACLs; the user's cache folder is already private")
	}
	w, err := Create(Options{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	for path, want := range map[string]os.FileMode{w.Dir(): 0o700, filepath.Join(w.Dir(), journalName): 0o600} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s has mode %o, want %o", filepath.Base(path), got, want)
		}
	}
}

func TestProcessAlive(t *testing.T) {
	if !processAlive(os.Getpid()) {
		t.Error("this process should count as alive")
	}
	if processAlive(deadPID(t)) {
		t.Error("an exited process should not count as alive")
	}
	if processAlive(0) || processAlive(-1) {
		t.Error("invalid process IDs should not count as alive")
	}
}

func TestDefaultRootIsInTheUserCacheFolder(t *testing.T) {
	root := Options{}.root()
	if !strings.HasSuffix(filepath.ToSlash(root), "stuntdb/runs") {
		t.Errorf("default root = %s, want …/stuntdb/runs", root)
	}
}
