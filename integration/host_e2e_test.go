//go:build integration

package integration_test

import (
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

const (
	hostTable = "package entities\n\n//ptah:schema:table name=\"nums\"\ntype Num struct {\n" +
		"\t//ptah:schema:field name=\"id\" type=\"BIGINT\" primary=\"true\"\n\tID int64\n" +
		"\t//ptah:schema:field name=\"n\" type=\"BIGINT\" not_null=\"true\"\n\tN int64\n" +
		"\t//ptah:schema:field name=\"old\" type=\"BIGINT\"\n\tOld int64\n}\n"
	hostTableChanged = "package entities\n\n//ptah:schema:table name=\"nums\"\ntype Num struct {\n" +
		"\t//ptah:schema:field name=\"id\" type=\"BIGINT\" primary=\"true\"\n\tID int64\n" +
		"\t//ptah:schema:field name=\"n\" type=\"BIGINT\" not_null=\"true\"\n\tN int64\n" +
		"\t//ptah:schema:field name=\"extra\" type=\"BIGINT\"\n\tExtra int64\n}\n"
)

// `ptah schema apply` creates a table through the provider under the host's
// apply lock, a second run plans nothing, and a changed declaration adds and
// drops a column.
func TestHostAppliesAndConvergesE2E(t *testing.T) {
	c := testcheck.New(t)
	address := withSearchPath(c, freshDatabase(c))
	first := writeFiles(c, map[string]string{"entities.go": hostTable})
	output, err := hostCommand(c, "schema", "apply", "--db-url", address, "--root-dir", first, "--auto-approve")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Matches, `(?s).*CREATE TABLE "nums" \(\n  "id" BIGINT PRIMARY KEY NOT NULL,\n  "n" BIGINT NOT NULL,\n  "old" BIGINT\n\);.*`)
	output, err = hostCommand(c, "schema", "apply", "--db-url", address, "--root-dir", first, "--dry-run")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Matches, `(?s).*Schema is synced, no changes to be made.*`)

	changed := writeFiles(c, map[string]string{"entities.go": hostTableChanged})
	output, err = hostCommand(c, "schema", "apply", "--db-url", address, "--root-dir", changed, "--auto-approve")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Matches, `(?s).*ALTER TABLE "nums" ADD COLUMN "extra" BIGINT;\nALTER TABLE "nums" DROP COLUMN "old";.*`)
	output, err = hostCommand(c, "schema", "apply", "--db-url", address, "--root-dir", changed, "--dry-run")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Matches, `(?s).*Schema is synced, no changes to be made.*`)
}

// A read the provider refuses reaches the operator with the provider's reason.
func TestHostShowsTheReadRefusalE2E(t *testing.T) {
	c := testcheck.New(t)
	address := freshDatabase(c)
	execSQL(c, address, `CREATE TABLE t (id bigint PRIMARY KEY)`, `CREATE VIEW v SQL SECURITY INVOKER AS SELECT id FROM t`)
	output, err := hostCommand(c, "db", "read", "--db-url", withSearchPath(c, address))
	c.Assert(err, qt.IsNotNil)
	c.Assert(output, qt.Contains, `view "public"."v" is not described by the provider yet`)
}
