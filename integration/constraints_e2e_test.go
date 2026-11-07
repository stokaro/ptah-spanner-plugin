//go:build integration

package integration_test

import (
	"os"
	"path/filepath"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// hostChange is the host change the tests in this file need: a host that
// lacks it refuses a created table's constraints before the provider plans
// them, and writes no SQL for a provider that owns its tables.
const hostChange = "stokaro/ptah#5559"

const foreignKeyTables = `CREATE TABLE p (id bigint PRIMARY KEY, name text);
CREATE TABLE ch (id bigint PRIMARY KEY, p_id bigint, CONSTRAINT ch_p_fk FOREIGN KEY (p_id) REFERENCES p (id));
CREATE TABLE x (id bigint PRIMARY KEY, y_id bigint, CONSTRAINT x_y_fk FOREIGN KEY (y_id) REFERENCES y (id));
CREATE TABLE y (id bigint PRIMARY KEY, x_id bigint, CONSTRAINT y_x_fk FOREIGN KEY (x_id) REFERENCES x (id));
CREATE TABLE pa (a bigint, b bigint, PRIMARY KEY (a, b));
CREATE TABLE ca (id bigint PRIMARY KEY, pa_a bigint, pa_b bigint, CONSTRAINT ca_pa_fk FOREIGN KEY (pa_a, pa_b) REFERENCES pa (a, b) ON DELETE CASCADE);
`

// Every foreign key of the created tables is a statement after both of its
// tables, as the built-in planned them, so two tables may reference each
// other and a composite key with ON DELETE CASCADE is created too. A host of
// contract 25.28 reads the keys back, so a second run plans nothing, and a
// declaration without the tables drops every key before the tables, as the
// built-in dropped them: Spanner refuses to drop a table a key references.
func TestHostCreatesForeignKeysE2E(t *testing.T) {
	c := testcheck.New(t)
	address := withSearchPath(c, freshDatabase(c))
	output, err := applySQL(c, address, foreignKeyTables)
	skipOnOldHost(c, output, err, "does not support SchemaDiff.ConstraintsAdded", hostChange)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Matches, `(?s).*CREATE TABLE "y" \([^;]*\);\n`+
		`ALTER TABLE "ch" ADD CONSTRAINT "ch_p_fk" FOREIGN KEY \("p_id"\) REFERENCES "p"\("id"\);\n`+
		`ALTER TABLE "ca" ADD CONSTRAINT "ca_pa_fk" FOREIGN KEY \("pa_a", "pa_b"\) REFERENCES "pa"\("a", "b"\) ON DELETE CASCADE;\n`+
		`ALTER TABLE "x" ADD CONSTRAINT "x_y_fk" FOREIGN KEY \("y_id"\) REFERENCES "y"\("id"\);\n`+
		`ALTER TABLE "y" ADD CONSTRAINT "y_x_fk" FOREIGN KEY \("x_id"\) REFERENCES "x"\("id"\);\n.*`)
	c.Assert(queryRows(c, address, `SELECT c.table_name, c.constraint_name, r.delete_rule FROM information_schema.table_constraints c
JOIN information_schema.referential_constraints r ON r.constraint_name = c.constraint_name
WHERE c.constraint_type = 'FOREIGN KEY' ORDER BY c.constraint_name`), qt.DeepEquals,
		[]string{"ca ca_pa_fk CASCADE", "ch ch_p_fk NO ACTION", "x x_y_fk NO ACTION", "y y_x_fk NO ACTION"})

	skipBeforeConstraints(c)
	output, err = planSQL(c, address, foreignKeyTables)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, synced)

	output, err = applySQL(c, address, "CREATE TABLE u (id bigint PRIMARY KEY);\n")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Matches, `(?s).*CREATE TABLE "u" \([^;]*\);\n`+
		`ALTER TABLE "ca" DROP CONSTRAINT "ca_pa_fk";\nALTER TABLE "ch" DROP CONSTRAINT "ch_p_fk";\n`+
		`ALTER TABLE "x" DROP CONSTRAINT "x_y_fk";\nALTER TABLE "y" DROP CONSTRAINT "y_x_fk";\n(DROP TABLE "[a-z]+";\n){6}.*`)
	c.Assert(queryRows(c, address, `SELECT table_name FROM information_schema.tables WHERE table_schema = 'public' AND table_name <> 'ptah_locks'
ORDER BY table_name`), qt.DeepEquals, []string{"u"})
}

const checkedTables = "package entities\n\n//ptah:schema:table name=\"authors\"\ntype Author struct {\n" +
	"\t//ptah:schema:field name=\"id\" type=\"BIGINT\" primary=\"true\"\n\tID int64\n" +
	"\t//ptah:schema:field name=\"status\" type=\"TEXT\" not_null=\"true\" default=\"active\" check=\"status IN ('active', 'retired')\"\n\tStatus string\n" +
	"\t//ptah:schema:field name=\"score\" type=\"BIGINT\" not_null=\"true\" default=\"0\" check=\"score >= 0\"\n\tScore int64\n" +
	"\t//ptah:schema:field name=\"kind\" type=\"ENUM\" enum=\"a,b,c\" not_null=\"true\" default=\"a\"\n\tKind string\n" +
	"\t//ptah:schema:field name=\"price\" type=\"FLOAT8\" check=\"price > 0\"\n\tPrice float64\n}\n"

// enumRefusal is the words of a host that turns an inline enum column of an
// engine a provider serves into a standalone enum type and refuses it: the
// host lowered it to text with a CHECK through the built-in Spanner
// descriptor, which the deletion of the built-in engine removed.
const enumRefusal = "does not support SchemaDiff.EnumsAdded"

// enumChange names the host change that lets an engine a provider serves say
// how it builds an enum column (stokaro/ptah#4204 S84).
const enumChange = "stokaro/ptah#5658"

// A column's CHECK is written in its definition under the name the built-in
// gave it, `<table>_<column>_check`, and an enum column is text with its
// values in a CHECK, listed once. A host of contract 25.28 asks the provider
// whether each CHECK Spanner keeps is the declared one, so a second run plans
// nothing; the built-in dropped and added `score >= 0` and `price > 0` on
// every run (stokaro/ptah#4204 S1). A host that turns the enum column into an
// enum type refuses the declaration until stokaro/ptah#5658 lands, and the
// test skips there.
func TestHostCreatesChecksE2E(t *testing.T) {
	c := testcheck.New(t)
	entities := withSearchPath(c, freshDatabase(c))
	dir := writeFiles(c, map[string]string{"entities.go": checkedTables})
	output, err := hostCommand(c, "schema", "apply", "--db-url", entities, "--root-dir", dir, "--auto-approve")
	skipOnOldHost(c, output, err, "does not support SchemaDiff.ConstraintsAdded", hostChange)
	skipOnOldHost(c, output, err, enumRefusal, enumChange)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, "\"status\" TEXT NOT NULL DEFAULT 'active' CONSTRAINT \"authors_status_check\" CHECK (status IN ('active', 'retired')),\n"+
		"  \"score\" BIGINT NOT NULL DEFAULT 0 CONSTRAINT \"authors_score_check\" CHECK (score >= 0),\n"+
		"  \"kind\" TEXT NOT NULL DEFAULT 'a' CONSTRAINT \"authors_kind_check\" CHECK (kind IN ('a', 'b', 'c')),\n"+
		"  \"price\" FLOAT8 CONSTRAINT \"authors_price_check\" CHECK (price > 0)\n);\n")
	c.Assert(queryRows(c, entities, `SELECT constraint_name FROM information_schema.check_constraints
WHERE constraint_name NOT LIKE 'CK_IS_NOT_NULL_%' ORDER BY constraint_name`), qt.DeepEquals,
		[]string{"authors_kind_check", "authors_price_check", "authors_score_check", "authors_status_check"})

	skipBeforeConstraints(c)
	output, err = hostCommand(c, "schema", "apply", "--db-url", entities, "--root-dir", dir, "--dry-run")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, synced)
}

// A table CHECK is a statement after the table, and a host of contract 25.28
// reads it back as the declared one, so a second run plans nothing. A host
// older than that reads no CHECK and the provider refuses the read, so the
// convergence is checked only on a host that reads constraints.
func TestHostCreatesATableCheckE2E(t *testing.T) {
	c := testcheck.New(t)
	const table = "CREATE TABLE k (id bigint PRIMARY KEY, score bigint NOT NULL DEFAULT 0, CONSTRAINT k_score_check CHECK (score >= 0));\n"
	address := withSearchPath(c, freshDatabase(c))
	output, err := applySQL(c, address, table)
	skipOnOldHost(c, output, err, "does not support SchemaDiff.ConstraintsAdded", hostChange)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, "ALTER TABLE \"k\" ADD CONSTRAINT \"k_score_check\" CHECK (score >= 0);\n")
	c.Assert(queryRows(c, address, `SELECT constraint_name FROM information_schema.check_constraints
WHERE constraint_name NOT LIKE 'CK_IS_NOT_NULL_%' ORDER BY constraint_name`), qt.DeepEquals, []string{"k_score_check"})

	skipBeforeConstraints(c)
	output, err = planSQL(c, address, table)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, synced)
}

// `ptah db read` writes the tables, the keys and the indexes the provider
// reads as the SQL that creates them, each type as Spanner stores it, and
// leaves the lock table out. The built-in wrote `timestamp(0) with time zone`,
// which Spanner refuses to replay (stokaro/ptah#4204 S3), and `ARRAY` for
// both array columns.
func TestHostReadsTablesAsSQLE2E(t *testing.T) {
	c := testcheck.New(t)
	address := withSearchPath(c, freshDatabase(c))
	output, err := applySQL(c, address, createdTables)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	output, err = hostCommand(c, "db", "read", "--db-url", address)
	skipOnOldHost(c, output, err, "has no schema rendering service", hostChange)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, "CREATE TABLE \"edges\" (\n  \"a\" bigint NOT NULL,\n  \"b\" bigint NOT NULL,\n  \"note\" character varying,\n  PRIMARY KEY (\"b\", \"a\")\n);\n")
	c.Assert(output, qt.Contains, "CREATE INDEX \"t_i\" ON \"t\" (\"a\" DESC, \"b\" NULLS FIRST) INCLUDE (\"c\");\n\nCREATE UNIQUE INDEX \"t_u\" ON \"t\" (\"c\");\n")
	c.Assert(output, qt.Contains, `"ts" timestamp with time zone,`)
	c.Assert(output, qt.Contains, `"ar" bigint[],`)
	c.Assert(output, qt.Not(qt.Contains), "ptah_locks")
}

// `ptah migrations generate` writes the provider's statements to the up file
// and their inverse to the down file, the same statements Ptah's built-in
// Spanner engine wrote, with each held type and default in Spanner's
// spelling.
func TestHostGeneratesMigrationsE2E(t *testing.T) {
	c := testcheck.New(t)
	address := withSearchPath(c, freshDatabase(c))
	output, err := applySQL(c, address, "CREATE TABLE t (id bigint PRIMARY KEY, a varchar(20), b bigint DEFAULT 0, c text);\nCREATE INDEX t_i ON t (a);\n")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	dir := writeFiles(c, map[string]string{"schema.sql": "CREATE TABLE t (id bigint PRIMARY KEY, a varchar(40), b bigint DEFAULT 5, c text NOT NULL);\n" +
		"CREATE INDEX t_i ON t (a DESC);\n"})
	migrations := c.TempDir()
	output, err = hostCommand(c, "migrations", "generate", "--db-url", address, "--schema-file", filepath.Join(dir, "schema.sql"), "--migrations-dir", migrations, "--name", "change")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	up, err := filepath.Glob(filepath.Join(migrations, "*_change.up.sql"))
	c.Assert(err, qt.IsNil)
	c.Assert(up, qt.HasLen, 1)
	down, err := filepath.Glob(filepath.Join(migrations, "*_change.down.sql"))
	c.Assert(err, qt.IsNil)
	c.Assert(down, qt.HasLen, 1)
	upText, err := os.ReadFile(up[0])
	c.Assert(err, qt.IsNil)
	downText, err := os.ReadFile(down[0])
	c.Assert(err, qt.IsNil)
	c.Assert(string(upText), qt.Matches, "(?s).*\nDROP INDEX \"t_i\";\nALTER TABLE \"t\" ALTER COLUMN \"a\" TYPE varchar\\(40\\);\nALTER TABLE \"t\" ALTER COLUMN \"b\" SET DEFAULT 5;\n"+
		"(-- [^\n]*\n)?ALTER TABLE \"t\" ALTER COLUMN \"c\" SET NOT NULL;\nCREATE INDEX \"t_i\" ON \"t\" \\(\"a\" DESC\\);.*")
	c.Assert(string(downText), qt.Matches, "(?s).*\nDROP INDEX \"t_i\";\nALTER TABLE \"t\" ALTER COLUMN \"a\" TYPE character varying\\(20\\);\nALTER TABLE \"t\" ALTER COLUMN \"b\" SET DEFAULT 0;\n"+
		"ALTER TABLE \"t\" ALTER COLUMN \"c\" DROP NOT NULL;\nCREATE INDEX \"t_i\" ON \"t\" \\(\"a\"\\);.*")
}
