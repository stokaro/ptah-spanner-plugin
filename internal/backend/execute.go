package backend

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/runtime"
	"google.golang.org/grpc/codes"
)

// Execute runs the units of one request in order and reports how many of them
// Spanner applied. It never runs a request again because of its ID: when the
// reply is lost, the host reads the database to learn what happened.
//
// Without a transaction each unit runs on its own and commits as it runs,
// which is how Spanner runs a schema statement: it refuses one inside an
// explicit transaction. A statement the server refuses applied nothing, and
// the units before it stay applied. A canceled call may have stopped while
// Spanner was applying a statement, so its outcome is UNKNOWN: PGAdapter waits
// for a schema change to finish, and a client that stops waiting does not stop
// the change.
//
// In a transaction the units run in it and commit with it. A unit that fails
// there aborts the transaction, which then commits nothing.
func (p *Provider) Execute(ctx context.Context, request *protocol.ExecuteRequest) (*protocol.Execution, error) {
	current, err := p.lookup(request.GetSessionId())
	if err != nil {
		return nil, err
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
	var run interface {
		Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	} = current.conn
	var record *transaction
	if id := request.GetTransactionId(); id != "" {
		if record, err = current.openTransaction(id); err != nil {
			return nil, err
		}
		run = record.tx
	}
	var applied uint32
	for _, statement := range statements {
		if err := ctx.Err(); err != nil {
			return stoppedAt(applied, fmt.Sprintf("the call ended before unit %d of %d ran", applied+1, len(units))), nil
		}
		if !current.guard(ctx) {
			return stoppedAt(applied, fmt.Sprintf("the session lost its lock before unit %d of %d", applied+1, len(units))), nil
		}
		if _, err := run.Exec(ctx, statement); err != nil {
			if record == nil {
				return failedAt(ctx, applied, len(units), err), nil
			}
			// A failed statement aborts the transaction, so none of its
			// units will commit.
			record.failed = true
			if refused, found := errors.AsType[*pgconn.PgError](err); found && refused.Code == "40001" && hostSplitsMigrations(ctx) {
				record.aborted = true
				return &protocol.Execution{Outcome: protocol.Execution_NOT_APPLIED, TransactionAborted: true,
					Diagnostic: &protocol.ProviderDiagnostic{Reason: "Spanner aborted the transaction for a conflict and applied nothing"}}, nil
			}
			return failedAt(ctx, applied, len(units), err), nil
		}
		applied++
	}
	return &protocol.Execution{Outcome: protocol.Execution_APPLIED, AppliedCount: applied}, nil
}

// failedAt reports a statement that failed with err after applied units of
// total. A canceled call may have stopped while the statement was applying,
// so nobody knows whether it applied. A refusal names the server's SQLSTATE,
// which says why without quoting the statement or a value, as the server's
// message can.
func failedAt(ctx context.Context, applied uint32, total int, err error) *protocol.Execution {
	if ctx.Err() != nil {
		return &protocol.Execution{
			Outcome:      protocol.Execution_UNKNOWN,
			AppliedCount: applied,
			Diagnostic:   &protocol.ProviderDiagnostic{Reason: fmt.Sprintf("the call ended while unit %d of %d was running", applied+1, total)},
		}
	}
	if refused, found := errors.AsType[*pgconn.PgError](err); found {
		return stoppedAt(applied, fmt.Sprintf("Spanner refused unit %d of %d with SQLSTATE %s", applied+1, total, refused.Code))
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
