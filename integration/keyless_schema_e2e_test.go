//go:build integration

package integration_test

import (
	"path/filepath"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// A table declared without a primary key is created as declared, and Spanner
// keys it on a hidden column rowid that the read leaves out, so the table
// converges and a column added later converges too. Ptah's built-in Spanner
// engine read the hidden column as a declared key column: it planned DROP
// CONSTRAINT "PK_nopk" and DROP COLUMN "rowid" on every run, and an applied
// change stopped at the DROP CONSTRAINT Spanner refused, after the statements
// before it ran.
func TestHostCreatesAndChangesATableWithoutAPrimaryKeyE2E(t *testing.T) {
	c := testcheck.New(t)
	address := withSearchPath(c, freshDatabase(c))
	const v1 = "CREATE TABLE nopk (a bigint, b varchar(10));\n"
	output, err := applySQL(c, address, v1)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, "CREATE TABLE \"nopk\" (\n  \"a\" bigint,\n  \"b\" varchar(10)\n);\n")
	c.Assert(queryRows(c, address, `SELECT column_name, is_hidden FROM information_schema.columns WHERE table_name = 'nopk' ORDER BY ordinal_position`),
		qt.DeepEquals, []string{"a NO", "b NO", "rowid YES"})
	output, err = planSQL(c, address, v1)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, synced)

	const v2 = "CREATE TABLE nopk (a bigint, b varchar(10), c bigint);\n"
	output, err = applySQL(c, address, v2)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, "ALTER TABLE \"nopk\" ADD COLUMN \"c\" bigint;\n")
	output, err = planSQL(c, address, v2)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, synced)
	output, err = hostCommand(c, "schema", "inspect", "--db-url", address, "--format", "hcl")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Not(qt.Contains), "rowid")
	c.Assert(output, qt.Not(qt.Contains), "primary_key")
}

// A YAML table in a schema of its own is created after CREATE SCHEMA IF NOT
// EXISTS, as the built-in planned it: Spanner refuses a table in a schema
// that does not exist (`Schema not found`). The schema statement is written
// once for every table a plan creates in that schema.
func TestHostCreatesATableInItsOwnSchemaE2E(t *testing.T) {
	c := testcheck.New(t)
	address := withSearchPath(c, freshDatabase(c))
	const declared = `tables:
  t:
    schema: app
    columns:
      id: {type: bigint, primary: true}
  u:
    schema: app
    columns:
      id: {type: bigint, primary: true}
  p:
    columns:
      id: {type: bigint, primary: true}
`
	path := filepath.Join(writeFiles(c, map[string]string{"schema.yaml": declared}), "schema.yaml")
	output, err := hostCommand(c, "schema", "apply", "--db-url", address, "--schemas", "public,app", "--schema-file", path, "--auto-approve")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, "CREATE SCHEMA IF NOT EXISTS \"app\";\nCREATE TABLE \"app\".\"t\" (\n")
	c.Assert(output, qt.Contains, "CREATE TABLE \"app\".\"u\" (\n")
	c.Assert(queryRows(c, address, `SELECT table_schema, table_name FROM information_schema.tables WHERE table_type = 'BASE TABLE' AND table_schema IN ('app', 'public')
AND table_name IN ('t', 'u', 'p') ORDER BY table_schema, table_name`), qt.DeepEquals, []string{"app t", "app u", "public p"})
	output, err = hostCommand(c, "schema", "apply", "--db-url", address, "--schemas", "public,app", "--schema-file", path, "--dry-run")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, synced)
}
