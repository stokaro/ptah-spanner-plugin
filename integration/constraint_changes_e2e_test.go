//go:build integration

package integration_test

import (
	"os"
	"path/filepath"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// The tests here need a host of contract 25.28, which reads the provider's
// foreign keys and CHECKs, hands it a kept table's constraint changes, asks
// it about CHECK equivalence and compares index NULLS orders. Each plan holds
// the statements Ptah's built-in Spanner engine wrote for the same documents,
// measured at host d59a3b63d on the emulator behind PGAdapter 0.56.2, except
// where the built-in never converged.

const (
	keyedTables = `CREATE TABLE p (id bigint PRIMARY KEY, name text);
CREATE TABLE q (id bigint PRIMARY KEY);
CREATE TABLE pa (a bigint, b bigint, PRIMARY KEY (a, b));
`
	keyedBefore = keyedTables + `CREATE TABLE ch (id bigint PRIMARY KEY, p_id bigint, q_id bigint, CONSTRAINT ch_p_fk FOREIGN KEY (p_id) REFERENCES p (id));
CREATE TABLE ca (id bigint PRIMARY KEY, pa_a bigint, pa_b bigint);
`
	keyedChanged = keyedTables + `CREATE TABLE ch (id bigint PRIMARY KEY, p_id bigint, q_id bigint,
  CONSTRAINT ch_p_fk FOREIGN KEY (p_id) REFERENCES p (id) ON DELETE CASCADE, CONSTRAINT ch_q_fk FOREIGN KEY (q_id) REFERENCES q (id));
CREATE TABLE ca (id bigint PRIMARY KEY, pa_a bigint, pa_b bigint, CONSTRAINT ca_pa_fk FOREIGN KEY (pa_a, pa_b) REFERENCES pa (a, b));
`
	// keyedAfter removes p, the key that references it and the column q_id
	// with its key.
	keyedAfter = `CREATE TABLE q (id bigint PRIMARY KEY);
CREATE TABLE pa (a bigint, b bigint, PRIMARY KEY (a, b));
CREATE TABLE ch (id bigint PRIMARY KEY, p_id bigint);
CREATE TABLE ca (id bigint PRIMARY KEY, pa_a bigint, pa_b bigint, CONSTRAINT ca_pa_fk FOREIGN KEY (pa_a, pa_b) REFERENCES pa (a, b));
`
)

// heldForeignKeys lists the foreign keys of the database at address as
// "<table> <name> <delete rule>".
func heldForeignKeys(c *qt.C, address string) []string {
	c.Helper()
	return queryRows(c, address, `SELECT c.table_name, c.constraint_name, r.delete_rule FROM information_schema.table_constraints c
JOIN information_schema.referential_constraints r ON r.constraint_name = c.constraint_name
WHERE c.constraint_type = 'FOREIGN KEY' ORDER BY c.constraint_name`)
}

// A kept table's foreign keys change one statement each, as the built-in
// changed them: a key added, a key whose rule changes dropped and added
// again, and a key dropped before the column it names and before the table
// it references, which Spanner refuses to drop while the key stands. Each
// drop carries no IF EXISTS, which Spanner refuses (stokaro/ptah#4204 S2),
// and each state converges.
func TestHostChangesForeignKeysOfKeptTablesE2E(t *testing.T) {
	c := testcheck.New(t)
	skipBeforeConstraints(c)
	address := withSearchPath(c, freshDatabase(c))
	output, err := applySQL(c, address, keyedBefore)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))

	output, err = applySQL(c, address, keyedChanged)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, `ALTER TABLE "ca" ADD CONSTRAINT "ca_pa_fk" FOREIGN KEY ("pa_a", "pa_b") REFERENCES "pa"("a", "b");`+"\n")
	c.Assert(output, qt.Contains, `ALTER TABLE "ch" DROP CONSTRAINT "ch_p_fk";`+"\n"+
		`ALTER TABLE "ch" ADD CONSTRAINT "ch_p_fk" FOREIGN KEY ("p_id") REFERENCES "p"("id") ON DELETE CASCADE;`+"\n"+
		`ALTER TABLE "ch" ADD CONSTRAINT "ch_q_fk" FOREIGN KEY ("q_id") REFERENCES "q"("id");`+"\n")
	c.Assert(heldForeignKeys(c, address), qt.DeepEquals, []string{"ca ca_pa_fk NO ACTION", "ch ch_p_fk CASCADE", "ch ch_q_fk NO ACTION"})
	output, err = planSQL(c, address, keyedChanged)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, synced)

	output, err = applySQL(c, address, keyedAfter)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Matches, `(?s).*ALTER TABLE "ch" DROP CONSTRAINT "ch_p_fk";\n.*DROP TABLE "p";\n.*`)
	c.Assert(output, qt.Matches, `(?s).*ALTER TABLE "ch" DROP CONSTRAINT "ch_q_fk";\n.*ALTER TABLE "ch" DROP COLUMN "q_id";\n.*`)
	c.Assert(output, qt.Not(qt.Contains), "IF EXISTS")
	c.Assert(heldForeignKeys(c, address), qt.DeepEquals, []string{"ca ca_pa_fk NO ACTION"})
	output, err = planSQL(c, address, keyedAfter)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, synced)
}

const (
	checkedBefore = `CREATE TABLE k (id bigint PRIMARY KEY, score bigint NOT NULL DEFAULT 0, status text, price float8,
  CONSTRAINT k_score_check CHECK (score >= 0));
`
	// checkedChanged changes k_score_check, adds three CHECKs, and adds the
	// column w with a CHECK of its own.
	checkedChanged = `CREATE TABLE k (id bigint PRIMARY KEY, score bigint NOT NULL DEFAULT 0, status text, price float8,
  w bigint CONSTRAINT k_w_check CHECK (w BETWEEN 1 AND 10),
  CONSTRAINT k_score_check CHECK (score >= 1), CONSTRAINT k_status_check CHECK (status IN ('active', 'retired')),
  CONSTRAINT k_price_check CHECK (price > 0));
`
	checkedAfter = `CREATE TABLE k (id bigint PRIMARY KEY, score bigint NOT NULL DEFAULT 0, status text, price float8,
  CONSTRAINT k_score_check CHECK (score >= 1), CONSTRAINT k_price_check CHECK (price > 0));
`
)

// heldChecks lists the CHECKs of the database at address as "<name> <clause>",
// less the ones Spanner keeps for NOT NULL columns.
func heldChecks(c *qt.C, address string) []string {
	c.Helper()
	return queryRows(c, address, `SELECT constraint_name, check_clause FROM information_schema.check_constraints
WHERE constraint_name NOT LIKE 'CK_IS_NOT_NULL_%' ORDER BY constraint_name`)
}

// A CHECK Spanner keeps in a form of its own converges: `score >= 0` is the
// held `(score >= '0'::bigint)`, which Ptah's built-in Spanner engine dropped
// and added on every run (stokaro/ptah#4204 S1), and so are an IN list, a
// BETWEEN and a comparison of double precision with an integer, which the
// server rewrites too. A changed CHECK is dropped and added again, a column
// added with a CHECK is added before it, and a CHECK is dropped before the
// column it names, as the built-in planned them.
func TestHostChangesChecksOfKeptTablesE2E(t *testing.T) {
	c := testcheck.New(t)
	skipBeforeConstraints(c)
	address := withSearchPath(c, freshDatabase(c))
	output, err := applySQL(c, address, checkedBefore)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	output, err = planSQL(c, address, checkedBefore)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, synced)

	output, err = applySQL(c, address, checkedChanged)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, `ALTER TABLE "k" ADD COLUMN "w" bigint;`+"\n")
	c.Assert(output, qt.Matches, `(?s).*ALTER TABLE "k" ADD COLUMN "w" bigint;\n.*ALTER TABLE "k" ADD CONSTRAINT "k_w_check" CHECK \(w BETWEEN 1 AND 10\);\n.*`)
	c.Assert(output, qt.Contains, `ALTER TABLE "k" DROP CONSTRAINT "k_score_check";`+"\n")
	c.Assert(output, qt.Matches, `(?s).*ALTER TABLE "k" DROP CONSTRAINT "k_score_check";\n.*ALTER TABLE "k" ADD CONSTRAINT "k_score_check" CHECK \(score >= 1\);\n.*`)
	c.Assert(output, qt.Contains, `ALTER TABLE "k" ADD CONSTRAINT "k_price_check" CHECK (price > 0);`+"\n")
	c.Assert(output, qt.Contains, `ALTER TABLE "k" ADD CONSTRAINT "k_status_check" CHECK (status IN ('active', 'retired'));`+"\n")
	c.Assert(heldChecks(c, address), qt.DeepEquals, []string{
		"k_price_check (price > ('0'::bigint)::double precision)",
		"k_score_check (score >= '1'::bigint)",
		"k_status_check (status = ANY (ARRAY['active'::text, 'retired'::text]))",
		"k_w_check ((w >= '1'::bigint) AND (w <= '10'::bigint))",
	})
	output, err = planSQL(c, address, checkedChanged)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, synced)

	output, err = applySQL(c, address, checkedAfter)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, `ALTER TABLE "k" DROP CONSTRAINT "k_status_check";`+"\n")
	c.Assert(output, qt.Matches, `(?s).*ALTER TABLE "k" DROP CONSTRAINT "k_w_check";\n.*ALTER TABLE "k" DROP COLUMN "w";\n.*`)
	c.Assert(heldChecks(c, address), qt.DeepEquals, []string{"k_price_check (price > ('0'::bigint)::double precision)", "k_score_check (score >= '1'::bigint)"})
	output, err = planSQL(c, address, checkedAfter)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, synced)
}

const (
	nullsTable  = "CREATE TABLE t (id bigint PRIMARY KEY, a bigint, b bigint);\n"
	nullsBefore = nullsTable + "CREATE INDEX t_a ON t (a);\nCREATE INDEX t_b ON t (b DESC);\nCREATE INDEX t_c ON t (a, b DESC);\n"
	// nullsAfter turns t_a and t_b against their direction's default and
	// spells t_c's defaults.
	nullsAfter = nullsTable + "CREATE INDEX t_a ON t (a NULLS FIRST);\nCREATE INDEX t_b ON t (b DESC NULLS LAST);\n" +
		"CREATE INDEX t_c ON t (a ASC NULLS LAST, b DESC NULLS FIRST);\n"
)

// An index key whose NULLS order changes is a change of the index: it is
// dropped and created again, as the built-in planned it. A key that spells
// its direction's default, NULLS LAST ascending or NULLS FIRST descending, is
// the key that leaves it out.
func TestHostComparesIndexNullsOrdersE2E(t *testing.T) {
	c := testcheck.New(t)
	skipBeforeConstraints(c)
	address := withSearchPath(c, freshDatabase(c))
	output, err := applySQL(c, address, nullsBefore)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))

	output, err = applySQL(c, address, nullsAfter)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, "DROP INDEX \"t_a\";\nDROP INDEX \"t_b\";\n")
	c.Assert(output, qt.Contains, "CREATE INDEX \"t_a\" ON \"t\" (\"a\" NULLS FIRST);\nCREATE INDEX \"t_b\" ON \"t\" (\"b\" DESC NULLS LAST);\n")
	c.Assert(output, qt.Not(qt.Contains), `"t_c"`)
	c.Assert(queryRows(c, address, `SELECT index_name, column_name, column_ordering FROM information_schema.index_columns
WHERE table_name = 't' AND index_name <> 'PRIMARY_KEY' ORDER BY index_name, ordinal_position`), qt.DeepEquals,
		[]string{"t_a a ASC NULLS FIRST", "t_b b DESC NULLS LAST", "t_c a ASC", "t_c b DESC"})
	output, err = planSQL(c, address, nullsAfter)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, synced)
}

// migrationTables are two tables referencing each other and one referencing
// a third, as the register's down-file measurement declared them.
const migrationTables = `CREATE TABLE u (id bigint PRIMARY KEY);
CREATE TABLE p (id bigint PRIMARY KEY, name text);
CREATE TABLE ch (id bigint PRIMARY KEY, p_id bigint, CONSTRAINT ch_p_fk FOREIGN KEY (p_id) REFERENCES p (id));
CREATE TABLE x (id bigint PRIMARY KEY, y_id bigint, CONSTRAINT x_y_fk FOREIGN KEY (y_id) REFERENCES y (id));
CREATE TABLE y (id bigint PRIMARY KEY, x_id bigint, CONSTRAINT y_x_fk FOREIGN KEY (x_id) REFERENCES x (id));
`

// `ptah migrations generate` of tables with foreign keys writes a down file
// that drops every key before the tables, since Spanner refuses to drop a
// table a key references, as the built-in's down file did; the migration
// runs up and down. The down file's plan needs a host that reads the
// provider's constraints and predicts the keys the up file adds
// (stokaro/ptah#4204 S41).
func TestHostGeneratesMigrationsWithForeignKeysE2E(t *testing.T) {
	c := testcheck.New(t)
	skipBeforeConstraints(c)
	address := withSearchPath(c, freshDatabase(c))
	dir := writeFiles(c, map[string]string{"schema.sql": migrationTables})
	migrations := c.TempDir()
	output, err := hostCommand(c, "migrations", "generate", "--db-url", address, "--schema-file", filepath.Join(dir, "schema.sql"),
		"--migrations-dir", migrations, "--name", "init")
	skipOnOldHost(c, output, err, "provider planner requires captured table state", "the host's prediction of a provider table's foreign keys")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	down, err := filepath.Glob(filepath.Join(migrations, "*_init.down.sql"))
	c.Assert(err, qt.IsNil)
	c.Assert(down, qt.HasLen, 1)
	downText, err := os.ReadFile(down[0])
	c.Assert(err, qt.IsNil)
	c.Assert(string(downText), qt.Matches, `(?s).*\nALTER TABLE "ch" DROP CONSTRAINT "ch_p_fk";\nALTER TABLE "x" DROP CONSTRAINT "x_y_fk";\n`+
		`ALTER TABLE "y" DROP CONSTRAINT "y_x_fk";\n(DROP TABLE "[a-z]+";\n){5}.*`)

	output, err = hostCommand(c, "migrations", "up", "--db-url", address, "--migrations-dir", migrations)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(heldForeignKeys(c, address), qt.DeepEquals, []string{"ch ch_p_fk NO ACTION", "x x_y_fk NO ACTION", "y y_x_fk NO ACTION"})
	output, err = hostCommand(c, "migrations", "down", "--db-url", address, "--migrations-dir", migrations, "--target", "0", "--confirm")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(queryRows(c, address, `SELECT table_name FROM information_schema.tables WHERE table_schema = 'public' AND table_name IN ('u', 'p', 'ch', 'x', 'y')`),
		qt.HasLen, 0)
}
