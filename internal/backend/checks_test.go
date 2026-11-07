package backend_test

import (
	"context"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"

	"github.com/stokaro/ptah-spanner-plugin/internal/backend"
	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// checkTable is a held table k with the CHECKs Spanner keeps for
// `score >= 0`, `status IN ('active', 'retired')` and `score >= 1`.
func checkTable() *protocol.CheckComparisonTable {
	return &protocol.CheckComparisonTable{
		Subject: &protocol.ComparisonIdentity{Kind: "table", Database: &protocol.ComparisonPart{}, Catalog: &protocol.ComparisonPart{},
			Schema: &protocol.ComparisonPart{Source: "public", Normalized: "public"}, Name: &protocol.ComparisonPart{Source: "k", Normalized: "k"}},
		Current: &protocol.ObservedTable{Name: "k", Schema: "public", Columns: []*protocol.ObservedColumn{
			{Name: "id", DataType: "bigint", ColumnType: "bigint", IsNullable: "NO"},
			{Name: "score", DataType: "bigint", ColumnType: "bigint", IsNullable: "NO"},
			{Name: "status", DataType: "character varying", ColumnType: "character varying", IsNullable: "YES"},
		}, Constraints: []*protocol.ObservedConstraint{
			{Name: "k_score_check", TableName: "k", Schema: "public", Type: "CHECK", CheckClause: new("(score >= '0'::bigint)")},
			{Name: "k_status_check", TableName: "k", Schema: "public", Type: "CHECK", CheckClause: new("(status = ANY (ARRAY['active'::text, 'retired'::text]))")},
			{Name: "k_min", TableName: "k", Schema: "public", Type: "CHECK", CheckClause: new("(score >= '1'::bigint)")},
		}},
	}
}

// A declared CHECK is the held one when the clause Spanner keeps is its
// stored form, and not when it says something else. Ptah's built-in Spanner
// engine called `score >= 0` and `(score >= '0'::bigint)` different and
// replaced the CHECK on every run (stokaro/ptah#4204 S1).
func TestCompareChecks(t *testing.T) {
	c := testcheck.New(t)
	table := checkTable()
	table.Pairs = []*protocol.CheckComparisonPair{
		{Constraint: "k_score_check", Declared: "score >= 0"},
		{Constraint: "k_status_check", Declared: "status IN ('active', 'retired')"},
		{Constraint: "k_min", Declared: "score >= 2"},
	}
	request := &protocol.CompareChecksRequest{Engine: "spanner", TargetFeatures: backend.Description().GetEngines()[0].GetFeatures(),
		Identifiers: &protocol.ComparisonIdentifiers{DefaultSchema: "public", Rules: &protocol.IdentifierRules{TableNames: protocol.IdentifierRules_EXACT,
			ColumnNames: protocol.IdentifierRules_EXACT, IndexNames: protocol.IdentifierRules_EXACT, IndexNamespace: protocol.IdentifierRules_SCHEMA}},
		Tables: []*protocol.CheckComparisonTable{table}}
	c.Assert(provider.ValidateCompareChecksRequest(backend.Description(), request), qt.IsNil)
	result, err := backend.New().CompareChecks(context.Background(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(provider.ValidateComparedChecksReply(backend.Description(), request, result), qt.IsNil)
	var answers []bool
	for _, check := range result.GetTables()[0].GetChecks() {
		answers = append(answers, check.GetEquivalent())
	}
	c.Assert(answers, qt.DeepEquals, []bool{true, true, false})
}
