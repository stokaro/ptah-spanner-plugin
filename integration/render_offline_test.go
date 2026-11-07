//go:build integration

package integration_test

import (
	"fmt"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"

	"github.com/stokaro/ptah-spanner-plugin/internal/tableop"
	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// Rendering and SQL analysis connect to nothing: the subprocess answers them
// without a session, and what it renders analyzes as common DDL.
func TestRenderAndAnalysisOfflineSubprocess(t *testing.T) {
	c := testcheck.New(t)
	client := startProvider(c)
	create, err := tableop.Capture("1", tableop.Owner("public", "items"), tableop.Operation{Action: tableop.CreateTable, PrimaryKey: []string{"id"},
		Columns: []tableop.Column{{Name: "id", Type: "BIGINT"}, {Name: "name", Type: "TEXT", Nullable: true}}})
	c.Assert(err, qt.IsNil)
	index, err := tableop.Capture("2", tableop.Owner("public", "items"), tableop.Operation{Action: tableop.CreateIndex, Index: &tableop.Index{Name: "items_name",
		Keys: []tableop.IndexKey{{Column: "name", Descending: true}}}})
	c.Assert(err, qt.IsNil)
	// The change names the operation's owner, as the host names it.
	table := tableop.Owner("public", "items")
	rendered, err := client.Render(c.Context(), &protocol.RenderRequest{Engine: "spanner", Changes: []*protocol.Change{
		{Id: "1", Object: table, Operation: &protocol.Change_Native{Native: &protocol.NativeOperation{Value: create, Role: protocol.NativeOperation_STATEMENT}}},
		{Id: "2", Object: table, Operation: &protocol.Change_Native{Native: &protocol.NativeOperation{Value: index, Role: protocol.NativeOperation_STATEMENT}}},
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
		{Id: "1", Object: table, Operation: &protocol.Change_DropTable{DropTable: &protocol.Empty{}}},
	}})
	c.Assert(err, qt.ErrorMatches, `.*\(InvalidArgument\): the provider renders the table operations it plans, views and sequences, and nothing else`)
	c.Assert(refused, qt.IsNil)
}

// The subprocess tells a host that lists render.omissions what Spanner leaves
// out of a declared column type, through the SDK's client, which checks the
// reply against the description it negotiated: a column declared integer is
// stored as bigint, without the 32-bit range. A host that does not list the
// feature gets the same statement and no omission.
func TestRenderReportsWhatSpannerLeavesOutOfflineSubprocess(t *testing.T) {
	tests := []struct {
		name     string
		features []string
		want     []string
	}{
		{name: "a host that lists render omissions", features: append(hostFeatures(), provider.RenderOmissionsFeature),
			want: []string{`unsupported column items.age type: integer: the 32-bit range; Spanner stores bigint ` +
				`(a CHECK ("age" BETWEEN -2147483648 AND 2147483647) keeps the range)`}},
		{name: "a host that does not", features: hostFeatures()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			client := startProviderWith(c, test.features...)
			create, err := tableop.Capture("1", tableop.Owner("public", "items"), tableop.Operation{Action: tableop.CreateTable, PrimaryKey: []string{"id"},
				Columns: []tableop.Column{{Name: "id", Type: "bigint"}, {Name: "age", Type: "integer", Nullable: true}}})
			c.Assert(err, qt.IsNil)
			rendered, err := client.Render(c.Context(), &protocol.RenderRequest{Engine: "spanner", TargetFeatures: client.Description().GetEngines()[0].GetFeatures(),
				Changes: []*protocol.Change{{Id: "1", Object: tableop.Owner("public", "items"),
					Operation: &protocol.Change_Native{Native: &protocol.NativeOperation{Value: create, Role: protocol.NativeOperation_STATEMENT}}}}})
			c.Assert(err, qt.IsNil)
			c.Assert(rendered.GetUnits(), qt.HasLen, 1)
			c.Assert(rendered.GetUnits()[0].GetSql(), qt.Equals, "CREATE TABLE \"items\" (\n  \"id\" bigint PRIMARY KEY NOT NULL,\n  \"age\" integer\n);")
			var omissions []string
			for _, o := range rendered.GetOmissions() {
				omissions = append(omissions, fmt.Sprintf("%s %s %s %s: %s (%s)", o.GetReason(), o.GetKind(), o.GetName(), o.GetProperty(), o.GetDetail(), o.GetRemedy()))
			}
			c.Assert(omissions, qt.DeepEquals, test.want)
		})
	}
}
