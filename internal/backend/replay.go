package backend

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/runtime"

	"github.com/stokaro/ptah-spanner-plugin/internal/devclean"
)

// GuardReplay judges each statement of a migration replay offline; see
// devclean.Replay. Spanner has no server a run could own: a database is
// created and its options are set through the instance's administration
// API, which a cleanup does not reach, so every scope confines a statement
// to the dev database the session selected.
func (*Provider) GuardReplay(ctx context.Context, request *protocol.GuardReplayRequest) (*protocol.GuardedReplay, error) {
	if request.GetEngine() != engineID {
		return nil, runtime.ErrorWithReason(codes.InvalidArgument, "the provider judges the replays of the spanner engine only")
	}
	result := &protocol.GuardedReplay{Verdicts: make([]*protocol.ReplayVerdict, len(request.GetStatements()))}
	for i, statement := range request.GetStatements() {
		if err := ctx.Err(); err != nil {
			return nil, status.FromContextError(err).Err()
		}
		verdict := &protocol.ReplayVerdict{}
		if operation := devclean.Replay(statement); operation != "" {
			verdict.Refused, verdict.Operation = true, operation
		}
		result.Verdicts[i] = verdict
	}
	return result, nil
}
