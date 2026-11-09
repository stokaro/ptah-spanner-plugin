//go:build integration

package integration_test

import (
	"debug/buildinfo"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// Each test here drives the host through an addition of contract 25.29: schemas
// on the common-object wire (schema.common-namespaces), the TTL clause of a SQL
// CREATE TABLE (source.sql-table-clauses), the reserved seed ledger
// (schema.reserved-seeds) and index predicates (schema.index-predicates). The
// statements are the ones Ptah's built-in Spanner engine planned for the same
// documents, measured on the emulator with Ptah v0.22.1, except where a test
// says otherwise.

// skipBeforeContract2529 skips the test when the host speaks a contract older
// than 25.29: the host pins a provider-sdk older than v0.27.0. Such a host
// lists none of the additions, so the provider serves it as before them and
// the host refuses the declaration or plans as it did. The release leg runs
// such a host until a Ptah release speaks 25.29. The decision reads the host
// binary, not an answer it gives, so a host that speaks 25.29 cannot pass a
// test by refusing.
func skipBeforeContract2529(c *qt.C) {
	c.Helper()
	host, _ := hostManifest(c)
	info, err := buildinfo.ReadFile(host)
	c.Assert(err, qt.IsNil)
	for _, dependency := range info.Deps {
		if dependency.Path != "ptah.run/provider-sdk" || dependency.Replace != nil {
			continue
		}
		var major, minor int
		if _, err := fmt.Sscanf(dependency.Version, "v%d.%d.", &major, &minor); err == nil && major == 0 && minor < 27 {
			c.Skipf("the host pins provider-sdk %s, which predates contract 25.29", dependency.Version)
		}
	}
}

// schemaFile writes body into a new directory as name and returns its path.
func schemaFile(c *qt.C, name, body string) string {
	c.Helper()
	return filepath.Join(writeFiles(c, map[string]string{name: body}), name)
}

// heldSchemas is every schema of the database address names that is not one
// Spanner keeps for its catalogs, in name order.
func heldSchemas(c *qt.C, address string) []string {
	c.Helper()
	return queryRows(c, address, `SELECT schema_name FROM information_schema.schemata
WHERE schema_name NOT IN ('information_schema', 'pg_catalog', 'spanner_sys') ORDER BY schema_name`)
}

// A SQL document that declares a schema and a table in another one plans
// CREATE SCHEMA IF NOT EXISTS for each before the table, as the built-in
// planned it, and converges. The read lists the schemas, so a second plan
// creates neither again. A provider without the namespaces of contract 25.29
// had both refused by the host (`does not support SchemaDiff.SchemasAdded`).
func TestHostCreatesDeclaredSchemasE2E(t *testing.T) {
	c := testcheck.New(t)
	skipBeforeContract2529(c)
	address := freshDatabase(c)
	path := schemaFile(c, "schema.sql", "CREATE SCHEMA es_app;\nCREATE TABLE app.t (id bigint NOT NULL, PRIMARY KEY (id));\n")
	output, err := hostCommand(c, "schema", "apply", "--db-url", address, "--schema-file", path, "--dry-run")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, "CREATE SCHEMA IF NOT EXISTS \"app\";\nCREATE SCHEMA IF NOT EXISTS \"es_app\";\nCREATE TABLE \"app\".\"t\" (\n")
	c.Assert(strings.Count(output, "CREATE SCHEMA"), qt.Equals, 2)

	output, err = hostCommand(c, "schema", "apply", "--db-url", address, "--schema-file", path, "--auto-approve")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(heldSchemas(c, address), qt.DeepEquals, []string{"app", "es_app", "public"})
	output, err = hostCommand(c, "schema", "apply", "--db-url", address, "--schema-file", path, "--dry-run")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, synced)
}

// A schema declared with nothing in it, in HCL and in SQL, is created, the
// catalog holds it, and the next plan is empty: the host test
// TestDeclaredEmptySchemaIsCreatedE2E for Spanner (stokaro/ptah-spanner-plugin#16).
func TestHostCreatesAnEmptyDeclaredSchemaE2E(t *testing.T) {
	tests := []struct{ name, body string }{
		{name: "schema.hcl", body: "schema \"es_app\" {\n}\n"},
		{name: "schema.sql", body: "CREATE SCHEMA es_app;\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			skipBeforeContract2529(c)
			address := freshDatabase(c)
			path := schemaFile(c, test.name, test.body)
			output, err := hostCommand(c, "schema", "apply", "--db-url", address, "--schema-file", path, "--dry-run")
			c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
			c.Assert(output, qt.Contains, `CREATE SCHEMA IF NOT EXISTS "es_app";`)
			c.Assert(heldSchemas(c, address), qt.DeepEquals, []string{"public"})

			output, err = hostCommand(c, "schema", "apply", "--db-url", address, "--schema-file", path, "--auto-approve")
			c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
			c.Assert(heldSchemas(c, address), qt.DeepEquals, []string{"es_app", "public"})
			output, err = hostCommand(c, "schema", "apply", "--db-url", address, "--schema-file", path, "--dry-run")
			c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
			c.Assert(output, qt.Contains, synced)
		})
	}
}

// A schema that exists and that a document declares is not created again,
// and a schema a document leaves out is kept, whatever is in it: the host
// never drops one a declaration does not name.
func TestHostKeepsHeldSchemasE2E(t *testing.T) {
	c := testcheck.New(t)
	skipBeforeContract2529(c)
	address := freshDatabase(c)
	execSQL(c, address, "CREATE SCHEMA es_app", "CREATE SCHEMA other", "CREATE TABLE other.kept (id bigint NOT NULL, PRIMARY KEY (id))")
	path := schemaFile(c, "schema.hcl", "schema \"es_app\" {\n}\n")
	output, err := hostCommand(c, "schema", "apply", "--db-url", address, "--schema-file", path, "--dry-run")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, synced)
	c.Assert(heldSchemas(c, address), qt.DeepEquals, []string{"es_app", "other", "public"})
}

// A generated migration creates the declared schema and its down file drops
// it after the table, as the built-in wrote both files; the down file drops a
// schema the declaration names and leaves the schema a table needed but no
// document declared, as the built-in left it. Up, down and up again run
// through the provider.
func TestHostGeneratesAndRevertsASchemaE2E(t *testing.T) {
	c := testcheck.New(t)
	skipBeforeContract2529(c)
	address := freshDatabase(c)
	dir := c.TempDir()
	path := schemaFile(c, "schema.sql", "CREATE SCHEMA es_app;\nCREATE TABLE app.t (id bigint NOT NULL, PRIMARY KEY (id));\n")
	output, err := hostCommand(c, "migrations", "generate", "--db-url", address, "--schema-file", path, "--migrations-dir", dir, "--name", "schemas")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	up, err := filepath.Glob(filepath.Join(dir, "*_schemas.up.sql"))
	c.Assert(err, qt.IsNil)
	c.Assert(up, qt.HasLen, 1)
	upBody, err := os.ReadFile(up[0])
	c.Assert(err, qt.IsNil)
	c.Assert(string(upBody), qt.Contains, "CREATE SCHEMA IF NOT EXISTS \"app\";\nCREATE SCHEMA IF NOT EXISTS \"es_app\";\nCREATE TABLE \"app\".\"t\" (")
	downBody, err := os.ReadFile(strings.Replace(up[0], ".up.sql", ".down.sql", 1))
	c.Assert(err, qt.IsNil)
	c.Assert(string(downBody), qt.Contains, "DROP TABLE \"app\".\"t\";\nDROP SCHEMA \"es_app\";")

	for _, step := range []struct {
		args []string
		want []string
	}{
		{args: []string{"migrations", "up"}, want: []string{"app", "es_app", "public"}},
		{args: []string{"migrations", "down", "--target", "0", "--confirm"}, want: []string{"app", "public"}},
		{args: []string{"migrations", "up"}, want: []string{"app", "es_app", "public"}},
	} {
		output, err := hostCommand(c, append(step.args, "--db-url", address, "--migrations-dir", dir)...)
		c.Assert(err, qt.IsNil, qt.Commentf("%v: %s", step.args, output))
		c.Assert(heldSchemas(c, address), qt.DeepEquals, step.want, qt.Commentf("%v", step.args))
	}
}

// A rehearsal on a dev database removes the schemas the plan created there
// once it is done, and the target keeps them: the read lists the dev
// database's schemas, and the host's cleanup removes the ones the run made.
func TestHostRehearsalLeavesNoSchemaOnTheDevDatabaseE2E(t *testing.T) {
	c := testcheck.New(t)
	skipBeforeContract2529(c)
	address, dev := freshDatabase(c), freshDatabase(c)
	path := schemaFile(c, "schema.sql", "CREATE SCHEMA es_app;\nCREATE TABLE app.t (id bigint NOT NULL, PRIMARY KEY (id));\n")
	output, err := hostCommand(c, "schema", "apply", "--db-url", address, "--dev-url", dev, "--schema-file", path, "--auto-approve")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(heldSchemas(c, address), qt.DeepEquals, []string{"app", "es_app", "public"})
	c.Assert(heldSchemas(c, dev), qt.DeepEquals, []string{"public"})
}
