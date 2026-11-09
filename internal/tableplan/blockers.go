package tableplan

import (
	"fmt"
	"slices"
	"strings"

	protocol "ptah.run/provider-sdk/protocol/experimental"
)

// blockedTable refuses dropping a table that a removal blocker names: Spanner
// refuses to drop a table a change stream names in its FOR clause (measured on
// the emulator behind PGAdapter 0.56.2: `Table t is tracked by 1 Change
// Stream: cs1`). A read reports those streams under schema.removal-blockers,
// so the plan, a dry run and a generated migration refuse the drop before a
// statement exists, where the run alone would stop at it after the statements
// before it. A host that does not carry blockers sends none, and the executor
// refuses the drop before the run's first statement instead.
func blockedTable(index int, table *protocol.PlanningTable) error {
	blockers := table.GetCurrent().GetRemovalBlockers()
	if len(blockers) == 0 {
		return nil
	}
	return &refusal{table: index, code: protocol.PlanningDiagnostic_UNSUPPORTED_FEATURE, message: fmt.Sprintf(
		"dropping table %s: %s, and Spanner refuses to drop a table a change stream names; "+
			"drop the change stream, or alter it to stop tracking the table, before dropping the table", phrase(table.GetSubject()), naming(blockers))}
}

// blockedColumn refuses dropping a column that a removal blocker names: Spanner
// refuses to drop a column a change stream names in the column list of its
// FOR clause. A stream that names the table whole names no column, so it
// blocks no column's removal.
func blockedColumn(index int, table *protocol.PlanningTable, column string) error {
	var blockers []*protocol.RemovalBlocker
	for _, blocker := range table.GetCurrent().GetRemovalBlockers() {
		if slices.Contains(blocker.GetColumns(), column) {
			blockers = append(blockers, blocker)
		}
	}
	if len(blockers) == 0 {
		return nil
	}
	return &refusal{table: index, code: protocol.PlanningDiagnostic_UNSUPPORTED_FEATURE, message: fmt.Sprintf(
		"dropping column %q of table %s: %s, and Spanner refuses to drop a column a change stream names; "+
			"alter the change stream to stop tracking the column, or drop it, before dropping the column", column, phrase(table.GetSubject()), naming(blockers))}
}

// naming says which objects name what a plan drops, in name order: `change
// stream "cs1" names it`, or `change streams "cs1" and "cs3" name it`. The
// provider reports change streams alone; another kind reads by its token.
func naming(blockers []*protocol.RemovalBlocker) string {
	names := make([]string, 0, len(blockers))
	kinds := make([]string, 0, len(blockers))
	for _, blocker := range blockers {
		names = append(names, fmt.Sprintf("%q", blocker.GetName()))
		kinds = append(kinds, strings.ReplaceAll(blocker.GetKind(), "_", " "))
	}
	slices.Sort(names)
	kinds = slices.Compact(slices.Sorted(slices.Values(kinds)))
	kind := strings.Join(kinds, " or ")
	if len(names) == 1 {
		return kind + " " + names[0] + " names it"
	}
	return kind + "s " + strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1] + " name it"
}
