package backend

import (
	"context"
	"crypto/rand"
	"errors"
	"maps"

	"github.com/jackc/pgx/v5"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/stokaro/ptah-spanner-plugin/internal/address"
	"github.com/stokaro/ptah-spanner-plugin/internal/commonobject"
)

// sessionCapabilities are the host capabilities a session states. A present
// false is an established answer; a capability the provider does not name
// stays unknown to the host.
//
// transactional_ddl and ddl_inside_transaction are false: Spanner refuses a
// schema statement inside an explicit transaction (`DDL statements are only
// allowed outside explicit transactions`, SQLSTATE 25000), so the host runs a
// plan's statements without one and every statement commits as it runs.
//
// unique_constraints is false: Spanner refuses the UNIQUE constraint spelling
// (`<UNIQUE> constraint is not supported, create a unique index instead.`) and
// keeps unique indexes.
//
// primary_key_alterable and rename_column_clause are false: a key is part of a
// Spanner table's storage layout, so changing it is refused, and a column
// rename answers `Only <TABLE> is supported for renaming`.
//
// The views and sequences the session carries add their own; see
// commonobject.Capabilities.
//
// Each was measured on the emulator behind PGAdapter by Ptah's built-in
// Spanner engine and is restated here because the provider plans with it.
// The offline facts state each of them too, beside the rest of the built-in
// engine's preset; see offlineCapabilities.
func sessionCapabilities() map[string]bool {
	capabilities := map[string]bool{
		"transactional_ddl":      false,
		"ddl_inside_transaction": false,
		"unique_constraints":     false,
		"primary_key_alterable":  false,
		"rename_column_clause":   false,
	}
	maps.Copy(capabilities, commonobject.Capabilities())
	return capabilities
}

// Open connects to the database the address names and returns a session bound
// to it. The address travels in the request, never on a command line, because
// it can carry a credential; no reason the provider writes quotes it.
func (p *Provider) Open(ctx context.Context, request *protocol.OpenRequest) (*protocol.Session, error) {
	if request.GetEngine() != engineID {
		return nil, runtime.ErrorWithReason(codes.InvalidArgument, "the provider serves the spanner engine only")
	}
	target, err := address.Parse(request.GetAddress())
	if err != nil {
		return nil, runtime.ErrorWithReason(codes.InvalidArgument, err.Error())
	}
	config, err := pgx.ParseConfig(target.ConnString)
	if err != nil {
		return nil, runtime.ErrorWithReason(codes.InvalidArgument, "the address carries a parameter the PostgreSQL driver cannot read")
	}
	// PGAdapter answers catalog queries from an emulation of its own, so the
	// provider sends each statement as text and keeps no prepared statement
	// across schema changes.
	config.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		if ctx.Err() != nil {
			return nil, status.FromContextError(ctx.Err()).Err()
		}
		return nil, runtime.ErrorWithReason(codes.Unavailable, "the provider could not connect to the Spanner endpoint the address names")
	}
	// PGAdapter answers `SELECT version()` itself, and only as a statement of
	// its own: Spanner has no version() function, so the two facts are read
	// apart.
	var serverVersion, database string
	err = conn.QueryRow(ctx, "SELECT version()").Scan(&serverVersion)
	if err == nil {
		err = conn.QueryRow(ctx, "SELECT current_database()").Scan(&database)
	}
	if err != nil {
		closeQuietly(ctx, conn)
		if ctx.Err() != nil {
			return nil, status.FromContextError(ctx.Err()).Err()
		}
		return nil, runtime.ErrorWithReason(codes.Unavailable, "the Spanner endpoint did not report its version and database")
	}
	if database != target.Database {
		closeQuietly(ctx, conn)
		return nil, runtime.ErrorWithReason(codes.FailedPrecondition, "the endpoint reports another database than the address names")
	}
	id := rand.Text()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sessions == nil {
		p.sessions = make(map[string]*session)
	}
	p.sessions[id] = &session{conn: conn, config: config, database: database}
	return &protocol.Session{
		Id:            id,
		Engine:        engineID,
		Database:      database,
		DefaultSchema: defaultSchema,
		ServerVersion: serverVersion,
		Features:      negotiatedFeatures(ctx),
		Identifiers:   identifierRules(),
		Capabilities:  sessionCapabilities(),
	}, nil
}

// Close releases one session. Closing a session twice succeeds.
func (p *Provider) Close(ctx context.Context, request *protocol.CloseRequest) error {
	p.mu.Lock()
	found := p.sessions[request.GetSessionId()]
	delete(p.sessions, request.GetSessionId())
	p.mu.Unlock()
	if found == nil {
		return nil
	}
	found.mu.Lock()
	defer found.mu.Unlock()
	found.rollbackTransactions(ctx)
	found.readers = nil
	found.results = nil
	found.lockMu.Lock()
	defer found.lockMu.Unlock()
	if errors.Join(found.releaseLocks(ctx), found.conn.Close(ctx)) != nil {
		return runtime.ErrorWithReason(codes.Internal, "the session did not close cleanly")
	}
	return nil
}

// lookup returns the open session id names.
func (p *Provider) lookup(id string) (*session, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	found := p.sessions[id]
	if found == nil {
		return nil, runtime.ErrorWithReason(codes.NotFound, "the session is closed or was never opened")
	}
	return found, nil
}

// closeQuietly closes a connection the provider is abandoning after a failed
// open. The open already reports its own failure, which a close error does
// not change. The close outlives the caller's cancellation, which may be the
// failure being reported.
func closeQuietly(ctx context.Context, conn *pgx.Conn) {
	_ = conn.Close(context.WithoutCancel(ctx))
}

// callFailed reports a failed database call. A canceled call keeps the
// context's error; any other failure says what failed and leaves the driver's
// text out, because it can quote a value or a credential.
func callFailed(ctx context.Context, reason string) error {
	if err := ctx.Err(); err != nil {
		return status.FromContextError(err).Err()
	}
	return runtime.ErrorWithReason(codes.Unavailable, reason)
}
