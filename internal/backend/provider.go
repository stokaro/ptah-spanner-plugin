// Package backend serves Cloud Spanner's PostgreSQL interface to Ptah through
// the experimental provider protocol. It opens sessions through PGAdapter,
// reads tables, columns, primary keys and secondary indexes from
// information_schema, owns the lifecycle of the engine's tables (it checks a
// declaration, predicts what a creation holds, judges column types and
// defaults in Spanner's spelling and plans every table statement), and
// renders and runs the statements it plans. It also types, reads and writes
// the rows a schema declares, and runs seed files with their ledger.
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
	"github.com/stokaro/ptah-spanner-plugin/internal/migrationlint"
	"github.com/stokaro/ptah-spanner-plugin/internal/rowdeletion"
	"github.com/stokaro/ptah-spanner-plugin/internal/tableop"
	"github.com/stokaro/ptah-spanner-plugin/internal/tableplan"
)

// ProviderID is the identity of this provider and the namespace of its model
// kinds.
const ProviderID = tableop.ProviderID

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
	// transactions are the session's transaction handles by ID, and active
	// the ID of the one whose transaction is open, if any; see
	// transactions.go. mu guards them.
	transactions map[string]*transaction
	active       string
	// readers are the metadata snapshot readers by ID; see metadata.go. mu
	// guards them.
	readers map[string]*reader
	// results are the operator statements' results kept open past their
	// first page, by page token; see statements.go. mu guards them.
	results map[string]*openResult
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
		// The provider owns the lifecycle of the engine's tables: it checks a
		// declaration, states its key columns, predicts what a creation holds,
		// judges columns in Spanner's spelling, and plans every statement
		// through its planning service; see tableplan.
		provider.ValidateSchemaFeature,
		provider.PrepareTablesFeature,
		provider.ProjectTableCreationsFeature,
		provider.ColumnsFeature,
		provider.PlanFeaturesFeature,
		// Its steps run in the lifecycle phase, ordered by their effects and
		// dependencies; see tableplan.planningOperation.
		provider.PlanningLifecycleFeature,
		// A changed primary key reaches the planning service, which refuses
		// it by name: Spanner cannot change a table's key.
		provider.PrimaryKeyChangesFeature,
		// Every Ptah command plans in the exact statement style, which is the
		// only style the provider writes: no statement carries an existence
		// guard, which Spanner refuses on ALTER.
		provider.PlanningPolicyFeature,
		provider.PlanningStatementStyleFeature,
		// The provider normalizes its table operations and the table settings
		// its planning service owns.
		provider.NormalizeValuesFeature,
		// The host takes a lock before it applies a plan; see locks.go.
		provider.LocksFeature,
		// Migrations keep their revision records, log and tags in the
		// tables Ptah's built-in Spanner engine kept them in, and read and
		// change the records in Spanner's read-write transactions, which
		// change data only; see history.go, metadata.go and
		// transactions.go.
		provider.TransactionsFeature,
		provider.HistoryFeature,
		provider.MetadataFeature,
		// A migration body runs statement by statement, each where Spanner
		// runs it; see migrationunits.
		provider.MigrationSplitFeature,
		// Declared rows are typed, read and written by the provider, and seed
		// files run through it with their ledger; see rows.go and seeds.go.
		provider.TypeRowsFeature,
		provider.ReadRowsFeature,
		provider.RenderRowWritesFeature,
		provider.SeedsFeature,
		// Migration lint splits a migration as Spanner reads it and checks
		// the SP rules; see lint.go.
		provider.LintFeature,
		// The host empties a dev database through the provider and asks it
		// whether a replayed statement stays inside one; see devclean.go and
		// replay.go.
		provider.DevCleanupFeature,
		provider.ReplayGuardFeature,
		// The statements an operator writes, such as a migration's check
		// assertions, run through the provider; see statements.go.
		provider.StatementsFeature,
	}
}

// engineFeatures is the feature list of the spanner engine: every feature of
// the provider, and the statement facts of its lint split, which an engine
// alone lists. The description, the session and the inspection snapshot all
// report it, so the host holds one target profile for observed, desired and
// planned values.
func engineFeatures() []string {
	return append(providerFeatures(), provider.LintStatementFactsFeature)
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
			LintRules:     migrationlint.Rules(),
			ValueModels:   append([]*protocol.ValueModel{tableop.Model()}, rowdeletion.Models()...),
			QueryDialect:  queryDialect(),
			Planning: []*protocol.ModelPlanning{{
				Id: tableplan.ServiceID, ParentKinds: tableplan.ParentKinds(), OperationKinds: []string{tableop.Kind},
			}},
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
