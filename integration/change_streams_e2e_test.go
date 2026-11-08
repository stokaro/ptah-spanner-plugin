//go:build integration

package integration_test

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// Spanner refuses to drop a table a change stream names, and a column one names
// in its column list, when the statement runs, after the statements before it.
// The provider does not model change streams. A read reports the streams that
// name each table to a host that carries removal blockers, and the table
// service refuses such a drop at plan time; on any host the executor refuses
// it before the run's first statement. These tests run the host against the
// emulator and show both refusals, and that the provider leaves alone the
// drops Spanner takes.

// The refusals of a dropped table and a dropped column, as the table service
// and the executor both end them. The executor names the unit, the table
// service the drop; both name the stream and say what to do.
const (
	tableRefusal  = `Spanner refuses to drop a table a change stream names; drop the change stream, or alter it to stop tracking the table, before dropping the table`
	columnRefusal = `Spanner refuses to drop a column a change stream names; alter the change stream to stop tracking the column, or drop it, before dropping the column`
)

// `ptah schema apply` of a declaration that drops a table or a column a
// change stream names is refused before any statement runs, at plan time on a
// host that carries removal blockers and before the first unit on any other:
// the table the plan creates first is not created, and t keeps its columns
// and its row. The reason names the stream, and says what to do.
func TestHostRefusesDroppingWhatAChangeStreamNamesE2E(t *testing.T) {
	tests := []struct {
		name   string
		stream string
		schema string
		named  string
		want   string
	}{
		{name: "a dropped table", stream: `CREATE CHANGE STREAM cs1 FOR t`, schema: "CREATE TABLE other (id bigint PRIMARY KEY);\n",
			named: `change stream "cs1"`, want: tableRefusal},
		{name: "a dropped column", stream: `CREATE CHANGE STREAM cs3 FOR t(a)`,
			schema: "CREATE TABLE t (id bigint PRIMARY KEY, b bigint);\nCREATE TABLE other (id bigint PRIMARY KEY);\n",
			named:  `change stream "cs3"`, want: columnRefusal},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			address := withSearchPath(c, freshDatabase(c))
			execSQL(c, address, append(trackedTable, test.stream)...)
			output, err := applySQL(c, address, test.schema)
			c.Assert(err, qt.ErrorMatches, "exit status 2", qt.Commentf("%s", output))
			c.Assert(output, qt.Contains, test.named)
			c.Assert(output, qt.Contains, test.want)
			c.Assert(columnsOf(c, address), qt.DeepEquals, []string{"t a", "t b", "t id"})
			c.Assert(queryRows(c, address, `SELECT a, b FROM t`), qt.DeepEquals, []string{"10 20"})
		})
	}
}

// On a host that carries removal blockers the table service refuses a drop a
// change stream names at plan time: `schema apply --dry-run` and `migrations
// generate` refuse it with the drop, the stream and what to do, and generate
// writes no file. A stream that names a table by a column list blocks the
// table's removal too.
func TestHostRefusesPlanningADropAChangeStreamNamesE2E(t *testing.T) {
	skipBeforeRemovalBlockers(testcheck.New(t))
	tests := []struct {
		name   string
		stream string
		schema string
		want   string
	}{
		{name: "a dropped table", stream: `CREATE CHANGE STREAM cs1 FOR t`, schema: "CREATE TABLE other (id bigint PRIMARY KEY);\n",
			want: `dropping table "t": change stream "cs1" names it, and ` + tableRefusal},
		{name: "a dropped table a stream names by a column", stream: `CREATE CHANGE STREAM cs3 FOR t(a)`, schema: "CREATE TABLE other (id bigint PRIMARY KEY);\n",
			want: `dropping table "t": change stream "cs3" names it, and ` + tableRefusal},
		{name: "a dropped column", stream: `CREATE CHANGE STREAM cs3 FOR t(a)`, schema: "CREATE TABLE t (id bigint PRIMARY KEY, b bigint);\n",
			want: `dropping column "a" of table "t": change stream "cs3" names it, and ` + columnRefusal},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			address := withSearchPath(c, freshDatabase(c))
			execSQL(c, address, append(trackedTable, test.stream)...)
			output, err := planSQL(c, address, test.schema)
			c.Assert(err, qt.ErrorMatches, "exit status 2", qt.Commentf("%s", output))
			c.Assert(output, qt.Contains, test.want)

			dir := writeFiles(c, map[string]string{"schema.sql": test.schema})
			migrations := filepath.Join(dir, "migrations")
			output, err = hostCommand(c, "migrations", "generate", "--db-url", address, "--schema-file", filepath.Join(dir, "schema.sql"),
				"--migrations-dir", migrations, "--name", "drop")
			c.Assert(err, qt.ErrorMatches, "exit status 2", qt.Commentf("%s", output))
			c.Assert(output, qt.Contains, test.want)
			written, err := filepath.Glob(filepath.Join(migrations, "*.sql"))
			c.Assert(err, qt.IsNil)
			c.Assert(written, qt.HasLen, 0)
			c.Assert(columnsOf(c, address), qt.DeepEquals, []string{"t a", "t b", "t id"})
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

// changeStreamsNotDescribed is the header line Ptah's built-in Spanner engine
// wrote into every document made from a read, and the provider's reads record
// under contract 25.41.
const changeStreamsNotDescribed = "// ptah:not-described change_stream reason=unsupported provenance=derived-from-target"

// changeStreamsRecord matches the record in the JSON of `ptah schema inspect`.
const changeStreamsRecord = `(?s).*"kind": "change_stream",\s+"name": "",\s+"reason": "unsupported",\s+"provenance": "derived-from-target".*`

// Every read says that it does not describe change streams, with a stream in
// the database and without one, as Ptah's built-in Spanner engine said it: the
// record belongs to the target, since listing only the streams a read found
// would say that no other one exists. The header of `ptah schema inspect`
// carries the line, its JSON the record, and an export of the read applies as
// a schema with nothing to change, so the host reads the line back.
func TestHostSaysAReadDoesNotDescribeChangeStreamsE2E(t *testing.T) {
	tests := []struct {
		name  string
		setup []string
	}{
		{name: "a stream FOR t", setup: append(slices.Clone(trackedTable), `CREATE CHANGE STREAM cs1 FOR t`)},
		{name: "a stream FOR ALL", setup: append(slices.Clone(trackedTable), `CREATE CHANGE STREAM cs2 FOR ALL`)},
		{name: "no stream", setup: trackedTable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			skipBeforeNotDescribed(c)
			address := withSearchPath(c, freshDatabase(c))
			execSQL(c, address, test.setup...)

			exported, err := hostCommand(c, "schema", "inspect", "--db-url", address, "--format", "hcl")
			c.Assert(err, qt.IsNil, qt.Commentf("%s", exported))
			c.Assert(exported, qt.Contains, changeStreamsNotDescribed+"\n")
			output, err := hostCommand(c, "schema", "inspect", "--db-url", address, "--format", "json")
			c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
			c.Assert(output, qt.Matches, changeStreamsRecord)

			_, document, found := strings.Cut(exported, "// Code generated")
			c.Assert(found, qt.IsTrue, qt.Commentf("%s", exported))
			hcl := filepath.Join(writeFiles(c, map[string]string{"schema.hcl": "// Code generated" + document}), "schema.hcl")
			output, err = hostCommand(c, "schema", "apply", "--db-url", address, "--schema-file", hcl, "--dry-run")
			c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
			c.Assert(output, qt.Contains, synced)
		})
	}
}

// `ptah-compat schema inspect` writes the same line into the header of its
// HCL, as it did when the host built in a Spanner engine.
func TestCompatSchemaInspectSaysItDoesNotDescribeChangeStreamsE2E(t *testing.T) {
	c := testcheck.New(t)
	skipBeforeNotDescribed(c)
	address := withSearchPath(c, freshDatabase(c))
	execSQL(c, address, append(slices.Clone(trackedTable), `CREATE CHANGE STREAM cs1 FOR t`)...)
	output, err := compatCommand(c, "schema", "inspect", "--url", address)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, changeStreamsNotDescribed+"\n")
}
