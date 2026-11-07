package backend

import (
	"context"
	"fmt"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/runtime"
	"google.golang.org/grpc/codes"
)

// Execute runs the units of one request in order and reports how many of them
// Spanner applied. It never runs a request again because of its ID: when the
// reply is lost, the host reads the database to learn what happened.
//
// Spanner refuses a schema statement inside an explicit transaction, so each
// unit runs on its own and commits as it runs. A statement the server refuses
// applied nothing, and the units before it stay applied. A canceled call may
// have stopped while Spanner was applying a statement, so its outcome is
// UNKNOWN: PGAdapter waits for a schema change to finish, and a client that
// stops waiting does not stop the change.
func (p *Provider) Execute(ctx context.Context, request *protocol.ExecuteRequest) (*protocol.Execution, error) {
	current, err := p.lookup(request.GetSessionId())
	if err != nil {
		return nil, err
	}
	if request.GetTransactionId() != "" {
		return nil, runtime.ErrorWithReason(codes.InvalidArgument, "Spanner runs schema statements outside transactions, and the provider offers none")
	}
	units := request.GetUnits()
	statements := make([]string, len(units))
	for i, unit := range units {
		statement, ok := unit.GetBody().(*protocol.Unit_Sql)
		if !ok {
			return nil, runtime.ErrorWithReason(codes.InvalidArgument, "the provider runs SQL units only")
		}
		statements[i] = statement.Sql
	}
	current.mu.Lock()
	defer current.mu.Unlock()
	var applied uint32
	for _, statement := range statements {
		if err := ctx.Err(); err != nil {
			return stoppedAt(applied, fmt.Sprintf("the call ended before unit %d of %d ran", applied+1, len(units))), nil
		}
		if !current.guard(ctx) {
			return stoppedAt(applied, fmt.Sprintf("the session lost its lock before unit %d of %d", applied+1, len(units))), nil
		}
		if _, err := current.conn.Exec(ctx, statement); err != nil {
			return failedAt(ctx, applied, len(units)), nil
		}
		applied++
	}
	return &protocol.Execution{Outcome: protocol.Execution_APPLIED, AppliedCount: applied}, nil
}

// failedAt reports a statement that failed after applied units of total. A
// canceled call may have stopped while the statement was applying, so nobody
// knows whether it applied.
func failedAt(ctx context.Context, applied uint32, total int) *protocol.Execution {
	if ctx.Err() != nil {
		return &protocol.Execution{
			Outcome:      protocol.Execution_UNKNOWN,
			AppliedCount: applied,
			Diagnostic:   &protocol.ProviderDiagnostic{Reason: fmt.Sprintf("the call ended while unit %d of %d was running", applied+1, total)},
		}
	}
	return stoppedAt(applied, fmt.Sprintf("Spanner refused unit %d of %d", applied+1, total))
}

// stoppedAt reports a batch that ran applied units and then stopped before
// the next one took effect: NOT_APPLIED when it applied none, PARTIAL when it
// applied some. The reason names the unit, never its text.
func stoppedAt(applied uint32, reason string) *protocol.Execution {
	outcome := protocol.Execution_PARTIAL
	if applied == 0 {
		outcome = protocol.Execution_NOT_APPLIED
	}
	return &protocol.Execution{Outcome: outcome, AppliedCount: applied, Diagnostic: &protocol.ProviderDiagnostic{Reason: reason}}
}
