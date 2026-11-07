package backend

import (
	"context"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-spanner-plugin/internal/coltype"
	"github.com/stokaro/ptah-spanner-plugin/internal/tableplan"
)

// CompareColumns judges each requested aspect of each column pair in
// Spanner's spelling: a declared `TEXT` and a held `character varying` are one
// type, and a declared `DEFAULT 0` and a held `'0'::bigint` one default.
func (*Provider) CompareColumns(ctx context.Context, request *protocol.CompareColumnsRequest) (*protocol.ComparedColumns, error) {
	result := &protocol.ComparedColumns{Complete: true}
	for _, table := range request.GetTables() {
		if err := ctx.Err(); err != nil {
			return nil, status.FromContextError(err).Err()
		}
		output := &protocol.ComparedColumnTable{Subject: proto.CloneOf(table.GetSubject()), CurrentSubject: proto.CloneOf(table.GetCurrentSubject())}
		for _, pair := range table.GetPairs() {
			desired := declaredColumn(table.GetDesired(), pair.GetDesiredColumn())
			current := observedColumn(table.GetCurrent(), pair.GetCurrentColumn())
			if desired == nil || current == nil {
				return nil, runtime.ErrorWithReason(codes.InvalidArgument, "a column pair names a column its table does not hold")
			}
			column := &protocol.ComparedColumn{Subject: proto.CloneOf(pair.GetSubject()), CurrentSubject: proto.CloneOf(pair.GetCurrentSubject())}
			for _, aspect := range pair.GetAspects() {
				receipt, err := compareAspect(aspect, table.GetDesired(), desired, current)
				if err != nil {
					return nil, err
				}
				column.Aspects = append(column.Aspects, receipt)
			}
			output.Columns = append(output.Columns, column)
		}
		result.Tables = append(result.Tables, output)
	}
	return result, nil
}

func compareAspect(aspect protocol.ColumnAspect, table *protocol.DeclaredTable, desired *protocol.DeclaredColumn, current *protocol.ObservedColumn) (*protocol.ColumnAspectReceipt, error) {
	receipt := &protocol.ColumnAspectReceipt{Aspect: aspect}
	switch aspect {
	case protocol.ColumnAspect_COLUMN_TYPE:
		held := heldType(current)
		declared := tableplan.ColumnType(table, desired)
		relation := protocol.ColumnDifference_CHANGED
		if coltype.Same(declared, held) {
			relation = protocol.ColumnDifference_SAME
		}
		receipt.Receipt = &protocol.ColumnAspectReceipt_Difference{Difference: &protocol.ColumnDifference{Relation: relation, Current: held, Desired: declared}}
	case protocol.ColumnAspect_COLUMN_DEFAULT:
		_, held := coltype.ReadDefault(current.DefaultExpression)
		_, declared := tableplan.DeclaredDefault(desired)
		relation := protocol.ColumnDifference_CHANGED
		if tableplan.SameDefault(table, desired, current) {
			relation = protocol.ColumnDifference_SAME
		}
		receipt.Receipt = &protocol.ColumnAspectReceipt_Difference{Difference: &protocol.ColumnDifference{Relation: relation, Current: held, Desired: declared}}
	case protocol.ColumnAspect_COLUMN_NULLABILITY:
		receipt.Receipt = &protocol.ColumnAspectReceipt_Nullability{Nullability: &protocol.ColumnNullability{Nullable: new(tableplan.EffectiveNullable(table, desired))}}
	case protocol.ColumnAspect_COLUMN_SEQUENCE:
		// The provider describes no identity column, so no sequence setting
		// differs.
		receipt.Receipt = &protocol.ColumnAspectReceipt_Sequence{Sequence: &protocol.ColumnSequenceDifferences{}}
	default:
		return nil, runtime.ErrorWithReason(codes.InvalidArgument, "the provider compares the type, default, nullability and sequence of a column only")
	}
	return receipt, nil
}

// ReadColumnDefaults reads each stored default as a literal or an
// expression: Spanner stores `DEFAULT 0` as `'0'::bigint`, which is the
// literal 0.
func (*Provider) ReadColumnDefaults(ctx context.Context, request *protocol.ReadColumnDefaultsRequest) (*protocol.ReadColumnDefaultsResult, error) {
	result := &protocol.ReadColumnDefaultsResult{Complete: true}
	for _, input := range request.GetColumns() {
		if err := ctx.Err(); err != nil {
			return nil, status.FromContextError(err).Err()
		}
		kind, value := coltype.ReadDefault(input.GetColumn().DefaultExpression)
		result.Columns = append(result.Columns, &protocol.ColumnDefaultReceipt{Subject: proto.CloneOf(input.GetSubject()), Kind: defaultReceiptKind(kind), Value: value})
	}
	return result, nil
}

func defaultReceiptKind(kind coltype.DefaultKind) protocol.ColumnDefaultReceipt_Kind {
	switch kind {
	case coltype.Literal:
		return protocol.ColumnDefaultReceipt_LITERAL
	case coltype.Expression:
		return protocol.ColumnDefaultReceipt_EXPRESSION
	default:
		return protocol.ColumnDefaultReceipt_ABSENT
	}
}

// heldType is the full type a read reports for a column.
func heldType(column *protocol.ObservedColumn) string {
	if column.GetColumnType() != "" {
		return column.GetColumnType()
	}
	return column.GetDataType()
}

func declaredColumn(table *protocol.DeclaredTable, name string) *protocol.DeclaredColumn {
	for _, column := range table.GetColumns() {
		if column.GetName() == name {
			return column
		}
	}
	return nil
}

func observedColumn(table *protocol.ObservedTable, name string) *protocol.ObservedColumn {
	for _, column := range table.GetColumns() {
		if column.GetName() == name {
			return column
		}
	}
	return nil
}
