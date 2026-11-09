package tableplan

import (
	"fmt"

	"google.golang.org/protobuf/proto"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-spanner-plugin/internal/coltype"
	"github.com/stokaro/ptah-spanner-plugin/internal/rowdeletion"
	"github.com/stokaro/ptah-spanner-plugin/internal/tableop"
)

// declaredPolicy is the row deletion policy a declared table carries among its
// facets, or nil for none.
func declaredPolicy(table *protocol.DeclaredTable) (*rowdeletion.Policy, error) {
	return facetPolicy(table.GetFeatures())
}

// heldPolicy is the row deletion policy an observed table holds, or nil for
// none.
func heldPolicy(table *protocol.ObservedTable) (*rowdeletion.Policy, error) {
	return facetPolicy(table.GetFeatures())
}

func facetPolicy(features *protocol.CapturedFeatures) (*rowdeletion.Policy, error) {
	for _, record := range features.GetFacets().GetRecords() {
		if record.GetKind() != rowdeletion.Kind || record.GetValue() == nil {
			continue
		}
		policy, err := rowdeletion.PolicyOf(record.GetValue())
		if err != nil {
			return nil, err
		}
		return &policy, nil
	}
	return nil, nil
}

// policyRefusal names what Spanner refuses in a declared table's policy: a
// column the table does not declare, and one that is not timestamptz
// (measured on the emulator: `Row Deletion Policy must be set on column of
// type TIMESTAMP`). It returns nil for a policy Spanner takes.
func policyRefusal(subject *protocol.ComparisonIdentity, table *protocol.DeclaredTable, policy rowdeletion.Policy) error {
	if err := rowdeletion.ValidateDeclared(phrase(subject), policy); err != nil {
		return err
	}
	column := declaredColumn(table, policy.Column)
	if column == nil {
		return fmt.Errorf("the row deletion policy of table %s counts from column %q, which the table does not declare", phrase(subject), policy.Column)
	}
	if canonical, err := coltype.Canonical(ColumnType(table, column)); err != nil || canonical != "timestamp with time zone" {
		return fmt.Errorf("the row deletion policy of table %s counts from column %q, which is not timestamptz; Spanner deletes rows by a timestamp column",
			phrase(subject), policy.Column)
	}
	return nil
}

// creationPolicy is the policy a CREATE TABLE writes for a declared table, or
// nil for none.
func creationPolicy(subject *protocol.ComparisonIdentity, table *protocol.DeclaredTable) (*tableop.RowDeletion, error) {
	policy, err := declaredPolicy(table)
	if err != nil || policy == nil {
		return nil, err
	}
	if err := policyRefusal(subject, table, *policy); err != nil {
		return nil, err
	}
	return &tableop.RowDeletion{Column: policy.Column, Interval: policy.Interval}, nil
}

// policyChanges adds a step for each row deletion policy change of the table
// at index: ADD TTL for a table without a policy, ALTER TTL for one with a
// policy, DROP TTL to remove it. Each change's receipt names its step. The
// step runs after the column additions of its table, since a new policy may
// count from a column the plan adds, and before the column removals, since
// Spanner refuses to drop the column a policy counts from.
func (p *planner) policyChanges(index int, table *protocol.PlanningTable) ([]*protocol.StepIdentity, error) {
	subject := table.GetSubject()
	var steps []*protocol.StepIdentity
	for _, change := range p.request.GetChanges() {
		if change.GetValue().GetValue().GetKind() != rowdeletion.ChangeKind || keyOf(change.GetSubject()) != keyOf(subject) {
			continue
		}
		planned, err := rowdeletion.ChangeOf(change.GetValue())
		if err != nil {
			return nil, err
		}
		operation := tableop.Operation{Action: tableop.DropRowDeletion}
		if planned.After != nil {
			if err := policyRefusal(subject, table.GetDesired(), *planned.After); err != nil {
				return nil, &refusal{table: index, code: protocol.PlanningDiagnostic_UNSUPPORTED_FEATURE, message: err.Error()}
			}
			operation = tableop.Operation{Action: tableop.AddRowDeletion, RowDeletion: &tableop.RowDeletion{Column: planned.After.Column, Interval: planned.After.Interval}}
			if planned.Before != nil {
				operation.Action = tableop.AlterRowDeletion
			}
		}
		policy := proto.CloneOf(subject)
		policy.Kind = rowdeletion.Kind
		id, err := p.add(index, subject, rankPolicy, operation, []string{policyNote(subject)}, readTable(subject), effect(policy, protocol.PlanningEffect_ALTER))
		if err != nil {
			return nil, err
		}
		p.result.Changes = append(p.result.Changes, &protocol.PlanningReceipt{Subject: proto.CloneOf(change.GetSubject()), Kind: rowdeletion.ChangeKind,
			Strategy: "Add, alter or drop the TTL clause in place, after the table's column additions and before its column removals",
			Steps:    []*protocol.StepIdentity{proto.CloneOf(id)}})
		steps = append(steps, id)
	}
	return steps, nil
}

// heldPolicyColumn refuses dropping the column the table's held policy counts
// from while the plan keeps that policy: Spanner refuses the DROP COLUMN
// (`ROW DELETION POLICY will break if drop/alter column named ...`), which
// would stop the plan after its earlier statements ran. A source that leaves
// the policy unmanaged, such as an HCL table without its platform "spanner"
// block, is the way to reach it.
func (p *planner) heldPolicyColumn(index int, table *protocol.PlanningTable, column string) error {
	held, err := heldPolicy(table.GetCurrent())
	if err != nil || held == nil || held.Column != column {
		return err
	}
	for _, change := range p.request.GetChanges() {
		if change.GetValue().GetValue().GetKind() != rowdeletion.ChangeKind || keyOf(change.GetSubject()) != keyOf(table.GetSubject()) {
			continue
		}
		planned, err := rowdeletion.ChangeOf(change.GetValue())
		if err != nil {
			return err
		}
		if planned.After == nil || planned.After.Column != column {
			return nil
		}
	}
	return &refusal{table: index, code: protocol.PlanningDiagnostic_UNSUPPORTED_FEATURE, message: fmt.Sprintf(
		"dropping column %q of table %s: the table's row deletion policy counts from it, and Spanner refuses to drop that column; "+
			"declare the table's policy on another column, or no policy, in the same change", column, phrase(table.GetSubject()))}
}

// policyNote is the comment a row deletion policy statement carries, in the
// words of Ptah's built-in Spanner engine.
func policyNote(subject *protocol.ComparisonIdentity) string {
	name := subject.GetName().GetSource()
	if schema := subject.GetSchema().GetSource(); schema != "" && schema != "public" {
		name = schema + "." + name
	}
	return "Row deletion policy on table: " + name
}
