package dialecttest_test

import (
	"strings"
	"testing"

	"github.com/exarth-labs/stuntdb/dialect"
	"github.com/exarth-labs/stuntdb/dialect/dialecttest"
	"github.com/exarth-labs/stuntdb/internal/fakedb"
)

// The fake engine must pass the whole suite. It behaves like the hard engines
// (shared snapshots, one open read per reader, empty text stored as NULL,
// upper-case names, late foreign-key checks, a tiny parameter limit), so this
// proves the suite itself doesn't assume SQLite behavior. It runs once per
// snapshot mode.
func TestFakeDBPassesTheSuite(t *testing.T) {
	for _, mode := range []dialect.SnapshotMode{dialect.SnapshotShared, dialect.SnapshotSingle} {
		t.Run(mode.String(), func(t *testing.T) {
			db := fakedb.New(fakedb.WithSnapshot(mode))
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
		})
	}
}
