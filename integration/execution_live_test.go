//go:build integration

package integration_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	providerruntime "github.com/stokaro/ptah/provider-sdk/runtime"

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
// and the result names the unit, never its text.
func TestExecutionSubprocess_FailurePath(t *testing.T) {
	tests := []struct {
		name       string
		statements []string
		outcome    protocol.Execution_Outcome
		wantErr    string
		tables     []string
	}{
		{name: "the second unit is refused", statements: []string{`CREATE TABLE a (id bigint PRIMARY KEY)`, `CREATE TABLE b (n smallint PRIMARY KEY)`, `CREATE TABLE c (id bigint PRIMARY KEY)`},
			outcome: protocol.Execution_PARTIAL, wantErr: `provider execution: PARTIAL \(1 units confirmed applied\): Spanner refused unit 2 of 3`, tables: []string{"a"}},
		{name: "the first unit is refused", statements: []string{`DROP TABLE missing`},
			outcome: protocol.Execution_NOT_APPLIED, wantErr: `provider execution: NOT_APPLIED \(0 units confirmed applied\): Spanner refused unit 1 of 1`},
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
