package sqlite_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/exarth-labs/stuntdb/dialect/dialecttest"
	"github.com/exarth-labs/stuntdb/dialects/sqlite"
)

func TestConformance(t *testing.T) {
	dialecttest.Run(t, dialecttest.Harness{
		Dialect:     sqlite.Dialect{},
		Namespace:   "main",
		NewDatabase: newDatabase,
		BrokenURL: func(secret string) string {
			return "sqlite:" + filepath.Join(os.TempDir(), "stuntdb-missing-"+secret, "app.db")
		},
	})
}

// newDatabase returns the URL of a new, empty database file: an empty file is
// a valid SQLite database with no tables.
func newDatabase(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "app.db")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return "sqlite:" + path
}
