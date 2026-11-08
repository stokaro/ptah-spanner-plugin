//go:build integration

package integration_test

import (
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// Each test here drives the host binary through the provider against the
// emulator, from a SQL schema document. The statements each plan holds are
// the ones Ptah's built-in Spanner engine wrote for the same documents,
// measured at host d59a3b63d on the emulator behind PGAdapter 0.56.2; where
// the built-in planned a statement Spanner refuses, or never converged, the
// test says what the provider does instead.

const createdTables = `CREATE TABLE edges (a bigint, b bigint, note text, PRIMARY KEY (b, a));
CREATE TABLE ty (id bigint PRIMARY KEY, i int, n numeric, f float8, r real, bo boolean, d date, ts timestamptz, j jsonb, by bytea,
  v varchar, vn varchar(7), tx text, ar bigint[], tar text[], u uuid);
CREATE TABLE t (id bigint PRIMARY KEY, a varchar(20), b bigint, c text);
CREATE INDEX t_i ON t (a DESC, b ASC NULLS FIRST) INCLUDE (c);
CREATE UNIQUE INDEX t_u ON t (c);
`

// A composite key in key order, every column type the PostgreSQL interface
// has, and indexes with a direction, a NULLS ordering, stored columns and
// uniqueness are created and converge. The built-in never converged on int
// and the array columns: it planned ALTER COLUMN ... TYPE for them on every
// run.
func TestHostCreatesTablesWithKeysTypesAndIndexesE2E(t *testing.T) {
	c := testcheck.New(t)
	address := withSearchPath(c, freshDatabase(c))
	output, err := applySQL(c, address, createdTables)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, "CREATE TABLE \"edges\" (\n  \"a\" bigint,\n  \"b\" bigint,\n  \"note\" text,\n  PRIMARY KEY (\"b\", \"a\")\n);\n")
	c.Assert(output, qt.Contains, "CREATE TABLE \"t\" (\n  \"id\" bigint PRIMARY KEY NOT NULL,\n  \"a\" varchar(20),\n  \"b\" bigint,\n  \"c\" text\n);\n")
	c.Assert(output, qt.Contains, "CREATE INDEX \"t_i\" ON \"t\" (\"a\" DESC, \"b\" NULLS FIRST) INCLUDE (\"c\");\nCREATE UNIQUE INDEX \"t_u\" ON \"t\" (\"c\");\n")
	c.Assert(queryRows(c, address, `SELECT index_name, column_name, column_ordering FROM information_schema.index_columns
WHERE table_name = 't' AND index_name = 't_i' ORDER BY ordinal_position NULLS LAST, column_name`), qt.DeepEquals,
		[]string{"t_i a DESC", "t_i b ASC NULLS FIRST", "t_i c <nil>"})
	c.Assert(queryRows(c, address, `SELECT column_name FROM information_schema.index_columns
WHERE table_name = 'edges' AND index_name = 'PRIMARY_KEY' ORDER BY ordinal_position`), qt.DeepEquals, []string{"b", "a"})

	output, err = planSQL(c, address, createdTables)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, synced)
}

// An index of a kept table that changes its direction, its stored columns,
// its uniqueness or the order of its keys is dropped and created again under
// its name.
func TestHostChangesIndexesOfAKeptTableE2E(t *testing.T) {
	c := testcheck.New(t)
	address := withSearchPath(c, freshDatabase(c))
	const table = "CREATE TABLE t (id bigint PRIMARY KEY, a varchar(20), b bigint, c text);\n"
	output, err := applySQL(c, address, table+"CREATE INDEX t_a ON t (a);\nCREATE INDEX t_b ON t (b);\nCREATE INDEX t_c ON t (c);\nCREATE INDEX t_d ON t (a, b);\n")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))

	changed := table + "CREATE INDEX t_a ON t (a DESC);\nCREATE INDEX t_b ON t (b) INCLUDE (a, c);\nCREATE UNIQUE INDEX t_c ON t (c);\nCREATE INDEX t_d ON t (b, a);\n"
	output, err = applySQL(c, address, changed)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, "DROP INDEX \"t_a\";\nDROP INDEX \"t_b\";\nDROP INDEX \"t_c\";\nDROP INDEX \"t_d\";\n")
	c.Assert(output, qt.Contains, "CREATE INDEX \"t_a\" ON \"t\" (\"a\" DESC);\nCREATE INDEX \"t_b\" ON \"t\" (\"b\") INCLUDE (\"a\", \"c\");\nCREATE UNIQUE INDEX \"t_c\" ON \"t\" (\"c\");\n"+
		"CREATE INDEX \"t_d\" ON \"t\" (\"b\", \"a\");\n")
	c.Assert(queryRows(c, address, `SELECT column_name FROM information_schema.index_columns WHERE table_name = 't' AND index_name = 't_d' ORDER BY ordinal_position`),
		qt.DeepEquals, []string{"b", "a"})
	output, err = planSQL(c, address, changed)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, synced)
}

// A dropped table loses its indexes first, and so does a dropped column:
// Spanner refuses to drop either while an index stands on it (`Cannot drop
// table u with indices: u_n`).
func TestHostDropsATableAndAnIndexedColumnE2E(t *testing.T) {
	c := testcheck.New(t)
	address := withSearchPath(c, freshDatabase(c))
	output, err := applySQL(c, address, "CREATE TABLE t (id bigint PRIMARY KEY, a varchar(20), b bigint);\nCREATE INDEX t_a ON t (a);\n"+
		"CREATE TABLE u (id bigint PRIMARY KEY, n bigint);\nCREATE INDEX u_n ON u (n);\n")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))

	const kept = "CREATE TABLE t (id bigint PRIMARY KEY, b bigint);\n"
	output, err = applySQL(c, address, kept)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Matches, `(?s).*DROP INDEX "t_a";\n.*ALTER TABLE "t" DROP COLUMN "a";\n.*`)
	c.Assert(output, qt.Matches, `(?s).*DROP INDEX "u_n";\n.*DROP TABLE "u";\n.*`)
	output, err = planSQL(c, address, kept)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, synced)
}

// The type changes Spanner makes in place, the length of character varying
// and character varying to and from bytea, are one ALTER COLUMN each, and
// converge.
func TestHostChangesColumnTypesE2E(t *testing.T) {
	c := testcheck.New(t)
	address := withSearchPath(c, freshDatabase(c))
	output, err := applySQL(c, address, "CREATE TABLE t (id bigint PRIMARY KEY, a varchar(20), c text, d text, e varchar(10));\n")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))

	const changed = "CREATE TABLE t (id bigint PRIMARY KEY, a varchar(40), c varchar(100), d bytea, e varchar(5));\n"
	output, err = applySQL(c, address, changed)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, "ALTER TABLE \"t\" ALTER COLUMN \"a\" TYPE varchar(40);\nALTER TABLE \"t\" ALTER COLUMN \"c\" TYPE varchar(100);\n"+
		"ALTER TABLE \"t\" ALTER COLUMN \"d\" TYPE bytea;\nALTER TABLE \"t\" ALTER COLUMN \"e\" TYPE varchar(5);\n")
	c.Assert(queryRows(c, address, `SELECT column_name, spanner_type FROM information_schema.columns WHERE table_name = 't' ORDER BY ordinal_position`),
		qt.DeepEquals, []string{"id bigint", "a character varying(40)", "c character varying(100)", "d bytea", "e character varying(5)"})
	output, err = planSQL(c, address, changed)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, synced)
}

// A type change Spanner refuses, and any type change of a key column, is
// refused before a statement is sent. The built-in planned `ALTER COLUMN "b"
// TYPE float8`, which Spanner refused at apply with `Cannot change type of
// column`.
func TestHostRefusesAColumnTypeChangeE2E(t *testing.T) {
	c := testcheck.New(t)
	address := withSearchPath(c, freshDatabase(c))
	output, err := applySQL(c, address, "CREATE TABLE t (id varchar(10) PRIMARY KEY, b bigint);\n")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))

	output, err = planSQL(c, address, "CREATE TABLE t (id varchar(10) PRIMARY KEY, b float8);\n")
	c.Assert(err, qt.IsNotNil)
	c.Assert(output, qt.Contains, `changing column "b" of table "t" from bigint to double precision: Spanner changes a column's type only between character varying and bytea, or the length of character varying`)
	output, err = planSQL(c, address, "CREATE TABLE t (id varchar(20) PRIMARY KEY, b bigint);\n")
	c.Assert(err, qt.IsNotNil)
	c.Assert(output, qt.Contains, `changing key column "id" of table "t" from character varying(10) to character varying(20): Spanner cannot change a key column`)
}

// A default is set, changed and dropped with one ALTER COLUMN each; a number
// and a boolean are written bare, which Spanner requires, and the stored
// spelling `'5'::bigint` reads back as the declared 5.
func TestHostChangesDefaultsE2E(t *testing.T) {
	c := testcheck.New(t)
	address := withSearchPath(c, freshDatabase(c))
	output, err := applySQL(c, address, "CREATE TABLE t (id bigint PRIMARY KEY, b bigint DEFAULT 0, c text DEFAULT 'x', d bigint DEFAULT 1, e boolean);\n")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))

	const changed = "CREATE TABLE t (id bigint PRIMARY KEY, b bigint DEFAULT 5, c text DEFAULT 'y', d bigint, e boolean DEFAULT true);\n"
	output, err = applySQL(c, address, changed)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, "ALTER TABLE \"t\" ALTER COLUMN \"b\" SET DEFAULT 5;\nALTER TABLE \"t\" ALTER COLUMN \"c\" SET DEFAULT 'y';\n"+
		"ALTER TABLE \"t\" ALTER COLUMN \"d\" DROP DEFAULT;\nALTER TABLE \"t\" ALTER COLUMN \"e\" SET DEFAULT true;\n")
	output, err = planSQL(c, address, changed)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, synced)
}

// NOT NULL is set, with the note Ptah's built-in Spanner engine wrote, and
// dropped; a NOT NULL column without a default is added nullable and then
// made NOT NULL, because Spanner refuses to add it otherwise.
func TestHostChangesNullabilityE2E(t *testing.T) {
	c := testcheck.New(t)
	address := withSearchPath(c, freshDatabase(c))
	output, err := applySQL(c, address, "CREATE TABLE t (id bigint PRIMARY KEY, a varchar(20), b bigint NOT NULL);\n")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))

	const changed = "CREATE TABLE t (id bigint PRIMARY KEY, a varchar(20) NOT NULL, b bigint, d bigint NOT NULL);\n"
	output, err = applySQL(c, address, changed)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, "ALTER TABLE \"t\" ADD COLUMN \"d\" bigint;\n"+
		"-- SPANNER: SET NOT NULL fails if any row of \"t\" holds NULL in \"d\"; the column declares no default to fill it with.\n"+
		"ALTER TABLE \"t\" ALTER COLUMN \"d\" SET NOT NULL;\n")
	c.Assert(output, qt.Contains, "-- SPANNER: SET NOT NULL fails if any row of \"t\" holds NULL in \"a\"; the column declares no default to fill it with.\n"+
		"ALTER TABLE \"t\" ALTER COLUMN \"a\" SET NOT NULL;\nALTER TABLE \"t\" ALTER COLUMN \"b\" DROP NOT NULL;\n")
	c.Assert(queryRows(c, address, `SELECT column_name, is_nullable FROM information_schema.columns WHERE table_name = 't' ORDER BY ordinal_position`),
		qt.DeepEquals, []string{"id NO", "a NO", "b YES", "d NO"})
	output, err = planSQL(c, address, changed)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, synced)
}

// A change of a primary key's members or of its order is refused with the
// reason, before a statement is sent. The built-in planned `DROP CONSTRAINT
// "PK_t"`, which Spanner refused at apply with `PK_t is not a constraint in
// t`.
func TestHostRefusesAPrimaryKeyChangeE2E(t *testing.T) {
	c := testcheck.New(t)
	address := withSearchPath(c, freshDatabase(c))
	const tables = "CREATE TABLE t (id bigint PRIMARY KEY, b bigint NOT NULL);\nCREATE TABLE k (a bigint, b bigint, PRIMARY KEY (a, b));\n"
	output, err := applySQL(c, address, tables)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))

	output, err = planSQL(c, address, "CREATE TABLE t (id bigint, b bigint NOT NULL, PRIMARY KEY (id, b));\nCREATE TABLE k (a bigint, b bigint, PRIMARY KEY (a, b));\n")
	c.Assert(err, qt.IsNotNil)
	c.Assert(output, qt.Contains, `changing the primary key of table "t" from (id) to (id, b): Spanner cannot change a table's primary key; create a new table with the key and copy the rows`)
	output, err = planSQL(c, address, "CREATE TABLE t (id bigint PRIMARY KEY, b bigint NOT NULL);\nCREATE TABLE k (a bigint, b bigint, PRIMARY KEY (b, a));\n")
	c.Assert(err, qt.IsNotNil)
	c.Assert(output, qt.Contains, `changing the primary key of table "k" from (a, b) to (b, a): Spanner cannot change a table's primary key`)
}

// A declaration Spanner cannot hold is refused before anything runs: a
// foreign key on jsonb in the built-in's words, and a type Spanner does not
// have.
func TestHostRefusesDeclarationsSpannerCannotHoldE2E(t *testing.T) {
	c := testcheck.New(t)
	address := withSearchPath(c, freshDatabase(c))
	output, err := planSQL(c, address, "CREATE TABLE pj (id bigint PRIMARY KEY, j jsonb);\n"+
		"CREATE TABLE cj (id bigint PRIMARY KEY, j jsonb, CONSTRAINT cj_fk FOREIGN KEY (j) REFERENCES pj (j));\n")
	c.Assert(err, qt.IsNotNil)
	c.Assert(output, qt.Contains, `invalid foreign key: Spanner type jsonb cannot participate in foreign keys: "cj"."j" references "pj"."j"`)
	output, err = planSQL(c, address, "CREATE TABLE s (id bigint PRIMARY KEY, n smallint);\n")
	c.Assert(err, qt.IsNotNil)
	c.Assert(output, qt.Contains, `column "n" of table "s": type "smallint": Spanner stores integers as bigint only`)
}

// notNullBase is the table the NOT NULL tests start from, and notNullName the
// column they add to it, in the YAML Ptah's built-in Spanner engine was
// measured with.
const (
	notNullBase = "tables:\n  items:\n    fields:\n      id:\n        type: BIGINT\n        primary: true\n"
	notNullName = "      name:\n        type: TEXT\n        nullable: false\n"
)

// A NOT NULL column that declares a default is added in one statement to a
// table holding a row, which the default fills, and the next plan is synced.
// Spanner takes the column with its default on a table with rows; it refuses
// a NOT NULL column without one (`Cannot add NOT NULL column items.name to
// existing table items`).
func TestHostAddsANotNullColumnToATableWithARowE2E_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	address := withSearchPath(c, freshDatabase(c))
	execSQL(c, address, "CREATE TABLE items (id bigint PRIMARY KEY)", "INSERT INTO items (id) VALUES (1)")
	schema := notNullBase + notNullName + "        default: \"'x'\"\n"
	output, err := policyApply(c, address, "schema.yaml", schema, apply)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, "ALTER TABLE \"items\" ADD COLUMN \"name\" TEXT NOT NULL DEFAULT 'x';\n")
	c.Assert(queryRows(c, address, `SELECT column_name, is_nullable FROM information_schema.columns WHERE table_name = 'items' ORDER BY ordinal_position`),
		qt.DeepEquals, []string{"id NO", "name NO"})
	c.Assert(queryRows(c, address, `SELECT id, name FROM items ORDER BY id`), qt.DeepEquals, []string{"1 x"})
	output, err = policyApply(c, address, "schema.yaml", schema, dryRun)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, synced)
}

// A NOT NULL column without a default is added nullable and then made NOT
// NULL. On a table holding a row the row has no value for it, so Spanner
// would refuse the SET NOT NULL after the ADD COLUMN committed. The provider
// counts the rows before the first statement and refuses the run: the column
// is not added, and the next plan is the same two statements.
func TestHostAddsANotNullColumnToATableWithARowE2E_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	address := withSearchPath(c, freshDatabase(c))
	execSQL(c, address, "CREATE TABLE items (id bigint PRIMARY KEY)", "INSERT INTO items (id) VALUES (1)")
	const note = "-- SPANNER: SET NOT NULL fails if any row of \"items\" holds NULL in \"name\"; the column declares no default to fill it with.\n"
	const plan = "ALTER TABLE \"items\" ADD COLUMN \"name\" TEXT;\n" + note + "ALTER TABLE \"items\" ALTER COLUMN \"name\" SET NOT NULL;\n"
	output, err := policyApply(c, address, "schema.yaml", notNullBase+notNullName, apply)
	c.Assert(err, qt.IsNotNil)
	c.Assert(output, qt.Contains, plan)
	c.Assert(output, qt.Contains, `NOT_APPLIED (0 units confirmed applied): unit 2 of 2 makes column "name" of table "items" NOT NULL, which 1 row breaks, so no unit ran`)
	c.Assert(queryRows(c, address, `SELECT column_name, is_nullable FROM information_schema.columns WHERE table_name = 'items' ORDER BY ordinal_position`),
		qt.DeepEquals, []string{"id NO"})
	output, err = policyApply(c, address, "schema.yaml", notNullBase+notNullName, dryRun)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, plan)
}

// keywordColumns are columns named after the words that open a table-level
// index on MySQL. Spanner takes each as a column name, and a type with
// parentheses is the shape a reader of the MySQL element would turn into an
// index named after the type, losing the column without an error.
const keywordColumns = "id bigint PRIMARY KEY, key text, spatial varchar(32), fulltext text, index varchar(32)"

// keywordRefusal is the words of a host that reads a table element of an
// engine a provider serves as MySQL reads it: the host read these words as
// columns for Spanner through the built-in Spanner descriptor, which the
// deletion of the built-in engine removed.
const keywordRefusal = "declares a table-level index on spanner rather than a column named after the word"

// keywordChange names the host change the test needs.
const keywordChange = "reading KEY, SPATIAL, FULLTEXT and INDEX as column names for an engine a provider serves"

// A SQL schema document whose columns are named key, spatial, fulltext and
// index creates those columns, and they agree with the columns Spanner gives
// a table the same statement creates, which the next plan leaves alone.
func TestHostCreatesColumnsNamedAfterIndexKeywordsE2E(t *testing.T) {
	c := testcheck.New(t)
	address := withSearchPath(c, freshDatabase(c))
	execSQL(c, address, "CREATE TABLE served ("+keywordColumns+")")
	const schema = "CREATE TABLE served (" + keywordColumns + ");\nCREATE TABLE kw (" + keywordColumns + ");\n"
	output, err := applySQL(c, address, schema)
	skipOnOldHost(c, output, err, keywordRefusal, keywordChange)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, "CREATE TABLE \"kw\" (\n  \"id\" bigint PRIMARY KEY NOT NULL,\n  \"key\" text,\n  \"spatial\" varchar(32),\n  \"fulltext\" text,\n  \"index\" varchar(32)\n);\n")
	c.Assert(output, qt.Not(qt.Contains), `"served"`)
	const columns = `SELECT column_name, spanner_type FROM information_schema.columns WHERE table_name = '%s' ORDER BY ordinal_position`
	c.Assert(queryRows(c, address, strings.Replace(columns, "%s", "kw", 1)), qt.DeepEquals, queryRows(c, address, strings.Replace(columns, "%s", "served", 1)))
	c.Assert(queryRows(c, address, strings.Replace(columns, "%s", "kw", 1)), qt.DeepEquals,
		[]string{"id bigint", "key character varying", "spatial character varying(32)", "fulltext character varying", "index character varying(32)"})
	output, err = planSQL(c, address, schema)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, synced)
}

// indexCommented is a table with an index, and commented the same with a
// comment on the index.
const (
	indexCommented = "CREATE TABLE u (id bigint PRIMARY KEY, n bigint);\nCREATE INDEX u_n_ix ON u (n);\n"
	commented      = indexCommented + "COMMENT ON INDEX u_n_ix IS 'first';\n"
)

// A comment on an index is refused before a statement is sent, on a created
// index and on a kept one, as a table's and a column's comment is: Spanner's
// PostgreSQL interface answers COMMENT ON INDEX with `Unknown statement`.
// Ptah's built-in Spanner engine skipped the comment with a note, and the
// provider left it out silently, so a plan reported synced while the
// declaration held a comment the database never would.
func TestHostRefusesAnIndexCommentE2E(t *testing.T) {
	c := testcheck.New(t)
	address := withSearchPath(c, freshDatabase(c))
	const refusal = `index "u_n_ix" of table "u" has a comment; Spanner's PostgreSQL interface stores no comment`
	output, err := applySQL(c, address, commented)
	c.Assert(err, qt.IsNotNil)
	c.Assert(output, qt.Contains, refusal)
	c.Assert(queryRows(c, address, `SELECT table_name FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'u'`), qt.HasLen, 0)

	output, err = applySQL(c, address, indexCommented)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	output, err = planSQL(c, address, commented)
	c.Assert(err, qt.IsNotNil)
	c.Assert(output, qt.Contains, refusal)
	output, err = planSQL(c, address, indexCommented)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, synced)
}
