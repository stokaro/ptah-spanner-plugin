package backend

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/runtime"

	"github.com/stokaro/ptah-spanner-plugin/internal/tableplan"
)

// PlanFeatures plans the tables the host creates, changes and drops; see
// tableplan. It reads no database. A change Spanner cannot make is a
// completed refusal that names the table, and a request the service cannot
// read is an error.
func (*Provider) PlanFeatures(ctx context.Context, request *protocol.PlanFeaturesRequest) (*protocol.PlannedFeatures, error) {
	result, err := tableplan.Plan(ctx, request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, status.FromContextError(ctx.Err()).Err()
		}
		return nil, runtime.ErrorWithReason(codes.InvalidArgument, err.Error())
	}
	return result, nil
}
