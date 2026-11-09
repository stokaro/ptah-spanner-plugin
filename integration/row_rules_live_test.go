//go:build integration

package integration_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// Spanner checks a unique index, NOT NULL and a shorter character limit
// against every row too, when the statement runs. The tests here run the
// provider's executor against the emulator and show the check it makes before
// the first unit for each, with Spanner's semantics: a unique index leaves out
// rows with a null key column, and a length is counted in characters.

// ruledRows is the table the tests start from: a non-unique index on a,
// whose value 5 every row holds, two of them with a null in b; a value of 5
// characters (6 bytes) in s and in the bytea column x; a bytea value in y that
// is not valid UTF-8; and an array element of 4 characters in xs.
var ruledRows = []string{
	`CREATE TABLE t (id bigint PRIMARY KEY, a bigint, b bigint, s varchar(20), x bytea, y bytea, xs varchar(10)[])`,
	`CREATE INDEX t_a ON t (a)`,
	`INSERT INTO t (id, a, b, s, x, y, xs) VALUES (1, 5, NULL, 'héllo', 'héllo'::bytea, '\xff'::bytea, ARRAY['abcd', 'a']), ` +
		`(2, 5, 2, NULL, NULL, NULL, NULL), (3, 5, NULL, 'ab', 'ab'::bytea, 'ok'::bytea, ARRAY['a'])`,
}

// ruledState is what ruledRows holds of t's schema: its columns with their
// nullability and type, and its indexes with their uniqueness.
var ruledState = []string{
	"a YES bigint", "b YES bigint", "id NO bigint", "s YES character varying(20)", "x YES bytea", "xs YES character varying(10)[]", "y YES bytea", "t_a NO",
}

// schemaOf lists the columns of table t, as "<name> <nullable> <type>", and
// then its indexes, as "<name> <unique>".
func schemaOf(c *qt.C, address string) []string {
	c.Helper()
	return append(queryRows(c, address, `SELECT column_name, is_nullable, spanner_type FROM information_schema.columns
WHERE table_schema = 'public' AND table_name = 't' ORDER BY column_name`),
		queryRows(c, address, `SELECT index_name, is_unique FROM information_schema.indexes
WHERE table_schema = 'public' AND table_name = 't' AND index_type = 'INDEX' ORDER BY index_name`)...)
}

// A request that, after its first unit, makes an index unique over rows that
// share a key, makes a column NOT NULL over a null, or limits a column's
// characters below a value it holds is refused before its first unit runs.
// The reason names the unit, the index or the column, and how many rows break
// the rule. Nothing applies: the table the request creates first is not
// created, and t keeps its columns and its index as they were.
func TestExecutionSubprocess_RefusesRulesRowsBreak(t *testing.T) {
	const created = `CREATE TABLE other (id bigint PRIMARY KEY)`
	tests := []struct {
		name    string
		units   []string
		wantErr string
	}{
		{name: "an index made unique", units: []string{`DROP INDEX "t_a"`, `CREATE UNIQUE INDEX "t_a" ON "t" ("a")`},
			wantErr: `provider execution: NOT_APPLIED \(0 units confirmed applied\): unit 2 of 2 creates unique index "t_a" on table "t", which 3 rows break, so no unit ran: ` +
				`Spanner refuses a unique index over rows that share a key; update or delete the rows that share a key before creating the index`},
		{name: "NOT NULL over nulls", units: []string{created, `ALTER TABLE "t" ALTER COLUMN "b" SET NOT NULL`},
			wantErr: `provider execution: NOT_APPLIED \(0 units confirmed applied\): unit 2 of 2 makes column "b" of table "t" NOT NULL, which 2 rows break, so no unit ran: ` +
				`Spanner refuses NOT NULL over a null; set a value in the rows that hold a null before making the column NOT NULL`},
		{name: "NOT NULL on a column the request adds without a default",
			units:   []string{`ALTER TABLE "t" ADD COLUMN "c" bigint`, `ALTER TABLE "t" ALTER COLUMN "c" SET NOT NULL`},
			wantErr: `provider execution: NOT_APPLIED \(0 units confirmed applied\): unit 2 of 2 makes column "c" of table "t" NOT NULL, which 3 rows break, so no unit ran: .*`},
		{name: "a shorter varchar", units: []string{created, `ALTER TABLE "t" ALTER COLUMN "s" TYPE varchar(4)`},
			wantErr: `provider execution: NOT_APPLIED \(0 units confirmed applied\): unit 2 of 2 limits column "s" of table "t" to 4 characters, which 1 row breaks, so no unit ran: ` +
				`Spanner refuses a limit a value exceeds; shorten the values that exceed it before limiting the column`},
		{name: "bytea to a shorter varchar", units: []string{created, `ALTER TABLE t ALTER COLUMN x TYPE varchar(4)`},
			wantErr: `provider execution: NOT_APPLIED \(0 units confirmed applied\): unit 2 of 2 limits column "x" of table "t" to 4 characters, which 1 row breaks, so no unit ran: .*`},
		{name: "bytea that is not UTF-8 to varchar", units: []string{created, `ALTER TABLE t ALTER COLUMN y TYPE varchar`},
			wantErr: `provider execution: NOT_APPLIED \(0 units confirmed applied\): the provider could not count the rows that break the limit of 2621440 characters ` +
				`on column "y" of table "t", which unit 2 of 2 sets, so no unit ran: Spanner refused the count with SQLSTATE P0001`},
		{name: "an array's elements shortened", units: []string{created, `ALTER TABLE t ALTER COLUMN xs TYPE varchar(3)[]`},
			wantErr: `provider execution: NOT_APPLIED \(0 units confirmed applied\): unit 2 of 2 limits the elements of column "xs" of table "t" to 3 characters, which 1 row breaks, .*`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			address := freshDatabase(c)
			execSQL(c, address, ruledRows...)
			client := startProvider(c)
			session := openSession(c, client, address)
			execution, err := client.Execute(c.Context(), &protocol.ExecuteRequest{SessionId: session.GetId(), RequestId: "e1", Units: sqlUnits(test.units...)})
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(execution.GetOutcome(), qt.Equals, protocol.Execution_NOT_APPLIED)
			c.Assert(execution.GetAppliedCount(), qt.Equals, uint32(0))
			c.Assert(tables(c, address), qt.DeepEquals, []string{"t"})
			c.Assert(schemaOf(c, address), qt.DeepEquals, ruledState)
		})
	}
}

// Requests the rows keep apply. A composite unique index over (5, NULL) twice
// and (5, 2) applies, because Spanner leaves out a row with a null key column. A
// bytea value of 5 characters in 6 bytes fits varchar(5), because Spanner
// counts characters. A longer limit is not counted. And the executor leaves
// NOT NULL to Spanner after a unit that fills the nulls, an UPDATE or a
// column added with a default: a count before the request would have refused
// the first and could not have read the column of the second.
func TestExecutionSubprocess_RunsRulesTheRowsKeep(t *testing.T) {
	const created = `CREATE TABLE other (id bigint PRIMARY KEY)`
	tests := []struct {
		name   string
		units  []string
		schema []string
	}{
		{name: "a composite unique index over null key columns", units: []string{created, `CREATE UNIQUE INDEX t_ab ON t (a, b)`},
			schema: []string{"a YES bigint", "b YES bigint", "id NO bigint", "s YES character varying(20)", "x YES bytea",
				"xs YES character varying(10)[]", "y YES bytea", "t_a NO", "t_ab YES"}},
		{name: "bytea to a varchar as long as a value in characters", units: []string{created, `ALTER TABLE t ALTER COLUMN x TYPE varchar(5)`},
			schema: []string{"a YES bigint", "b YES bigint", "id NO bigint", "s YES character varying(20)", "x YES character varying(5)",
				"xs YES character varying(10)[]", "y YES bytea", "t_a NO"}},
		{name: "a longer varchar", units: []string{created, `ALTER TABLE t ALTER COLUMN s TYPE varchar(30)`},
			schema: []string{"a YES bigint", "b YES bigint", "id NO bigint", "s YES character varying(30)", "x YES bytea",
				"xs YES character varying(10)[]", "y YES bytea", "t_a NO"}},
		{name: "nulls filled before NOT NULL", units: []string{`UPDATE t SET b = 0 WHERE b IS NULL`, `ALTER TABLE t ALTER COLUMN b SET NOT NULL`},
			schema: []string{"a YES bigint", "b NO bigint", "id NO bigint", "s YES character varying(20)", "x YES bytea",
				"xs YES character varying(10)[]", "y YES bytea", "t_a NO"}},
		{name: "a column added with a default made NOT NULL", units: []string{`ALTER TABLE t ADD COLUMN c bigint DEFAULT 1`, `ALTER TABLE t ALTER COLUMN c SET NOT NULL`},
			schema: []string{"a YES bigint", "b YES bigint", "c NO bigint", "id NO bigint", "s YES character varying(20)", "x YES bytea",
				"xs YES character varying(10)[]", "y YES bytea", "t_a NO"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			address := freshDatabase(c)
			execSQL(c, address, ruledRows...)
			client := startProvider(c)
			session := openSession(c, client, address)
			execution, err := client.Execute(c.Context(), &protocol.ExecuteRequest{SessionId: session.GetId(), RequestId: "e1", Units: sqlUnits(test.units...)})
			c.Assert(err, qt.IsNil)
			c.Assert(execution.GetOutcome(), qt.Equals, protocol.Execution_APPLIED)
			c.Assert(execution.GetAppliedCount(), qt.Equals, uint32(len(test.units))) // #nosec G115 -- a test request holds two units
			c.Assert(schemaOf(c, address), qt.DeepEquals, test.schema)
		})
	}
}
