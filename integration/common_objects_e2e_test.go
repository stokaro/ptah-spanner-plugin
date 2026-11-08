//go:build integration

package integration_test

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// The tests in this file declare views and sequences, which travel on the
// host's common-object wire, and drive the host binary through the provider
// against the emulator. The statements are the ones Ptah's built-in Spanner
// engine wrote for the same declarations, measured at host d59a3b63d on the
// emulator behind PGAdapter 0.56.2.

const viewTable = "CREATE TABLE t (id bigint PRIMARY KEY, a bigint, b varchar(20));\n"

// A view is created after its table with its query on a line of its own, as
// declared, and converges against the spelling Spanner stores it in, is replaced in place when its
// query changes and dropped when it is no longer declared. A read writes it
// back, `schema stats` counts it, and a generated migration replaces it back
// in place, where the built-in dropped it and created it again.
func TestHostCreatesReplacesAndDropsAViewE2E(t *testing.T) {
	c := testcheck.New(t)
	address := withSearchPath(c, freshDatabase(c))
	const declared = viewTable + "CREATE VIEW v AS select  \"id\", a from t;\n"
	output, err := applySQL(c, address, declared)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, "CREATE VIEW \"v\" AS\nselect  \"id\", a from t;\n")
	c.Assert(queryRows(c, address, `SELECT table_name, view_definition FROM information_schema.views WHERE table_schema = 'public'`), qt.DeepEquals,
		[]string{"v SELECT id, a FROM t"})
	for _, spelling := range []string{declared, viewTable + "CREATE VIEW v AS SELECT id, a FROM t;\n"} {
		output, err = planSQL(c, address, spelling)
		c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
		c.Assert(output, qt.Contains, synced)
	}

	output, err = hostCommand(c, "schema", "inspect", "--db-url", address, "--format", "hcl")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, "view \"v\" {\n  schema = schema.public\n  as = \"SELECT id, a FROM t\"\n}\n")
	output, err = hostCommand(c, "schema", "stats", "--db-url", address)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, "ptah_schema_views{dialect=\"spanner\"} 1\n")

	const replaced = viewTable + "CREATE VIEW v AS SELECT id, a, b FROM t;\n"
	migrations := c.TempDir()
	path := filepath.Join(writeFiles(c, map[string]string{"schema.sql": replaced}), "schema.sql")
	output, err = hostCommand(c, "migrations", "generate", "--db-url", address, "--schema-file", path, "--migrations-dir", migrations, "--name", "second")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	down, err := filepath.Glob(filepath.Join(migrations, "*_second.down.sql"))
	c.Assert(err, qt.IsNil)
	c.Assert(down, qt.HasLen, 1)
	reverse, err := os.ReadFile(down[0])
	c.Assert(err, qt.IsNil)
	c.Assert(string(reverse), qt.Contains, "\nCREATE OR REPLACE VIEW \"v\" AS\nSELECT id, a FROM t;")

	output, err = applySQL(c, address, replaced)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, "CREATE OR REPLACE VIEW \"v\" AS\nSELECT id, a, b FROM t;\n")
	output, err = planSQL(c, address, replaced)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, synced)

	output, err = applySQL(c, address, viewTable)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, "DROP VIEW \"v\";\n")
	c.Assert(queryRows(c, address, `SELECT table_name FROM information_schema.views WHERE table_schema = 'public'`), qt.HasLen, 0)
}

const sequenceTable = "CREATE TABLE ts (id bigint NOT NULL DEFAULT nextval('s1') PRIMARY KEY, x bigint);\n"

// A sequence is created before the table that draws from it, with the
// counter start a declaration states, and a changed start restarts the
// counter. A default that draws from a sequence converges although Spanner
// stores it as nextval('s1'::text); read without the cast, it would be set
// again on every plan, as the built-in set it. A read writes the sequences
// back with their counter starts, through the quoted catalog spelling.
func TestHostCreatesRestartsAndDropsSequencesE2E(t *testing.T) {
	c := testcheck.New(t)
	address := withSearchPath(c, freshDatabase(c))
	const v1 = "CREATE SEQUENCE s1;\nCREATE SEQUENCE s2 START WITH 1000;\n" + sequenceTable
	output, err := applySQL(c, address, v1)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, "CREATE SEQUENCE \"s1\";\nCREATE SEQUENCE \"s2\" START COUNTER WITH 1000;\nCREATE TABLE \"ts\" (\n")
	output, err = planSQL(c, address, v1)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, synced)

	output, err = hostCommand(c, "schema", "inspect", "--db-url", address, "--format", "hcl")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, "sequence \"s1\" {\n  schema = schema.public\n  type = \"bigint\"\n  start = 1\n}\n")
	c.Assert(output, qt.Contains, "sequence \"s2\" {\n  schema = schema.public\n  type = \"bigint\"\n  start = 1000\n}\n")
	output, err = hostCommand(c, "schema", "stats", "--db-url", address)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, "ptah_schema_sequences{dialect=\"spanner\"} 2\n")

	const v2 = "CREATE SEQUENCE s1;\nCREATE SEQUENCE s2 START WITH 5000;\nCREATE SEQUENCE s3;\n" + sequenceTable
	output, err = applySQL(c, address, v2)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, "CREATE SEQUENCE \"s3\";\nALTER SEQUENCE \"s2\" RESTART COUNTER WITH 5000;\n")
	c.Assert(queryRows(c, address, `SELECT sequence_name, counter_start_value FROM "information_schema".sequences ORDER BY sequence_name`), qt.DeepEquals,
		[]string{"s1 1", "s2 5000", "s3 1"})
	output, err = planSQL(c, address, v2)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, synced)

	output, err = applySQL(c, address, "CREATE SEQUENCE s1;\n"+sequenceTable)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, "DROP SEQUENCE \"s2\";\nDROP SEQUENCE \"s3\";\n")
	c.Assert(queryRows(c, address, `SELECT sequence_name FROM "information_schema".sequences ORDER BY sequence_name`), qt.DeepEquals, []string{"s1"})
}

// What Spanner cannot hold is refused before a statement is sent: a sequence
// option other than its counter start, and a sequence that skips a range,
// which the common sequence has no place for, is refused by the read rather
// than left out of it.
func TestHostRefusesViewsAndSequencesSpannerCannotHoldE2E(t *testing.T) {
	c := testcheck.New(t)
	address := withSearchPath(c, freshDatabase(c))
	output, err := planSQL(c, address, "CREATE SEQUENCE s INCREMENT BY 2;\nCREATE TABLE t (id bigint PRIMARY KEY);\n")
	c.Assert(err, qt.IsNotNil)
	c.Assert(output, qt.Contains, `sequence "s" declares INCREMENT, MINVALUE, MAXVALUE, CACHE or CYCLE: a Spanner sequence is bit-reversed and takes START COUNTER WITH alone`)

	execSQL(c, address, `CREATE SEQUENCE skips BIT_REVERSED_POSITIVE SKIP RANGE 1 100`)
	output, err = planSQL(c, address, "CREATE TABLE t (id bigint PRIMARY KEY);\n")
	c.Assert(err, qt.IsNotNil)
	c.Assert(output, qt.Contains, `sequence "public"."skips" skips a range of values, which the provider does not describe yet`)
}

// One plan that removes a table, its index and a view that reads it drops the
// index and the view before the table, each without CASCADE, which Spanner
// refuses whatever the object, and nothing is left to plan. Spanner also
// refuses to drop a table that still has an index or a view that reads it.
func TestHostRemovesATableWithItsIndexAndViewE2E(t *testing.T) {
	c := testcheck.New(t)
	address := withSearchPath(c, freshDatabase(c))
	execSQL(c, address, `CREATE TABLE gone (id bigint PRIMARY KEY, kind text)`, `CREATE INDEX gone_kind ON gone (kind)`,
		`CREATE VIEW gone_v SQL SECURITY INVOKER AS SELECT g.id FROM gone g`)
	const kept = "CREATE TABLE kept (id bigint PRIMARY KEY);\n"
	output, err := applySQL(c, address, kept)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Matches, `(?s).*DROP INDEX "gone_kind";\n.*DROP TABLE "gone";\n.*`)
	c.Assert(output, qt.Matches, `(?s).*DROP VIEW "gone_v";\n.*DROP TABLE "gone";\n.*`)
	c.Assert(output, qt.Not(qt.Contains), "CASCADE")
	c.Assert(queryRows(c, address, `SELECT table_name FROM information_schema.tables WHERE table_schema = 'public' AND table_name LIKE 'gone%'`), qt.HasLen, 0)
	output, err = planSQL(c, address, kept)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, synced)
}

// viewShapes are views of a table t and a table u, each with its query as a
// schema document declares it.
const viewShapes = `CREATE TABLE t (id bigint PRIMARY KEY, name varchar(20), flag boolean, price double precision);
CREATE TABLE u (id bigint PRIMARY KEY, t_id bigint, label text);
CREATE VIEW v_plain AS SELECT id, name FROM t;
CREATE VIEW v_spelled AS select "id", name from t;
CREATE VIEW v_count AS SELECT count(*) AS n FROM t;
CREATE VIEW v_call AS SELECT id, lower(name) AS lname FROM t;
CREATE VIEW v_flag AS SELECT id FROM t WHERE flag;
CREATE VIEW v_distinct AS SELECT DISTINCT name FROM t;
CREATE VIEW v_union AS SELECT id FROM t UNION ALL SELECT id FROM u;
CREATE VIEW v_quoted AS SELECT id AS "Id" FROM t;
CREATE VIEW v_star AS SELECT * FROM t;
CREATE VIEW v_literal AS SELECT id FROM t WHERE id > 0;
CREATE VIEW v_in AS SELECT id FROM t WHERE name IN ('a', 'b');
CREATE VIEW v_join AS SELECT t.id, u.label FROM t JOIN u ON u.t_id = t.id;
CREATE VIEW v_case AS SELECT id, CASE WHEN id > 5 THEN 'big' ELSE 'small' END AS size FROM t;
CREATE VIEW v_limit AS SELECT id FROM t ORDER BY id LIMIT 10;
CREATE VIEW v_alias AS SELECT x.id FROM t AS x;
CREATE VIEW v_price AS SELECT id FROM t WHERE price > 1.5;
`

// replacedView finds the view a plan replaces.
var replacedView = regexp.MustCompile(`(?m)^CREATE OR REPLACE VIEW "([a-z_]+)" AS$`)

// Spanner stores its own rendering of a view's query: `WHERE id > 0` is
// stored as `WHERE (id > '0'::bigint)`, a join in parentheses, a LIMIT cast
// to bigint, an alias without AS and its columns unqualified, every column of
// a SELECT * named and the columns of a UNION qualified.
//
// A host older than contract 25.33 compares the texts by its own reading: it
// folds case, quotes and spacing, strips the qualifiers of a UNION's columns
// and expands a SELECT * with the declared columns, so those views converge,
// and any view whose query Spanner rewrites further is replaced on every plan,
// as Ptah's built-in Spanner engine replaced it.
//
// A host of 25.37 asks the provider with the columns of every declared table
// (schema.view-query-context). The provider reads both queries into one form
// (commonobject.SameViewQuery) and leaves a pair it cannot read, here the
// CASE, to the host's reading, so only that view is replaced; the columns'
// types are what read `price > 1.5` as Spanner's
// `price > (1.5)::double precision`. A host of 25.33 to 25.36 asks without
// the columns and takes no undecided answer: the provider reads the queries
// without column types, and the comparison of a double precision column and
// the SELECT * it cannot expand are replaced as well.
func TestHostReplacesTheViewsSpannerRewritesE2E(t *testing.T) {
	c := testcheck.New(t)
	address := withSearchPath(c, freshDatabase(c))
	output, err := applySQL(c, address, viewShapes)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(queryRows(c, address, `SELECT view_definition FROM information_schema.views WHERE table_name IN ('v_literal', 'v_join', 'v_star') ORDER BY table_name`),
		qt.DeepEquals, []string{
			"SELECT t.id, u.label FROM (t JOIN u ON ((u.t_id = t.id)))",
			"SELECT id FROM t WHERE (id > '0'::bigint)",
			"SELECT id, name, flag, price FROM t",
		})
	output, err = planSQL(c, address, viewShapes)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	var replaced []string
	for _, match := range replacedView.FindAllStringSubmatch(output, -1) {
		replaced = append(replaced, match[1])
	}
	want := map[string][]string{
		"text":     {"v_alias", "v_case", "v_in", "v_join", "v_limit", "v_literal", "v_price"},
		"provider": {"v_case", "v_price", "v_star"},
		"context":  {"v_case"},
	}[hostViewQueries(c)]
	c.Assert(replaced, qt.DeepEquals, want, qt.Commentf("%s", output))
}
