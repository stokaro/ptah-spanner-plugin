package tableplan_test

import (
	"context"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-spanner-plugin/internal/backend"
	"github.com/stokaro/ptah-spanner-plugin/internal/rowdeletion"
	"github.com/stokaro/ptah-spanner-plugin/internal/tableplan"
	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// features carries the policy of table name as a captured facet.
func features(c *qt.C, representation protocol.ValueInput_Representation, name string, policy rowdeletion.Policy) *protocol.CapturedFeatures {
	c.Helper()
	value, err := rowdeletion.Facet(name, representation, &protocol.Reference{Engine: "spanner", Kind: "table", Schema: "public", Name: name}, policy)
	c.Assert(err, qt.IsNil)
	return &protocol.CapturedFeatures{Facets: &protocol.ComparisonState{Representation: representation,
		Records: []*protocol.ComparisonRecord{{Subject: subject(name), Kind: rowdeletion.Kind, Value: value}}}}
}

// policyChange is the comparison's change of table t's policy.
func policyChange(c *qt.C, change rowdeletion.Change) *protocol.PlanningChange {
	c.Helper()
	value, err := rowdeletion.CaptureChange("change-0", &protocol.Reference{Engine: "spanner", Kind: "table", Schema: "public", Name: "t"}, change)
	c.Assert(err, qt.IsNil)
	return &protocol.PlanningChange{Subject: subject("t"), Value: value}
}

// A created table carries its policy in its CREATE TABLE, after the column
// list, with the interval as declared.
func TestPlan_CreatesATableWithItsRowDeletionPolicy(t *testing.T) {
	c := testcheck.New(t)
	desired := &protocol.DeclaredTable{Name: "events", Columns: []*protocol.DeclaredColumn{
		{Name: "id", Type: "bigint", Primary: true}, {Name: "created_at", Type: "TIMESTAMPTZ", Nullable: true},
	}, Features: features(c, protocol.ValueInput_DESIRED, "events", rowdeletion.Policy{Column: "created_at", Interval: "30 days"})}
	r := request([]*protocol.PlanningTable{{Subject: subject("events"), Action: protocol.PlanningTable_CREATE, Desired: desired}},
		common("common/000000", "events", effect(subject("events"), protocol.PlanningEffect_CREATE)))
	steps, _, _ := plan(c, r)
	c.Assert(statements(steps), qt.DeepEquals, []string{
		"CREATE TABLE \"events\" (\n  \"id\" bigint PRIMARY KEY NOT NULL,\n  \"created_at\" TIMESTAMPTZ\n) TTL INTERVAL '30 days' ON \"created_at\";",
	})
}

// A kept table's policy changes in place, with the note Ptah's built-in
// Spanner engine wrote: ADD TTL for a table without one, ALTER TTL for one
// with one, DROP TTL to remove it. A policy changes after the columns its
// table adds and before the columns it drops.
func TestPlan_ChangesTheRowDeletionPolicyOfAKeptTable(t *testing.T) {
	stored := rowdeletion.Policy{Column: "created_at", Interval: "4 WEEKS 2 DAYS"}
	tests := []struct {
		name      string
		change    rowdeletion.Change
		statement string
	}{
		{name: "a new policy", change: rowdeletion.Change{After: &rowdeletion.Policy{Column: "seen_at", Interval: "7 days"}},
			statement: `ALTER TABLE "t" ADD TTL INTERVAL '7 days' ON "seen_at";`},
		{name: "a replaced policy", change: rowdeletion.Change{Before: &stored, After: &rowdeletion.Policy{Column: "seen_at", Interval: "60 days"}},
			statement: `ALTER TABLE "t" ALTER TTL INTERVAL '60 days' ON "seen_at";`},
		{name: "a removed policy", change: rowdeletion.Change{Before: &stored}, statement: `ALTER TABLE "t" DROP TTL;`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			table := &protocol.PlanningTable{Subject: subject("t"), Action: protocol.PlanningTable_ALTER,
				Desired: &protocol.DeclaredTable{Name: "t", Columns: []*protocol.DeclaredColumn{{Name: "id", Type: "bigint", Primary: true},
					{Name: "created_at", Type: "timestamptz", Nullable: true}, {Name: "seen_at", Type: "timestamptz", Nullable: true}}},
				Current: &protocol.ObservedTable{Name: "t", Schema: "public", Columns: []*protocol.ObservedColumn{
					{Name: "id", DataType: "bigint", ColumnType: "bigint", IsNullable: "NO", PrimaryKey: true},
					{Name: "created_at", DataType: "timestamp with time zone", ColumnType: "timestamp with time zone", IsNullable: "YES"},
					{Name: "old", DataType: "bigint", ColumnType: "bigint", IsNullable: "YES"},
				}, Constraints: []*protocol.ObservedConstraint{{Name: "PK_t", Type: "PRIMARY KEY", ColumnNames: []string{"id"}}}}}
			r := request([]*protocol.PlanningTable{table},
				&protocol.CommonPlanningStep{Id: &protocol.StepIdentity{Owner: "ptah.run/provider-common", Name: "common/000000"}, Parent: subject("t"),
					AddedColumn: &protocol.PlanningColumn{Name: "seen_at", Type: "timestamptz"},
					Metadata: &protocol.PlanningMetadata{Effects: []*protocol.PlanningEffect{effect(subject("t"), protocol.PlanningEffect_READ),
						effect(child("t", "column", "seen_at"), protocol.PlanningEffect_CREATE)}}},
				common("common/000001", "t", effect(subject("t"), protocol.PlanningEffect_READ), effect(child("t", "column", "old"), protocol.PlanningEffect_DROP)))
			r.Changes = []*protocol.PlanningChange{policyChange(c, test.change)}
			steps, dependencies, result := plan(c, r)
			c.Assert(statements(steps), qt.DeepEquals, []string{
				`ALTER TABLE "t" ADD COLUMN "seen_at" timestamptz;`, `ALTER TABLE "t" DROP COLUMN "old";`, test.statement,
			})
			c.Assert(steps[2].Notes, qt.DeepEquals, []string{"Row deletion policy on table: t"})
			c.Assert(dependencies, qt.DeepEquals, [][2]string{{steps[0].Name, steps[2].Name}, {steps[2].Name, steps[1].Name}})
			c.Assert(result.GetChanges(), qt.HasLen, 1)
			c.Assert(result.GetChanges()[0].GetSteps()[0].GetName(), qt.Equals, steps[2].Name)
		})
	}
}

// refusal plans r and returns the one refusal the planner answers with, after
// checking the reply as the SDK checks it.
func refusal(c *qt.C, r *protocol.PlanFeaturesRequest) string {
	c.Helper()
	result, err := tableplan.Plan(context.Background(), r)
	c.Assert(err, qt.IsNil)
	c.Assert(provider.ValidatePlannedFeaturesReply(backend.Description(), r, result), qt.IsNil)
	c.Assert(result.GetDiagnostics(), qt.HasLen, 1)
	c.Assert(result.GetContributions(), qt.HasLen, 0)
	return result.GetDiagnostics()[0].GetMessage()
}

// A created table's policy Spanner refuses is refused before a statement is
// planned: one that counts from a column the table does not declare, or from
// one that is not timestamptz.
func TestPlan_RefusesAPolicySpannerRefuses(t *testing.T) {
	tests := []struct {
		name   string
		policy rowdeletion.Policy
		want   string
	}{
		{name: "an undeclared column", policy: rowdeletion.Policy{Column: "missing", Interval: "30 days"},
			want: `the row deletion policy of table "events" counts from column "missing", which the table does not declare`},
		{name: "a column that is not timestamptz", policy: rowdeletion.Policy{Column: "n", Interval: "30 days"},
			want: `the row deletion policy of table "events" counts from column "n", which is not timestamptz; Spanner deletes rows by a timestamp column`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			r := request([]*protocol.PlanningTable{{Subject: subject("events"), Action: protocol.PlanningTable_CREATE, Desired: &protocol.DeclaredTable{Name: "events",
				Columns:  []*protocol.DeclaredColumn{{Name: "id", Type: "bigint", Primary: true}, {Name: "n", Type: "bigint", Nullable: true}},
				Features: features(c, protocol.ValueInput_DESIRED, "events", test.policy)}}},
				common("common/000000", "events", effect(subject("events"), protocol.PlanningEffect_CREATE)))
			c.Assert(refusal(c, r), qt.Equals, test.want)
		})
	}
}

// policyColumnDrop is a kept table whose held policy counts from column old,
// which the declaration drops.
func policyColumnDrop(c *qt.C) (*protocol.PlanFeaturesRequest, rowdeletion.Policy) {
	c.Helper()
	stored := rowdeletion.Policy{Column: "old", Interval: "7 DAYS"}
	return request([]*protocol.PlanningTable{{Subject: subject("t"), Action: protocol.PlanningTable_ALTER,
		Desired: &protocol.DeclaredTable{Name: "t", Columns: []*protocol.DeclaredColumn{{Name: "id", Type: "bigint", Primary: true}}},
		Current: &protocol.ObservedTable{Name: "t", Schema: "public", Columns: []*protocol.ObservedColumn{
			{Name: "id", DataType: "bigint", ColumnType: "bigint", IsNullable: "NO", PrimaryKey: true},
			{Name: "old", DataType: "timestamp with time zone", ColumnType: "timestamp with time zone", IsNullable: "YES"},
		}, Constraints: []*protocol.ObservedConstraint{{Name: "PK_t", Type: "PRIMARY KEY", ColumnNames: []string{"id"}}},
			Features: features(c, protocol.ValueInput_OBSERVED, "t", stored)}}},
		common("common/000000", "t", effect(subject("t"), protocol.PlanningEffect_READ), effect(child("t", "column", "old"), protocol.PlanningEffect_DROP))), stored
}

// A column drop the held policy still counts from is refused: Spanner refuses
// it, and a source that leaves the policy unmanaged, as an HCL table without
// its platform block, would otherwise stop the plan after a prefix.
func TestPlan_RefusesDroppingThePolicyColumn(t *testing.T) {
	c := testcheck.New(t)
	r, _ := policyColumnDrop(c)
	c.Assert(refusal(c, r), qt.Equals, `dropping column "old" of table "t": the table's row deletion policy counts from it, and Spanner refuses to drop that column; `+
		`declare the table's policy on another column, or no policy, in the same change`)
}

// A column drop goes ahead when the same change removes the held policy.
func TestPlan_DropsAPolicyColumnWithItsPolicy(t *testing.T) {
	c := testcheck.New(t)
	r, stored := policyColumnDrop(c)
	r.Changes = []*protocol.PlanningChange{policyChange(c, rowdeletion.Change{Before: &stored})}
	steps, dependencies, _ := plan(c, r)
	c.Assert(statements(steps), qt.DeepEquals, []string{`ALTER TABLE "t" DROP COLUMN "old";`, `ALTER TABLE "t" DROP TTL;`})
	c.Assert(dependencies, qt.DeepEquals, [][2]string{{steps[1].Name, steps[0].Name}})
}

// The offline check reports a policy Spanner refuses with the table.
func TestValidate_RowDeletionPolicy(t *testing.T) {
	c := testcheck.New(t)
	problems := tableplan.Validate(&protocol.ValidateSchemaRequest{Tables: []*protocol.ValidationTable{{Subject: subject("events"), Declaration: &protocol.DeclaredTable{
		Name: "events", Columns: []*protocol.DeclaredColumn{{Name: "id", Type: "bigint", Primary: true}, {Name: "n", Type: "bigint", Nullable: true}},
		Features: features(c, protocol.ValueInput_DESIRED, "events", rowdeletion.Policy{Column: "n", Interval: "30 days"})}}}})
	c.Assert(problems, qt.DeepEquals, []tableplan.Problem{{Code: protocol.SchemaDiagnostic_UNSUPPORTED_FEATURE, Kind: "row deletion policy", Object: `"events"`,
		Message: `the row deletion policy of table "events" counts from column "n", which is not timestamptz; Spanner deletes rows by a timestamp column`}})
}
