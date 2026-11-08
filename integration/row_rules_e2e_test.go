//go:build integration

package integration_test

import (
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// Ptah makes an index unique by DROP INDEX and then CREATE UNIQUE INDEX, and
// changes a column's nullability and length one statement each. Spanner runs
// each statement on its own, so when the rows break the new rule, the
// statements before it would stay applied: the table would lose its index, or
// keep half of the change (stokaro/ptah#4204 S94). These tests run the host
// against the emulator and show the provider refuse such a plan before its
// first statement, and apply the changes the rows keep.

// `ptah schema apply` of a declaration whose index becomes unique over rows
// that share a key, whose columns become NOT NULL over a null or shorter than
// a value, or which adds a NOT NULL column without a default to a table with
// rows, is refused before any statement runs, and t keeps its schema.
func TestHostRefusesARuleRowsBreakE2E(t *testing.T) {
	tests := []struct {
		name   string
		before string
		rows   string
		after  string
		want   string
		schema []string
	}{
		{name: "an index made unique",
			before: "CREATE TABLE t (id bigint PRIMARY KEY, a bigint);\nCREATE INDEX t_a ON t (a);\n",
			rows:   `INSERT INTO t (id, a) VALUES (1, 5), (2, 5), (3, NULL), (4, NULL)`,
			after:  "CREATE TABLE t (id bigint PRIMARY KEY, a bigint);\nCREATE UNIQUE INDEX t_a ON t (a);\n",
			want:   `unit 2 of 2 creates unique index "t_a" on table "t", which 2 rows break, so no unit ran: Spanner refuses a unique index over rows that share a key`,
			schema: []string{"a YES bigint", "id NO bigint", "t_a NO"}},
		{name: "two columns made NOT NULL",
			before: "CREATE TABLE t (id bigint PRIMARY KEY, a bigint, b bigint);\n",
			rows:   `INSERT INTO t (id, a, b) VALUES (1, 1, NULL), (2, 2, 2)`,
			after:  "CREATE TABLE t (id bigint PRIMARY KEY, a bigint NOT NULL, b bigint NOT NULL);\n",
			want:   `unit 2 of 2 makes column "b" of table "t" NOT NULL, which 1 row breaks, so no unit ran: Spanner refuses NOT NULL over a null`,
			schema: []string{"a YES bigint", "b YES bigint", "id NO bigint"}},
		{name: "a NOT NULL column without a default added",
			before: "CREATE TABLE t (id bigint PRIMARY KEY, a bigint);\n",
			rows:   `INSERT INTO t (id, a) VALUES (1, 1), (2, 2)`,
			after:  "CREATE TABLE t (id bigint PRIMARY KEY, a bigint, c bigint NOT NULL);\n",
			want:   `unit 2 of 2 makes column "c" of table "t" NOT NULL, which 2 rows break, so no unit ran: Spanner refuses NOT NULL over a null`,
			schema: []string{"a YES bigint", "id NO bigint"}},
		{name: "two columns shortened",
			before: "CREATE TABLE t (id bigint PRIMARY KEY, s1 varchar(20), s2 varchar(20));\n",
			rows:   `INSERT INTO t (id, s1, s2) VALUES (1, 'abc', 'héllo'), (2, NULL, NULL)`,
			after:  "CREATE TABLE t (id bigint PRIMARY KEY, s1 varchar(10), s2 varchar(3));\n",
			want:   `unit 2 of 2 limits column "s2" of table "t" to 3 characters, which 1 row breaks, so no unit ran: Spanner refuses a limit a value exceeds`,
			schema: []string{"id NO bigint", "s1 YES character varying(20)", "s2 YES character varying(20)"}},
		{name: "an array's elements shortened",
			before: "CREATE TABLE t (id bigint PRIMARY KEY, s1 varchar(20), xs varchar(10)[]);\n",
			rows:   `INSERT INTO t (id, s1, xs) VALUES (1, 'abc', ARRAY['abcd', 'a'])`,
			after:  "CREATE TABLE t (id bigint PRIMARY KEY, s1 varchar(10), xs varchar(3)[]);\n",
			want:   `unit 2 of 2 limits the elements of column "xs" of table "t" to 3 characters, which 1 row breaks, so no unit ran`,
			schema: []string{"id NO bigint", "s1 YES character varying(20)", "xs YES character varying(10)[]"}},
		{name: "bytea that is not UTF-8 changed to varchar",
			before: "CREATE TABLE t (id bigint PRIMARY KEY, s1 varchar(20), x bytea);\n",
			rows:   `INSERT INTO t (id, s1, x) VALUES (1, 'abc', '\xff'::bytea)`,
			after:  "CREATE TABLE t (id bigint PRIMARY KEY, s1 varchar(10), x varchar(5));\n",
			want: `the provider could not count the rows that break the limit of 5 characters on column "x" of table "t", which unit 2 of 2 sets, so no unit ran: ` +
				`Spanner refused the count with SQLSTATE P0001`,
			schema: []string{"id NO bigint", "s1 YES character varying(20)", "x YES bytea"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			skipBeforeConstraints(c)
			address := withSearchPath(c, freshDatabase(c))
			output, err := applySQL(c, address, test.before)
			c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
			execSQL(c, address, test.rows)

			output, err = applySQL(c, address, test.after)
			c.Assert(err, qt.ErrorMatches, "exit status 2", qt.Commentf("%s", output))
			c.Assert(output, qt.Contains, "NOT_APPLIED (0 units confirmed applied): "+test.want)
			c.Assert(schemaOf(c, address), qt.DeepEquals, test.schema)
		})
	}
}

// Changes the rows keep apply and converge: an index made unique over rows
// whose shared key holds a null, which Spanner leaves out of a unique index;
// two columns made NOT NULL over values; and two columns shortened to lengths
// their values fit, counted in characters.
func TestHostAppliesARuleTheRowsKeepE2E(t *testing.T) {
	tests := []struct {
		name   string
		before string
		rows   string
		after  string
		schema []string
	}{
		{name: "an index made unique over a key with nulls",
			before: "CREATE TABLE t (id bigint PRIMARY KEY, a bigint, b bigint);\nCREATE INDEX t_ab ON t (a, b);\n",
			rows:   `INSERT INTO t (id, a, b) VALUES (1, 5, NULL), (2, 5, NULL), (3, 6, 1)`,
			after:  "CREATE TABLE t (id bigint PRIMARY KEY, a bigint, b bigint);\nCREATE UNIQUE INDEX t_ab ON t (a, b);\n",
			schema: []string{"a YES bigint", "b YES bigint", "id NO bigint", "t_ab YES"}},
		{name: "two columns made NOT NULL",
			before: "CREATE TABLE t (id bigint PRIMARY KEY, a bigint, b bigint);\n",
			rows:   `INSERT INTO t (id, a, b) VALUES (1, 1, 1)`,
			after:  "CREATE TABLE t (id bigint PRIMARY KEY, a bigint NOT NULL, b bigint NOT NULL);\n",
			schema: []string{"a NO bigint", "b NO bigint", "id NO bigint"}},
		{name: "two columns shortened to fit their values",
			before: "CREATE TABLE t (id bigint PRIMARY KEY, s1 varchar(20), s2 varchar(20));\n",
			rows:   `INSERT INTO t (id, s1, s2) VALUES (1, 'abc', 'héllo')`,
			after:  "CREATE TABLE t (id bigint PRIMARY KEY, s1 varchar(10), s2 varchar(5));\n",
			schema: []string{"id NO bigint", "s1 YES character varying(10)", "s2 YES character varying(5)"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			skipBeforeConstraints(c)
			address := withSearchPath(c, freshDatabase(c))
			output, err := applySQL(c, address, test.before)
			c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
			execSQL(c, address, test.rows)

			output, err = applySQL(c, address, test.after)
			c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
			c.Assert(schemaOf(c, address), qt.DeepEquals, test.schema)
			output, err = planSQL(c, address, test.after)
			c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
			c.Assert(output, qt.Contains, synced)
		})
	}
}
