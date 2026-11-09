package tableplan

import (
	"slices"
	"strings"

	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-spanner-plugin/internal/checkexpr"
	"github.com/stokaro/ptah-spanner-plugin/internal/tableop"
)

// The constraint types a read reports and the planner changes; a table's
// primary key is planned on its own.
const (
	foreignKeyType = "FOREIGN KEY"
	checkType      = "CHECK"
)

// keyDrop is a step that drops a foreign key, with the table that holds the
// key and the table it references, whose removals it has to precede: Spanner
// refuses to drop a table a foreign key references (`It is referenced by one
// or more foreign keys`).
type keyDrop struct {
	step              *protocol.StepIdentity
	table, referenced tableKey
}

// constraintAddition is a constraint a step adds, and the table a foreign key
// references.
type constraintAddition struct {
	name       string
	operation  tableop.Operation
	referenced tableKey
}

// sortedAdditions orders constraint additions by name, as the host ordered a
// table's constraint changes for Ptah's built-in Spanner engine.
func sortedAdditions(additions []constraintAddition) []constraintAddition {
	return slices.SortedStableFunc(slices.Values(additions), func(a, b constraintAddition) int { return strings.Compare(a.name, b.name) })
}

// SameCheck reports whether a declared CHECK expression and the clause a held
// table keeps for it are one constraint on Spanner, typing the expressions by
// the table's columns; see checkexpr. The CHECK equivalence service answers
// the host with it, and the planner decides with it which CHECK of a kept
// table to replace, so the two never disagree about a CHECK.
func SameCheck(declared, held string, current *protocol.ObservedTable) bool {
	columns := make(map[string]string, len(current.GetColumns()))
	for _, column := range current.GetColumns() {
		columns[column.GetName()] = heldType(column)
	}
	return checkexpr.Equivalent(declared, held, columns)
}

// declaredCheck is a CHECK a declared table states.
type declaredCheck struct {
	name, expression string
}

// declaredChecks are the CHECKs a declared table states, in declaration
// order: each column's own, under the name the column's definition gives it,
// with an enum column's value list, and each table CHECK. A table CHECK
// replaces a column's CHECK of the same name, as a declared constraint wins
// over one the host synthesizes from a column.
func declaredChecks(subject *protocol.ComparisonIdentity, table *protocol.DeclaredTable) ([]declaredCheck, error) {
	var checks []declaredCheck
	for _, declared := range table.GetColumns() {
		column, err := ddlColumn(subject, table, declared)
		if err != nil {
			return nil, err
		}
		if column.Check != "" {
			checks = append(checks, declaredCheck{name: column.CheckName, expression: column.Check})
		}
	}
	operations, err := tableChecks(subject, table)
	if err != nil {
		return nil, err
	}
	for _, operation := range operations {
		checks = slices.DeleteFunc(checks, func(check declaredCheck) bool { return check.name == operation.Name })
		checks = append(checks, declaredCheck{name: operation.Name, expression: operation.Expression})
	}
	return checks, nil
}

// heldConstraints are the foreign keys and CHECKs a table holds, by name, in
// the order its observation lists them.
func heldConstraints(table *protocol.ObservedTable) []*protocol.ObservedConstraint {
	var held []*protocol.ObservedConstraint
	for _, constraint := range table.GetConstraints() {
		if kind := strings.ToUpper(strings.TrimSpace(constraint.GetType())); kind == foreignKeyType || kind == checkType {
			held = append(held, constraint)
		}
	}
	return held
}

// constraintChanges plans the foreign keys and CHECKs of a kept table: a step
// that drops each one the table holds and no longer declares, or declares
// otherwise, and a step that adds each one it declares and does not hold as
// declared, after the drop of the same name. A foreign key is compared by its
// columns, the table and columns it references and its ON DELETE rule, and a
// CHECK by SameCheck. It returns the drops and the additions.
func (p *planner) constraintChanges(index int, table *protocol.PlanningTable) (drops, additions []*protocol.StepIdentity, err error) {
	subject, desired, current := table.GetSubject(), table.GetDesired(), table.GetCurrent()
	keys, err := foreignKeys(subject, desired)
	if err != nil {
		return nil, nil, &refusal{table: index, code: protocol.PlanningDiagnostic_UNSUPPORTED_FEATURE, message: err.Error()}
	}
	checks, err := declaredChecks(subject, desired)
	if err != nil {
		return nil, nil, &refusal{table: index, code: protocol.PlanningDiagnostic_UNSUPPORTED_FEATURE, message: err.Error()}
	}
	kept := make(map[string]bool)
	dropped := make(map[string]*protocol.StepIdentity)
	holds := heldConstraints(current)
	slices.SortStableFunc(holds, func(a, b *protocol.ObservedConstraint) int { return strings.Compare(a.GetName(), b.GetName()) })
	for _, held := range holds {
		if p.declaredAsHeld(subject, held, keys, checks, current) {
			kept[held.GetName()] = true
			continue
		}
		referenced := referencedBy(held, subject)
		id, err := p.add(index, subject, rankConstraint, tableop.Operation{Action: tableop.DropConstraint, Name: held.GetName()}, nil,
			p.constraintEffects(subject, held.GetName(), protocol.PlanningEffect_DROP, referenced)...)
		if err != nil {
			return nil, nil, err
		}
		drops = append(drops, id)
		dropped[held.GetName()] = id
		if referenced != (tableKey{}) {
			p.keyDrops = append(p.keyDrops, keyDrop{step: id, table: keyOf(subject), referenced: referenced})
		}
	}
	var planned []constraintAddition
	for _, key := range keys {
		if !kept[key.Name] {
			planned = append(planned, constraintAddition{name: key.Name, operation: tableop.Operation{Action: tableop.AddForeignKey, ForeignKey: &key},
				referenced: tableKey{schema: key.RefSchema, name: key.RefTable}})
		}
	}
	for _, check := range checks {
		if !kept[check.name] {
			planned = append(planned, constraintAddition{name: check.name, operation: tableop.Operation{Action: tableop.AddCheck, Name: check.name,
				Expression: check.expression}})
		}
	}
	for _, addition := range sortedAdditions(planned) {
		id, err := p.add(index, subject, rankConstraint, addition.operation, nil,
			p.constraintEffects(subject, addition.name, protocol.PlanningEffect_CREATE, addition.referenced)...)
		if err != nil {
			return nil, nil, err
		}
		p.depend(dropped[addition.name], id)
		if creation, found := p.created[addition.referenced]; found {
			p.depend(creation, id)
		}
		additions = append(additions, id)
	}
	return drops, additions, nil
}

// declaredAsHeld reports whether the declaration states a held constraint as
// the table holds it.
func (p *planner) declaredAsHeld(subject *protocol.ComparisonIdentity, held *protocol.ObservedConstraint, keys []tableop.ForeignKey, checks []declaredCheck,
	current *protocol.ObservedTable,
) bool {
	if strings.EqualFold(held.GetType(), foreignKeyType) {
		position := slices.IndexFunc(keys, func(key tableop.ForeignKey) bool { return key.Name == held.GetName() })
		return position >= 0 && sameForeignKey(keys[position], held, subject.GetSchema().GetSource())
	}
	position := slices.IndexFunc(checks, func(check declaredCheck) bool { return check.name == held.GetName() })
	return position >= 0 && !held.GetNotEnforced() && SameCheck(checks[position].expression, held.GetCheckClause(), current)
}

// sameForeignKey reports whether a declared foreign key and a held one are
// one: the same columns in order, the same referenced table and columns, and
// the same ON DELETE rule, an absent rule being NO ACTION. A held key with an
// ON UPDATE rule, a MATCH type, deferral or no enforcement is none the
// provider declares.
func sameForeignKey(declared tableop.ForeignKey, held *protocol.ObservedConstraint, defaultSchema string) bool {
	referenced := referencedBy(held, &protocol.ComparisonIdentity{Schema: &protocol.ComparisonPart{Source: defaultSchema}})
	return slices.Equal(declared.Columns, heldColumns(held)) && declared.RefSchema == referenced.schema && declared.RefTable == referenced.name &&
		slices.Equal(declared.RefColumns, heldReferencedColumns(held)) && referentialRule(declared.OnDelete) == referentialRule(held.GetDeleteRule()) &&
		referentialRule(held.GetUpdateRule()) == "NO ACTION" && held.GetMatch() == "" && !held.GetDeferrable() && held.GetInitially() == "" &&
		!held.GetNotEnforced()
}

// referentialRule spells a rule as information_schema does, an absent rule
// being NO ACTION.
func referentialRule(rule string) string {
	rule = strings.ToUpper(strings.TrimSpace(rule))
	if rule == "" {
		return "NO ACTION"
	}
	return rule
}

func heldColumns(held *protocol.ObservedConstraint) []string {
	if len(held.GetColumnNames()) > 0 || held.GetColumnName() == "" {
		return held.GetColumnNames()
	}
	return []string{held.GetColumnName()}
}

func heldReferencedColumns(held *protocol.ObservedConstraint) []string {
	if len(held.GetForeignColumns()) > 0 || held.GetForeignColumn() == "" {
		return held.GetForeignColumns()
	}
	return []string{held.GetForeignColumn()}
}

// referencedBy names the table a held foreign key references; an unqualified
// one is in the schema of the table subject. A CHECK references none.
func referencedBy(held *protocol.ObservedConstraint, subject *protocol.ComparisonIdentity) tableKey {
	if held.GetForeignTable() == "" {
		return tableKey{}
	}
	schema := held.GetForeignSchema()
	if schema == "" {
		schema = subject.GetSchema().GetSource()
	}
	return tableKey{schema: schema, name: held.GetForeignTable()}
}

// constraintEffects are the effects of a step that adds or drops the
// constraint name of the table subject: it reads the table, changes the
// constraint, and for a foreign key reads the table it references when the
// plan names that table.
func (p *planner) constraintEffects(subject *protocol.ComparisonIdentity, name string, action protocol.PlanningEffect_Action,
	referenced tableKey,
) []*protocol.PlanningEffect {
	effects := []*protocol.PlanningEffect{readTable(subject), effect(childSubject(subject, "constraint", name), action)}
	if target, found := p.tables[referenced]; found && referenced != keyOf(subject) {
		effects = append(effects, readTable(target.GetSubject()))
	}
	return effects
}

// dropForeignKeys adds a step that drops each foreign key a removed table
// holds. Spanner refuses to drop a table a foreign key references, so each
// key is dropped before the removal of its own table and of the table it
// references, which orderKeyDrops arranges once every removal is planned.
// Ptah's built-in Spanner engine dropped them so. A CHECK leaves with its
// table.
func (p *planner) dropForeignKeys(index int, table *protocol.PlanningTable) error {
	subject := table.GetSubject()
	for _, held := range heldConstraints(table.GetCurrent()) {
		if !strings.EqualFold(held.GetType(), foreignKeyType) {
			continue
		}
		referenced := referencedBy(held, subject)
		id, err := p.add(index, subject, rankConstraint, tableop.Operation{Action: tableop.DropConstraint, Name: held.GetName()}, nil,
			p.constraintEffects(subject, held.GetName(), protocol.PlanningEffect_DROP, referenced)...)
		if err != nil {
			return err
		}
		p.keyDrops = append(p.keyDrops, keyDrop{step: id, table: keyOf(subject), referenced: referenced})
	}
	return nil
}

// orderKeyDrops puts the drop of each foreign key before the removal of the
// table that holds it and of the table it references, where the plan removes
// them. A removed table's key is dropped after every table the plan creates,
// so a creation Spanner refuses stops the plan before it destroys anything,
// as Ptah's built-in Spanner engine ordered it.
func (p *planner) orderKeyDrops() {
	for _, drop := range p.keyDrops {
		if _, removed := p.dropped[drop.table]; removed {
			for _, creation := range p.creationOrder {
				p.depend(creation, drop.step)
			}
		}
		if removal, found := p.dropped[drop.table]; found {
			p.depend(drop.step, removal)
		}
		if removal, found := p.dropped[drop.referenced]; found && drop.referenced != drop.table {
			p.depend(drop.step, removal)
		}
	}
}
