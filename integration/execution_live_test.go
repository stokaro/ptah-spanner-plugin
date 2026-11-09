//go:build integration

package integration_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	providerruntime "ptah.run/provider-sdk/runtime"

	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

func sqlUnits(statements ...string) []*protocol.Unit {
	units := make([]*protocol.Unit, len(statements))
	for i, statement := range statements {
		units[i] = &protocol.Unit{ChangeId: string(rune('a' + i)), Body: &protocol.Unit_Sql{Sql: statement}}
	}
	return units
}

// Each unit commits as it runs, and the result says how many applied.
func TestExecutionSubprocess(t *testing.T) {
	c := testcheck.New(t)
	address := freshDatabase(c)
	client := startProvider(c)
	session := openSession(c, client, address)
	execution, err := client.Execute(c.Context(), &protocol.ExecuteRequest{SessionId: session.GetId(), RequestId: "e1",
		Units: sqlUnits(`CREATE TABLE a (id bigint PRIMARY KEY)`, `ALTER TABLE a ADD COLUMN n bigint`)})
	c.Assert(err, qt.IsNil)
	c.Assert(execution.GetOutcome(), qt.Equals, protocol.Execution_APPLIED)
	c.Assert(execution.GetAppliedCount(), qt.Equals, uint32(2))
	c.Assert(tableNames(c, client, session.GetId()), qt.DeepEquals, []string{"a"})
}

// A unit Spanner refuses applies nothing, the units before it stay applied,
// and the result names the unit and the server's SQLSTATE, never its text.
func TestExecutionSubprocess_FailurePath(t *testing.T) {
	tests := []struct {
		name       string
		statements []string
		outcome    protocol.Execution_Outcome
		wantErr    string
		tables     []string
	}{
		{name: "the second unit is refused", statements: []string{`CREATE TABLE a (id bigint PRIMARY KEY)`, `CREATE TABLE b (n smallint PRIMARY KEY)`, `CREATE TABLE c (id bigint PRIMARY KEY)`},
			outcome: protocol.Execution_PARTIAL, wantErr: `provider execution: PARTIAL \(1 units confirmed applied\): Spanner refused unit 2 of 3 with SQLSTATE P0001`, tables: []string{"a"}},
		{name: "the first unit is refused", statements: []string{`DROP TABLE missing`},
			outcome: protocol.Execution_NOT_APPLIED, wantErr: `provider execution: NOT_APPLIED \(0 units confirmed applied\): Spanner refused unit 1 of 1 with SQLSTATE P0001`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			address := freshDatabase(c)
			client := startProvider(c)
			session := openSession(c, client, address)
			execution, err := client.Execute(c.Context(), &protocol.ExecuteRequest{SessionId: session.GetId(), RequestId: "e1", Units: sqlUnits(test.statements...)})
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(execution.GetOutcome(), qt.Equals, test.outcome)
			c.Assert(tableNames(c, client, session.GetId()), qt.DeepEquals, test.tables)
		})
	}
}

// tableNames reads the names of the tables a session's database holds.
func tableNames(c *qt.C, client *providerruntime.Client, session string) []string {
	c.Helper()
	snapshot, err := client.Inspect(c.Context(), &protocol.InspectRequest{SessionId: session})
	c.Assert(err, qt.IsNil)
	var names []string
	for _, table := range snapshot.GetTables() {
		names = append(names, table.GetIdentity().GetName())
	}
	return names
}

// Spanner runs a schema change outside transactions, so the provider offers
// none and refuses a request that names one before running anything.
func TestExecutionSubprocess_RefusesATransaction(t *testing.T) {
	c := testcheck.New(t)
	address := freshDatabase(c)
	client := startProvider(c)
	session := openSession(c, client, address)
	execution, err := client.Execute(c.Context(), &protocol.ExecuteRequest{SessionId: session.GetId(), RequestId: "e1", TransactionId: "tx",
		Units: sqlUnits(`CREATE TABLE a (id bigint PRIMARY KEY)`)})
	c.Assert(err, qt.IsNotNil)
	c.Assert(execution, qt.IsNil)
}

// trackedTable is the table the change stream tests start from: three columns
// and one row.
var trackedTable = []string{`CREATE TABLE t (id bigint PRIMARY KEY, a bigint, b bigint)`, `INSERT INTO t (id, a, b) VALUES (1, 10, 20)`}

// columnsOf lists the columns of the default schema's tables t and other.
func columnsOf(c *qt.C, address string) []string {
	c.Helper()
	return queryRows(c, address, `SELECT table_name, column_name FROM information_schema.columns
WHERE table_schema = 'public' AND table_name IN ('t', 'other') ORDER BY table_name, column_name`)
}

// A request whose units drop a table or a column a change stream names is
// refused before its first unit runs: Spanner would refuse the drop only when
// it ran, after the units before it. The reason names the unit, the table or
// column and the stream, and what to do. Nothing of the request applies, and
// the table keeps its columns and its row.
func TestExecutionSubprocess_RefusesDropsAChangeStreamNames(t *testing.T) {
	tests := []struct {
		name    string
		stream  string
		units   []string
		wantErr string
	}{
		{name: "a table the stream names", stream: `CREATE CHANGE STREAM cs1 FOR t`, units: []string{`CREATE TABLE other (id bigint PRIMARY KEY)`, `DROP TABLE "t"`},
			wantErr: `provider execution: NOT_APPLIED \(0 units confirmed applied\): unit 2 of 2 drops table "t", which change stream "cs1" names, so no unit ran: ` +
				`Spanner refuses to drop a table a change stream names; drop the change stream, or alter it to stop tracking the table, before dropping the table`},
		{name: "a table the stream names with a column list", stream: `CREATE CHANGE STREAM cs3 FOR t(a)`, units: []string{`DROP TABLE t`},
			wantErr: `provider execution: NOT_APPLIED \(0 units confirmed applied\): unit 1 of 1 drops table "t", which change stream "cs3" names, so no unit ran: .*`},
		{name: "a column the stream lists", stream: `CREATE CHANGE STREAM cs3 FOR t(a)`, units: []string{`CREATE TABLE other (id bigint PRIMARY KEY)`, `ALTER TABLE "t" DROP COLUMN "a"`},
			wantErr: `provider execution: NOT_APPLIED \(0 units confirmed applied\): unit 2 of 2 drops column "a" of table "t", which change stream "cs3" names, so no unit ran: ` +
				`Spanner refuses to drop a column a change stream names; alter the change stream to stop tracking the column, or drop it, before dropping the column`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			address := freshDatabase(c)
			execSQL(c, address, append(trackedTable, test.stream)...)
			client := startProvider(c)
			session := openSession(c, client, address)
			execution, err := client.Execute(c.Context(), &protocol.ExecuteRequest{SessionId: session.GetId(), RequestId: "e1", Units: sqlUnits(test.units...)})
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(execution.GetOutcome(), qt.Equals, protocol.Execution_NOT_APPLIED)
			c.Assert(execution.GetAppliedCount(), qt.Equals, uint32(0))
			c.Assert(columnsOf(c, address), qt.DeepEquals, []string{"t a", "t b", "t id"})
			c.Assert(queryRows(c, address, `SELECT a, b FROM t`), qt.DeepEquals, []string{"10 20"})
		})
	}
}

// Spanner drops what no change stream names explicitly: a table and a column
// under a stream FOR ALL, and a column of a table a stream tracks whole. A
// request that drops or alters the stream before the drop leaves the drop to
// Spanner, which takes it: the catalog read before the request no longer says
// what the stream names when the drop runs.
func TestExecutionSubprocess_RunsDropsNoChangeStreamNames(t *testing.T) {
	tests := []struct {
		name    string
		stream  string
		units   []string
		columns []string
	}{
		{name: "a stream FOR ALL", stream: `CREATE CHANGE STREAM cs2 FOR ALL`, units: []string{`ALTER TABLE t DROP COLUMN a`, `DROP TABLE t`}},
		{name: "a column of a table the stream tracks whole", stream: `CREATE CHANGE STREAM cs1 FOR t`, units: []string{`ALTER TABLE "t" DROP COLUMN "a"`},
			columns: []string{"t b", "t id"}},
		{name: "the stream is dropped first", stream: `CREATE CHANGE STREAM cs1 FOR t`, units: []string{`DROP CHANGE STREAM cs1`, `DROP TABLE t`}},
		{name: "the stream is altered first", stream: `CREATE CHANGE STREAM cs3 FOR t(a)`, units: []string{`ALTER CHANGE STREAM cs3 SET FOR ALL`, `ALTER TABLE t DROP COLUMN a`},
			columns: []string{"t b", "t id"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			address := freshDatabase(c)
			execSQL(c, address, append(trackedTable, test.stream)...)
			client := startProvider(c)
			session := openSession(c, client, address)
			execution, err := client.Execute(c.Context(), &protocol.ExecuteRequest{SessionId: session.GetId(), RequestId: "e1", Units: sqlUnits(test.units...)})
			c.Assert(err, qt.IsNil)
			c.Assert(execution.GetOutcome(), qt.Equals, protocol.Execution_APPLIED)
			c.Assert(execution.GetAppliedCount(), qt.Equals, uint32(len(test.units))) // #nosec G115 -- a test request holds two units
			c.Assert(columnsOf(c, address), qt.DeepEquals, test.columns)
		})
	}
}
