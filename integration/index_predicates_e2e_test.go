//go:build integration

package integration_test

import (
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// filteredIndexes is a SQL schema document with three filtered indexes: one
// key, two keys written out of byte order, and a unique index filtered on the
// column it stores.
const filteredIndexes = `CREATE TABLE t (id bigint NOT NULL, c bigint, d varchar(10), PRIMARY KEY (id));
CREATE INDEX t_c ON t (c) WHERE c IS NOT NULL;
CREATE INDEX t_cd ON t (c, d) WHERE d IS NOT NULL AND c IS NOT NULL;
CREATE UNIQUE INDEX t_u ON t (c) INCLUDE (d) WHERE d IS NOT NULL;
`

// indexFilters is each user index of table t with the filter the catalog
// keeps for it.
func indexFilters(c *qt.C, address string) []string {
	c.Helper()
	return queryRows(c, address, `SELECT index_name, COALESCE(filter, '') FROM information_schema.indexes
WHERE table_schema = 'public' AND table_name = 't' AND index_type = 'INDEX' ORDER BY index_name`)
}

// A filtered index is created with its WHERE clause, and the next plan
// converges, because the read and the declaration state the clause in the
// one spelling Spanner keeps (schema.index-predicates). The built-in created
// the same indexes and planned DROP INDEX and CREATE INDEX for every one of
// them on every later run, and its `db read` dropped the clause, measured
// with Ptah v0.22.1. A changed clause rebuilds the index, and `ptah db read`
// writes each clause so its output replays and converges.
func TestHostRoundTripsFilteredIndexesE2E(t *testing.T) {
	c := testcheck.New(t)
	skipBeforeContract2529(c)
	address := freshDatabase(c)
	output, err := applySQL(c, address, filteredIndexes)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	for _, statement := range []string{
		`CREATE INDEX "t_c" ON "t" ("c") WHERE "c" IS NOT NULL;`,
		`CREATE INDEX "t_cd" ON "t" ("c", "d") WHERE "c" IS NOT NULL AND "d" IS NOT NULL;`,
		`CREATE UNIQUE INDEX "t_u" ON "t" ("c") INCLUDE ("d") WHERE "d" IS NOT NULL;`,
	} {
		c.Assert(output, qt.Contains, statement)
	}
	c.Assert(indexFilters(c, address), qt.DeepEquals, []string{
		"t_c c IS NOT NULL", "t_cd c IS NOT NULL AND d IS NOT NULL", "t_u c IS NOT NULL AND d IS NOT NULL",
	})
	output, err = planSQL(c, address, filteredIndexes)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, synced)

	changed := strings.Replace(filteredIndexes, "WHERE d IS NOT NULL AND c IS NOT NULL", "WHERE c IS NOT NULL", 1)
	output, err = applySQL(c, address, changed)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, "DROP INDEX \"t_cd\";\nCREATE INDEX \"t_cd\" ON \"t\" (\"c\", \"d\") WHERE \"c\" IS NOT NULL;")
	c.Assert(strings.Count(output, "DROP INDEX"), qt.Equals, 1)
	c.Assert(indexFilters(c, address), qt.DeepEquals, []string{"t_c c IS NOT NULL", "t_cd c IS NOT NULL", "t_u c IS NOT NULL AND d IS NOT NULL"})

	exported, err := hostCommand(c, "db", "read", "--db-url", address)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", exported))
	c.Assert(exported, qt.Contains, `CREATE UNIQUE INDEX "t_u" ON "t" ("c") INCLUDE ("d") WHERE "d" IS NOT NULL;`)
	statements := exported[strings.Index(exported, "CREATE TABLE"):]
	replayed := freshDatabase(c)
	output, err = applySQL(c, replayed, statements)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(indexFilters(c, replayed), qt.DeepEquals, indexFilters(c, address))
	output, err = planSQL(c, replayed, statements)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, synced)
}

// A WHERE clause Spanner creates no index with is refused before a statement
// is sent, with the rule Spanner states, and nothing is created: a test other
// than IS NOT NULL, a column the index neither keys nor stores, and a unique
// index's own key, which Spanner filters by itself.
func TestHostRefusesIndexFiltersSpannerCannotCreateE2E(t *testing.T) {
	tests := []struct{ name, index, wantErr string }{
		{name: "a comparison", index: "CREATE INDEX t_r ON t (c) WHERE c > 0",
			wantErr: `index "t_r" of table "t" has a WHERE filter Spanner cannot create: Spanner takes only <column> IS NOT NULL tests joined by AND`},
		{name: "a column outside the index", index: "CREATE INDEX t_o ON t (c) WHERE d IS NOT NULL",
			wantErr: `index "t_o" of table "t" filters on column "d", which the index neither keys nor stores`},
		{name: "a unique index's key", index: "CREATE UNIQUE INDEX t_k ON t (c) WHERE c IS NOT NULL",
			wantErr: `index "t_k" of table "t" is unique and filters on its key "c"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			skipBeforeContract2529(c)
			address := freshDatabase(c)
			output, err := applySQL(c, address, "CREATE TABLE t (id bigint NOT NULL, c bigint, d varchar(10), PRIMARY KEY (id));\n"+test.index+";\n")
			c.Assert(err, qt.IsNotNil)
			c.Assert(output, qt.Contains, test.wantErr)
			c.Assert(queryRows(c, address, `SELECT table_name FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 't'`), qt.HasLen, 0)
		})
	}
}
