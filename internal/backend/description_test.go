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

// The provider reads a table's foreign keys and CHECKs, plans their changes on
// a kept table, decides whether a declared CHECK is the one Spanner keeps, and
// states each index key's NULLS order on both sides: the four additions of
// contract 25.28.
func TestDescription_ReadsAndPlansConstraints(t *testing.T) {
	c := testcheck.New(t)
	description := backend.Description()
	engine := description.GetEngines()[0]
	for _, feature := range []string{provider.ObservedConstraintsFeature, provider.ConstraintChangesFeature, provider.CheckEquivalenceFeature,
		provider.IndexNullsOrderFeature} {
		c.Assert(engine.GetFeatures(), qt.Contains, feature)
		c.Assert(description.GetFeatures(), qt.Contains, feature)
	}
}

// The four additions of contract 25.29: schemas on the common-object wire,
// the TTL clause of a SQL CREATE TABLE, the reserved seed ledger and index
// predicates. A host that lists every feature of the provider reads the
// SCHEMA kind and the claim in the description.
func TestDescription_OffersTheAdditionsOfContract2529(t *testing.T) {
	c := testcheck.New(t)
	additions := []string{provider.CommonNamespacesFeature, provider.SQLTableClausesFeature, provider.ReservedSeedsFeature, provider.IndexPredicatesFeature}
	description := backend.Description()
	for _, feature := range additions {
		c.Assert(description.GetEngines()[0].GetFeatures(), qt.Contains, feature)
		c.Assert(description.GetFeatures(), qt.Contains, feature)
	}
	listed := provider.NegotiatedDescription(description, description.GetFeatures())
	c.Assert(provider.ValidateDescription(listed, nil), qt.IsNil)
	engine := listed.GetEngines()[0]
	c.Assert(engine.GetCommonObjects().GetKinds(), qt.Contains, protocol.CommonObjectSupport_SCHEMA)
	c.Assert(engine.GetSqlSchema().GetMode(), qt.Equals, protocol.SQLSchemaDeclaration_FRAGMENTS)
	c.Assert(engine.GetSqlSchema().GetTableClauses(), qt.HasLen, 1)
	c.Assert(engine.GetSqlSchema().GetTableClauses()[0].GetWords(), qt.DeepEquals, []string{"TTL"})
}

// A host of an earlier contract receives the description without the SCHEMA
// kind and without the TTL claim, and is planned as before them.
func TestDescription_LeavesTheAdditionsOfContract2529ToAHostThatListsThem(t *testing.T) {
	c := testcheck.New(t)
	unlisted := provider.NegotiatedDescription(backend.Description(), []string{provider.CommonObjectsFeature})
	c.Assert(provider.ValidateDescription(unlisted, nil), qt.IsNil)
	engine := unlisted.GetEngines()[0]
	for _, feature := range []string{provider.CommonNamespacesFeature, provider.SQLTableClausesFeature, provider.ReservedSeedsFeature, provider.IndexPredicatesFeature} {
		c.Assert(engine.GetFeatures(), qt.Not(qt.Contains), feature)
	}
	c.Assert(engine.GetCommonObjects().GetKinds(), qt.DeepEquals,
		[]protocol.CommonObjectSupport_Kind{protocol.CommonObjectSupport_VIEW, protocol.CommonObjectSupport_SEQUENCE})
	c.Assert(engine.GetSqlSchema().GetTableClauses(), qt.HasLen, 0)
}

// The engine declares change streams as a kind its reads leave out (contract
// 25.41), for a host that lists schema.not-described; a host that does not
// receives neither the feature nor the kind.
func TestDescription_DeclaresChangeStreamsNotDescribed(t *testing.T) {
	tests := []struct {
		name     string
		features []string
		kinds    []string
	}{
		{name: "a host that lists the feature", features: []string{provider.NotDescribedFeature}, kinds: []string{"change_stream"}},
		{name: "a host that does not", features: nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			negotiated := provider.NegotiatedDescription(backend.Description(), test.features)
			c.Assert(provider.ValidateDescription(negotiated, nil), qt.IsNil)
			c.Assert(negotiated.GetEngines()[0].GetNotDescribedKinds(), qt.DeepEquals, test.kinds)
		})
	}
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

// The provider empties a dev database the host names and judges what a
// replay runs on one; it provisions none, because a Spanner database is
// created through the instance's administration API.
func TestDescription_TheProviderCleansDevDatabases(t *testing.T) {
	c := testcheck.New(t)
	description := backend.Description()
	engine := description.GetEngines()[0]
	for _, feature := range []string{provider.DevCleanupFeature, provider.ReplayGuardFeature} {
		c.Assert(engine.GetFeatures(), qt.Contains, feature)
		c.Assert(description.GetFeatures(), qt.Contains, feature)
	}
	c.Assert(description.GetFeatures(), qt.Not(qt.Contains), provider.DevDatabaseFeature)
}

// The engine states how it builds an enum column, the form Ptah's built-in
// Spanner engine used: TEXT under a CHECK that lists the values. A host that
// does not list schema.enum-columns receives no form, and keeps the enum a
// type of its own.
func TestDescription_StatesTheEnumColumnForm(t *testing.T) {
	c := testcheck.New(t)
	description := backend.Description()
	c.Assert(description.GetFeatures(), qt.Contains, provider.EnumColumnsFeature)
	listed := provider.NegotiatedDescription(description, description.GetFeatures())
	c.Assert(provider.ValidateDescription(listed, nil), qt.IsNil)
	engine := listed.GetEngines()[0]
	c.Assert(engine.GetFeatures(), qt.Contains, provider.EnumColumnsFeature)
	c.Assert(engine.GetEnumColumns().GetForm(), qt.Equals, protocol.EnumColumns_CHECK)
	c.Assert(engine.GetEnumColumns().GetCheckType(), qt.Equals, "TEXT")

	unlisted := provider.NegotiatedDescription(description, nil)
	c.Assert(provider.ValidateDescription(unlisted, nil), qt.IsNil)
	c.Assert(unlisted.GetEngines()[0].GetEnumColumns(), qt.IsNil)
	c.Assert(unlisted.GetEngines()[0].GetFeatures(), qt.Not(qt.Contains), provider.EnumColumnsFeature)
}

// The engine states that Spanner ignores DROP VIEW IF EXISTS of a name a
// table holds, so the host reads a schema document as the server runs it. A
// host that does not list source.sql-statement-facts reads the fact as
// false, and refuses such a document, as it does for a server that refuses
// the statement.
func TestDescription_StatesTheStatementFactsOfSpanner(t *testing.T) {
	c := testcheck.New(t)
	description := backend.Description()
	c.Assert(description.GetFeatures(), qt.Contains, provider.SQLStatementFactsFeature)
	listed := provider.NegotiatedDescription(description, description.GetFeatures())
	c.Assert(provider.ValidateDescription(listed, nil), qt.IsNil)
	engine := listed.GetEngines()[0]
	c.Assert(engine.GetFeatures(), qt.Contains, provider.SQLStatementFactsFeature)
	c.Assert(engine.GetSqlSchema().GetMode(), qt.Equals, protocol.SQLSchemaDeclaration_FRAGMENTS)
	c.Assert(engine.GetSqlSchema().GetDropViewIfExistsSkipsOtherKinds(), qt.IsTrue)

	unlisted := provider.NegotiatedDescription(description, nil)
	c.Assert(provider.ValidateDescription(unlisted, nil), qt.IsNil)
	c.Assert(unlisted.GetEngines()[0].GetFeatures(), qt.Not(qt.Contains), provider.SQLStatementFactsFeature)
	c.Assert(unlisted.GetEngines()[0].GetSqlSchema().GetDropViewIfExistsSkipsOtherKinds(), qt.IsFalse)
}
