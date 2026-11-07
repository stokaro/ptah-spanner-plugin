package backend_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"

	"github.com/stokaro/ptah-spanner-plugin/internal/backend"
	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// viewQueriesRequest asks about three views over table t: v_literal, whose
// stored query is Spanner's rendering of the declared one, v_case, a CASE the
// comparison does not read, and v_star, a SELECT * Spanner stores with t's
// columns named. With columns the request carries t's declared columns and
// target features list schema.view-query-context.
func viewQueriesRequest(features []string, columns []*protocol.ComparisonColumn) *protocol.CompareViewQueriesRequest {
	view := func(name, body string) *protocol.ObservedView {
		return &protocol.ObservedView{Identity: &protocol.Reference{Engine: "spanner", Kind: "view", Schema: "public", Name: name}, Body: body}
	}
	return &protocol.CompareViewQueriesRequest{Engine: "spanner", TargetFeatures: features, DeclaredColumns: columns, Pairs: []*protocol.ViewQueryPair{
		{Current: view("v_literal", "SELECT id FROM t WHERE (id > '0'::bigint)"), Declared: "SELECT id FROM t WHERE id > 0"},
		{Current: view("v_case", "SELECT CASE WHEN (id > '5'::bigint) THEN 'big'::text ELSE 'small'::text END AS size FROM t"),
			Declared: "SELECT CASE WHEN id > 5 THEN 'big' ELSE 'small' END AS size FROM t"},
		{Current: view("v_star", "SELECT id, name FROM t"), Declared: "SELECT * FROM t"},
	}}
}

// tColumns are the declared columns of table t.
func tColumns() []*protocol.ComparisonColumn {
	table := &protocol.ComparisonIdentity{Kind: "table", Database: &protocol.ComparisonPart{}, Catalog: &protocol.ComparisonPart{},
		Schema: &protocol.ComparisonPart{Source: "public", Normalized: "public"}, Name: &protocol.ComparisonPart{Source: "t", Normalized: "t"}}
	return []*protocol.ComparisonColumn{{Table: table, Name: "id", Type: "bigint"}, {Table: table, Name: "name", Type: "varchar(20)"}}
}

// A host that sends the declared columns has a rewritten query and a SELECT *
// of the held columns answered as the held query, and a query the provider
// does not read left to it. A host that does not send them takes no
// undecided answer: the rewritten query is the held one, and the CASE and the
// SELECT *, which the provider cannot read without the columns, are not.
func TestCompareViewQueries(t *testing.T) {
	for _, test := range []struct {
		name           string
		features       []string
		columns        []*protocol.ComparisonColumn
		wantEquivalent []bool
		wantUndecided  []bool
	}{
		{name: "a host that sends the declared columns", features: []string{provider.ViewQueryContextFeature}, columns: tColumns(),
			wantEquivalent: []bool{true, false, true}, wantUndecided: []bool{false, true, false}},
		{name: "a host that asks without them", features: make([]string, 0),
			wantEquivalent: []bool{true, false, false}, wantUndecided: []bool{false, false, false}},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)

			result, err := backend.New().CompareViewQueries(c.Context(), viewQueriesRequest(test.features, test.columns))

			c.Assert(err, qt.IsNil)
			c.Assert(result.GetComplete(), qt.IsTrue)
			equivalent, undecided := make([]bool, 0, len(result.GetQueries())), make([]bool, 0, len(result.GetQueries()))
			for _, answer := range result.GetQueries() {
				equivalent = append(equivalent, answer.GetEquivalent())
				undecided = append(undecided, answer.GetUndecided())
			}
			c.Assert(equivalent, qt.DeepEquals, test.wantEquivalent)
			c.Assert(undecided, qt.DeepEquals, test.wantUndecided)
		})
	}
}
