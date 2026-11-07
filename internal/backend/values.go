package backend

import (
	"context"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// NormalizeValues normalizes the provider's own models. The provider declares
// none yet: its common observations and predictions carry no facet. The SDK
// requires the service beside complete column observations, so it answers an
// empty batch and refuses any value, which no host sends to a provider that
// declares no model.
func (*Provider) NormalizeValues(ctx context.Context, request *protocol.NormalizeValuesRequest) (*protocol.NormalizedValues, error) {
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	if len(request.GetValues()) != 0 {
		return nil, runtime.ErrorWithReason(codes.InvalidArgument, "the provider declares no value model to normalize")
	}
	return &protocol.NormalizedValues{Complete: true}, nil
}
