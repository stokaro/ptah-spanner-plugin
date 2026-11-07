package backend

import (
	"context"
	"errors"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/stokaro/ptah/provider-sdk/runtime"
	"google.golang.org/grpc/codes"
)

// transactionLimit bounds the handles one session keeps. A finished handle
// keeps its outcome until the session closes, so a long session would
// otherwise grow without end.
const transactionLimit = 1024

// transaction is one handle of a session. The handle's transaction runs on a
// connection of its own, opened at begin and closed at finish, so the
// session's connection stays outside every transaction: it reads the catalog
// and runs schema statements, which Spanner refuses inside a transaction,
// while the handle is open.
type transaction struct {
	conn *pgx.Conn
	tx   pgx.Tx
	// outcome is the handle's terminal outcome once it finished, or the
	// outcome a finish reports when the handle never opened a transaction.
	outcome protocol.TransactionResult_Outcome
	// failed is set when a statement in the transaction failed. Spanner
	// aborts the transaction then, as PostgreSQL does, so it can only be
	// rolled back.
	failed bool
	// reason says why a commit ended in another outcome.
	reason string
	// aborted is set when Spanner aborted the transaction for a conflict
	// and applied nothing, so a new attempt may succeed.
	aborted bool
}

// BeginTransaction opens the transaction a caller-chosen handle names, on a
// connection of its own.
//
// Spanner's transactions change data only: a schema statement in one is
// refused (`DDL statements are only allowed outside explicit transactions`),
// and the session states transactional_ddl false, so the host uses them for
// its revision records and never wraps a migration's schema statements. A
// session has at most one open transaction.
func (p *Provider) BeginTransaction(ctx context.Context, request *protocol.BeginTransactionRequest) (*protocol.Transaction, error) {
	current, err := p.lookup(request.GetSessionId())
	if err != nil {
		return nil, err
	}
	current.mu.Lock()
	defer current.mu.Unlock()
	record, err := current.reserveTransaction(request.GetTransactionId())
	if err != nil {
		return nil, err
	}
	if current.active != "" {
		return nil, runtime.ErrorWithReason(codes.FailedPrecondition, "the session has an open transaction; finish it before beginning another")
	}
	if !current.guard(ctx) {
		return nil, runtime.ErrorWithReason(codes.FailedPrecondition, "the session lost a lock it held and begins no transaction")
	}
	conn, err := pgx.ConnectConfig(ctx, current.config.Copy())
	if err != nil {
		return nil, callFailed(ctx, "the provider could not open a connection for the transaction")
	}
	// Spanner's default isolation is serializable, which DEFAULT selects
	// without promising it; SERIALIZABLE asks for it by name.
	options := pgx.TxOptions{}
	if request.GetIsolation() == protocol.TransactionIsolation_SERIALIZABLE {
		options.IsoLevel = pgx.Serializable
	}
	tx, err := conn.BeginTx(ctx, options)
	if err != nil {
		closeQuietly(ctx, conn)
		return nil, callFailed(ctx, "the provider could not begin the transaction")
	}
	record.conn, record.tx = conn, tx
	current.active = request.GetTransactionId()
	return &protocol.Transaction{SessionId: request.GetSessionId(), Id: request.GetTransactionId(), Isolation: request.GetIsolation()}, nil
}

// reserveTransaction records a new handle that has not opened a transaction.
// A handle is used once: a second begin with the same ID is refused, even
// after the first finished. The caller holds mu.
func (s *session) reserveTransaction(id string) (*transaction, error) {
	if s.transactions[id] != nil {
		return nil, runtime.ErrorWithReason(codes.AlreadyExists, "the transaction ID was used before")
	}
	if len(s.transactions) >= transactionLimit {
		return nil, runtime.ErrorWithReason(codes.ResourceExhausted, "the session used every transaction handle it keeps; open another session")
	}
	if s.transactions == nil {
		s.transactions = make(map[string]*transaction)
	}
	record := &transaction{outcome: protocol.TransactionResult_NOT_STARTED}
	s.transactions[id] = record
	return record, nil
}

// FinishTransaction commits or rolls back a handle's transaction and reports
// the outcome; a finished handle reports its outcome again without a second
// commit or rollback. Finishing a handle that never began fences it, so a
// late begin with the same ID opens nothing.
func (p *Provider) FinishTransaction(ctx context.Context, request *protocol.FinishTransactionRequest) (*protocol.TransactionResult, error) {
	current, err := p.lookup(request.GetSessionId())
	if err != nil {
		return nil, err
	}
	current.mu.Lock()
	defer current.mu.Unlock()
	record := current.transactions[request.GetTransactionId()]
	if record == nil {
		record, err = current.reserveTransaction(request.GetTransactionId())
		if err != nil {
			return nil, err
		}
	}
	if record.tx != nil {
		current.finish(ctx, request.GetTransactionId(), record, request.GetAction())
	}
	result := &protocol.TransactionResult{Outcome: record.outcome}
	// The host may begin the work again after an abort, and reads the flag
	// only where it splits migrations into units.
	if record.aborted && request.GetAction() == protocol.FinishTransactionRequest_COMMIT && hostSplitsMigrations(ctx) {
		result.Aborted = true
	}
	if request.GetAction() == protocol.FinishTransactionRequest_COMMIT && record.outcome != protocol.TransactionResult_COMMITTED ||
		request.GetAction() == protocol.FinishTransactionRequest_ROLLBACK && record.outcome == protocol.TransactionResult_UNKNOWN {
		reason := record.reason
		if reason == "" {
			reason = "the transaction never began"
		}
		result.Message = reason
		if hostTakesReasons(ctx) {
			result.Diagnostic = &protocol.ProviderDiagnostic{Reason: reason}
		}
	}
	return result, nil
}

// finish ends a handle's open transaction with action and records the
// outcome. A transaction a statement failed in is rolled back whatever the
// action, and so is one whose session lost a lock it held. The caller holds
// mu.
func (s *session) finish(ctx context.Context, id string, record *transaction, action protocol.FinishTransactionRequest_Action) {
	// The finish outlives the caller's cancellation: a transaction left open
	// holds its locks in Spanner until it times out.
	ctx = context.WithoutCancel(ctx)
	switch {
	case action == protocol.FinishTransactionRequest_ROLLBACK:
		record.outcome = protocol.TransactionResult_ROLLED_BACK
	case record.failed:
		record.outcome, record.reason = protocol.TransactionResult_ROLLED_BACK, "a statement in the transaction failed, so it was rolled back"
	case !s.guard(ctx):
		record.outcome, record.reason = protocol.TransactionResult_ROLLED_BACK, "the session lost a lock it held, so the transaction was rolled back"
	default:
		err := record.tx.Commit(ctx)
		switch refused, found := errors.AsType[*pgconn.PgError](err); {
		case err == nil:
			record.outcome = protocol.TransactionResult_COMMITTED
		case found && refused.Code == "40001":
			// Spanner aborted the transaction for a conflict, and PGAdapter
			// could not run it again: nothing of it committed.
			record.outcome, record.reason, record.aborted = protocol.TransactionResult_ROLLED_BACK, "Spanner aborted the transaction for a conflict and applied nothing", true
		default:
			record.outcome, record.reason = protocol.TransactionResult_UNKNOWN, "Spanner did not confirm the commit, and it was not sent again"
		}
	}
	if record.outcome != protocol.TransactionResult_COMMITTED && record.outcome != protocol.TransactionResult_UNKNOWN {
		// A rollback that fails still leaves nothing committed: no commit
		// was sent, and closing the connection ends the transaction.
		_ = record.tx.Rollback(ctx)
	}
	closeQuietly(ctx, record.conn)
	record.conn, record.tx = nil, nil
	s.active = ""
	s.closeReaders(id)
	s.closeResults(id)
}

// rollbackTransactions rolls back every open transaction of a closing
// session. The caller holds mu.
func (s *session) rollbackTransactions(ctx context.Context) {
	for id, record := range s.transactions {
		if record.tx != nil {
			s.finish(ctx, id, record, protocol.FinishTransactionRequest_ROLLBACK)
		}
	}
}

// openTransaction returns the open transaction id names, or an error that
// says why the handle cannot run a statement. The caller holds mu.
func (s *session) openTransaction(id string) (*transaction, error) {
	record := s.transactions[id]
	switch {
	case record == nil || record.tx == nil:
		return nil, runtime.ErrorWithReason(codes.FailedPrecondition, "the transaction is not open")
	case record.failed:
		return nil, runtime.ErrorWithReason(codes.FailedPrecondition, "a statement in the transaction failed; roll it back")
	}
	return record, nil
}

// hostSplitsMigrations reports whether the host negotiated migration splits,
// which carry the flags that say Spanner aborted a transaction.
func hostSplitsMigrations(ctx context.Context) bool {
	return slices.Contains(provider.HostFeatures(ctx), provider.MigrationSplitFeature)
}

// hostTakesReasons reports whether the host negotiated reasons it shows.
func hostTakesReasons(ctx context.Context) bool {
	return slices.Contains(provider.HostFeatures(ctx), provider.ErrorReasonsFeature)
}
