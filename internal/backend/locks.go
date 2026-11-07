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
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-spanner-plugin/internal/locktable"
)

// lockRecord is one lock ID of a session. A lock ID is used once: a second
// request with the same ID gets the answer of the first.
type lockRecord struct {
	name    string
	outcome protocol.LockResult_Outcome
}

// TryLock makes one attempt to take a lock and does not wait.
//
// Spanner has no session lock, so a lock is a row of the provider's lock
// table that names its holder and the time its lease ends; see locktable. The
// row is taken in a read-write transaction, which Spanner allows for data
// where it refuses one for a schema change. A holder whose lease has ended,
// such as a process that died, loses the row to the next attempt.
func (p *Provider) TryLock(ctx context.Context, request *protocol.LockRequest) (*protocol.LockResult, error) {
	current, err := p.lookup(request.GetScope().GetSessionId())
	if err != nil {
		return nil, err
	}
	current.lockMu.Lock()
	defer current.lockMu.Unlock()
	record, fresh, err := current.reserve(request.GetScope())
	if err != nil {
		return nil, err
	}
	switch {
	case current.lost:
		return receipt(ctx, request, protocol.LockResult_LOST, "the session lost a lock it held and takes no other"), nil
	case !fresh:
		current.verify(ctx, request.GetScope().GetLockId(), record)
		return receipt(ctx, request, record.outcome, ""), nil
	}
	conn, err := current.lockConnection(ctx)
	if err != nil {
		return nil, err
	}
	taken, err := locktable.Take(ctx, conn, record.name, request.GetScope().GetLockId())
	if err != nil {
		var refused *pgconn.PgError
		if errors.As(err, &refused) && slices.Contains([]string{"23505", "40001"}, refused.Code) {
			// Another session inserted the row first, or Spanner aborted
			// this transaction for one that touched it: the commit failed,
			// so nothing was taken.
			record.outcome = protocol.LockResult_BUSY
			return receipt(ctx, request, record.outcome, "another session holds the lock or is taking it"), nil
		}
		return nil, callFailed(ctx, "the lock attempt did not complete, and whether it took the lock is unknown")
	}
	if !taken {
		record.outcome = protocol.LockResult_BUSY
		return receipt(ctx, request, record.outcome, "another session holds the lock"), nil
	}
	record.outcome = protocol.LockResult_ACQUIRED
	return receipt(ctx, request, record.outcome, ""), nil
}

// CheckLock proves that the session still holds the lock now, rather than
// repeating an earlier answer, and renews a lease with little left.
func (p *Provider) CheckLock(ctx context.Context, request *protocol.LockRequest) (*protocol.LockResult, error) {
	current, err := p.lookup(request.GetScope().GetSessionId())
	if err != nil {
		return nil, err
	}
	current.lockMu.Lock()
	defer current.lockMu.Unlock()
	record, _, err := current.reserve(request.GetScope())
	if err != nil {
		return nil, err
	}
	current.verify(ctx, request.GetScope().GetLockId(), record)
	switch record.outcome {
	case protocol.LockResult_BUSY:
		return receipt(ctx, request, protocol.LockResult_NOT_ACQUIRED, "the attempt with this lock ID found the lock busy"), nil
	case protocol.LockResult_LOST:
		return receipt(ctx, request, record.outcome, "the lock's lease ended or another session took the lock"), nil
	case protocol.LockResult_ACQUIRED:
		return receipt(ctx, request, record.outcome, ""), nil
	default:
		return receipt(ctx, request, record.outcome, "the lock ID holds no lock"), nil
	}
}

// ReleaseLock gives a lock back. Releasing an ID that never took a lock fences
// it, so a late TryLock with that ID takes nothing.
func (p *Provider) ReleaseLock(ctx context.Context, request *protocol.LockRequest) (*protocol.LockResult, error) {
	current, err := p.lookup(request.GetScope().GetSessionId())
	if err != nil {
		return nil, err
	}
	current.lockMu.Lock()
	defer current.lockMu.Unlock()
	record, _, err := current.reserve(request.GetScope())
	if err != nil {
		return nil, err
	}
	switch record.outcome {
	case protocol.LockResult_ACQUIRED:
		if err := locktable.Give(ctx, current.lockConn, record.name, request.GetScope().GetLockId()); err != nil {
			record.outcome, current.lost = protocol.LockResult_LOST, true
			return nil, callFailed(ctx, "the lock could not be released; its lease ends on its own")
		}
		record.outcome = protocol.LockResult_RELEASED
	case protocol.LockResult_BUSY:
		record.outcome = protocol.LockResult_NOT_ACQUIRED
	}
	return receipt(ctx, request, record.outcome, ""), nil
}

// reserve returns the record of a lock ID and whether this call created it.
// The caller holds lockMu. The SDK has already refused a request without a
// session, a lock ID or a name.
func (s *session) reserve(scope *protocol.LockScope) (*lockRecord, bool, error) {
	if s.locks == nil {
		s.locks = make(map[string]*lockRecord)
	}
	if record := s.locks[scope.GetLockId()]; record != nil {
		if record.name != scope.GetName() {
			return nil, false, runtime.ErrorWithReason(codes.InvalidArgument, "a lock ID names one lock")
		}
		return record, false, nil
	}
	record := &lockRecord{name: scope.GetName(), outcome: protocol.LockResult_NOT_ACQUIRED}
	s.locks[scope.GetLockId()] = record
	return record, true, nil
}

// verify checks a lock the session holds, and extends its lease when little
// of it is left (see locktable.Check). When the lease ended,
// another holder took the row or the check failed, the session has lost the
// lock, and it stays fenced. The caller holds lockMu.
func (s *session) verify(ctx context.Context, lockID string, record *lockRecord) {
	if record.outcome != protocol.LockResult_ACQUIRED {
		return
	}
	held, err := locktable.Check(ctx, s.lockConn, record.name, lockID)
	if s.lost || err != nil || !held {
		record.outcome, s.lost = protocol.LockResult_LOST, true
	}
}

// guard verifies every lock the session holds. Execute calls it before each
// unit, because a statement must not run after the lock that protects it is
// gone.
func (s *session) guard(ctx context.Context) bool {
	s.lockMu.Lock()
	defer s.lockMu.Unlock()
	for lockID, record := range s.locks {
		s.verify(ctx, lockID, record)
	}
	return !s.lost
}

// lockConnection returns the session's lock connection, opened on first use
// with the session's own settings, and makes sure the lock table exists. A
// lock has a connection of its own, so CheckLock is answered while a statement
// runs on the session's connection. The caller holds lockMu.
func (s *session) lockConnection(ctx context.Context) (*pgx.Conn, error) {
	if s.lockConn == nil {
		conn, err := pgx.ConnectConfig(ctx, s.config.Copy())
		if err != nil {
			return nil, callFailed(ctx, "the provider could not open its lock connection")
		}
		s.lockConn = conn
	}
	if err := locktable.Create(ctx, s.lockConn); err != nil {
		return nil, callFailed(ctx, "the provider could not create its lock table")
	}
	return s.lockConn, nil
}

// releaseLocks gives back every lock the session holds and closes the lock
// connection. Close calls it; a lock it cannot give back keeps its lease until
// the lease ends. The caller holds lockMu.
func (s *session) releaseLocks(ctx context.Context) error {
	var failures []error
	for lockID, record := range s.locks {
		if record.outcome == protocol.LockResult_ACQUIRED {
			failures = append(failures, locktable.Give(ctx, s.lockConn, record.name, lockID))
			record.outcome = protocol.LockResult_RELEASED
		}
	}
	if s.lockConn != nil {
		failures = append(failures, s.lockConn.Close(ctx))
		s.lockConn = nil
	}
	s.lost = true
	return errors.Join(failures...)
}

// receipt answers a lock request with its own scope and request ID, and the
// reason for an outcome that is not a success when the host takes reasons.
func receipt(ctx context.Context, request *protocol.LockRequest, outcome protocol.LockResult_Outcome, reason string) *protocol.LockResult {
	result := &protocol.LockResult{Scope: proto.CloneOf(request.GetScope()), RequestId: request.GetRequestId(), Outcome: outcome}
	if reason != "" && slices.Contains(provider.HostFeatures(ctx), provider.ErrorReasonsFeature) && !lockSucceeded(outcome) {
		result.Diagnostic = &protocol.ProviderDiagnostic{Reason: reason}
	}
	return result
}

// lockSucceeded reports the outcomes a lock call succeeds with, which carry no
// reason.
func lockSucceeded(outcome protocol.LockResult_Outcome) bool {
	return outcome == protocol.LockResult_ACQUIRED || outcome == protocol.LockResult_RELEASED || outcome == protocol.LockResult_NOT_ACQUIRED
}
