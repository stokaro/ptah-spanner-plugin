package backend

import (
	"context"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/stokaro/ptah/provider-sdk/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/stokaro/ptah-spanner-plugin/internal/commonobject"
)

// PrepareCommonObjects returns the declared views and sequences in Spanner's
// spelling, offline: each view's query as Spanner writes it back and each
// sequence's type as a read reports it, so the host compares a declaration
// with a read and a view applied once is not planned again; see
// commonobject.Prepare.
func (*Provider) PrepareCommonObjects(ctx context.Context, request *protocol.PrepareCommonObjectsRequest) (*protocol.PreparedCommonObjects, error) {
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	if err := provider.ValidatePrepareCommonObjectsRequest(Description(), request); err != nil {
		return nil, runtime.ErrorWithReason(codes.InvalidArgument, err.Error())
	}
	result := &protocol.PreparedCommonObjects{Complete: true, Prepared: commonobject.Prepare(request.GetDeclared())}
	if err := provider.ValidatePreparedCommonObjectsReply(Description(), request, result); err != nil {
		return nil, runtime.ErrorWithReason(codes.Internal, err.Error())
	}
	return result, nil
}
