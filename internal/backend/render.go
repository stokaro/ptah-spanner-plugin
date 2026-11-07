package backend

import (
	"context"
	"errors"
	"slices"
	"strings"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/stokaro/ptah/provider-sdk/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-spanner-plugin/internal/coltype"
	"github.com/stokaro/ptah-spanner-plugin/internal/commonobject"
	"github.com/stokaro/ptah-spanner-plugin/internal/ddl"
	"github.com/stokaro/ptah-spanner-plugin/internal/tableop"
)

// Render writes the SQL statement of each table operation the provider
// planned, in the order the changes arrive, in either form the host asks for:
// every operation is one statement, so the executed units and a migration
// file hold the same text. It connects to nothing. A change it cannot write
// refuses the whole batch, so the host never receives part of a plan.
//
// For a host that lists render.omissions the result also names what Spanner
// leaves out of the columns the statements declare; see appendOmissions.
func (*Provider) Render(ctx context.Context, request *protocol.RenderRequest) (*protocol.RenderResult, error) {
	result := &protocol.RenderResult{Complete: true}
	report := slices.Contains(request.GetTargetFeatures(), provider.RenderOmissionsFeature)
	for _, change := range request.GetChanges() {
		if err := ctx.Err(); err != nil {
			return nil, status.FromContextError(err).Err()
		}
		statement, err := renderChange(change)
		if err != nil {
			return nil, runtime.ErrorWithReason(codes.InvalidArgument, err.Error())
		}
		result.Units = append(result.Units, &protocol.Unit{ChangeId: change.GetId(), Body: &protocol.Unit_Sql{Sql: statement}})
		if report {
			result.Omissions = appendOmissions(result.Omissions, change)
		}
	}
	return result, nil
}

// appendOmissions adds to omissions what Spanner leaves out of the columns
// the table operation of change declares, each once. The statement keeps the
// declared type, which Spanner accepts and stores as another: a column
// declared integer is a bigint column, and nothing else reports that the
// 32-bit range is gone (coltype.Widened). A CHECK keeps the range of a
// scalar column, and the remedy says so. A change that carries no table
// operation, such as a view's, declares no column.
func appendOmissions(omissions []*protocol.RenderOmission, change *protocol.Change) []*protocol.RenderOmission {
	value := change.GetNative().GetValue().GetValue()
	if value == nil {
		return omissions
	}
	operation, err := tableop.Decode(value.GetPayload())
	if err != nil {
		return omissions
	}
	table := value.GetOwner().GetName()
	if schema := value.GetOwner().GetSchema(); schema != "" && schema != ddl.DefaultSchema {
		table = schema + "." + table
	}
	for _, column := range operation.Types() {
		detail, widened := coltype.Widened(column.Type)
		if !widened {
			continue
		}
		omission := &protocol.RenderOmission{Reason: "unsupported", Kind: "column", Name: table + "." + column.Name, Property: "type", Detail: detail}
		if !strings.HasSuffix(strings.TrimSpace(column.Type), "]") {
			omission.Remedy = "a CHECK (" + ddl.Quote(column.Name) + " BETWEEN -2147483648 AND 2147483647) keeps the range"
		}
		if !slices.ContainsFunc(omissions, func(earlier *protocol.RenderOmission) bool { return proto.Equal(earlier, omission) }) {
			omissions = append(omissions, omission)
		}
	}
	return omissions
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
