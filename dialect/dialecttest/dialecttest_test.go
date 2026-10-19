package dialecttest_test

import (
	"strings"
	"testing"

	"github.com/exarth-labs/stuntdb/dialect/dialecttest"
	"github.com/exarth-labs/stuntdb/internal/fakedb"
)

// The fake engine must pass the whole suite. It behaves like the hard engines
// (shared snapshots, empty text stored as NULL, upper-case names, late foreign-key
// checks, a tiny parameter limit), so this proves the suite itself doesn't
// assume SQLite behavior.
func TestFakeDBPassesTheSuite(t *testing.T) {
	db := fakedb.New()
	dialecttest.Run(t, dialecttest.Harness{
		Dialect:   db,
		Namespace: "APP",
		NewDatabase: func(t *testing.T) string {
			return "fake:" + strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())
		},
		BrokenURL: func(secret string) string { return "fake:admin:" + secret + "@nowhere" },
	})
	if db.Statements() == 0 {
		t.Error("the suite ran no read statements")
	}
}
