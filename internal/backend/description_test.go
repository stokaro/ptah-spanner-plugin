package backend_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"

	"github.com/stokaro/ptah-spanner-plugin/internal/backend"
	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

func TestDescription_Validates(t *testing.T) {
	c := testcheck.New(t)
	c.Assert(provider.ValidateDescription(backend.Description(), []string{
		provider.InspectFeature, provider.RenderFeature, provider.ExecuteFeature, provider.AnalyzeSQLFeature,
	}), qt.IsNil)
}

func TestDescription_DeclaresTheEngine(t *testing.T) {
	c := testcheck.New(t)
	description := backend.Description()
	c.Assert(description.GetProviderId(), qt.Equals, "github.com/stokaro/ptah-spanner-plugin")
	c.Assert(description.GetEngines(), qt.HasLen, 1)
	engine := description.GetEngines()[0]
	c.Assert(engine.GetId(), qt.Equals, "spanner")
	c.Assert(engine.GetAliases(), qt.DeepEquals, []string{"cloudspanner", "google-spanner", "google_spanner"})
	c.Assert(engine.GetDefaultSchema(), qt.Equals, "public")
}

// Names resolve by their exact spelling on the PostgreSQL interface, measured
// on the emulator: after CREATE TABLE "Abc", a statement naming "abc" answers
// `Table not found: abc`.
func TestDescription_NamesCompareExactly(t *testing.T) {
	c := testcheck.New(t)
	rules := backend.Description().GetEngines()[0].GetIdentifiers()
	c.Assert(rules.GetTableNames(), qt.Equals, protocol.IdentifierRules_EXACT)
	c.Assert(rules.GetColumnNames(), qt.Equals, protocol.IdentifierRules_EXACT)
	c.Assert(rules.GetIndexNames(), qt.Equals, protocol.IdentifierRules_EXACT)
	c.Assert(rules.GetIndexNamespace(), qt.Equals, protocol.IdentifierRules_SCHEMA)
}

// The provider owns the lifecycle of the engine's tables: one planning
// service with parent kinds plans every table statement, so the host plans no
// common table, and a changed primary key reaches it to be refused by name.
func TestDescription_TheProviderOwnsItsTables(t *testing.T) {
	c := testcheck.New(t)
	description := backend.Description()
	engine := description.GetEngines()[0]
	c.Assert(engine.GetFeatures(), qt.Not(qt.Contains), provider.CommonTableIdentityFeature)
	for _, feature := range []string{provider.PlanFeaturesFeature, provider.ValidateSchemaFeature, provider.PrepareTablesFeature, provider.ProjectTableCreationsFeature,
		provider.ColumnsFeature, provider.PlanningLifecycleFeature, provider.PrimaryKeyChangesFeature, provider.PlanningStatementStyleFeature} {
		c.Assert(engine.GetFeatures(), qt.Contains, feature)
		c.Assert(description.GetFeatures(), qt.Contains, feature)
	}
	c.Assert(engine.GetPlanning(), qt.HasLen, 1)
	c.Assert(engine.GetPlanning()[0].GetParentKinds(), qt.DeepEquals, []string{"github.com/stokaro/ptah-spanner-plugin/row-deletion-policy"})
	c.Assert(engine.GetPlanning()[0].GetOperationKinds(), qt.DeepEquals, []string{"github.com/stokaro/ptah-spanner-plugin/table-operation"})
}

// Migrations keep their records through the provider: revision history, the
// log and tags, transactions for the records, and a split of each body into
// the statements Spanner runs one by one.
func TestDescription_MigrationsKeepTheirRecords(t *testing.T) {
	c := testcheck.New(t)
	description := backend.Description()
	engine := description.GetEngines()[0]
	for _, feature := range []string{provider.TransactionsFeature, provider.HistoryFeature, provider.MetadataFeature, provider.MigrationSplitFeature} {
		c.Assert(engine.GetFeatures(), qt.Contains, feature)
		c.Assert(description.GetFeatures(), qt.Contains, feature)
	}
}

// The statements an operator writes, such as a migration's check assertions,
// run through the provider.
func TestDescription_RunsOperatorStatements(t *testing.T) {
	c := testcheck.New(t)
	description := backend.Description()
	c.Assert(description.GetEngines()[0].GetFeatures(), qt.Contains, provider.StatementsFeature)
	c.Assert(description.GetFeatures(), qt.Contains, provider.StatementsFeature)
}

// The only line runs on the emulator in CI and stays best effort, because
// the emulator is not the managed service.
func TestDescription_TheLineIsBestEffortOnTheEmulator(t *testing.T) {
	c := testcheck.New(t)
	lines := backend.Description().GetEngines()[0].GetReleaseLines()
	c.Assert(lines, qt.HasLen, 1)
	c.Assert(lines[0].GetLine(), qt.Equals, "0")
	c.Assert(lines[0].GetSupport(), qt.Equals, protocol.ReleaseLine_BEST_EFFORT)
	c.Assert(lines[0].GetImage(), qt.Equals, backend.EmulatorImage)
	c.Assert(lines[0].GetUnprobed(), qt.Matches, `The managed service is not tested.*`)
}

func TestServer_Validates(t *testing.T) {
	c := testcheck.New(t)
	c.Assert(backend.Server().Validate(), qt.IsNil)
}

// Every feature the provider lists has a service, and a description that
// offers a service the server lacks is refused.
func TestServer_RefusesAServiceTheServerLacks(t *testing.T) {
	c := testcheck.New(t)
	server := backend.Server()
	server.Description.Features = append(server.Description.Features, provider.EmbeddingStoreFeature)
	c.Assert(server.Validate(), qt.ErrorIs, provider.ErrProtocol)
}
