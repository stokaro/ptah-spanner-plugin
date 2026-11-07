package tablefacets_test

import (
	"context"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-spanner-plugin/internal/backend"
	"github.com/stokaro/ptah-spanner-plugin/internal/rowdeletion"
	"github.com/stokaro/ptah-spanner-plugin/internal/tablefacets"
	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

func subject(name string) *protocol.ComparisonIdentity {
	return &protocol.ComparisonIdentity{Kind: "table", Database: &protocol.ComparisonPart{}, Catalog: &protocol.ComparisonPart{},
		Schema: &protocol.ComparisonPart{Source: "public", Normalized: "public"}, Parent: &protocol.ComparisonPart{}, Name: &protocol.ComparisonPart{Source: name, Normalized: name}}
}

func reference(name string) *protocol.Reference {
	return &protocol.Reference{Engine: "spanner", Kind: "table", Schema: "public", Name: name}
}

// record is the policy of table name on one side.
func record(c *qt.C, representation protocol.ValueInput_Representation, name string, policy rowdeletion.Policy) *protocol.ComparisonRecord {
	c.Helper()
	value, err := rowdeletion.Facet(name, representation, reference(name), policy)
	c.Assert(err, qt.IsNil)
	return &protocol.ComparisonRecord{Subject: subject(name), Kind: rowdeletion.Kind, Targets: []string{"spanner"}, Value: value}
}

// complete is a coverage that knows every table's policy: a Go or YAML
// source, and an unnarrowed read.
func complete(representation protocol.ValueInput_Representation) *protocol.ComparisonCoverage {
	model := rowdeletion.DesiredModel()
	if representation == protocol.ValueInput_OBSERVED {
		model = rowdeletion.ObservedModel()
	}
	return &protocol.ComparisonCoverage{Kinds: []*protocol.ComparisonKindCoverage{{Model: model, Sources: []*protocol.ComparisonKnowledgeSource{{
		Outside: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_COMPLETE}}}}}}
}

// narrowed is a read of the public schema alone, as the host covers it:
// unknown outside the schema, complete for the tables in it.
func narrowed() *protocol.ComparisonCoverage {
	return &protocol.ComparisonCoverage{Kinds: []*protocol.ComparisonKindCoverage{{Model: rowdeletion.ObservedModel(), Sources: []*protocol.ComparisonKnowledgeSource{{
		Outside: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_UNINSPECTED, Reason: "The read covers the selected schemas only"},
		Regions: []*protocol.ComparisonRegion{{
			Scope: &protocol.ComparisonNamespace{Targets: []string{"spanner"}, SubjectKinds: []string{"table"}, Database: &protocol.ComparisonPart{},
				Catalog: &protocol.ComparisonPart{}, Schema: &protocol.ComparisonPart{Source: "public", Normalized: "public"}, Parent: &protocol.ComparisonPart{}, PathExact: true},
			Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_COMPLETE},
		}},
	}}}}}
}

// unknown is a coverage that establishes nothing, as an HCL document without
// a platform block.
func unknown(representation protocol.ValueInput_Representation) *protocol.ComparisonCoverage {
	coverage := complete(representation)
	coverage.Kinds[0].Sources[0].Outside = &protocol.ModelKnowledge{State: protocol.ModelKnowledge_UNINSPECTED, Reason: "not declared"}
	return coverage
}

func compareRequest(desired, current []*protocol.ComparisonRecord, desiredCoverage, currentCoverage *protocol.ComparisonCoverage, tables ...string) *protocol.CompareModelsRequest {
	request := &protocol.CompareModelsRequest{Engine: "spanner", ComparisonId: tablefacets.ComparisonID, TargetFeatures: backend.Description().GetEngines()[0].GetFeatures(),
		Identifiers: &protocol.ComparisonIdentifiers{DefaultSchema: "public", Rules: backend.Description().GetEngines()[0].GetIdentifiers()},
		Kinds:       []string{rowdeletion.Kind},
		Desired:     &protocol.ComparisonState{Representation: protocol.ValueInput_DESIRED, Records: desired, Coverage: desiredCoverage},
		Current:     &protocol.ComparisonState{Representation: protocol.ValueInput_OBSERVED, Records: current, Coverage: currentCoverage}}
	for _, table := range tables {
		request.Parents = append(request.Parents, &protocol.ComparisonParent{Subject: subject(table), Desired: true, Current: true})
	}
	return request
}

// compare runs the comparison, checks the reply as the SDK checks it, and
// returns each change by its table.
func compare(c *qt.C, request *protocol.CompareModelsRequest) (map[string]rowdeletion.Change, *protocol.ComparedModels) {
	c.Helper()
	result, err := tablefacets.Compare(context.Background(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(provider.ValidateComparedModelsReply(backend.Description(), request, result), qt.IsNil)
	changes := make(map[string]rowdeletion.Change)
	for _, compared := range result.GetChanges() {
		change, err := rowdeletion.ChangeOf(compared.GetValue())
		c.Assert(err, qt.IsNil)
		changes[compared.GetSubject().GetName().GetSource()] = change
	}
	return changes, result
}

// A table both sides hold changes when its policy differs: the interval by
// the hours it denotes, the column exactly. A Go or YAML source declares no
// policy for a table it gives none, so a held one is dropped.
func TestCompare_ChangesPoliciesThatDiffer(t *testing.T) {
	c := testcheck.New(t)
	stored := rowdeletion.Policy{Column: "created_at", Interval: "4 WEEKS 2 DAYS"}
	request := compareRequest(
		[]*protocol.ComparisonRecord{
			record(c, protocol.ValueInput_DESIRED, "same", rowdeletion.Policy{Column: "created_at", Interval: "30 days"}),
			record(c, protocol.ValueInput_DESIRED, "longer", rowdeletion.Policy{Column: "created_at", Interval: "60 days"}),
			record(c, protocol.ValueInput_DESIRED, "moved", rowdeletion.Policy{Column: "seen_at", Interval: "30 days"}),
			record(c, protocol.ValueInput_DESIRED, "added", rowdeletion.Policy{Column: "ts", Interval: "7 days"}),
		},
		[]*protocol.ComparisonRecord{
			record(c, protocol.ValueInput_OBSERVED, "same", stored), record(c, protocol.ValueInput_OBSERVED, "longer", stored),
			record(c, protocol.ValueInput_OBSERVED, "moved", stored), record(c, protocol.ValueInput_OBSERVED, "dropped", stored),
		},
		complete(protocol.ValueInput_DESIRED), complete(protocol.ValueInput_OBSERVED), "same", "longer", "moved", "added", "dropped")
	changes, _ := compare(c, request)
	c.Assert(changes, qt.DeepEquals, map[string]rowdeletion.Change{
		"longer":  {Before: &stored, After: &rowdeletion.Policy{Column: "created_at", Interval: "60 days"}},
		"moved":   {Before: &stored, After: &rowdeletion.Policy{Column: "seen_at", Interval: "30 days"}},
		"added":   {After: &rowdeletion.Policy{Column: "ts", Interval: "7 days"}},
		"dropped": {Before: &stored},
	})
}

// A source that cannot state a table's policy, as an HCL table without its
// platform block, leaves the held policy alone.
func TestCompare_LeavesAnUnmanagedPolicyAlone(t *testing.T) {
	c := testcheck.New(t)
	request := compareRequest(nil, []*protocol.ComparisonRecord{record(c, protocol.ValueInput_OBSERVED, "t", rowdeletion.Policy{Column: "c", Interval: "7 DAYS"})},
		unknown(protocol.ValueInput_DESIRED), complete(protocol.ValueInput_OBSERVED), "t")
	changes, result := compare(c, request)
	c.Assert(changes, qt.HasLen, 0)
	c.Assert(result.GetUndecided(), qt.HasLen, 0)
}

// A read narrowed to a schema knows the policies of that schema's tables, so
// a declared policy on a table without one is added.
func TestCompare_ANarrowedReadKnowsItsSchema(t *testing.T) {
	c := testcheck.New(t)
	request := compareRequest([]*protocol.ComparisonRecord{record(c, protocol.ValueInput_DESIRED, "t", rowdeletion.Policy{Column: "c", Interval: "7 days"})}, nil,
		complete(protocol.ValueInput_DESIRED), narrowed(), "t")
	changes, result := compare(c, request)
	c.Assert(changes, qt.DeepEquals, map[string]rowdeletion.Change{"t": {After: &rowdeletion.Policy{Column: "c", Interval: "7 days"}}})
	c.Assert(result.GetUndecided(), qt.HasLen, 0)
}

// A declared policy on a table whose policy the read did not establish is
// undecided, never a change.
func TestCompare_AnUnknownReadIsUndecided(t *testing.T) {
	c := testcheck.New(t)
	request := compareRequest([]*protocol.ComparisonRecord{record(c, protocol.ValueInput_DESIRED, "t", rowdeletion.Policy{Column: "c", Interval: "7 days"})}, nil,
		complete(protocol.ValueInput_DESIRED), unknown(protocol.ValueInput_OBSERVED), "t")
	changes, result := compare(c, request)
	c.Assert(changes, qt.HasLen, 0)
	c.Assert(result.GetUndecided(), qt.HasLen, 1)
	c.Assert(result.GetUndecided()[0].GetReason(), qt.Equals, "The read did not establish whether the table holds a row deletion policy")
}

// A table only one side holds carries its policy with it: CREATE TABLE writes
// it and DROP TABLE removes it.
func TestCompare_TablesOneSideHoldsChangeNothing(t *testing.T) {
	c := testcheck.New(t)
	request := compareRequest([]*protocol.ComparisonRecord{record(c, protocol.ValueInput_DESIRED, "created", rowdeletion.Policy{Column: "c", Interval: "7 days"})},
		[]*protocol.ComparisonRecord{record(c, protocol.ValueInput_OBSERVED, "dropped", rowdeletion.Policy{Column: "c", Interval: "7 DAYS"})},
		complete(protocol.ValueInput_DESIRED), complete(protocol.ValueInput_OBSERVED))
	request.Parents = []*protocol.ComparisonParent{{Subject: subject("created"), Desired: true}, {Subject: subject("dropped"), Current: true}}
	changes, _ := compare(c, request)
	c.Assert(changes, qt.HasLen, 0)
}

// The reverse restores the policy the table held, in the server's spelling,
// or removes the one the forward change added, and says the rows the forward
// policy deleted stay deleted.
func TestReverse(t *testing.T) {
	stored := rowdeletion.Policy{Column: "created_at", Interval: "4 WEEKS 2 DAYS"}
	declared := rowdeletion.Policy{Column: "seen_at", Interval: "60 days"}
	tests := []struct {
		name               string
		forward, reverse   rowdeletion.Change
		strategy           string
		limitations        []string
		forwardStateExists bool
	}{
		{name: "a replaced policy", forward: rowdeletion.Change{Before: &stored, After: &declared}, reverse: rowdeletion.Change{Before: &declared, After: &stored},
			strategy: "Restore the table's prior row deletion policy in place", forwardStateExists: true,
			limitations: []string{"Restoring the prior row deletion policy cannot recover rows the server deleted under the forward policy."}},
		{name: "an added policy", forward: rowdeletion.Change{After: &declared}, reverse: rowdeletion.Change{Before: &declared},
			strategy: "Remove the row deletion policy the forward change added", forwardStateExists: true,
			limitations: []string{"Restoring the prior row deletion policy cannot recover rows the server deleted under the forward policy."}},
		{name: "a dropped policy", forward: rowdeletion.Change{Before: &stored}, reverse: rowdeletion.Change{After: &stored},
			strategy: "Restore the table's prior row deletion policy in place"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			value, err := rowdeletion.CaptureChange("change-0", reference("t"), test.forward)
			c.Assert(err, qt.IsNil)
			request := &protocol.ReverseChangesRequest{Engine: "spanner", ReversalId: tablefacets.ReversalID, TargetFeatures: backend.Description().GetEngines()[0].GetFeatures(),
				Identifiers: &protocol.ComparisonIdentifiers{DefaultSchema: "public", Rules: backend.Description().GetEngines()[0].GetIdentifiers()},
				Changes:     []*protocol.PlanningChange{{Subject: subject("t"), Value: value}}}
			result, err := tablefacets.Reverse(context.Background(), request)
			c.Assert(err, qt.IsNil)
			c.Assert(provider.ValidateReversedChangesReply(backend.Description(), request, result), qt.IsNil)
			reversed := result.GetChanges()[0]
			change, err := rowdeletion.ChangeOf(reversed.GetValue())
			c.Assert(err, qt.IsNil)
			c.Assert(change, qt.DeepEquals, test.reverse)
			c.Assert(reversed.GetStrategy(), qt.Equals, test.strategy)
			c.Assert(reversed.GetLimitations(), qt.DeepEquals, test.limitations)
			c.Assert(reversed.GetForwardState()[0].GetValue() != nil, qt.Equals, test.forwardStateExists)
			c.Assert(proto.Equal(reversed.GetSubject(), subject("t")), qt.IsTrue)
		})
	}
}

// Each policy counts one table, under the name Ptah's built-in Spanner engine
// gave the metric.
func TestReport(t *testing.T) {
	c := testcheck.New(t)
	value, err := rowdeletion.Facet("v", protocol.ValueInput_OBSERVED, reference("t"), rowdeletion.Policy{Column: "c", Interval: "7 DAYS"})
	c.Assert(err, qt.IsNil)
	request := &protocol.ReportValuesRequest{Engine: "spanner", TargetFeatures: backend.Description().GetEngines()[0].GetFeatures(),
		Values: []*protocol.ValueInput{{Id: "v", Representation: protocol.ValueInput_OBSERVED, Value: value.GetValue()}}}
	c.Assert(provider.ValidateReportValuesRequest(backend.Description(), request), qt.IsNil)
	result, err := tablefacets.Report(context.Background(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(provider.ValidateValueReportsReply(backend.Description(), request, result), qt.IsNil)
	c.Assert(result.GetValues()[0].GetCounts(), qt.HasLen, 1)
	c.Assert(result.GetValues()[0].GetCounts()[0].GetName(), qt.Equals, "spanner_row_deletion_policy_tables")
	c.Assert(result.GetValues()[0].GetCounts()[0].GetValue(), qt.Equals, uint64(1))
}
