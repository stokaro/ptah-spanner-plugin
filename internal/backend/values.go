package backend

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/runtime"

	"github.com/stokaro/ptah-spanner-plugin/internal/rowdeletion"
	"github.com/stokaro/ptah-spanner-plugin/internal/tableop"
)

// NormalizeValues normalizes the provider's own models: the table operations
// it plans, and the row deletion policy its table lifecycle service owns. A
// value of any other model refuses the batch.
func (*Provider) NormalizeValues(ctx context.Context, request *protocol.NormalizeValuesRequest) (*protocol.NormalizedValues, error) {
	result := &protocol.NormalizedValues{Complete: true}
	for _, input := range request.GetValues() {
		if err := ctx.Err(); err != nil {
			return nil, status.FromContextError(err).Err()
		}
		var normalized *protocol.NormalizedValue
		var err error
		switch input.GetValue().GetKind() {
		case tableop.Kind:
			normalized, err = tableop.Normalize(input)
		case rowdeletion.Kind:
			normalized, err = rowdeletion.Normalize(input)
		default:
			return nil, runtime.ErrorWithReason(codes.InvalidArgument, "the provider normalizes its table operations and row deletion policies only")
		}
		if err != nil {
			return nil, runtime.ErrorWithReason(codes.InvalidArgument, err.Error())
		}
		result.Values = append(result.Values, normalized)
	}
	return result, nil
}
