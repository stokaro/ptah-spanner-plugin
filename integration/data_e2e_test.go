//go:build integration

package integration_test

import (
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"os"
	"path/filepath"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// The tests here drive the host binary through the provider's row and seed
// services against the emulator. What Ptah's built-in Spanner engine did with
// the same inputs, measured at host d59a3b63d on the emulator behind PGAdapter
// 0.56.2, is said where the provider does something else.

// declaredModels declares the table kinds and its rows in kinds.yaml.
const declaredModels = `package data

//ptah:schema:table name="kinds"
//ptah:schema:data table="kinds" key="id" file="kinds.yaml"
type Kind struct {
	//ptah:schema:field name="id" type="BIGINT" primary="true"
	ID int64
	//ptah:schema:field name="b" type="BOOLEAN"
	B bool
	//ptah:schema:field name="f" type="DOUBLE PRECISION"
	F float64
	//ptah:schema:field name="n" type="NUMERIC"
	N string
	//ptah:schema:field name="t" type="TIMESTAMPTZ"
	T string
	//ptah:schema:field name="d" type="DATE"
	D string
	//ptah:schema:field name="s" type="VARCHAR(100)"
	S string
	//ptah:schema:field name="j" type="JSONB"
	J string
	//ptah:schema:field name="by" type="BYTEA"
	By string
}
`

// declaredRows are written in spellings Spanner stores in another form: an
// exponent, unsorted JSON with 1.50 and 2e0, a moment with no offset and one
// below the microsecond, and bytea in hex digits.
const declaredRows = `- id: 1
  b: true
  f: 1.5
  n: "1e3"
  t: "2026-01-02 03:04:05"
  d: "2026-01-02"
  s: "it's"
  j: '{"b": 1, "a": [1.50, 2e0]}'
  by: '\x6869'
- id: 2
  b: false
  f: -0.25
  n: "12.50"
  t: "2026-06-30T23:59:59.1234567Z"
  d: "1999-12-31"
  s: "ünïcode"
  j: '[]'
  by: ""
`

// declaredProject writes the Go declaration and its rows into a directory of
// their own and returns it.
func declaredProject(c *qt.C) string {
	c.Helper()
	return writeFiles(c, map[string]string{"models.go": declaredModels, "kinds.yaml": declaredRows, "go.mod": "module data\n\ngo 1.26\n"})
}

// Declared rows are written with the table, read back in the values the
// declaration names, and converge: the second plan holds nothing although
// Spanner stores 1e3 as 1000, sorts the JSON members, writes 2e0 as 2 and
// rounds the moment to the microsecond. A moment written without an offset is
// UTC; the built-in sent it bare, and Spanner read it in America/Los_Angeles.
// Text in bytea hex is the bytes it spells, as the built-in stored it.
func TestHostAppliesDeclaredRowsAndConvergesE2E(t *testing.T) {
	c := testcheck.New(t)
	address := withSearchPath(c, freshDatabase(c))
	project := declaredProject(c)
	output, err := hostCommand(c, "schema", "apply", "--db-url", address, "--root-dir", project, "--auto-approve")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, `INSERT INTO "kinds" ("b", "by", "d", "f", "id", "j", "n", "s", "t") VALUES (TRUE, '\x6869'::bytea, '2026-01-02', 1.5, 1, `+
		`'{"a":[1.5,2],"b":1}', '1000', 'it''s', '2026-01-02 03:04:05+00:00');`)
	c.Assert(queryRows(c, address, `SELECT id, b, f, n::text, d::text, s, j::text, by FROM kinds ORDER BY id`), qt.DeepEquals, []string{
		`1 true 1.5 1000 2026-01-02 it's {"a": [1.5, 2], "b": 1} [104 105]`,
		`2 false -0.25 12.5 1999-12-31 ünïcode [] []`,
	})
	c.Assert(queryRows(c, address, `SELECT id FROM kinds WHERE t = '2026-01-02T03:04:05Z' OR t = '2026-06-30T23:59:59.123457Z' ORDER BY id`),
		qt.DeepEquals, []string{"1", "2"})

	output, err = hostCommand(c, "schema", "apply", "--db-url", address, "--root-dir", project, "--dry-run")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, synced)
}

// Rows that drifted are written back: a deleted row is inserted again and a
// changed one updated. `migrations data` writes the same statements, and its
// down file restores what the database held, in the provider's spelling: the
// built-in wrote the live bytea as '\x'::bytea too, but the live moment in the
// zone of the machine running Ptah.
func TestHostPlansDeclaredRowDriftE2E(t *testing.T) {
	c := testcheck.New(t)
	address := withSearchPath(c, freshDatabase(c))
	project := declaredProject(c)
	output, err := hostCommand(c, "schema", "apply", "--db-url", address, "--root-dir", project, "--auto-approve")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	execSQL(c, address, "UPDATE kinds SET s = 'changed', by = '\\x00ff'::bytea WHERE id = 2", "DELETE FROM kinds WHERE id = 1")

	output, err = hostCommand(c, "migrations", "data", "--db-url", address, "--root-dir", project, "--migrations-dir", c.TempDir(),
		"--dry-run", "--allow-destructive")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, `UPDATE "kinds" SET "b" = FALSE, "by" = '\x'::bytea, "d" = '1999-12-31', "f" = -0.25, "j" = '[]', "n" = '12.5', `+
		`"s" = 'ünïcode', "t" = '2026-06-30 23:59:59.123457+00:00' WHERE "id" = 2;`)
	c.Assert(output, qt.Contains, `UPDATE "kinds" SET "b" = FALSE, "by" = '\x00ff'::bytea, "d" = '1999-12-31', "f" = -0.25, "j" = '[]', "n" = '12.5', `+
		`"s" = 'changed', "t" = '2026-06-30 23:59:59.123457+00:00' WHERE "id" = 2;`)
	c.Assert(output, qt.Contains, `DELETE FROM "kinds" WHERE "id" = 1;`)

	output, err = hostCommand(c, "schema", "apply", "--db-url", address, "--root-dir", project, "--auto-approve")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(queryRows(c, address, `SELECT id, s, by FROM kinds ORDER BY id`), qt.DeepEquals, []string{"1 it's [104 105]", "2 ünïcode []"})
	output, err = hostCommand(c, "schema", "apply", "--db-url", address, "--root-dir", project, "--dry-run")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, synced)
}

const seedTables = "CREATE TABLE countries (code varchar(2) PRIMARY KEY, name varchar(255) NOT NULL);\n" +
	"CREATE TABLE users (id bigint PRIMARY KEY, email varchar(100));\n"

// seedFiles are the seed files every seed test starts from.
var seedFiles = map[string]string{
	"001_countries.all.sql": "INSERT INTO countries (code, name) VALUES ('US', 'United States');\nINSERT INTO countries (code, name) VALUES ('DE', 'Germany');\n",
	"002_users.dev.sql":     "INSERT INTO users (id, email) VALUES (1, 'dev@example.com');\n",
}

// seedDatabase is a database holding the seed tables.
func seedDatabase(c *qt.C) string {
	c.Helper()
	address := withSearchPath(c, freshDatabase(c))
	output, err := applySQL(c, address, seedTables)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	return address
}

// seedDirectory writes the seed files and extra ones into a directory.
func seedDirectory(c *qt.C, extra map[string]string) string {
	c.Helper()
	files := make(map[string]string, len(seedFiles)+len(extra))
	maps.Copy(files, seedFiles)
	maps.Copy(files, extra)
	return writeFiles(c, files)
}

func checksum(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// A seed run applies the files of its environment and records each in
// schema_seeds, the ledger and layout the built-in engine kept; a second run
// skips them, and a file edited since it ran stops the run.
func TestHostSeedsAndRecordsTheLedgerE2E(t *testing.T) {
	c := testcheck.New(t)
	address := seedDatabase(c)
	seeds := seedDirectory(c, nil)
	output, err := hostCommand(c, "seed", "--db-url", address, "--env", "dev", "--seeds-dir", seeds)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, "Applied seeds: 2")
	c.Assert(queryRows(c, address, `SELECT code, name FROM countries ORDER BY code`), qt.DeepEquals, []string{"DE Germany", "US United States"})
	c.Assert(queryRows(c, address, `SELECT seed_path, env, checksum FROM schema_seeds ORDER BY seed_path`), qt.DeepEquals, []string{
		"001_countries.all.sql dev " + checksum(seedFiles["001_countries.all.sql"]),
		"002_users.dev.sql dev " + checksum(seedFiles["002_users.dev.sql"]),
	})
	c.Assert(queryRows(c, address, `SELECT column_name, spanner_type, is_nullable FROM information_schema.columns
WHERE table_name = 'schema_seeds' ORDER BY ordinal_position`), qt.DeepEquals, []string{
		"seed_path character varying(512) NO", "env character varying(128) NO", "checksum character varying NO", "applied_at timestamp with time zone NO",
	})

	output, err = hostCommand(c, "seed", "--db-url", address, "--env", "dev", "--seeds-dir", seeds)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, "Skipped seeds: 2")

	c.Assert(os.WriteFile(filepath.Join(seeds, "002_users.dev.sql"), []byte("INSERT INTO users (id, email) VALUES (2, 'x@example.com');\n"), 0o600), qt.IsNil)
	output, err = hostCommand(c, "seed", "--db-url", address, "--env", "dev", "--seeds-dir", seeds)
	c.Assert(err, qt.IsNotNil)
	c.Assert(output, qt.Matches, `(?s).*002_users\.dev\.sql.*checksum.*`)
	c.Assert(queryRows(c, address, `SELECT id FROM users ORDER BY id`), qt.DeepEquals, []string{"1"})
}

// A file whose statement hits an existing key keeps nothing of itself and
// records nothing. With --idempotent the conflict reads as data already
// there: the file is rolled back whole and recorded. The built-in failed
// that run on `ROLLBACK TO SAVEPOINT`, which Spanner does not have
// (`This connection has no active transaction`).
func TestHostSeedKeyConflictKeepsNothingE2E(t *testing.T) {
	c := testcheck.New(t)
	address := seedDatabase(c)
	seeds := seedDirectory(c, map[string]string{
		"003_again.all.sql": "INSERT INTO countries (code, name) VALUES ('FR', 'France');\nINSERT INTO countries (code, name) VALUES ('US', 'United States');\n",
	})
	output, err := hostCommand(c, "seed", "--db-url", address, "--env", "dev", "--seeds-dir", seeds)
	c.Assert(err, qt.IsNotNil)
	c.Assert(output, qt.Contains, "003_again.all.sql")
	c.Assert(queryRows(c, address, `SELECT code FROM countries ORDER BY code`), qt.DeepEquals, []string{"DE", "US"})
	c.Assert(queryRows(c, address, `SELECT seed_path FROM schema_seeds ORDER BY seed_path`), qt.DeepEquals, []string{"001_countries.all.sql", "002_users.dev.sql"})

	output, err = hostCommand(c, "seed", "--db-url", address, "--env", "dev", "--seeds-dir", seeds, "--idempotent")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(queryRows(c, address, `SELECT code FROM countries ORDER BY code`), qt.DeepEquals, []string{"DE", "US"})
	c.Assert(queryRows(c, address, `SELECT seed_path FROM schema_seeds ORDER BY seed_path`), qt.DeepEquals,
		[]string{"001_countries.all.sql", "002_users.dev.sql", "003_again.all.sql"})
}

// A file holding a statement Spanner runs only outside a transaction is
// refused before anything of it runs, at that statement. The built-in ran the
// file's first statement and failed on the second inside its transaction.
func TestHostRefusesASeedSchemaStatementBeforeRunningE2E(t *testing.T) {
	c := testcheck.New(t)
	address := seedDatabase(c)
	seeds := writeFiles(c, map[string]string{
		"001_ddl.all.sql": "INSERT INTO countries (code, name) VALUES ('NL', 'Netherlands');\nCREATE TABLE seeded (id bigint PRIMARY KEY);\n",
	})
	output, err := hostCommand(c, "seed", "--db-url", address, "--env", "dev", "--seeds-dir", seeds)
	c.Assert(err, qt.IsNotNil)
	c.Assert(output, qt.Contains, "the provider refuses the file before running it, at line 2, column 1: "+
		"statement 2 is a schema statement, which Spanner runs only outside a transaction; a seed file runs in one")
	c.Assert(queryRows(c, address, `SELECT code FROM countries`), qt.HasLen, 0)
	c.Assert(queryRows(c, address, `SELECT table_name FROM information_schema.tables WHERE table_name = 'seeded'`), qt.HasLen, 0)
}

// A database the built-in engine seeded keeps its ledger: an entry the
// built-in wrote, in the table its statement created, skips the file it
// names. A table of the ledger's name in another layout is never adopted.
func TestHostContinuesTheBuiltInSeedLedgerE2E(t *testing.T) {
	c := testcheck.New(t)
	address := seedDatabase(c)
	execSQL(c, address, `CREATE TABLE IF NOT EXISTS schema_seeds (
    seed_path VARCHAR(512) PRIMARY KEY,
    env VARCHAR(128) NOT NULL,
    checksum TEXT NOT NULL,
    applied_at TIMESTAMPTZ NOT NULL
)`, "INSERT INTO schema_seeds (seed_path, env, checksum, applied_at) VALUES ('001_countries.all.sql', 'dev', '"+
		checksum(seedFiles["001_countries.all.sql"])+"', CURRENT_TIMESTAMP)")
	output, err := hostCommand(c, "seed", "--db-url", address, "--env", "dev", "--seeds-dir", seedDirectory(c, nil))
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, "Applied seeds: 1")
	c.Assert(output, qt.Contains, "Skipped seeds: 1")
	c.Assert(queryRows(c, address, `SELECT code FROM countries`), qt.HasLen, 0)

	foreign := seedDatabase(c)
	execSQL(c, foreign, "CREATE TABLE schema_seeds (seed_path varchar(512) PRIMARY KEY, note text)")
	output, err = hostCommand(c, "seed", "--db-url", foreign, "--env", "dev", "--seeds-dir", seedDirectory(c, nil))
	c.Assert(err, qt.IsNotNil)
	c.Assert(output, qt.Contains, "the table public.schema_seeds holds another layout than the seed ledger the provider keeps")
	c.Assert(queryRows(c, foreign, `SELECT code FROM countries`), qt.HasLen, 0)
}
