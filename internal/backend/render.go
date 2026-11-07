package backend

import (
	"context"
	"errors"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/stokaro/ptah-spanner-plugin/internal/commonobject"
	"github.com/stokaro/ptah-spanner-plugin/internal/tableop"
)

// Render writes the SQL statement of each table operation the provider
// planned, in the order the changes arrive, in either form the host asks for:
// every operation is one statement, so the executed units and a migration
// file hold the same text. It connects to nothing. A change it cannot write
// refuses the whole batch, so the host never receives part of a plan.
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

// renderChange writes the statement of one native table operation. The
// operation's owner is its table; an ALTER_TABLE operation names the same
// table as its parent.
func renderChange(change *protocol.Change) (string, error) {
	if common := change.GetCommonObject(); common != nil {
		return commonobject.Render(common)
	}
	native := change.GetNative()
	if native == nil {
		return "", refusal("the provider renders the table operations it plans, views and sequences, and nothing else")
	}
	owner := native.GetValue().GetValue().GetOwner()
	switch native.GetRole() {
	case protocol.NativeOperation_STATEMENT:
		if native.GetParent() != nil {
			return "", refusal("a standalone table operation has no parent")
		}
	case protocol.NativeOperation_ALTER_TABLE:
		// The host names a table in the default schema without its schema.
		parent := native.GetParent()
		schema := parent.GetSchema()
		if schema == "" {
			schema = defaultSchema
		}
		if parent.GetName() != owner.GetName() || schema != owner.GetSchema() || parent.GetKind() != "table" {
			return "", refusal("a table operation names its own table as its parent")
		}
	default:
		return "", refusal("a table operation is a statement or an ALTER TABLE")
	}
	statement, err := tableop.Statement(native.GetValue())
	if err != nil {
		return "", refusal(err.Error())
	}
	return statement, nil
}

// refusal is a reason the provider shows the operator. It names what the
// provider cannot write and why, and never quotes a value or an address.
func refusal(reason string) error { return errors.New(reason) }
