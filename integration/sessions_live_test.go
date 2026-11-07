//go:build integration

package integration_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-spanner-plugin/internal/backend"
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

// A session states the facts the provider plans with, measured on the server,
// and the offline facts an engine description states for a command without a
// server agree with each of them, so a command plans with one answer whether
// it connects or not.
func TestSessionsSubprocess_AgreeWithTheOfflineCapabilities(t *testing.T) {
	c := testcheck.New(t)
	session := openSession(c, startProvider(c), freshDatabase(c))
	offline := backend.Description().GetEngines()[0].GetDefaultCapabilities()
	c.Assert(session.GetCapabilities(), qt.Not(qt.HasLen), 0)
	for name, value := range session.GetCapabilities() {
		stated, established := offline[name]
		c.Assert(established, qt.IsTrue, qt.Commentf("capability %s", name))
		c.Assert(stated, qt.Equals, value, qt.Commentf("capability %s", name))
	}
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
