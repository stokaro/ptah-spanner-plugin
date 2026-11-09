package backend

import (
	"context"

	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-spanner-plugin/internal/tableplan"
)

// CompareChecks answers whether each declared CHECK and the CHECK a table
// holds under its name are one constraint on Spanner, by
// tableplan.SameCheck, which the planner decides with too. It reads the
// request alone. Ptah's built-in Spanner engine compared the texts by
// PostgreSQL's rules, and dropped and added `score >= 0` on every run against
// the clause Spanner keeps, `(score >= '0'::bigint)` (stokaro/ptah#4204 S1).
func (*Provider) CompareChecks(ctx context.Context, request *protocol.CompareChecksRequest) (*protocol.ComparedChecks, error) {
	result := &protocol.ComparedChecks{Complete: true}
	for _, table := range request.GetTables() {
		if err := ctx.Err(); err != nil {
			return nil, status.FromContextError(err).Err()
		}
		answer := &protocol.ComparedCheckTable{Subject: proto.CloneOf(table.GetSubject())}
		for _, pair := range table.GetPairs() {
			answer.Checks = append(answer.Checks, &protocol.ComparedCheck{Constraint: pair.GetConstraint(),
				Equivalent: tableplan.SameCheck(pair.GetDeclared(), heldClause(table.GetCurrent(), pair.GetConstraint()), table.GetCurrent())})
		}
		result.Tables = append(result.Tables, answer)
	}
	return result, nil
}

// heldClause is the clause of the CHECK a held table keeps under name. The
// SDK has checked that the table holds one.
func heldClause(table *protocol.ObservedTable, name string) string {
	for _, constraint := range table.GetConstraints() {
		if constraint.GetName() == name {
			return constraint.GetCheckClause()
		}
	}
	return ""
}
