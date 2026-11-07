//go:build integration

package integration_test

import (
	"os"
	"path/filepath"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// realmRefusal is the words of a host that names a provider's dev database
// with the query its built-in Spanner engine's descriptor states, on a
// connection that has no database/sql operations.
const realmRefusal = "does not expose database/sql operations"

// realmChange names the host change that asks the provider's session instead.
const realmChange = "the provider naming its dev database realm"

// readFile returns the text of the file at path.
func readFile(c *qt.C, path string) string {
	c.Helper()
	data, err := os.ReadFile(path) // #nosec G304 -- path is a file the test's own command wrote in its temporary directory
	c.Assert(err, qt.IsNil)
	return string(data)
}

// userTables lists the base tables and views of the database's user schemas,
// as schema and name.
func userTables(c *qt.C, address string) []string {
	c.Helper()
	return queryRows(c, address, "SELECT table_schema, table_name FROM information_schema.tables "+
		"WHERE table_schema NOT IN ('information_schema', 'pg_catalog', 'spanner_sys') ORDER BY 1, 2")
}

// A plan rehearsed on a dev database before it reaches the target leaves the
// dev database as it found it. Ptah's built-in Spanner engine stopped here:
// its cleanup read pg_get_userbyid, which Spanner does not have, so the
// target was never changed.
func TestDevDatabaseE2E_SchemaApplyRehearsesOnTheDevDatabase(t *testing.T) {
	c := testcheck.New(t)
	target, dev := withSearchPath(c, freshDatabase(c)), withSearchPath(c, freshDatabase(c))
	path := filepath.Join(writeFiles(c, map[string]string{"schema.sql": "CREATE TABLE items (\n  id bigint NOT NULL,\n  name varchar(50),\n  PRIMARY KEY (id)\n);\n" +
		"CREATE INDEX items_name ON items (name);\n"}), "schema.sql")
	output, err := hostCommand(c, "schema", "apply", "--db-url", target, "--schema-file", path, "--dev-url", dev, "--auto-approve")
	skipOnOldHost(c, output, err, realmRefusal, realmChange)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(userTables(c, target), qt.DeepEquals, []string{"public items", "public ptah_locks"})
	c.Assert(userTables(c, dev), qt.HasLen, 0)
}

// A dev database that holds something is refused before anything runs, on
// the provider's list of what a cleanup would remove: the target gains only
// the lock the run took, and the dev database keeps what it held.
func TestDevDatabaseE2E_ADevDatabaseThatIsNotEmptyIsRefused(t *testing.T) {
	c := testcheck.New(t)
	target, dev := withSearchPath(c, freshDatabase(c)), withSearchPath(c, freshDatabase(c))
	execSQL(c, dev, "CREATE TABLE leftover (id bigint NOT NULL, PRIMARY KEY (id))")
	path := filepath.Join(writeFiles(c, map[string]string{"schema.sql": "CREATE TABLE items (\n  id bigint NOT NULL,\n  PRIMARY KEY (id)\n);\n"}), "schema.sql")
	output, err := hostCommand(c, "schema", "apply", "--db-url", target, "--schema-file", path, "--dev-url", dev, "--auto-approve")
	skipOnOldHost(c, output, err, realmRefusal, realmChange)
	c.Assert(err, qt.IsNotNil)
	c.Assert(output, qt.Contains, `connected database is not clean: found table "leftover" in schema "public"`)
	c.Assert(userTables(c, target), qt.DeepEquals, []string{"public ptah_locks"})
	c.Assert(userTables(c, dev), qt.DeepEquals, []string{"public leftover"})
}

// migrations generate --replay replays the directory on the dev database,
// plans the next migration from what the replay built, and empties the dev
// database afterwards. The built-in engine refused every replay on Spanner,
// which had no way to lock a dev database.
func TestDevDatabaseE2E_GenerateReplaysTheDirectory(t *testing.T) {
	c := testcheck.New(t)
	dev := withSearchPath(c, freshDatabase(c))
	dir := writeFiles(c, map[string]string{
		"1700000001_items.up.sql":   "CREATE TABLE items (\n  id bigint NOT NULL,\n  name varchar(50),\n  PRIMARY KEY (id)\n);\nCREATE INDEX items_name ON items (name);\nINSERT INTO items (id, name) VALUES (1, 'a');\n",
		"1700000001_items.down.sql": "DROP INDEX items_name;\nDROP TABLE items;\n",
	})
	schema := filepath.Join(writeFiles(c, map[string]string{"schema.sql": "CREATE TABLE items (\n  id bigint NOT NULL,\n  name varchar(50),\n  PRIMARY KEY (id)\n);\n" +
		"CREATE INDEX items_name ON items (name);\nCREATE TABLE tags (\n  id bigint NOT NULL,\n  PRIMARY KEY (id)\n);\n"}), "schema.sql")
	output, err := hostCommand(c, "migrations", "generate", "--schema-file", schema, "--migrations-dir", dir, "--name", "tags", "--dev-url", dev, "--replay")
	skipOnOldHost(c, output, err, realmRefusal, realmChange)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	generated, err := filepath.Glob(filepath.Join(dir, "*_tags.up.sql"))
	c.Assert(err, qt.IsNil)
	c.Assert(generated, qt.HasLen, 1)
	c.Assert(readFile(c, generated[0]), qt.Contains, `CREATE TABLE "tags"`)
	c.Assert(readFile(c, generated[0]), qt.Not(qt.Contains), `"items"`)
	c.Assert(userTables(c, dev), qt.DeepEquals, []string{"public ptah_locks"})
}

// A replayed statement whose effects outlive the dev database's cleanup is
// refused by the provider's replay guard, and the dev database is emptied of
// what the replay ran before it.
func TestDevDatabaseE2E_ReplayRefusesAStatementThatLeavesTheDevDatabase(t *testing.T) {
	c := testcheck.New(t)
	dev := withSearchPath(c, freshDatabase(c))
	dir := writeFiles(c, map[string]string{
		"1700000001_options.up.sql":   "CREATE TABLE items (\n  id bigint NOT NULL,\n  PRIMARY KEY (id)\n);\nALTER DATABASE db SET spanner.default_leader = 'us-east1';\n",
		"1700000001_options.down.sql": "DROP TABLE items;\n",
	})
	schema := filepath.Join(writeFiles(c, map[string]string{"schema.sql": "CREATE TABLE items (\n  id bigint NOT NULL,\n  PRIMARY KEY (id)\n);\n"}), "schema.sql")
	output, err := hostCommand(c, "migrations", "generate", "--schema-file", schema, "--migrations-dir", dir, "--name", "next", "--dev-url", dev, "--replay")
	skipOnOldHost(c, output, err, realmRefusal, realmChange)
	c.Assert(err, qt.IsNotNil)
	c.Assert(output, qt.Contains, "spanner migration replay rejects ALTER DATABASE because its effects cannot be confined to the disposable database realm")
	c.Assert(userTables(c, dev), qt.DeepEquals, []string{"public ptah_locks"})
}
