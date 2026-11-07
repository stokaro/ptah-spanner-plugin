package backend

import (
	"context"
	"fmt"
	"slices"
	"strings"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-spanner-plugin/internal/coltype"
	"github.com/stokaro/ptah-spanner-plugin/internal/tableplan"
)

// PrepareTables states which declared columns are key columns: the table's
// key list, then each column marked primary, in key order. It reads no
// database and changes nothing else of a declaration.
func (*Provider) PrepareTables(ctx context.Context, request *protocol.PrepareTablesRequest) (*protocol.PreparedTables, error) {
	result := &protocol.PreparedTables{Complete: true}
	for _, table := range request.GetTables() {
		if err := ctx.Err(); err != nil {
			return nil, status.FromContextError(err).Err()
		}
		result.Tables = append(result.Tables, &protocol.PreparedTable{Subject: proto.CloneOf(table.GetSubject()), ColumnPrimaryKeysPrepared: true,
			ColumnPrimaryKeys: tableplan.PrimaryKey(table.GetDesired())})
	}
	return result, nil
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
