package backend

import (
	"context"
	"slices"

	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-spanner-plugin/internal/commonobject"
)

// CompareViewQueries answers whether each declared view's query and the query
// Spanner keeps for the view are one query, by commonobject.SameViewQuery,
// typed by the columns the host sends for every declared table
// (schema.view-query-context). Spanner stores its own rendering of a query,
// with casts, parentheses and qualifiers the declaration does not write, so
// the host's comparison of the two texts replaces such a view on every plan
// (S91). A pair SameViewQuery cannot read, or reads as two queries, is left
// to the host's own reading of the texts, which folds case, quotes and
// spacing and expands a `*` itself: an answer of this provider never calls a
// view the host's reading keeps a different query.
//
// A host of contracts 25.33 to 25.36 asks without the columns and takes no
// undecided answer. Such a pair is read with no declared columns, and is one
// query also when the provider's spelling of the declared query
// (commonobject.ViewQuery) is the stored one; any other pair is answered as
// two queries. A `SELECT *` is then replaced on every plan, where the host's
// reading expands it with the declared columns: answering it equivalent
// without them would hide a column the table gained.
func (*Provider) CompareViewQueries(ctx context.Context, request *protocol.CompareViewQueriesRequest) (*protocol.ComparedViewQueries, error) {
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	withColumns := slices.Contains(request.GetTargetFeatures(), provider.ViewQueryContextFeature)
	var tables map[string][]commonobject.Column
	if withColumns {
		tables = declaredTables(request.GetDeclaredColumns())
	}
	result := &protocol.ComparedViewQueries{Complete: true}
	for _, pair := range request.GetPairs() {
		declared, held := pair.GetDeclared(), pair.GetCurrent().GetBody()
		answer := &protocol.ComparedViewQuery{Identity: proto.CloneOf(pair.GetCurrent().GetIdentity())}
		answer.Equivalent = commonobject.SameViewQuery(declared, held, tables) || !withColumns && commonobject.ViewQuery(declared) == held
		answer.Undecided = withColumns && !answer.GetEquivalent()
		result.Queries = append(result.Queries, answer)
	}
	return result, nil
}

// declaredTables groups the declared columns by their table's name, in the
// order the declaration lists them.
func declaredTables(columns []*protocol.ComparisonColumn) map[string][]commonobject.Column {
	tables := make(map[string][]commonobject.Column)
	for _, column := range columns {
		name := column.GetTable().GetName().GetNormalized()
		tables[name] = append(tables[name], commonobject.Column{Name: column.GetName(), Type: column.GetType()})
	}
	return tables
}
