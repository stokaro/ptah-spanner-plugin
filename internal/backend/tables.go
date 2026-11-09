package backend

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"
	"ptah.run/provider-sdk/runtime"

	"github.com/stokaro/ptah-spanner-plugin/internal/coltype"
	"github.com/stokaro/ptah-spanner-plugin/internal/ddl"
	"github.com/stokaro/ptah-spanner-plugin/internal/indexfilter"
	"github.com/stokaro/ptah-spanner-plugin/internal/tableplan"
)

// PrepareTables states which declared columns are key columns: the table's
// key list, then each column marked primary, in key order. Where the host
// negotiated the index NULLS order, it also states the NULLS order of each
// key part of each declared index, and where it negotiated index predicates,
// each declared index's WHERE clause, each in the form a read states a held
// one. It reads no database and changes nothing else of a declaration.
func (*Provider) PrepareTables(ctx context.Context, request *protocol.PrepareTablesRequest) (*protocol.PreparedTables, error) {
	result := &protocol.PreparedTables{Complete: true}
	states := indexStatements{nulls: slices.Contains(request.GetTargetFeatures(), provider.IndexNullsOrderFeature),
		predicates: slices.Contains(request.GetTargetFeatures(), provider.IndexPredicatesFeature)}
	for _, table := range request.GetTables() {
		if err := ctx.Err(); err != nil {
			return nil, status.FromContextError(err).Err()
		}
		prepared := &protocol.PreparedTable{Subject: proto.CloneOf(table.GetSubject()), ColumnPrimaryKeysPrepared: true,
			ColumnPrimaryKeys: tableplan.PrimaryKey(table.GetDesired())}
		if states.nulls || states.predicates {
			prepared.Indexes = states.prepare(table.GetDesired())
		}
		result.Tables = append(result.Tables, prepared)
	}
	return result, nil
}

// indexStatements are what table preparation states of each declared index,
// as the host negotiated them: the NULLS orders, the WHERE clause, or both.
type indexStatements struct {
	nulls, predicates bool
}

// prepare states, for each index a table declares in declaration order, what
// the host negotiated:
//
//   - with nulls, the NULLS order of each key part: FIRST or LAST where it
//     differs from Spanner's default for the part's direction, and empty where
//     it is the default; see ddl.IndexKey.StatedNulls. A key listed as fields
//     is in the default order. Ptah's built-in Spanner engine compared the
//     orders by the same rule.
//   - with predicates, the WHERE clause in the spelling Spanner keeps; see
//     preparedCondition.
func (s indexStatements) prepare(table *protocol.DeclaredTable) []*protocol.PreparedIndex {
	indexes := make([]*protocol.PreparedIndex, 0, len(table.GetIndexes()))
	for _, index := range table.GetIndexes() {
		prepared := &protocol.PreparedIndex{Name: index.GetName()}
		if s.nulls && len(index.GetParts()) == 0 {
			prepared.Nulls = make([]string, len(index.GetFields()))
		}
		for _, part := range index.GetParts() {
			if s.nulls {
				prepared.Nulls = append(prepared.Nulls, ddl.IndexKey{Descending: part.GetDescending(), Nulls: part.GetNulls()}.StatedNulls())
			}
		}
		if s.predicates {
			prepared.Condition = preparedCondition(index.GetCondition())
		}
		indexes = append(indexes, prepared)
	}
	return indexes
}

// preparedCondition is a declared WHERE clause in the spelling Spanner keeps
// it in (indexfilter.Spelling). A clause Spanner creates no index with is
// stated as written: schema validation refuses it before a plan is made, and
// the host needs a clause stated exactly when the declaration has one.
func preparedCondition(condition string) string {
	if strings.TrimSpace(condition) == "" {
		return ""
	}
	columns, err := indexfilter.Parse(condition)
	if err != nil {
		return strings.TrimSpace(condition)
	}
	return indexfilter.Spelling(columns)
}

// ProjectTableCreations predicts what a read reports for each declared table
// once CREATE TABLE has run: every column in order, with the type Spanner
// stores, its nullability and its default in the stored spelling. It reads no
// database. A type Spanner refuses fails the batch with the reason.
func (*Provider) ProjectTableCreations(ctx context.Context, request *protocol.ProjectTableCreationsRequest) (*protocol.ProjectedTableCreations, error) {
	result := &protocol.ProjectedTableCreations{Complete: true}
	for _, input := range request.GetTables() {
		if err := ctx.Err(); err != nil {
			return nil, status.FromContextError(err).Err()
		}
		table := input.GetDeclaration()
		keys := tableplan.PrimaryKey(table)
		output := &protocol.ProjectedTableCreation{Subject: proto.CloneOf(input.GetSubject()), ColumnPrimaryKeysPrepared: true, ColumnPrimaryKeys: keys}
		for i, column := range table.GetColumns() {
			canonical, err := coltype.Canonical(tableplan.ColumnType(table, column))
			if err != nil {
				return nil, runtime.ErrorWithReason(codes.InvalidArgument, fmt.Sprintf("column %q of table %q: %v", column.GetName(), table.GetName(), err))
			}
			nullable := "NO"
			if tableplan.EffectiveNullable(table, column) {
				nullable = "YES"
			}
			output.Columns = append(output.Columns, &protocol.ObservedColumn{
				Name: column.GetName(), DataType: coltype.DataType(canonical), FormattedType: canonical, ColumnType: canonical, IsNullable: nullable,
				PrimaryKey: slices.Contains(keys, column.GetName()), OrdinalPosition: int64(i + 1), DefaultExpression: storedDefault(column, canonical),
			})
		}
		result.Tables = append(result.Tables, output)
	}
	return result, nil
}

// storedDefault predicts a default in the spelling Spanner stores: a literal
// as a quoted value cast to the column's type, which coltype.ReadDefault reads
// back as the same literal, and an expression as written.
func storedDefault(column *protocol.DeclaredColumn, canonical string) *string {
	switch kind, value := tableplan.DeclaredDefault(column); kind {
	case coltype.Literal:
		return new("'" + strings.ReplaceAll(value, "'", "''") + "'::" + canonical)
	case coltype.Expression:
		return new(value)
	default:
		return nil
	}
}
