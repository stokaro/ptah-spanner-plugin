package backend

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-spanner-plugin/internal/history"
	"github.com/stokaro/ptah-spanner-plugin/internal/ownedtable"
)

// maxSnapshotBytes bounds a history snapshot below the transport's message
// limit, with room for the reply's framing.
const maxSnapshotBytes = 3 << 20

// Diagnostic codes of a history result that did not apply.
const (
	historyLockLost      = "spanner.lock-lost"
	historyLayout        = "spanner.history-layout"
	historyMissing       = "spanner.history-missing"
	historyRecord        = "spanner.history-record"
	historyUnavailable   = "spanner.history-unavailable"
	historyNotConfirmed  = "spanner.history-unconfirmed"
	historyWriteRefused  = "spanner.history-write-refused"
	historyNoTransaction = "spanner.history-transaction"
)

// PrepareHistory creates a missing revision table in the layout of the
// request's format, and checks an existing one holds exactly that layout. It
// never changes an existing table or its rows. Spanner runs the statements
// that create the table outside a transaction, each committing as it runs, so
// a failure after the first one was sent is UNKNOWN.
func (p *Provider) PrepareHistory(ctx context.Context, request *protocol.PrepareHistoryRequest) (*protocol.HistoryResult, error) {
	current, err := p.lookup(request.GetScope().GetSessionId())
	if err != nil {
		return nil, err
	}
	current.mu.Lock()
	defer current.mu.Unlock()
	store := history.StoreOf(request.GetScope())
	answer := func(outcome protocol.HistoryResult_Outcome, code, message string) *protocol.HistoryResult {
		return &protocol.HistoryResult{Scope: proto.CloneOf(request.GetScope()), RequestId: request.GetRequestId(), Outcome: outcome,
			DiagnosticCode: code, Message: message}
	}
	if !current.guard(ctx) {
		return answer(protocol.HistoryResult_REFUSED, historyLockLost, "the session lost a lock it held"), nil
	}
	exists, err := ownedtable.Locate(ctx, current.conn, store.Definition())
	switch {
	case errors.Is(err, ownedtable.ErrLayout):
		return answer(protocol.HistoryResult_REFUSED, historyLayout, "a table of the revision table's name exists and "+layoutReason(err)), nil
	case err != nil:
		return answer(protocol.HistoryResult_REFUSED, historyUnavailable, "the provider could not read the catalog"), nil
	case exists:
		return answer(protocol.HistoryResult_APPLIED, "", ""), nil
	}
	for i, statement := range store.Statements() {
		if !current.guard(ctx) {
			if i == 0 {
				return answer(protocol.HistoryResult_REFUSED, historyLockLost, "the session lost a lock it held"), nil
			}
			return answer(protocol.HistoryResult_UNKNOWN, historyLockLost, "the session lost a lock it held while it created the revision table"), nil
		}
		if _, err := current.conn.Exec(ctx, statement); err != nil {
			return answer(protocol.HistoryResult_UNKNOWN, historyNotConfirmed, "Spanner did not confirm the statement that creates the revision table"), nil
		}
	}
	if exists, err := ownedtable.Locate(ctx, current.conn, store.Definition()); err != nil || !exists {
		return answer(protocol.HistoryResult_UNKNOWN, historyNotConfirmed, "the revision table the provider created could not be read back"), nil
	}
	return answer(protocol.HistoryResult_APPLIED, "", ""), nil
}

// ReadHistory reads every record of a revision table, in the request's
// transaction when it names one. A missing table is a complete snapshot that
// says so; a table that holds another layout, a row no record stands for or
// more records than the request takes fails the read.
func (p *Provider) ReadHistory(ctx context.Context, request *protocol.ReadHistoryRequest) (*protocol.HistorySnapshot, error) {
	current, err := p.lookup(request.GetScope().GetSessionId())
	if err != nil {
		return nil, err
	}
	current.mu.Lock()
	defer current.mu.Unlock()
	var q history.Querier = current.conn
	var record *transaction
	if id := request.GetScope().GetTransactionId(); id != "" {
		if record, err = current.openTransaction(id); err != nil {
			return nil, err
		}
		q = record.tx
	}
	store := history.StoreOf(request.GetScope())
	// The catalog is read on the session's connection, which is outside every
	// transaction: the table's layout cannot change inside one, because
	// Spanner changes a schema outside transactions only.
	exists, err := ownedtable.Locate(ctx, current.conn, store.Definition())
	switch {
	case errors.Is(err, ownedtable.ErrLayout):
		return nil, runtime.ErrorWithReason(codes.FailedPrecondition, "a table of the revision table's name exists and "+layoutReason(err))
	case err != nil:
		return nil, callFailed(ctx, "the provider could not read the catalog")
	}
	snapshot := &protocol.HistorySnapshot{Scope: proto.CloneOf(request.GetScope()), Exists: exists, Complete: true}
	if !exists {
		return snapshot, nil
	}
	snapshot.Revisions, err = store.Read(ctx, q, request.GetMaxRevisions())
	switch {
	case errors.Is(err, history.ErrTooMany):
		return nil, runtime.ErrorWithReason(codes.ResourceExhausted, "the revision table holds more records than the read takes")
	case errors.Is(err, history.ErrRecord):
		return nil, runtime.ErrorWithReason(codes.FailedPrecondition, "the revision table holds a row no revision record stands for")
	case err != nil:
		if record != nil {
			record.failed = true
		}
		return nil, callFailed(ctx, "the provider could not read the revision table")
	}
	if proto.Size(snapshot) > maxSnapshotBytes {
		return nil, runtime.ErrorWithReason(codes.ResourceExhausted, "the revision table is larger than a snapshot carries")
	}
	return snapshot, nil
}

// MutateHistory compares the stored record of a version with the expected
// one and, when they are equal, writes the replacement, in one read-write
// transaction: the request's, or one of its own that it commits. A conflict
// writes nothing. A failure before a commit was sent wrote nothing, because
// Spanner applies a transaction only at its commit; a commit Spanner did not
// confirm is UNKNOWN. A request is never sent twice.
func (p *Provider) MutateHistory(ctx context.Context, request *protocol.MutateHistoryRequest) (*protocol.HistoryResult, error) {
	current, err := p.lookup(request.GetScope().GetSessionId())
	if err != nil {
		return nil, err
	}
	current.mu.Lock()
	defer current.mu.Unlock()
	answer := func(outcome protocol.HistoryResult_Outcome, code, message string) *protocol.HistoryResult {
		return &protocol.HistoryResult{Scope: proto.CloneOf(request.GetScope()), RequestId: request.GetRequestId(), Outcome: outcome,
			DiagnosticCode: code, Message: message}
	}
	if !current.guard(ctx) {
		return answer(protocol.HistoryResult_REFUSED, historyLockLost, "the session lost a lock it held"), nil
	}
	store := history.StoreOf(request.GetScope())
	exists, err := ownedtable.Locate(ctx, current.conn, store.Definition())
	switch {
	case errors.Is(err, ownedtable.ErrLayout):
		return answer(protocol.HistoryResult_REFUSED, historyLayout, "a table of the revision table's name exists and "+layoutReason(err)), nil
	case err != nil:
		return answer(protocol.HistoryResult_REFUSED, historyUnavailable, "the provider could not read the catalog"), nil
	case !exists:
		return answer(protocol.HistoryResult_REFUSED, historyMissing, "the revision table does not exist"), nil
	}
	if id := request.GetScope().GetTransactionId(); id != "" {
		record, err := current.openTransaction(id)
		if err != nil {
			return answer(protocol.HistoryResult_REFUSED, historyNoTransaction, "the transaction is not open or a statement in it failed"), nil
		}
		outcome, err := store.Mutate(ctx, record.tx, request)
		switch {
		case errors.Is(err, history.ErrRecord):
			return answer(protocol.HistoryResult_REFUSED, historyRecord, "the revision record does not fit the revision table"), nil
		case err != nil:
			// The transaction is aborted and commits nothing.
			record.failed = true
			return answer(protocol.HistoryResult_REFUSED, historyWriteRefused, "the revision table refused the write, and the transaction can only be rolled back"), nil
		}
		return answer(outcome, "", ""), nil
	}
	// The lock was checked above, which renews a lease with little left, so
	// the check before the commit reads the lock row and writes nothing while
	// the transaction is open.
	tx, err := current.conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return answer(protocol.HistoryResult_REFUSED, historyUnavailable, "the provider could not begin a transaction"), nil
	}
	// A rollback that fails leaves nothing written: no commit was sent.
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	outcome, err := store.Mutate(ctx, tx, request)
	switch {
	case errors.Is(err, history.ErrRecord):
		return answer(protocol.HistoryResult_REFUSED, historyRecord, "the revision record does not fit the revision table"), nil
	case err != nil:
		return answer(protocol.HistoryResult_REFUSED, historyWriteRefused, "the revision table refused the write"), nil
	case outcome != protocol.HistoryResult_APPLIED:
		return answer(outcome, "", ""), nil
	case !current.guard(ctx):
		return answer(protocol.HistoryResult_REFUSED, historyLockLost, "the session lost a lock it held, so the write was not committed"), nil
	}
	err = tx.Commit(ctx)
	switch refused, found := errors.AsType[*pgconn.PgError](err); {
	case err == nil:
		return answer(protocol.HistoryResult_APPLIED, "", ""), nil
	case found && refused.Code == "40001":
		return answer(protocol.HistoryResult_REFUSED, historyWriteRefused, "Spanner aborted the write for a conflict and applied nothing"), nil
	default:
		return answer(protocol.HistoryResult_UNKNOWN, historyNotConfirmed, "Spanner did not confirm the commit, and it was not sent again"), nil
	}
}

// layoutReason says how a table differs from a store's layout, from an
// ownedtable.LayoutError.
func layoutReason(err error) string {
	detail := "its layout differs"
	if layout, found := errors.AsType[*ownedtable.LayoutError](err); found {
		detail = layout.Detail
	}
	return "does not hold the layout the provider keeps it in: " + detail
}
