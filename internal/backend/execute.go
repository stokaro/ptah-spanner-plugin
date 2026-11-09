package backend

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/grpc/codes"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/runtime"

	"github.com/stokaro/ptah-spanner-plugin/internal/catalog"
	"github.com/stokaro/ptah-spanner-plugin/internal/changestream"
	"github.com/stokaro/ptah-spanner-plugin/internal/constraintrows"
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
//
// Outside a transaction a request is refused before its first unit runs when
// Spanner would refuse one of its units for what the database holds, but only
// when that unit runs, after the units before it have committed: a unit that
// drops a table or a column a change stream names (see changestream), and a
// unit after the first that adds a rule rows of the table break: a CHECK, a
// foreign key, a unique index, NOT NULL or a shorter character limit (see
// constraintrows). The reason names the unit, the object and why.
// Spanner runs no schema statement in a transaction, so neither runs in one.
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
	if request.GetTransactionId() == "" {
		if refused := trackedDrop(ctx, current.conn, statements); refused != nil {
			return refused, nil
		}
		if refused := brokenConstraint(ctx, current.conn, statements); refused != nil {
			return refused, nil
		}
	}
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

// trackedDrop refuses statements that drop a table or a column a change stream
// names, before any of them runs: it reports NOT_APPLIED with a reason that
// names the first such drop and its streams. It returns nil when no statement
// drops one, and reads the catalog only when a statement drops something. A
// catalog it cannot read refuses the statements too: the read is what tells
// a drop Spanner takes from one it refuses after the statements before it.
func trackedDrop(ctx context.Context, q catalog.Queryer, statements []string) *protocol.Execution {
	drops := changestream.Drops(statements)
	if len(drops) == 0 {
		return nil
	}
	tracking, err := changestream.Read(ctx, q)
	if err != nil {
		if ctx.Err() != nil {
			return stoppedAt(0, fmt.Sprintf("the call ended before unit 1 of %d ran", len(statements)))
		}
		return stoppedAt(0, fmt.Sprintf("the provider could not read which tables and columns change streams name, so it ran none of the %d units, "+
			"which drop a table or a column", len(statements)))
	}
	if reason := changestream.Refusal(drops, tracking, len(statements)); reason != "" {
		return stoppedAt(0, reason)
	}
	return nil
}

// brokenConstraint refuses statements that add, after their first statement,
// a rule that rows of the table already break, before any of them runs: it
// reports NOT_APPLIED with a reason that names the first such rule, its table
// and how many rows break it. It counts the rows of each addition in statement
// order and returns nil when none breaks. A count that fails refuses the
// statements too: Spanner checks the same rows when the unit runs, after the
// units before it.
func brokenConstraint(ctx context.Context, q catalog.Queryer, statements []string) *protocol.Execution {
	for _, addition := range constraintrows.Additions(statements) {
		rows, err := addition.Count(ctx, q)
		switch {
		case err != nil && ctx.Err() != nil:
			return stoppedAt(0, fmt.Sprintf("the call ended before unit 1 of %d ran", len(statements)))
		case err != nil:
			return stoppedAt(0, constraintrows.Uncounted(addition, err, len(statements)))
		case rows > 0:
			return stoppedAt(0, constraintrows.Refusal(addition, rows, len(statements)))
		}
	}
	return nil
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
