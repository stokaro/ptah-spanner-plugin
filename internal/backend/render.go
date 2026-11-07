package backend

import (
	"context"
	"errors"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/stokaro/ptah-spanner-plugin/internal/ddl"
)

// Render writes one statement for each change the host planned, in the order
// the changes arrive. It connects to nothing. A change it cannot write refuses
// the whole batch, so the host never receives part of a plan.
func (*Provider) Render(ctx context.Context, request *protocol.RenderRequest) (*protocol.RenderResult, error) {
	result := &protocol.RenderResult{Complete: true}
	for _, change := range request.GetChanges() {
		if err := ctx.Err(); err != nil {
			return nil, status.FromContextError(err).Err()
		}
		statement, err := renderChange(change)
		if err != nil {
			return nil, runtime.ErrorWithReason(codes.InvalidArgument, err.Error())
		}
		result.Units = append(result.Units, &protocol.Unit{ChangeId: change.GetId(), Body: &protocol.Unit_Sql{Sql: statement}})
	}
	return result, nil
}

// renderChange writes the statement of one change.
func renderChange(change *protocol.Change) (string, error) {
	object := change.GetObject()
	if object.GetName() == "" || object.GetCatalog() != "" || len(object.GetPath()) != 0 {
		return "", refusal("a Spanner table has a schema and a name and nothing else")
	}
	schema, table := object.GetSchema(), object.GetName()
	switch operation := change.GetOperation().(type) {
	case *protocol.Change_CreateTable:
		return createTable(schema, table, operation.CreateTable)
	case *protocol.Change_DropTable:
		return ddl.DropTable(schema, table), nil
	case *protocol.Change_AddColumn:
		if operation.AddColumn.GetPrimaryKey() {
			return "", refusal("Spanner cannot add a primary key column to an existing table")
		}
		if len(operation.AddColumn.GetFacets()) != 0 {
			return "", refusal("the provider adds a column with no engine settings")
		}
		return ddl.AddColumn(schema, table, ddlColumn(operation.AddColumn))
	case *protocol.Change_DropColumn:
		return ddl.DropColumn(schema, table, operation.DropColumn), nil
	default:
		return "", refusal("the provider creates and drops tables and adds and drops columns; other changes come with later releases")
	}
}

// createTable writes CREATE TABLE for a planned table.
func createTable(schema, name string, table *protocol.Table) (string, error) {
	if len(table.GetFacets()) != 0 || len(table.GetIndexes()) != 0 || table.GetPrimaryKey() != nil {
		return "", refusal("the provider creates a table from its columns alone")
	}
	columns := make([]ddl.Column, 0, len(table.GetColumns()))
	var primaryKey []string
	for _, column := range table.GetColumns() {
		if len(column.GetFacets()) != 0 || column.GetObserved() != nil {
			return "", refusal("the provider creates a column from its name, type, nullability and default alone")
		}
		columns = append(columns, ddlColumn(column))
		if column.GetPrimaryKey() {
			primaryKey = append(primaryKey, column.GetName())
		}
	}
	return ddl.CreateTable(schema, name, columns, primaryKey)
}

func ddlColumn(column *protocol.Column) ddl.Column {
	return ddl.Column{Name: column.GetName(), Type: column.GetType(), Nullable: column.GetNullable(), Default: column.DefaultExpression}
}

// refusal is a reason the provider shows the operator. It names what the
// provider cannot write and why, and never quotes a value or an address.
func refusal(reason string) error { return errors.New(reason) }
