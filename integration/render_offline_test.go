//go:build integration

package integration_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"

	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// Rendering and SQL analysis connect to nothing: the subprocess answers them
// without a session, and what it renders analyzes as common DDL.
func TestRenderAndAnalysisOfflineSubprocess(t *testing.T) {
	c := testcheck.New(t)
	client := startProvider(c)
	table := &protocol.Reference{Engine: "spanner", Database: "app", Schema: "public", Kind: "table", Name: "items"}
	rendered, err := client.Render(c.Context(), &protocol.RenderRequest{Engine: "spanner", Changes: []*protocol.Change{
		{Id: "1", Object: table, Operation: &protocol.Change_CreateTable{CreateTable: &protocol.Table{Columns: []*protocol.Column{
			{Name: "id", Type: "BIGINT", PrimaryKey: true}, {Name: "name", Type: "TEXT", Nullable: true},
		}}}},
		{Id: "2", Object: table, Operation: &protocol.Change_DropColumn{DropColumn: "name"}},
	}})
	c.Assert(err, qt.IsNil)
	c.Assert(rendered.GetUnits(), qt.HasLen, 2)
	source := rendered.GetUnits()[0].GetSql() + "\n" + rendered.GetUnits()[1].GetSql()
	request := &protocol.AnalyzeSQLRequest{Engine: "spanner", Sources: []string{source}}
	analysis, err := client.AnalyzeSQL(c.Context(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(provider.ValidateSQLAnalysis(request, analysis), qt.IsNil)
	c.Assert(analysis.GetFiles()[0].GetStatements(), qt.HasLen, 2)

	refused, err := client.Render(c.Context(), &protocol.RenderRequest{Engine: "spanner", Changes: []*protocol.Change{
		{Id: "1", Object: table, Operation: &protocol.Change_AddColumn{AddColumn: &protocol.Column{Name: "n", Type: "BIGINT"}}},
	}})
	c.Assert(err, qt.ErrorMatches, `.*\(InvalidArgument\): column "n" of table "items" is NOT NULL without a default, which Spanner refuses to add to an existing table`)
	c.Assert(refused, qt.IsNil)
}
