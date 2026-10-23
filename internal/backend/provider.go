// Package backend declares the Spanner provider's description and holds the
// session service the runtime requires. The provider implements no other
// service yet.
package backend

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
)

// ProviderID is the identity of this provider and the namespace of its model
// kinds.
const ProviderID = "github.com/stokaro/ptah-spanner-plugin"

// engineID is the engine name the host selects this provider by.
const engineID = "spanner"

// Provider is the session service. It cannot open a session: the runtime
// requires the service to exist, and this provider has no connection code yet.
type Provider struct{}

// New returns the session service.
func New() *Provider { return &Provider{} }

// Description declares the engine and no feature. A host that negotiates with
// this description finds no service to call.
func Description() *protocol.Description {
	return &protocol.Description{
		Revision: provider.Revision, ProviderId: ProviderID,
		Version: "0.0.0-dev", SemanticIdentity: "spanner-scaffold-1",
		Features: make([]string, 0),
		Engines: []*protocol.Engine{{
			Id: engineID, Aliases: aliases(), Identifiers: identifierRules(), ReleaseLines: releaseLines(),
			Features: make([]string, 0),
		}},
	}
}

// Open refuses every request: the provider has no connection code yet.
func (*Provider) Open(_ context.Context, _ *protocol.OpenRequest) (*protocol.Session, error) {
	return nil, status.Error(codes.Unimplemented, "the Spanner provider is a scaffold and opens no sessions yet")
}

// Close reports the session as unknown, because Open never creates one.
func (*Provider) Close(_ context.Context, _ *protocol.CloseRequest) error {
	return status.Error(codes.NotFound, "unknown Spanner session")
}
