//go:build integration

package integration_test

import (
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// The provider does not model change streams: a read leaves them out, so a
// plan may drop a table or a column one names. Spanner refuses such a drop
// when it runs, after the statements before it. These tests run the host
// against the emulator and show that the provider refuses the drop before any
// statement runs, and leaves alone the drops Spanner takes.

// `ptah schema apply` of a declaration that drops a table or a column a
// change stream names is refused before any statement runs: the table the
// plan creates first is not created, and t keeps its columns and its row. The
// reason names the drop and the stream, and says what to do.
func TestHostRefusesDroppingWhatAChangeStreamNamesE2E(t *testing.T) {
	tests := []struct {
		name   string
		stream string
		schema string
		want   string
	}{
		{name: "a dropped table", stream: `CREATE CHANGE STREAM cs1 FOR t`, schema: "CREATE TABLE other (id bigint PRIMARY KEY);\n",
			want: `unit 2 of 2 drops table "t", which change stream "cs1" names, so no unit ran: Spanner refuses to drop a table a change stream names; ` +
				`drop the change stream, or alter it to stop tracking the table, before dropping the table`},
		{name: "a dropped column", stream: `CREATE CHANGE STREAM cs3 FOR t(a)`,
			schema: "CREATE TABLE t (id bigint PRIMARY KEY, b bigint);\nCREATE TABLE other (id bigint PRIMARY KEY);\n",
			want: `unit 2 of 2 drops column "a" of table "t", which change stream "cs3" names, so no unit ran: Spanner refuses to drop a column a change stream names; ` +
				`alter the change stream to stop tracking the column, or drop it, before dropping the column`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			address := withSearchPath(c, freshDatabase(c))
			execSQL(c, address, append(trackedTable, test.stream)...)
			output, err := applySQL(c, address, test.schema)
			c.Assert(err, qt.ErrorMatches, "exit status 2", qt.Commentf("%s", output))
			c.Assert(output, qt.Contains, test.want)
			c.Assert(columnsOf(c, address), qt.DeepEquals, []string{"t a", "t b", "t id"})
			c.Assert(queryRows(c, address, `SELECT a, b FROM t`), qt.DeepEquals, []string{"10 20"})
		})
	}
}

// A change stream FOR ALL names no table, so a plan that drops a column and a
// table under one applies, and so does a plan that drops a column of a table
// a stream tracks whole.
func TestHostDropsWhatNoChangeStreamNamesE2E(t *testing.T) {
	tests := []struct {
		name  string
		setup []string
	}{
		{name: "a stream FOR ALL", setup: []string{`CREATE TABLE other (id bigint PRIMARY KEY)`, `CREATE CHANGE STREAM cs2 FOR ALL`}},
		{name: "a stream that tracks the table whole", setup: []string{`CREATE TABLE other (id bigint PRIMARY KEY)`, `CREATE CHANGE STREAM cs1 FOR t`}},
	}
	const schema = "CREATE TABLE t (id bigint PRIMARY KEY, b bigint);\n"
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			address := withSearchPath(c, freshDatabase(c))
			execSQL(c, address, append(trackedTable, test.setup...)...)
			output, err := applySQL(c, address, schema)
			c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
			c.Assert(columnsOf(c, address), qt.DeepEquals, []string{"t b", "t id"})
			c.Assert(queryRows(c, address, `SELECT b FROM t`), qt.DeepEquals, []string{"20"})
			output, err = planSQL(c, address, schema)
			c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
			c.Assert(output, qt.Contains, synced)
		})
	}
}

// `ptah migrations up` runs each statement of a migration in a request of its
// own. A statement that drops a table or a column a change stream names is
// refused before it runs, under the provider's reason: the table keeps its
// columns and its row, and the revision row records the migration as failed
// with none of its statements applied.
func TestHostMigrationRefusesDroppingWhatAChangeStreamNamesE2E(t *testing.T) {
	tests := []struct {
		name   string
		stream string
		body   string
		want   string
	}{
		{name: "a dropped table", stream: `CREATE CHANGE STREAM cs1 FOR t`, body: "DROP TABLE t;\n",
			want: `unit 1 of 1 drops table "t", which change stream "cs1" names, so no unit ran: Spanner refuses to drop a table a change stream names`},
		{name: "a dropped column", stream: `CREATE CHANGE STREAM cs3 FOR t(a)`, body: "ALTER TABLE t DROP COLUMN a;\n",
			want: `unit 1 of 1 drops column "a" of table "t", which change stream "cs3" names, so no unit ran: Spanner refuses to drop a column a change stream names`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			address := withSearchPath(c, freshDatabase(c))
			execSQL(c, address, append(trackedTable, test.stream)...)
			dir := writeFiles(c, map[string]string{"1700000001_drop.up.sql": test.body, "1700000001_drop.down.sql": "SELECT 1;\n"})
			output, err := migrate(c, "up", "--db-url", address, "--migrations-dir", dir, "--allow-destructive")
			c.Assert(err, qt.ErrorMatches, "exit status 2", qt.Commentf("%s", output))
			c.Assert(output, qt.Contains, test.want)
			c.Assert(queryRows(c, address, `SELECT version, state, applied, total FROM schema_migrations`), qt.DeepEquals, []string{"1700000001 failed 0 1"})
			c.Assert(queryRows(c, address, `SELECT error FROM schema_migrations`)[0], qt.Contains, test.want)
			c.Assert(columnsOf(c, address), qt.DeepEquals, []string{"t a", "t b", "t id"})
			c.Assert(queryRows(c, address, `SELECT a, b FROM t`), qt.DeepEquals, []string{"10 20"})
		})
	}
}

// A migration that drops the change stream and then the table it names runs
// whole: when its DROP TABLE runs, in a request of its own, no stream names
// the table any more.
func TestHostMigrationDropsAChangeStreamThenItsTableE2E(t *testing.T) {
	c := testcheck.New(t)
	address := withSearchPath(c, freshDatabase(c))
	execSQL(c, address, append(trackedTable, `CREATE CHANGE STREAM cs1 FOR t`)...)
	dir := writeFiles(c, map[string]string{"1700000001_drop.up.sql": "DROP CHANGE STREAM cs1;\nDROP TABLE t;\n", "1700000001_drop.down.sql": "SELECT 1;\n"})
	output, err := migrate(c, "up", "--db-url", address, "--migrations-dir", dir, "--allow-destructive")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(queryRows(c, address, `SELECT version, state, applied, total FROM schema_migrations`), qt.DeepEquals, []string{"1700000001 applied 2 2"})
	c.Assert(tables(c, address), qt.DeepEquals, []string{"ptah_locks", "schema_migrations", "schema_migrations_log"})
}
