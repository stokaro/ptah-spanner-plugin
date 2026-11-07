// Package backend serves Cloud Spanner's PostgreSQL interface to Ptah through
// the experimental provider protocol. It opens sessions through PGAdapter,
// reads tables, columns, primary keys, secondary indexes, foreign keys, CHECKs
// and row deletion policies from information_schema, owns the lifecycle of the
// engine's tables (it checks a declaration, predicts what a creation holds,
// judges column types, defaults and CHECKs in Spanner's spelling and plans
// every table statement), and renders and runs the statements it plans. It
// reads and writes a table's row deletion policy in Go, YAML and HCL sources,
// reads it in a SQL document's TTL clause, and compares, reverses and counts
// it. It also types, reads and writes the rows a schema declares, runs seed
// files with their ledger, and carries schemas, views and sequences on the
// common-object wire.
//
// The provider describes what it reads completely or refuses the read: an
// object it does not describe yet, such as an interleaved table, fails the
// read with a reason instead of disappearing from it, because the host would
// plan to create what a read leaves out. A host that cannot take a table's
// foreign keys and CHECKs gets the same refusal for them.
package backend

import (
	"context"
	"sync"

	"github.com/jackc/pgx/v5"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"

	"github.com/stokaro/ptah-spanner-plugin/internal/buildversion"
	"github.com/stokaro/ptah-spanner-plugin/internal/commonobject"
	"github.com/stokaro/ptah-spanner-plugin/internal/migrationlint"
	"github.com/stokaro/ptah-spanner-plugin/internal/policysource"
	"github.com/stokaro/ptah-spanner-plugin/internal/rowdeletion"
	"github.com/stokaro/ptah-spanner-plugin/internal/tablefacets"
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
		// The query dialect states the lexical rules the host reads Spanner's
		// text with; see queryDialect.
		provider.QueryLexicalFeature,
		// A failed call carries a reason the provider wrote.
		provider.ErrorReasonsFeature,
		// A read reports each column whole, the primary key in key order and
		// the secondary indexes a table holds.
		provider.ObservedColumnsFeature,
		provider.ObservedPrimaryKeysFeature,
		provider.ObservedIndexesFeature,
		// A read reports each table's foreign keys and CHECKs, each CHECK as
		// Spanner keeps it, and each index key's NULLS order where it is not
		// the default for its direction, which table preparation states the
		// same way for a declared index; see catalog and
		// ddl.IndexKey.StatedNulls.
		provider.ObservedConstraintsFeature,
		provider.IndexNullsOrderFeature,
		// A read and table preparation state a filtered index's WHERE
		// clause in the one spelling Spanner keeps, so the host compares
		// the two exactly; see indexfilter.
		provider.IndexPredicatesFeature,
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
		// The engine states how it builds an enum column: TEXT under a
		// CHECK that lists the values, as Ptah's built-in Spanner engine
		// built it; see enumColumns.
		provider.EnumColumnsFeature,
		// A kept table's changed foreign keys and CHECKs reach the planning
		// service, which drops and adds them, and the host asks the provider
		// whether a declared CHECK and the one Spanner keeps are one; see
		// tableplan.SameCheck.
		provider.ConstraintChangesFeature,
		provider.CheckEquivalenceFeature,
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
		// The engine states the capability facts the host plans it with
		// when no session states the server's, and those of its release
		// line; see offlineCapabilities.
		provider.DefaultCapabilitiesFeature,
		// A table's row deletion policy: Go, YAML and HCL sources declare
		// it and exports write it (see policysource), the comparison and
		// its reversal read the interval by the hours it denotes, and
		// `ptah schema stats` counts the tables that have one (see
		// tablefacets).
		provider.DecodeSourceFeature,
		provider.EncodeSourceFeature,
		provider.ConvertValuesFeature,
		provider.HCLSourceFeature,
		provider.HCLPlatformFeature,
		// A SQL schema document declares the policy with Spanner's TTL
		// clause of CREATE TABLE, which the provider claims and reads; see
		// policysource.SQLClause.
		provider.SQLTableClausesFeature,
		provider.CompareModelsFeature,
		provider.ReverseChangesFeature,
		provider.ReportCountsFeature,
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
		// A read reports the seed ledger as reserved, so a plan never drops
		// it; see reservedPurpose.
		provider.ReservedSeedsFeature,
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
		// Views and sequences travel on the common-object wire: the host
		// compares and orders them, the provider reads, spells and renders
		// them; see commonobject.
		provider.CommonObjectsFeature,
		// Schemas travel on the same wire: a read lists them, the host plans
		// their creation and removal, and the provider renders both; see
		// commonobject.
		provider.CommonNamespacesFeature,
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
// provider, below the one the SDK it is built with speaks (25.31). The
// additions of contracts 25.28 and 25.29 that it uses are negotiated, and a
// host that lists none of them is served as before they existed:
//
//   - without observed constraints, a read refuses a table's foreign keys and
//     CHECKs, and comparisons and plans carry none;
//   - without common namespaces, a read lists no schema and the provider
//     creates the schema of a table it creates in one of its own;
//   - without SQL table clauses, a SQL document's TTL clause stays the host's
//     to refuse;
//   - without the reserved seed ledger, a read reports schema_seeds as a
//     table;
//   - without index predicates, a read and a declaration of a filtered index
//     are refused.
//
// The live tests run against such a host and against one that negotiates
// them. An older host is refused before a session opens.
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
			EnumColumns:   enumColumns(),
			ReleaseLines:  releaseLines(),
			Features:      engineFeatures(),
			LintRules:     migrationlint.Rules(),
			ValueModels:   append([]*protocol.ValueModel{tableop.Model()}, rowdeletion.Models()...),
			QueryDialect:  queryDialect(),
			// The facts an offline command plans the engine with.
			DefaultCapabilities: offlineCapabilities(),
			Planning: []*protocol.ModelPlanning{{
				Id: tableplan.ServiceID, ParentKinds: tableplan.ParentKinds(), ChangeKinds: tableplan.ChangeKinds(), OperationKinds: []string{tableop.Kind},
			}},
			Comparisons:        []*protocol.ModelComparison{tablefacets.Comparison()},
			Reversals:          []*protocol.ModelReversal{tablefacets.Reversal()},
			Reports:            tablefacets.Reports(),
			YamlSourceKeys:     policysource.YAMLKeys(),
			GoSourceDirectives: policysource.GoDirectives(),
			HclSource:          &protocol.HCLSourceDeclaration{Blocks: []*protocol.HCLSourceBlock{policysource.HCLPlatformBlock()}},
			SqlSchema: &protocol.SQLSchemaDeclaration{Mode: protocol.SQLSchemaDeclaration_FRAGMENTS,
				TableClauses: []*protocol.SQLTableClause{policysource.SQLClause()}},
			CommonObjects: commonobject.Support(),
		}},
	}
}

// enumColumns states how the engine builds a column a source declares as an
// enum (schema.enum-columns, contract 25.31). Spanner's PostgreSQL interface
// has no enum type: the emulator answers CREATE TYPE ... AS ENUM with
// `Statement is not supported`. Ptah's built-in Spanner engine built the
// column as TEXT under a CHECK that lists the values, named
// `<table>_<column>_check`, and the host builds the same column for a host
// that negotiates the feature, so the provider receives an ordinary column
// with its CHECK. A host that does not keeps the enum a type of its own and
// refuses it, as it does for every engine of contract 25.30.
func enumColumns() *protocol.EnumColumns {
	return &protocol.EnumColumns{Form: protocol.EnumColumns_CHECK, CheckType: "TEXT"}
}

// negotiatedFeatures is the engine's feature list as the host holds it: the
// two-way features the host did not list are left out, as the SDK leaves them
// out of the description the host reads. A session and an inspection snapshot
// report it, so the host can check them against that description.
func negotiatedFeatures(ctx context.Context) []string {
	return provider.NegotiatedDescription(Description(), provider.HostFeatures(ctx)).GetEngines()[0].GetFeatures()
}
