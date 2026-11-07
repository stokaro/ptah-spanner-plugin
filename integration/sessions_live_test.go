//go:build integration

package integration_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

func TestSessionsSubprocess(t *testing.T) {
	c := testcheck.New(t)
	address := freshDatabase(c)
	client := startProvider(c)
	first, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "spanner", Address: address})
	c.Assert(err, qt.IsNil)
	// PGAdapter reports its PostgreSQL compatibility level, and the session
	// names the database the address names.
	c.Assert(first.GetServerVersion(), qt.Matches, `PostgreSQL [0-9.]+`)
	c.Assert(first.GetDatabase(), qt.Matches, `t[0-9a-f]{12}`)
	c.Assert(first.GetDefaultSchema(), qt.Equals, "public")
	c.Assert(first.GetCapabilities()["ddl_inside_transaction"], qt.IsFalse)
	second, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "spanner", Address: address})
	c.Assert(err, qt.IsNil)
	c.Assert(second.GetId(), qt.Not(qt.Equals), first.GetId())
	c.Assert(client.CloseSession(c.Context(), first.GetId()), qt.IsNil)
	c.Assert(client.CloseSession(c.Context(), first.GetId()), qt.IsNil)
	// A closed session reads nothing; the other session still does.
	_, err = client.Inspect(c.Context(), &protocol.InspectRequest{SessionId: first.GetId()})
	c.Assert(err, qt.ErrorMatches, `.*\(NotFound\).*`)
	snapshot, err := client.Inspect(c.Context(), &protocol.InspectRequest{SessionId: second.GetId()})
	c.Assert(err, qt.IsNil)
	c.Assert(snapshot.GetComplete(), qt.IsTrue)
	c.Assert(snapshot.GetTables(), qt.HasLen, 0)
	c.Assert(client.CloseSession(c.Context(), second.GetId()), qt.IsNil)
}

// A session states that Spanner runs no schema statement inside an explicit
// transaction, and the server refuses one there: the fact is a measurement
// rather than a value copied from Ptah's built-in Spanner engine. The host
// runs a plan's statements outside a transaction on that fact.
func TestSessionsSubprocess_TheServerRefusesDDLInsideATransaction(t *testing.T) {
	c := testcheck.New(t)
	address := freshDatabase(c)
	session := openSession(c, startProvider(c), address)
	for _, capability := range []string{"ddl_inside_transaction", "transactional_ddl"} {
		stated, established := session.GetCapabilities()[capability]
		c.Assert(established, qt.IsTrue, qt.Commentf("capability %s", capability))
		c.Assert(stated, qt.IsFalse, qt.Commentf("capability %s", capability))
	}
	conn := connect(c, address)
	transaction, err := conn.Begin(c.Context())
	c.Assert(err, qt.IsNil)
	_, err = transaction.Exec(c.Context(), `CREATE TABLE in_tx (id bigint PRIMARY KEY)`)
	c.Assert(err, qt.ErrorMatches, `.*DDL statements are only allowed outside explicit transactions.*`)
	c.Assert(transaction.Rollback(c.Context()), qt.IsNil)
	c.Assert(queryRows(c, address, `SELECT table_name FROM information_schema.tables WHERE table_name = 'in_tx'`), qt.HasLen, 0)
}

// A refused open never quotes the address, which can carry a credential.
func TestSessionsSubprocess_FailurePath(t *testing.T) {
	tests := []struct {
		name, address, wantErr string
	}{
		{name: "another scheme", address: "postgres://alice:s3cret@127.0.0.1:1/app",
			wantErr: `.*\(InvalidArgument\): a Spanner address is spanner://host:port/database, with an optional query string`},
		{name: "nothing listens", address: "spanner://alice:s3cret@127.0.0.1:1/app?sslmode=disable&connect_timeout=2",
			wantErr: `.*\(Unavailable\): the provider could not connect to the Spanner endpoint the address names`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			client := startProvider(c)
			session, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "spanner", Address: test.address})
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(err.Error(), qt.Not(qt.Contains), "s3cret")
			c.Assert(session, qt.IsNil)
		})
	}
}
