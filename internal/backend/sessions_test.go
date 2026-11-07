package backend_test

import (
	"context"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/stokaro/ptah-spanner-plugin/internal/backend"
	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// Open refuses what it cannot connect with before it reaches any server, and
// its reason never quotes the address.
func TestOpen_FailurePath(t *testing.T) {
	tests := []struct {
		name    string
		request *protocol.OpenRequest
		code    codes.Code
		wantErr string
	}{
		{name: "another engine", request: &protocol.OpenRequest{Engine: "postgres", Address: "spanner://host/app"},
			code: codes.InvalidArgument, wantErr: `.*the provider serves the spanner engine only.*`},
		// #nosec G101 -- a made-up credential the test checks the refusal never quotes
		{name: "another scheme", request: &protocol.OpenRequest{Engine: "spanner", Address: "postgres://alice:s3cret@host/app"},
			code: codes.InvalidArgument, wantErr: `.*a Spanner address is spanner://host:port/database, with an optional query string.*`},
		{name: "no database", request: &protocol.OpenRequest{Engine: "spanner", Address: "spanner://alice:s3cret@host"},
			code: codes.InvalidArgument, wantErr: `.*a Spanner address is spanner://host:port/database.*`},
		{name: "a parameter the driver cannot read", request: &protocol.OpenRequest{Engine: "spanner", Address: "spanner://alice:s3cret@host/app?sslmode=sometimes"},
			code: codes.InvalidArgument, wantErr: `.*the address carries a parameter the PostgreSQL driver cannot read.*`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			session, err := backend.New().Open(context.Background(), test.request)
			c.Assert(status.Code(err), qt.Equals, test.code)
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(err.Error(), qt.Not(qt.Contains), "s3cret")
			c.Assert(session, qt.IsNil)
		})
	}
}

// Closing a session the process never opened succeeds, as a repeated close
// does.
func TestClose_UnknownSession(t *testing.T) {
	c := testcheck.New(t)
	c.Assert(backend.New().Close(context.Background(), &protocol.CloseRequest{SessionId: "missing"}), qt.IsNil)
}

func TestUnknownSession_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	spanner := backend.New()
	snapshot, err := spanner.Inspect(context.Background(), &protocol.InspectRequest{SessionId: "missing"})
	c.Assert(status.Code(err), qt.Equals, codes.NotFound)
	c.Assert(snapshot, qt.IsNil)
	execution, err := spanner.Execute(context.Background(), &protocol.ExecuteRequest{SessionId: "missing"})
	c.Assert(status.Code(err), qt.Equals, codes.NotFound)
	c.Assert(execution, qt.IsNil)
}
