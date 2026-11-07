package backend

import (
	"context"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/stokaro/ptah/provider-sdk/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/stokaro/ptah-spanner-plugin/internal/rowdeletion"
)

// ConvertValues turns a desired row deletion policy into the observed one a
// creation leaves, and an observed policy into the declaration that keeps it.
// The two carry the same column and interval: the observed interval keeps the
// server's stored spelling, which a declaration may use, and the comparison
// reads both spellings by the hours they denote.
func (*Provider) ConvertValues(ctx context.Context, request *protocol.ConvertValuesRequest) (*protocol.NormalizedValues, error) {
	if err := provider.ValidateConversionRequest(Description(), request); err != nil {
		return nil, runtime.ErrorWithReason(codes.InvalidArgument, err.Error())
	}
	result := &protocol.NormalizedValues{Complete: true}
	for _, value := range request.GetValues() {
		if err := ctx.Err(); err != nil {
			return nil, status.FromContextError(err).Err()
		}
		policy, err := rowdeletion.PolicyOf(value)
		if err != nil {
			return nil, runtime.ErrorWithReason(codes.InvalidArgument, err.Error())
		}
		converted, err := rowdeletion.Facet(value.GetId(), request.GetTo(), value.GetValue().GetOwner(), policy)
		if err != nil {
			return nil, runtime.ErrorWithReason(codes.InvalidArgument, err.Error())
		}
		converted.Value.Context = value.GetValue().GetContext()
		result.Values = append(result.Values, converted)
	}
	if err := provider.ValidateConvertedValuesReply(Description(), request, result); err != nil {
		return nil, runtime.ErrorWithReason(codes.InvalidArgument, err.Error())
	}
	return result, nil
}
