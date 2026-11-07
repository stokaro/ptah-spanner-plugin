// Package backend serves Cloud Spanner's PostgreSQL interface to Ptah through
// the experimental provider protocol. It opens sessions through PGAdapter,
// reads tables, columns, primary keys and secondary indexes from
// information_schema, renders and runs the statements the host plans for
// them, and judges column types and defaults in Spanner's spelling.
//
// The provider describes what it reads completely or refuses the read: an
// object it does not describe yet, such as a foreign key or a view, fails the
// read with a reason instead of disappearing from it, because the host would
// plan to create what a read leaves out.
package backend

import (
	"context"
	"sync"

	"github.com/jackc/pgx/v5"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"

	"github.com/stokaro/ptah-spanner-plugin/internal/buildversion"
)

// ProviderID is the identity of this provider and the namespace of its model
// kinds.
const ProviderID = "github.com/stokaro/ptah-spanner-plugin"

// engineID is the engine name the host selects this provider by. It is also
// the scheme of the addresses Open accepts.
const engineID = "spanner"

// defaultSchema is the schema a Spanner database keeps its tables in when a
// statement names none.
const defaultSchema = "public"

// semanticIdentity names what the provider's readings and statements mean. A
// saved plan binds to it, so it changes when the same input would mean
// something else.
const semanticIdentity = "spanner-postgresql-1"

// Provider serves the spanner engine. The zero value is ready to use, and one
// value serves every session of the process.
type Provider struct {
	mu       sync.Mutex
	sessions map[string]*session
}

// session is one open database. Its mutex serializes the calls that use its
// connection, because one PostgreSQL-wire connection runs one statement at a
// time.
type session struct {
	mu       sync.Mutex
	conn     *pgx.Conn
	config   *pgx.ConnConfig
	database string
	// lockMu guards the lock connection, the lock records and lost. A lock
	// check takes it and not mu, so the host's watch of a lock is answered
	// while a statement holds mu: the watch checks every second and counts a
	// check unanswered for ten seconds as a lost lock. A caller that holds both
	// took mu first.
	lockMu   sync.Mutex
	lockConn *pgx.Conn
	locks    map[string]*lockRecord
	// lost is set once a lock the session held is gone. It never clears: the
	// session runs nothing more.
	lost bool
}

// New returns a provider with no open session.
func New() *Provider { return &Provider{} }

// providerFeatures are the services and behaviors the provider offers.
func providerFeatures() []string {
	return []string{
		provider.InspectFeature,
		provider.RenderFeature,
		provider.ExecuteFeature,
		provider.AnalyzeSQLFeature,
		// A failed call carries a reason the provider wrote.
		provider.ErrorReasonsFeature,
		// A read reports each column whole, the primary key in key order and
		// the secondary indexes a table holds.
		provider.ObservedColumnsFeature,
		provider.ObservedPrimaryKeysFeature,
		provider.ObservedIndexesFeature,
		// The SDK pairs the observations above with the normalization
		// service; the provider declares no model for it.
		provider.NormalizeValuesFeature,
		// The host takes a lock before it applies a plan; see locks.go.
		provider.LocksFeature,
	}
}

// engineFeatures is the feature list of the spanner engine: every feature of
// the provider, and common-table identity, under which the host plans the
// engine's tables and columns with its common planner and the provider
// renders each change. The description, the session and the inspection
// snapshot all report it, so the host holds one target profile for observed,
// desired and planned values.
func engineFeatures() []string {
	return append(providerFeatures(), provider.CommonTableIdentityFeature)
}

// minimumContract is the oldest contract a host must speak to start the
// provider: the one the SDK it is built with speaks, which is the contract its
// live tests run against. An older host is refused before a session opens.
const minimumContract = 27

// Description declares the provider and the spanner engine.
func Description() *protocol.Description {
	return &protocol.Description{
		Revision:         provider.Revision,
		MinContract:      &protocol.ContractVersion{Major: provider.Revision, Minor: minimumContract},
		ProviderId:       ProviderID,
		Version:          buildversion.Version(),
		SemanticIdentity: semanticIdentity,
		Features:         providerFeatures(),
		Engines: []*protocol.Engine{{
			Id:            engineID,
			Aliases:       aliases(),
			DefaultSchema: defaultSchema,
			Identifiers:   identifierRules(),
			ReleaseLines:  releaseLines(),
			Features:      engineFeatures(),
		}},
	}
}

// negotiatedFeatures is the engine's feature list as the host holds it: the
// two-way features the host did not list are left out, as the SDK leaves them
// out of the description the host reads. A session and an inspection snapshot
// report it, so the host can check them against that description.
func negotiatedFeatures(ctx context.Context) []string {
	return provider.NegotiatedDescription(Description(), provider.HostFeatures(ctx)).GetEngines()[0].GetFeatures()
}
