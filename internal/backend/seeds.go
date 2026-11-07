package backend

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/runtime"
	"google.golang.org/grpc/codes"

	"github.com/stokaro/ptah-spanner-plugin/internal/seedfile"
	"github.com/stokaro/ptah-spanner-plugin/internal/seedledger"
)

// seedAttempts bounds how often a seed's transaction runs when Spanner aborts
// it for a conflicting transaction. PGAdapter retries an aborted transaction
// itself first, so an abort that reaches the provider is rare.
const seedAttempts = 3

// The SQLSTATE codes a seed run tells apart: a key that already exists, and a
// transaction Spanner aborted for a conflict, which applied nothing.
const (
	uniqueViolation      = "23505"
	serializationFailure = "40001"
)

// ListSeedTables lists the base tables of one schema, the default one when
// the request names none, in name order, for the host's protected-table
// check.
func (p *Provider) ListSeedTables(ctx context.Context, request *protocol.ListSeedTablesRequest) (*protocol.SeedTables, error) {
	current, err := p.lookup(request.GetSessionId())
	if err != nil {
		return nil, err
	}
	schema := request.GetSchema()
	if schema == "" {
		schema = defaultSchema
	}
	current.mu.Lock()
	defer current.mu.Unlock()
	rows, err := current.conn.Query(ctx, `SELECT table_name FROM information_schema.tables
WHERE table_schema = $1 AND table_type = 'BASE TABLE' ORDER BY table_name`, schema)
	if err != nil {
		return nil, callFailed(ctx, "the provider could not list the schema's tables")
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, callFailed(ctx, "the provider could not list the schema's tables")
	}
	result := &protocol.SeedTables{}
	for _, name := range names {
		result.Tables = append(result.Tables, &protocol.Reference{Kind: "table", Schema: schema, Name: name})
	}
	return result, nil
}

// ApplySeed runs a seed file and records it in the ledger in one read-write
// transaction. A file holding a statement Spanner runs only outside a
// transaction, or any other statement that is not a row write or a query, is
// refused before anything runs; see seedfile.Statements. A statement that hits
// an existing key ends the transaction with nothing kept and answers
// KEY_CONFLICT.
func (p *Provider) ApplySeed(ctx context.Context, request *protocol.ApplySeedRequest) (*protocol.SeedResult, error) {
	current, err := p.lookup(request.GetSessionId())
	if err != nil {
		return nil, err
	}
	seed := request.GetSeed()
	result := &protocol.SeedResult{RequestId: request.GetRequestId(), Path: seed.GetPath()}
	statements, err := seedfile.Statements(seed.GetText())
	if refused, ok := errors.AsType[*seedfile.Refusal](err); ok {
		line, column := seedfile.Position(seed.GetText(), refused.Offset)
		result.Outcome, result.Reason = protocol.SeedResult_REFUSED, refused.Reason
		// #nosec G115 -- the position is inside one request's seed text, which the transport bounds far below 4 GiB
		result.Position = &protocol.SourcePosition{ByteOffset: uint64(refused.Offset), Line: uint32(line), Column: uint32(column)}
		return result, nil
	}
	if err != nil {
		return nil, runtime.ErrorWithReason(codes.InvalidArgument, err.Error())
	}
	current.mu.Lock()
	defer current.mu.Unlock()
	return current.runSeed(ctx, result, seed, statements), nil
}

// RecordSeed writes the ledger entry of a seed without running its file, as
// an idempotent run does after a key conflict.
func (p *Provider) RecordSeed(ctx context.Context, request *protocol.RecordSeedRequest) (*protocol.SeedResult, error) {
	current, err := p.lookup(request.GetSessionId())
	if err != nil {
		return nil, err
	}
	result := &protocol.SeedResult{RequestId: request.GetRequestId(), Path: request.GetSeed().GetPath()}
	current.mu.Lock()
	defer current.mu.Unlock()
	return current.runSeed(ctx, result, request.GetSeed(), nil), nil
}

// ReadSeedLedger reads the whole ledger in path order. Reading never creates
// it: a missing ledger is a complete answer that says so, and a table of its
// name in another layout is refused.
func (p *Provider) ReadSeedLedger(ctx context.Context, request *protocol.ReadSeedLedgerRequest) (*protocol.SeedLedger, error) {
	current, err := p.lookup(request.GetSessionId())
	if err != nil {
		return nil, err
	}
	current.mu.Lock()
	defer current.mu.Unlock()
	exists, err := seedledger.Check(ctx, current.conn)
	if foreign := (seedledger.Foreign{}); errors.As(err, &foreign) {
		return nil, runtime.ErrorWithReason(codes.FailedPrecondition, foreign.Error())
	}
	if err != nil {
		return nil, callFailed(ctx, "the provider could not read the seed ledger's layout")
	}
	ledger := &protocol.SeedLedger{Exists: exists, Complete: true}
	if !exists {
		return ledger, nil
	}
	rows, err := current.conn.Query(ctx, seedledger.Read(request.GetMaxEntries()))
	if err != nil {
		return nil, callFailed(ctx, "the provider could not read the seed ledger")
	}
	defer rows.Close()
	for rows.Next() {
		if len(ledger.Entries) == int(request.GetMaxEntries()) {
			return nil, runtime.ErrorWithReason(codes.ResourceExhausted, fmt.Sprintf("the seed ledger holds more than %d entries", request.GetMaxEntries()))
		}
		entry := &protocol.SeedLedgerEntry{}
		var applied time.Time
		if err := rows.Scan(&entry.Path, &entry.Checksum, &entry.Env, &applied); err != nil {
			return nil, callFailed(ctx, "the provider could not read the seed ledger")
		}
		entry.AppliedAtSeconds, entry.AppliedAtNanos = applied.Unix(), int32(applied.Nanosecond()) // #nosec G115 -- a nanosecond count is below 1e9
		ledger.Entries = append(ledger.Entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, callFailed(ctx, "the provider could not read the seed ledger")
	}
	return ledger, nil
}

// runSeed makes sure the ledger exists, then runs the statements of a seed
// file, none for RecordSeed, and its ledger entry in one transaction, again
// when Spanner aborts the transaction for a conflict, at most seedAttempts
// times. The caller holds the session's mutex.
func (s *session) runSeed(ctx context.Context, result *protocol.SeedResult, seed *protocol.Seed, statements []seedfile.Statement) *protocol.SeedResult {
	if !s.guard(ctx) {
		return seedOutcome(result, protocol.SeedResult_NOT_APPLIED, "the session lost a lock it held; nothing ran")
	}
	if reason := s.prepareLedger(ctx); reason != "" {
		return seedOutcome(result, protocol.SeedResult_NOT_APPLIED, reason)
	}
	for range seedAttempts {
		outcome, reason, aborted := s.seedTransaction(ctx, seed, statements)
		if outcome == protocol.SeedResult_KEY_CONFLICT && statements == nil {
			// A record runs no statement of the file, so its only conflict is
			// another run writing the same entry at the same time.
			outcome, reason = protocol.SeedResult_NOT_APPLIED, "another run wrote the seed's ledger entry at the same time; nothing committed"
		}
		if !aborted {
			return seedOutcome(result, outcome, reason)
		}
	}
	return seedOutcome(result, protocol.SeedResult_NOT_APPLIED,
		fmt.Sprintf("Spanner aborted the seed's transaction %d times for conflicting transactions; nothing committed", seedAttempts))
}

// prepareLedger creates a missing ledger outside a transaction, as Spanner
// runs a schema statement, and refuses a table of its name in another layout.
// It returns the reason it stopped, or nothing.
func (s *session) prepareLedger(ctx context.Context) string {
	if _, err := s.conn.Exec(ctx, seedledger.Create); err != nil {
		return "the seed ledger could not be created; nothing ran"
	}
	if _, err := seedledger.Check(ctx, s.conn); err != nil {
		if foreign := (seedledger.Foreign{}); errors.As(err, &foreign) {
			return foreign.Error() + "; nothing ran"
		}
		return "the seed ledger's layout could not be read; nothing ran"
	}
	return ""
}

// seedTransaction runs the statements and the ledger entry in one read-write
// transaction. It reports aborted when Spanner aborted the transaction for a
// conflict, which applied nothing, so a new attempt may succeed. Until COMMIT
// is sent nothing can have committed; a failed COMMIT the server answered
// committed nothing either, and only a COMMIT that got no answer is UNKNOWN.
func (s *session) seedTransaction(ctx context.Context, seed *protocol.Seed, statements []seedfile.Statement) (protocol.SeedResult_Outcome, string, bool) {
	tx, err := s.conn.Begin(ctx)
	if err != nil {
		return protocol.SeedResult_NOT_APPLIED, "the seed's transaction did not begin; nothing ran", false
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()
	for _, statement := range statements {
		if _, err := tx.Exec(ctx, statement.Body); err != nil {
			return statementFailed(ctx, err, fmt.Sprintf("statement %d on line %d", statement.Number, statement.Line))
		}
	}
	if _, err := tx.Exec(ctx, seedledger.Remove, seed.GetPath()); err != nil {
		return statementFailed(ctx, err, "the ledger entry")
	}
	if _, err := tx.Exec(ctx, seedledger.Record, seed.GetPath(), seed.GetEnv(), seed.GetChecksum()); err != nil {
		return statementFailed(ctx, err, "the ledger entry")
	}
	if !s.guard(ctx) {
		return protocol.SeedResult_NOT_APPLIED, "the session lost a lock it held before the seed committed; nothing committed", false
	}
	if err := tx.Commit(ctx); err != nil {
		var refused *pgconn.PgError
		switch {
		case errors.Is(err, pgx.ErrTxCommitRollback):
			return protocol.SeedResult_NOT_APPLIED, "Spanner rolled the seed's transaction back at commit; nothing committed", false
		case !errors.As(err, &refused) || ctx.Err() != nil:
			return protocol.SeedResult_UNKNOWN, "the seed's commit got no answer, so whether it committed is unknown", false
		case refused.Code == serializationFailure:
			return protocol.SeedResult_NOT_APPLIED, "", true
		case refused.Code == uniqueViolation:
			return protocol.SeedResult_KEY_CONFLICT, "", false
		default:
			return protocol.SeedResult_NOT_APPLIED, fmt.Sprintf("Spanner refused the seed's commit with SQLSTATE %s; nothing committed", refused.Code), false
		}
	}
	committed = true
	return protocol.SeedResult_APPLIED, "", false
}

// statementFailed is the outcome of a statement that failed inside the seed's
// transaction, which then rolls back: nothing committed. The reason names the
// statement and the SQLSTATE, never the server's message, which can quote a
// value.
func statementFailed(ctx context.Context, err error, what string) (protocol.SeedResult_Outcome, string, bool) {
	var refused *pgconn.PgError
	switch {
	case ctx.Err() != nil:
		return protocol.SeedResult_NOT_APPLIED, fmt.Sprintf("the call ended while %s ran; nothing committed", what), false
	case !errors.As(err, &refused):
		return protocol.SeedResult_NOT_APPLIED, fmt.Sprintf("%s did not complete; nothing committed", what), false
	case refused.Code == serializationFailure:
		return protocol.SeedResult_NOT_APPLIED, "", true
	case refused.Code == uniqueViolation:
		return protocol.SeedResult_KEY_CONFLICT, "", false
	default:
		return protocol.SeedResult_NOT_APPLIED, fmt.Sprintf("Spanner refused %s with SQLSTATE %s; nothing committed", what, refused.Code), false
	}
}

// seedOutcome sets the outcome and the reason of a seed result.
func seedOutcome(result *protocol.SeedResult, outcome protocol.SeedResult_Outcome, reason string) *protocol.SeedResult {
	result.Outcome, result.Reason = outcome, reason
	return result
}
