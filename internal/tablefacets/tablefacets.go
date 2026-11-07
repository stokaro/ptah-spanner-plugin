// Package tablefacets compares, reverses and counts the table facets the
// provider owns: a table's row deletion policy. Each service is offline and
// reads no database; the host sends the captured values and the coverage that
// says what each side knows.
//
// A table only one side holds carries its policy with it: CREATE TABLE writes
// the declared policy and DROP TABLE removes the held one, so the comparison
// reports changes only for tables both sides hold.
package tablefacets

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-spanner-plugin/internal/rowdeletion"
	"github.com/stokaro/ptah-spanner-plugin/internal/tableop"
)

// ComparisonID names the comparison of row deletion policies.
const ComparisonID = "spanner-row-deletion-policies"

// ReversalID names the reversal of row deletion policy changes.
const ReversalID = "spanner-row-deletion-policies"

// MetricTables is the metric a policy counts toward, as Ptah's built-in
// Spanner engine named it.
const MetricTables = "spanner_row_deletion_policy_tables"

// Comparison is the comparison service the engine declares.
func Comparison() *protocol.ModelComparison {
	return &protocol.ModelComparison{Id: ComparisonID, Placement: protocol.ModelComparison_FACETS, Kinds: []string{rowdeletion.Kind},
		ChangeKinds: []string{rowdeletion.ChangeKind}, OwnerKinds: []string{"table"}}
}

// Reversal is the reversal service the engine declares.
func Reversal() *protocol.ModelReversal {
	return &protocol.ModelReversal{Id: ReversalID, ChangeKinds: []string{rowdeletion.ChangeKind}}
}

// Reports are the counts the engine declares for its desired and observed
// policies.
func Reports() []*protocol.ModelReport {
	var reports []*protocol.ModelReport
	for _, representation := range []protocol.ValueInput_Representation{protocol.ValueInput_DESIRED, protocol.ValueInput_OBSERVED} {
		reports = append(reports, &protocol.ModelReport{Kind: rowdeletion.Kind, Representation: representation,
			Metrics: []*protocol.ModelMetric{{Name: MetricTables, Help: "Tables with a captured Spanner row deletion policy"}}})
	}
	return reports
}

// Compare reports a change for every table both sides hold whose policy
// differs: the column compared exactly and the interval by the hours it
// denotes. A table the declaration gives no policy is declared to have none
// only where the desired coverage establishes it, as a Go or YAML source
// does; elsewhere the held policy is left alone. A declared policy on a table
// the read did not establish is undecided.
func Compare(ctx context.Context, request *protocol.CompareModelsRequest) (*protocol.ComparedModels, error) {
	if request.GetComparisonId() != ComparisonID {
		return nil, fmt.Errorf("the request names another comparison")
	}
	desired := records(request, request.GetDesired())
	current := records(request, request.GetCurrent())
	result := &protocol.ComparedModels{Complete: true, Desired: proto.CloneOf(request.GetDesired())}
	parents := slices.Clone(request.GetParents())
	slices.SortFunc(parents, func(a, b *protocol.ComparisonParent) int {
		return provider.CompareIdentities(a.GetSubject(), b.GetSubject())
	})
	for _, parent := range parents {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		subject := parent.GetSubject()
		if subject.GetKind() != "table" || !parent.GetDesired() || !parent.GetCurrent() {
			continue
		}
		key := identityKey(subject)
		var change rowdeletion.Change
		want, wanted := desired[key]
		held, holds := current[key]
		switch {
		case wanted && holds:
			if rowdeletion.Equivalent(want, held) {
				continue
			}
			change = rowdeletion.Change{Before: &held, After: &want}
		case wanted:
			if !known(request.GetCurrent().GetCoverage(), subject, observedSide) {
				result.Undecided = append(result.Undecided, &protocol.ComparisonUndecided{Kind: rowdeletion.Kind, Subject: proto.CloneOf(subject),
					Reason: "The read did not establish whether the table holds a row deletion policy"})
				continue
			}
			change = rowdeletion.Change{After: &want}
		case holds:
			if !known(request.GetDesired().GetCoverage(), subject, desiredSide) {
				continue
			}
			change = rowdeletion.Change{Before: &held}
		default:
			continue
		}
		value, err := rowdeletion.CaptureChange("change-"+strconv.Itoa(len(result.Changes)), owner(subject), change)
		if err != nil {
			return nil, err
		}
		result.Changes = append(result.Changes, &protocol.ComparedChange{Kind: rowdeletion.Kind, Subject: proto.CloneOf(subject), Value: value})
	}
	return result, ctx.Err()
}

// records reads the policies a side holds that the comparison compares, by
// their table.
func records(request *protocol.CompareModelsRequest, state *protocol.ComparisonState) map[string]rowdeletion.Policy {
	policies := make(map[string]rowdeletion.Policy)
	if !slices.Contains(request.GetKinds(), rowdeletion.Kind) {
		return policies
	}
	for _, record := range state.GetRecords() {
		if record.GetKind() != rowdeletion.Kind || record.GetValue() == nil || !forSpanner(record.GetTargets()) {
			continue
		}
		policy, err := rowdeletion.PolicyOf(record.GetValue())
		if err != nil {
			continue
		}
		policies[identityKey(record.GetSubject())] = policy
	}
	return policies
}

// known reports whether a side's coverage establishes what the side says
// about the policy of subject, so that a policy it does not carry is a known
// absence. A subject record decides for its subject. Otherwise every source of
// the kind must establish it: a region that covers the subject decides, an
// unknown one first, and the source's outside knowledge decides where no
// region covers it. A read narrowed to schemas is such a source: unknown
// outside, complete in each schema it read. A defaulted claim establishes a
// desired absence only.
func known(coverage *protocol.ComparisonCoverage, subject *protocol.ComparisonIdentity, side side) bool {
	for _, record := range coverage.GetSubjects() {
		if record.GetKind() == rowdeletion.Kind && provider.CompareIdentities(record.GetSubject(), subject) == 0 && forSpanner(record.GetTargets()) {
			return established(record.GetKnowledge(), side)
		}
	}
	for _, record := range coverage.GetKinds() {
		if record.GetModel().GetKind() != rowdeletion.Kind {
			continue
		}
		if len(record.GetSources()) == 0 {
			return false
		}
		for _, source := range record.GetSources() {
			if !sourceEstablishes(source, subject, side) {
				return false
			}
		}
		return true
	}
	return false
}

// sourceEstablishes reports whether one knowledge source establishes the
// policy of subject.
func sourceEstablishes(source *protocol.ComparisonKnowledgeSource, subject *protocol.ComparisonIdentity, side side) bool {
	covered := false
	for _, region := range source.GetRegions() {
		if !regionCovers(region.GetScope(), subject) {
			continue
		}
		if !established(region.GetKnowledge(), side) {
			return false
		}
		covered = true
	}
	return covered || established(source.GetOutside(), side)
}

// regionCovers reports whether a coverage region names subject: its targets
// and subject kinds, where it lists them, admit the subject, and each part it
// states matches the subject's. An absent part matches anything; a present
// empty one matches only an empty part.
func regionCovers(scope *protocol.ComparisonNamespace, subject *protocol.ComparisonIdentity) bool {
	if len(scope.GetTargets()) > 0 && !forSpanner(scope.GetTargets()) {
		return false
	}
	if len(scope.GetSubjectKinds()) > 0 && !slices.Contains(scope.GetSubjectKinds(), subject.GetKind()) {
		return false
	}
	for _, pair := range [][2]*protocol.ComparisonPart{{scope.GetDatabase(), subject.GetDatabase()}, {scope.GetCatalog(), subject.GetCatalog()},
		{scope.GetSchema(), subject.GetSchema()}, {scope.GetParent(), subject.GetParent()}} {
		if pair[0] != nil && pair[0].GetNormalized() != pair[1].GetNormalized() {
			return false
		}
	}
	prefix, path := scope.GetPathPrefix(), subject.GetPath()
	if len(prefix) > len(path) || scope.GetPathExact() && len(prefix) != len(path) {
		return false
	}
	for i, part := range prefix {
		if part.GetNormalized() != path[i].GetNormalized() {
			return false
		}
	}
	return true
}

// side is the captured side of a comparison a coverage describes.
type side int

const (
	// observedSide is what the database holds, as a read captured it.
	observedSide side = iota
	// desiredSide is what the declarations ask for.
	desiredSide
)

func established(knowledge *protocol.ModelKnowledge, side side) bool {
	switch knowledge.GetState() {
	case protocol.ModelKnowledge_COMPLETE, protocol.ModelKnowledge_ABSENT:
		return true
	case protocol.ModelKnowledge_DEFAULTED:
		return side == desiredSide
	default:
		return false
	}
}

// forSpanner reports whether a value scoped to targets applies to Spanner. A
// value scoped to no target applies to every engine.
func forSpanner(targets []string) bool {
	return len(targets) == 0 || slices.ContainsFunc(targets, func(target string) bool {
		return strings.EqualFold(strings.TrimSpace(target), tableop.Engine)
	})
}

// owner is the table a change names, as the comparison identity names it.
func owner(subject *protocol.ComparisonIdentity) *protocol.Reference {
	return &protocol.Reference{Engine: tableop.Engine, Kind: "table", Database: subject.GetDatabase().GetSource(), Schema: subject.GetSchema().GetSource(),
		Name: subject.GetName().GetSource()}
}

// identityKey names a table by the normalized parts of its identity.
func identityKey(subject *protocol.ComparisonIdentity) string {
	var key strings.Builder
	for _, part := range []*protocol.ComparisonPart{subject.GetDatabase(), subject.GetCatalog(), subject.GetSchema(), subject.GetName()} {
		key.WriteString(strconv.Quote(part.GetNormalized()))
	}
	return key.String()
}

// Reverse undoes each change: the policy the table held is restored in place,
// or removed when the forward change added the first one. The forward state
// is the policy the forward change leaves, as the server would report it. A
// forward change that sets a policy cannot bring back the rows Spanner deleted
// under it, which the reverse says.
func Reverse(ctx context.Context, request *protocol.ReverseChangesRequest) (*protocol.ReversedChanges, error) {
	if request.GetReversalId() != ReversalID {
		return nil, fmt.Errorf("the request names another reversal")
	}
	result := &protocol.ReversedChanges{Complete: true}
	for i, planned := range request.GetChanges() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		forward, err := rowdeletion.ChangeOf(planned.GetValue())
		if err != nil {
			return nil, err
		}
		table := planned.GetValue().GetValue().GetOwner()
		reversed := &protocol.ReversedChange{Subject: proto.CloneOf(planned.GetSubject()), Strategy: "Restore the table's prior row deletion policy in place"}
		if forward.Before == nil {
			reversed.Strategy = "Remove the row deletion policy the forward change added"
		}
		value, err := rowdeletion.CaptureChange("reverse-"+strconv.Itoa(i), table, rowdeletion.Change{Before: forward.After, After: forward.Before})
		if err != nil {
			return nil, err
		}
		reversed.Value = value
		state := &protocol.ForwardState{Placement: protocol.ForwardState_FACET, Kind: rowdeletion.Kind}
		if forward.After != nil {
			if state.Value, err = rowdeletion.Facet("forward-"+strconv.Itoa(i), protocol.ValueInput_OBSERVED, table, *forward.After); err != nil {
				return nil, err
			}
			reversed.Limitations = []string{"Restoring the prior row deletion policy cannot recover rows the server deleted under the forward policy."}
		}
		reversed.ForwardState = []*protocol.ForwardState{state}
		result.Changes = append(result.Changes, reversed)
	}
	return result, ctx.Err()
}

// Report counts each policy once toward MetricTables.
func Report(ctx context.Context, request *protocol.ReportValuesRequest) (*protocol.ValueReports, error) {
	result := &protocol.ValueReports{Complete: true}
	for _, value := range request.GetValues() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if value.GetValue().GetKind() != rowdeletion.Kind {
			return nil, fmt.Errorf("the provider counts row deletion policies only")
		}
		result.Values = append(result.Values, &protocol.ValueReport{Id: value.GetId(), Counts: []*protocol.MetricCount{{Name: MetricTables, Value: 1}}})
	}
	return result, nil
}
