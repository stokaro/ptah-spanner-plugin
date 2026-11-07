// Package tableplan plans the tables of the spanner engine: their creation,
// the changes of a table the plan keeps, and their removal. It is the
// provider's table lifecycle service, which the host hands every table it
// creates, changes or drops, with the host's placeholder steps for each.
//
// Each statement is a step of its own with a tableop operation, because
// Spanner runs one action per ALTER TABLE and every schema statement outside
// a transaction. A step replaces the host placeholder it stands for, or joins
// the plan beside them, ordered by explicit dependencies: an index after its
// table, a foreign key after both its tables, a column change between the
// index removals and additions of its table, the removal of an index before
// its table's.
//
// A change Spanner cannot make is refused here, before a statement is sent:
// a change of a primary key, a type change other than between character
// varying and bytea or on a key column, and a column type Spanner does not
// have. The refusal names the table and why.
package tableplan

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-spanner-plugin/internal/rowdeletion"
	"github.com/stokaro/ptah-spanner-plugin/internal/tableop"
)

// ServiceID names the table lifecycle service.
const ServiceID = "spanner-tables"

// ParentKinds are the table settings the service owns, by which the host
// assigns it the tables of the engine.
func ParentKinds() []string { return []string{rowdeletion.Kind} }

// The ranks order steps that nothing else orders: the host schedules ready
// steps by owner and then by name, so a table's creation comes before an
// index, and an index before a constraint, as Ptah's built-in Spanner engine
// wrote them.
const (
	rankTable      = "1-table"
	rankColumn     = "2-column"
	rankIndex      = "3-index"
	rankConstraint = "4-constraint"
)

// planner is the state of one planning request.
type planner struct {
	request      *protocol.PlanFeaturesRequest
	contribution *protocol.PlanningContribution
	result       *protocol.PlannedFeatures
	// receipts are the steps each table's parent receipt names, by the
	// table's index in the request.
	receipts map[int][]*protocol.StepIdentity
	// created is the step that creates each table the plan creates.
	created map[tableKey]*protocol.StepIdentity
	// tables are the tables of the request by key.
	tables map[tableKey]*protocol.PlanningTable
}

// tableKey names a table by its schema and its name, which the engine
// compares exactly.
type tableKey struct{ schema, name string }

// refusal is a change the service cannot plan, at the table index of the
// request it belongs to.
type refusal struct {
	table   int
	code    protocol.PlanningDiagnostic_Code
	message string
}

func (r *refusal) Error() string { return r.message }

// Plan plans every table the request creates, changes or drops, and returns
// the steps, the rewrites that replace the host's placeholders, and a receipt
// for each table and setting the service owns. A change the service cannot
// plan returns a completed refusal naming its table.
func Plan(ctx context.Context, request *protocol.PlanFeaturesRequest) (*protocol.PlannedFeatures, error) {
	if request.GetPlanningId() != ServiceID {
		return nil, fmt.Errorf("the request names another planning service")
	}
	p := &planner{
		request:      request,
		contribution: &protocol.PlanningContribution{Owner: tableop.ProviderID},
		result:       &protocol.PlannedFeatures{Complete: true},
		receipts:     make(map[int][]*protocol.StepIdentity),
		created:      make(map[tableKey]*protocol.StepIdentity),
		tables:       make(map[tableKey]*protocol.PlanningTable),
	}
	for _, table := range request.GetTables() {
		p.tables[keyOf(table.GetSubject())] = table
	}
	// Creations first, so a foreign key of one created table can name the
	// creation of another whatever their order in the request.
	for _, action := range []protocol.PlanningTable_Action{protocol.PlanningTable_CREATE, protocol.PlanningTable_ALTER, protocol.PlanningTable_DROP,
		protocol.PlanningTable_REBUILD} {
		for i, table := range request.GetTables() {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if table.GetAction() != action {
				continue
			}
			if err := p.plan(i, table); err != nil {
				return refused(err)
			}
		}
	}
	for i, table := range request.GetTables() {
		if table.GetAction() != protocol.PlanningTable_CREATE {
			continue
		}
		if err := p.constraints(i, table); err != nil {
			return refused(err)
		}
	}
	if err := p.claimRest(); err != nil {
		return refused(err)
	}
	for i, table := range request.GetTables() {
		if table.GetAction() == protocol.PlanningTable_CONTEXT {
			continue
		}
		for _, kind := range request.GetParentKinds() {
			p.result.Parents = append(p.result.Parents, &protocol.PlanningParentReceipt{Action: table.GetAction(), Receipt: &protocol.PlanningReceipt{
				Subject: proto.CloneOf(table.GetSubject()), Kind: kind, Strategy: strategy(table.GetAction()), Steps: cloneSteps(p.receipts[i]),
			}})
		}
	}
	if len(p.contribution.GetSteps()) > 0 {
		p.result.Contributions = []*protocol.PlanningContribution{p.contribution}
	}
	return p.result, ctx.Err()
}

func refused(err error) (*protocol.PlannedFeatures, error) {
	declined, ok := errors.AsType[*refusal](err)
	if !ok {
		return nil, err
	}
	return &protocol.PlannedFeatures{Complete: true, Diagnostics: []*protocol.PlanningDiagnostic{{
		Code: declined.code, Kind: rowdeletion.Kind, Message: declined.message,
		Input: &protocol.PlanningDiagnostic_Parent{Parent: uint32(declined.table)}, // #nosec G115 -- a request holds far fewer tables than math.MaxUint32
	}}}, nil
}

func strategy(action protocol.PlanningTable_Action) string {
	switch action {
	case protocol.PlanningTable_CREATE:
		return "Create the table, then its indexes and constraints"
	case protocol.PlanningTable_DROP:
		return "Drop the table's indexes, then the table"
	default:
		return "Change the table one statement at a time"
	}
}

func (p *planner) plan(index int, table *protocol.PlanningTable) error {
	switch table.GetAction() {
	case protocol.PlanningTable_CREATE:
		return p.create(index, table)
	case protocol.PlanningTable_ALTER:
		return p.alter(index, table)
	case protocol.PlanningTable_DROP:
		return p.drop(index, table)
	default:
		return &refusal{table: index, code: protocol.PlanningDiagnostic_UNSUPPORTED_FEATURE,
			message: fmt.Sprintf("table %s: Spanner cannot rebuild a table, and the provider plans no rebuild", phrase(table.GetSubject()))}
	}
}

// create replaces the host's creation of a table with CREATE TABLE and adds
// a step for each index the table declares.
func (p *planner) create(index int, table *protocol.PlanningTable) error {
	desired := table.GetDesired()
	if desired == nil || table.GetCurrent() != nil {
		return fmt.Errorf("a created table needs its declaration and no observation")
	}
	common := p.commonStep(table.GetSubject(), protocol.PlanningEffect_CREATE, "table")
	if common == nil {
		return fmt.Errorf("the host planned no creation of table %s", phrase(table.GetSubject()))
	}
	operation, err := createTable(table.GetSubject(), desired)
	if err != nil {
		return &refusal{table: index, code: protocol.PlanningDiagnostic_INVALID_SCHEMA, message: err.Error()}
	}
	step, err := p.replace(index, common, rankTable, operation, protocol.NativeOperation_STATEMENT)
	if err != nil {
		return err
	}
	p.created[keyOf(table.GetSubject())] = step
	for _, declared := range desired.GetIndexes() {
		created, err := indexOperation(declared)
		if err != nil {
			return &refusal{table: index, code: protocol.PlanningDiagnostic_UNSUPPORTED_FEATURE, message: fmt.Sprintf("table %s: %v", phrase(table.GetSubject()), err)}
		}
		id, err := p.add(index, table.GetSubject(), rankIndex, created, nil, readTable(table.GetSubject()), effect(indexSubject(table.GetSubject(), declared.GetName()), protocol.PlanningEffect_CREATE))
		if err != nil {
			return err
		}
		p.depend(step, id)
	}
	return nil
}

// constraints adds a step for each foreign key and table CHECK constraint a
// created table declares, after the creation of the table and of every
// created table it references. A foreign key is a statement of its own after
// both tables, so two tables may reference each other.
func (p *planner) constraints(index int, table *protocol.PlanningTable) error {
	subject := table.GetSubject()
	keys, err := foreignKeys(subject, table.GetDesired())
	if err != nil {
		return &refusal{table: index, code: protocol.PlanningDiagnostic_UNSUPPORTED_FEATURE, message: err.Error()}
	}
	for _, key := range keys {
		effects := []*protocol.PlanningEffect{readTable(subject), effect(childSubject(subject, "constraint", key.Name), protocol.PlanningEffect_CREATE)}
		referenced := tableKey{schema: key.RefSchema, name: key.RefTable}
		if target, found := p.tables[referenced]; found && referenced != keyOf(subject) {
			effects = append(effects, readTable(target.GetSubject()))
		}
		id, err := p.add(index, subject, rankConstraint, tableop.Operation{Action: tableop.AddForeignKey, ForeignKey: &key}, nil, effects...)
		if err != nil {
			return err
		}
		p.depend(p.created[keyOf(subject)], id)
		if creation, found := p.created[referenced]; found && referenced != keyOf(subject) {
			p.depend(creation, id)
		}
	}
	checks, err := tableChecks(subject, table.GetDesired())
	if err != nil {
		return &refusal{table: index, code: protocol.PlanningDiagnostic_UNSUPPORTED_FEATURE, message: err.Error()}
	}
	for _, check := range checks {
		id, err := p.add(index, subject, rankConstraint, check, nil, readTable(subject), effect(childSubject(subject, "constraint", check.Name), protocol.PlanningEffect_CREATE))
		if err != nil {
			return err
		}
		p.depend(p.created[keyOf(subject)], id)
	}
	return nil
}

// drop replaces the host's removal of a table with DROP TABLE, after a step
// that drops each index the table holds: Spanner refuses to drop a table
// that has an index (`Cannot drop table t with indices: t_i`).
func (p *planner) drop(index int, table *protocol.PlanningTable) error {
	current := table.GetCurrent()
	if current == nil {
		return fmt.Errorf("a dropped table needs its observation")
	}
	common := p.commonStep(table.GetSubject(), protocol.PlanningEffect_DROP, "table")
	if common == nil {
		return fmt.Errorf("the host planned no removal of table %s", phrase(table.GetSubject()))
	}
	step, err := p.replace(index, common, rankTable, tableop.Operation{Action: tableop.DropTable}, protocol.NativeOperation_STATEMENT)
	if err != nil {
		return err
	}
	for _, held := range current.GetIndexes() {
		id, err := p.add(index, table.GetSubject(), rankIndex, tableop.Operation{Action: tableop.DropIndex, Name: held.GetName()}, nil,
			readTable(table.GetSubject()), effect(indexSubject(table.GetSubject(), held.GetName()), protocol.PlanningEffect_DROP))
		if err != nil {
			return err
		}
		p.depend(id, step)
	}
	return nil
}

// alter plans a table the plan keeps: it claims the host's steps that add
// and drop its columns and indexes, and adds a step for each change of a
// column's type, default and nullability. A change of the primary key is
// refused.
func (p *planner) alter(index int, table *protocol.PlanningTable) error {
	desired, current := table.GetDesired(), table.GetCurrent()
	if desired == nil || current == nil {
		return fmt.Errorf("an altered table needs its declaration and its observation")
	}
	subject := table.GetSubject()
	if declared, held := primaryKey(desired), heldPrimaryKey(current); !slices.Equal(declared, held) {
		return &refusal{table: index, code: protocol.PlanningDiagnostic_UNSUPPORTED_FEATURE, message: fmt.Sprintf(
			"changing the primary key of table %s from (%s) to (%s): Spanner cannot change a table's primary key; create a new table with the key and copy the rows",
			phrase(subject), strings.Join(held, ", "), strings.Join(declared, ", "))}
	}
	var removals, additions, alterations []*protocol.StepIdentity
	for _, common := range p.request.GetCommonSteps() {
		if !proto.Equal(common.GetParent(), subject) || common.GetCommonObject() != nil {
			continue
		}
		step, kind, err := p.claimAlteration(index, table, common)
		if err != nil {
			return err
		}
		switch kind {
		case "index-removal":
			removals = append(removals, step)
		case "index-addition":
			additions = append(additions, step)
		case "column-addition":
			alterations = append(alterations, step)
		}
	}
	changed, err := p.columnChanges(index, table)
	if err != nil {
		return err
	}
	// A column changes after the indexes it may lose are gone and before the
	// indexes it may gain are built.
	for _, step := range slices.Concat(changed, alterations) {
		for _, removal := range removals {
			p.depend(removal, step)
		}
		for _, addition := range additions {
			p.depend(step, addition)
		}
	}
	return nil
}

// claimAlteration replaces one host step of an altered table and reports
// what it stood for.
func (p *planner) claimAlteration(index int, table *protocol.PlanningTable, common *protocol.CommonPlanningStep) (*protocol.StepIdentity, string, error) {
	subject := table.GetSubject()
	if added := common.GetAddedColumn(); added != nil {
		return p.addColumn(index, table, common, added)
	}
	if rename := common.GetRenamedIndex(); rename != nil {
		return nil, "", &refusal{table: index, code: protocol.PlanningDiagnostic_UNSUPPORTED_FEATURE,
			message: fmt.Sprintf("table %s: the provider does not rename index %q", phrase(subject), rename.GetFrom())}
	}
	for _, change := range common.GetMetadata().GetEffects() {
		target := change.GetSubject()
		switch {
		case target.GetKind() == "column" && change.GetAction() == protocol.PlanningEffect_DROP:
			step, err := p.replace(index, common, rankColumn, tableop.Operation{Action: tableop.DropColumn, Name: target.GetName().GetSource()}, protocol.NativeOperation_ALTER_TABLE)
			return step, "column-removal", err
		case target.GetKind() == "index" && change.GetAction() == protocol.PlanningEffect_DROP:
			step, err := p.replace(index, common, rankIndex, tableop.Operation{Action: tableop.DropIndex, Name: target.GetName().GetSource()}, protocol.NativeOperation_STATEMENT)
			return step, "index-removal", err
		case target.GetKind() == "index" && change.GetAction() == protocol.PlanningEffect_CREATE:
			declared := declaredIndex(table.GetDesired(), target.GetName().GetSource())
			if declared == nil {
				return nil, "", fmt.Errorf("table %s does not declare the index %q the host adds", phrase(subject), target.GetName().GetSource())
			}
			operation, err := indexOperation(declared)
			if err != nil {
				return nil, "", &refusal{table: index, code: protocol.PlanningDiagnostic_UNSUPPORTED_FEATURE, message: fmt.Sprintf("table %s: %v", phrase(subject), err)}
			}
			step, err := p.replace(index, common, rankIndex, operation, protocol.NativeOperation_STATEMENT)
			return step, "index-addition", err
		}
	}
	return nil, "", fmt.Errorf("table %s has a host step the provider does not plan", phrase(subject))
}

// addColumn replaces the host's addition of a column. A NOT NULL column
// without a default is added nullable and then made NOT NULL, because
// Spanner refuses to add it otherwise; the second step carries the note
// Ptah's built-in Spanner engine wrote.
func (p *planner) addColumn(index int, table *protocol.PlanningTable, common *protocol.CommonPlanningStep, added *protocol.PlanningColumn) (*protocol.StepIdentity, string, error) {
	subject := table.GetSubject()
	declared := declaredColumn(table.GetDesired(), added.GetName())
	if declared == nil {
		return nil, "", fmt.Errorf("table %s does not declare the column %q the host adds", phrase(subject), added.GetName())
	}
	column, err := ddlColumn(subject, table.GetDesired(), declared)
	if err != nil {
		return nil, "", &refusal{table: index, code: protocol.PlanningDiagnostic_INVALID_SCHEMA, message: err.Error()}
	}
	if column.Check != "" {
		return nil, "", &refusal{table: index, code: protocol.PlanningDiagnostic_UNSUPPORTED_FEATURE, message: fmt.Sprintf(
			"adding column %q with a CHECK constraint to table %s: the provider does not read CHECK constraints back yet, so it adds none to an existing table",
			column.Name, phrase(subject))}
	}
	if declared.GetForeignReference() != "" {
		return nil, "", &refusal{table: index, code: protocol.PlanningDiagnostic_UNSUPPORTED_FEATURE, message: fmt.Sprintf(
			"adding column %q with a foreign key to table %s: the provider does not read foreign keys back yet, so it adds none to an existing table",
			column.Name, phrase(subject))}
	}
	notNull := !column.Nullable && column.Default == nil
	if notNull {
		column.Nullable = true
	}
	step, err := p.replace(index, common, rankColumn, tableop.Operation{Action: tableop.AddColumn, Column: &column}, protocol.NativeOperation_ALTER_TABLE)
	if err != nil || !notNull {
		return step, "column-addition", err
	}
	id, err := p.add(index, subject, rankColumn, tableop.Operation{Action: tableop.SetNotNull, Name: column.Name}, []string{notNullNote(subject, column.Name)},
		readTable(subject), effect(childSubject(subject, "column", column.Name), protocol.PlanningEffect_ALTER))
	if err != nil {
		return nil, "", err
	}
	p.depend(step, id)
	return id, "column-addition", nil
}

// columnChanges adds a step for each change of type, default and
// nullability of a column both sides hold.
func (p *planner) columnChanges(index int, table *protocol.PlanningTable) ([]*protocol.StepIdentity, error) {
	subject, desired, current := table.GetSubject(), table.GetDesired(), table.GetCurrent()
	keys := primaryKey(desired)
	var steps []*protocol.StepIdentity
	for _, declared := range desired.GetColumns() {
		held := observedColumn(current, declared.GetName())
		if held == nil {
			continue
		}
		operations, notes, err := alterColumn(subject, desired, declared, held, keys)
		if err != nil {
			return nil, &refusal{table: index, code: protocol.PlanningDiagnostic_UNSUPPORTED_FEATURE, message: err.Error()}
		}
		for i, operation := range operations {
			id, err := p.add(index, subject, rankColumn, operation, notes[i], readTable(subject),
				effect(childSubject(subject, "column", declared.GetName()), protocol.PlanningEffect_ALTER))
			if err != nil {
				return nil, err
			}
			steps = append(steps, id)
		}
	}
	return steps, nil
}

// claimRest refuses a host step of a table the service plans that no step
// replaced, naming it, rather than leave it for the host to refuse without
// the provider's words.
func (p *planner) claimRest() error {
	claimed := make(map[string]bool)
	for _, rewrite := range p.result.GetRewrites() {
		for _, source := range rewrite.GetSources() {
			claimed[source.GetName()] = true
		}
	}
	for _, common := range p.request.GetCommonSteps() {
		parent := common.GetParent()
		if parent == nil || common.GetCommonObject() != nil || claimed[common.GetId().GetName()] {
			continue
		}
		table, found := p.tables[keyOf(parent)]
		if !found || table.GetAction() == protocol.PlanningTable_CONTEXT {
			continue
		}
		return fmt.Errorf("the provider left the host step %s of table %s unplanned", common.GetId().GetName(), phrase(parent))
	}
	return nil
}

// commonStep finds the host step that does action to the table subject
// itself.
func (p *planner) commonStep(subject *protocol.ComparisonIdentity, action protocol.PlanningEffect_Action, kind string) *protocol.CommonPlanningStep {
	for _, common := range p.request.GetCommonSteps() {
		if !proto.Equal(common.GetParent(), subject) || common.GetCommonObject() != nil {
			continue
		}
		for _, change := range common.GetMetadata().GetEffects() {
			if change.GetAction() == action && change.GetSubject().GetKind() == kind && proto.Equal(change.GetSubject(), subject) {
				return common
			}
		}
	}
	return nil
}

// replace adds a step that does operation in place of the host step common,
// with that step's effects.
func (p *planner) replace(index int, common *protocol.CommonPlanningStep, rank string, operation tableop.Operation, role protocol.NativeOperation_Role) (*protocol.StepIdentity, error) {
	subject := p.request.GetTables()[index].GetSubject()
	id := p.stepID(rank, subject)
	value, err := tableop.Capture(id.GetName(), owner(subject), operation)
	if err != nil {
		return nil, err
	}
	metadata := proto.CloneOf(common.GetMetadata())
	metadata.Transaction = protocol.PlanningMetadata_FORBIDDEN
	metadata.Impact, metadata.Reason = value.GetOperationSemantics().GetImpact(), value.GetOperationSemantics().GetReason()
	p.contribution.Steps = append(p.contribution.Steps, &protocol.PlanningStep{Id: id, Metadata: metadata, Operation: planningOperation(subject, value, role, nil)})
	p.result.Rewrites = append(p.result.Rewrites, &protocol.PlanningRewrite{Sources: []*protocol.StepIdentity{proto.CloneOf(common.GetId())}, Replacement: proto.CloneOf(id)})
	p.receipts[index] = append(p.receipts[index], proto.CloneOf(id))
	return id, nil
}

// add adds a step that does operation to the table subject beside the host's
// steps, with effects.
func (p *planner) add(index int, subject *protocol.ComparisonIdentity, rank string, operation tableop.Operation, notes []string, effects ...*protocol.PlanningEffect) (*protocol.StepIdentity, error) {
	id := p.stepID(rank, subject)
	value, err := tableop.Capture(id.GetName(), owner(subject), operation)
	if err != nil {
		return nil, err
	}
	role := protocol.NativeOperation_ALTER_TABLE
	switch operation.Action {
	case tableop.CreateIndex, tableop.DropIndex:
		role = protocol.NativeOperation_STATEMENT
	}
	metadata := &protocol.PlanningMetadata{Transaction: protocol.PlanningMetadata_FORBIDDEN, Effects: effects,
		Impact: value.GetOperationSemantics().GetImpact(), Reason: value.GetOperationSemantics().GetReason()}
	p.contribution.Steps = append(p.contribution.Steps, &protocol.PlanningStep{Id: id, Metadata: metadata, Operation: planningOperation(subject, value, role, notes)})
	p.receipts[index] = append(p.receipts[index], proto.CloneOf(id))
	return id, nil
}

// stepID names the next step on the table subject under rank. The step
// count keeps names distinct and ordered as they were planned.
func (p *planner) stepID(rank string, subject *protocol.ComparisonIdentity) *protocol.StepIdentity {
	return &protocol.StepIdentity{Owner: tableop.ProviderID,
		Name: fmt.Sprintf("%s/%04d/%s.%s", rank, len(p.contribution.GetSteps()), subject.GetSchema().GetSource(), subject.GetName().GetSource())}
}

func (p *planner) depend(before, after *protocol.StepIdentity) {
	if before == nil || after == nil {
		return
	}
	p.contribution.Dependencies = append(p.contribution.Dependencies, &protocol.PlanningDependency{Before: proto.CloneOf(before), After: proto.CloneOf(after)})
}

// planningOperation is the operation of a step. Every step runs in the
// lifecycle phase, which spans the host's whole sequence, so its effects and
// dependencies alone place it: an index replaced under its own name is
// dropped and created again by two steps, which the default phase refuses.
func planningOperation(subject *protocol.ComparisonIdentity, value *protocol.NormalizedValue, role protocol.NativeOperation_Role, notes []string) *protocol.PlanningOperation {
	operation := &protocol.PlanningOperation{Value: value, Role: role, Phase: protocol.PlanningOperation_LIFECYCLE, Notes: notes}
	if role == protocol.NativeOperation_ALTER_TABLE {
		operation.Parent = proto.CloneOf(subject)
	}
	return operation
}

func owner(subject *protocol.ComparisonIdentity) *protocol.Reference {
	return tableop.Owner(subject.GetSchema().GetSource(), subject.GetName().GetSource())
}

func keyOf(subject *protocol.ComparisonIdentity) tableKey {
	return tableKey{schema: subject.GetSchema().GetSource(), name: subject.GetName().GetSource()}
}

// phrase names a table for a message, as Ptah's host names one.
func phrase(subject *protocol.ComparisonIdentity) string {
	if schema := subject.GetSchema().GetSource(); schema != "" && schema != "public" {
		return fmt.Sprintf("%q.%q", schema, subject.GetName().GetSource())
	}
	return fmt.Sprintf("%q", subject.GetName().GetSource())
}

func readTable(subject *protocol.ComparisonIdentity) *protocol.PlanningEffect {
	return effect(subject, protocol.PlanningEffect_READ)
}

func effect(subject *protocol.ComparisonIdentity, action protocol.PlanningEffect_Action) *protocol.PlanningEffect {
	return &protocol.PlanningEffect{Subject: proto.CloneOf(subject), Action: action}
}

// childSubject is the identity of a column or a constraint of the table
// subject, built as the host builds one: the table's database, catalog and
// schema, the table's name as the parent, and the child's exact name.
func childSubject(subject *protocol.ComparisonIdentity, kind, name string) *protocol.ComparisonIdentity {
	child := indexSubject(subject, name)
	child.Kind, child.Parent = kind, proto.CloneOf(subject.GetName())
	return child
}

// indexSubject is the identity of an index of the table subject. An index
// name is unique in its schema, so the identity names no parent table.
func indexSubject(subject *protocol.ComparisonIdentity, name string) *protocol.ComparisonIdentity {
	return &protocol.ComparisonIdentity{Kind: "index", Database: proto.CloneOf(subject.GetDatabase()), Catalog: proto.CloneOf(subject.GetCatalog()),
		Schema: proto.CloneOf(subject.GetSchema()), Name: &protocol.ComparisonPart{Source: name, Normalized: name}}
}

func cloneSteps(steps []*protocol.StepIdentity) []*protocol.StepIdentity {
	result := make([]*protocol.StepIdentity, len(steps))
	for i, step := range steps {
		result[i] = proto.CloneOf(step)
	}
	return result
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

func declaredIndex(table *protocol.DeclaredTable, name string) *protocol.DeclaredIndex {
	for _, index := range table.GetIndexes() {
		if index.GetName() == name {
			return index
		}
	}
	return nil
}

// notNullNote is the note a SET NOT NULL carries when the column declares no
// default, in the words of Ptah's built-in Spanner engine.
func notNullNote(subject *protocol.ComparisonIdentity, column string) string {
	return fmt.Sprintf("SPANNER: SET NOT NULL fails if any row of %q holds NULL in %q; the column declares no default to fill it with.",
		subject.GetName().GetSource(), column)
}

// heldType is the full type a read reports for a column.
func heldType(column *protocol.ObservedColumn) string {
	if column.GetColumnType() != "" {
		return column.GetColumnType()
	}
	return column.GetDataType()
}

// canonicalHeld is the canonical spelling of a held type, which a read
// reports already canonical.
func canonicalHeld(column *protocol.ObservedColumn) string {
	return strings.ToLower(strings.TrimSpace(heldType(column)))
}
