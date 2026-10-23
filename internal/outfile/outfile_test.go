package outfile

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommit(t *testing.T) {
	tests := []struct {
		name     string
		existing bool
	}{
		{"new file", false},
		{"replaces an existing file", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			final := filepath.Join(dir, "app.db")
			partial := filepath.Join(dir, ".app.db.partial-run1")
			if tt.existing {
				writeFile(t, final, "old")
			}
			writeFile(t, partial, "new")

			if err := Commit(context.Background(), partial, final); err != nil {
				t.Fatalf("Commit: %v", err)
			}
			if got := readFile(t, final); got != "new" {
				t.Errorf("final file holds %q, want %q", got, "new")
			}
			if _, err := os.Stat(partial); !os.IsNotExist(err) {
				t.Errorf("the partial file still exists: %v", err)
			}
		})
	}
}

func TestCommitNew(t *testing.T) {
	dir := t.TempDir()
	final := filepath.Join(dir, "stuntdb.yaml")
	partial := filepath.Join(dir, ".stuntdb.yaml.partial-1")
	writeFile(t, partial, "new")
	if err := CommitNew(context.Background(), partial, final); err != nil {
		t.Fatalf("CommitNew: %v", err)
	}
	if got := readFile(t, final); got != "new" {
		t.Errorf("final file holds %q, want %q", got, "new")
	}
	if _, err := os.Stat(partial); !os.IsNotExist(err) {
		t.Errorf("the partial file still exists: %v", err)
	}

	// A second file never replaces the first.
	writeFile(t, partial, "newer")
	err := CommitNew(context.Background(), partial, final)
	if !errors.Is(err, fs.ErrExist) {
		t.Fatalf("CommitNew over an existing file = %v, want fs.ErrExist", err)
	}
	if strings.Contains(err.Error(), dir) {
		t.Errorf("the error contains the path: %q", err)
	}
	if got := readFile(t, final); got != "new" {
		t.Errorf("the existing file was replaced: it holds %q", got)
	}
	if got := readFile(t, partial); got != "newer" {
		t.Errorf("the partial file holds %q; the caller removes it", got)
	}
	if err := CommitNew(context.Background(), partial, filepath.Join(dir, "sub", "x")); err == nil {
		t.Error("CommitNew into another folder succeeded")
	}
}

func TestCommitErrorsHidePaths(t *testing.T) {
	dir := t.TempDir()
	const secret = "secret-folder-name"
	missing := filepath.Join(dir, secret, ".app.db.partial-run1")
	err := Commit(context.Background(), missing, filepath.Join(dir, secret, "app.db"))
	if err == nil {
		t.Fatal("Commit of a missing file succeeded")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("the error contains the path: %q", err)
	}
}

func TestCommitNeedsTheSameFolder(t *testing.T) {
	dir := t.TempDir()
	partial := filepath.Join(dir, ".app.db.partial-run1")
	writeFile(t, partial, "new")
	if err := Commit(context.Background(), partial, filepath.Join(dir, "sub", "app.db")); err == nil {
		t.Error("Commit into another folder succeeded")
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
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
