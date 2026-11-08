package constraintrows

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/stokaro/ptah-spanner-plugin/internal/ddl"
	"github.com/stokaro/ptah-spanner-plugin/internal/sqltext"
)

// maxReason is the longest reason a host shows: one line of at most 512
// bytes (provider.ValidateDiagnostic).
const maxReason = 512

// wording is how the reasons name an addition's rule and what to do about the
// rows that break it.
type wording struct {
	// action is what the unit does, with the rule's names, and bare the same
	// without them.
	action, bare string
	// rule names the rule with its names, and bareRule without them, after
	// "the rows that break".
	rule, bareRule string
	// verb is what the unit does to the rule.
	verb string
	// why is Spanner's refusal, and remedy what to do instead.
	why, remedy string
}

// words returns the wording of addition.
func (a Addition) words() wording {
	table := tablePhrase(a.Table)
	column := ""
	if len(a.Columns) > 0 {
		column = fmt.Sprintf("column %q of table %s", a.Columns[0], table)
	}
	refusal := "Spanner refuses to add a constraint a row breaks"
	switch a.Kind {
	case ForeignKey:
		named := named("foreign key", a.Name)
		return wording{action: "adds " + named + " to table " + table, bare: "adds a foreign key", rule: named + " of table " + table, bareRule: "a foreign key",
			verb: "adds", why: refusal,
			remedy: "update or delete the rows that break it, or add the rows they reference, before adding the constraint"}
	case UniqueIndex:
		named := named("unique index", a.Name)
		return wording{action: "creates " + named + " on table " + table, bare: "creates a unique index", rule: named + " of table " + table,
			bareRule: "a unique index", verb: "creates", why: "Spanner refuses a unique index over rows that share a key",
			remedy: "update or delete the rows that share a key before creating the index"}
	case NotNull:
		return wording{action: "makes " + column + " NOT NULL", bare: "makes a column NOT NULL", rule: "NOT NULL on " + column, bareRule: "NOT NULL on a column",
			verb: "sets", why: "Spanner refuses NOT NULL over a null",
			remedy: "set a value in the rows that hold a null before making the column NOT NULL"}
	case CharacterLimit:
		limited := column
		if a.Array {
			limited = "the elements of " + column
		}
		return wording{action: fmt.Sprintf("limits %s to %d characters", limited, a.Limit), bare: "limits a column's characters",
			rule: fmt.Sprintf("the limit of %d characters on %s", a.Limit, limited), bareRule: "a limit on a column's characters",
			verb: "sets", why: "Spanner refuses a limit a value exceeds",
			remedy: "shorten the values that exceed it before limiting the column"}
	}
	named := named("CHECK", a.Name)
	return wording{action: "adds " + named + " to table " + table, bare: "adds a CHECK", rule: named + " of table " + table, bareRule: "a CHECK",
		verb: "adds", why: refusal, remedy: "update or delete the rows that break it before adding the constraint"}
}

// Refusal returns why a batch of total units cannot run when rows of the table
// break the rule addition adds: it names the unit, the rule, the table and how
// many rows break it, and what to do, in one line a host shows.
func Refusal(addition Addition, rows int64, total int) string {
	counted := "1 row breaks"
	if rows != 1 {
		counted = fmt.Sprintf("%d rows break", rows)
	}
	w := addition.words()
	reason := func(action string) string {
		return fmt.Sprintf("unit %d of %d %s, which %s, so no unit ran: %s; %s", addition.Unit+1, total, action, counted, w.why, w.remedy)
	}
	return fitting(reason(w.action), reason(w.bare))
}

// Uncounted returns why a batch of total units cannot run when the provider
// could not count the rows that break the rule addition adds. Spanner checks
// the same rows when the unit runs, so a count that fails, such as on a row
// where a CHECK divides by zero or a bytea value that is not valid UTF-8, may
// stand for a refusal that comes after the units before it. The reason names
// the server's SQLSTATE, never its message, which may quote a value.
func Uncounted(addition Addition, err error, total int) string {
	failure := "the count failed"
	if refused, found := errors.AsType[*pgconn.PgError](err); found {
		failure = "Spanner refused the count with SQLSTATE " + refused.Code
	}
	w := addition.words()
	reason := func(rule string) string {
		return fmt.Sprintf("the provider could not count the rows that break %s, which unit %d of %d %s, so no unit ran: %s",
			rule, addition.Unit+1, total, w.verb, failure)
	}
	return fitting(reason(w.rule), reason(w.bareRule))
}

// fitting returns the first reason a host shows whole, or the last one.
func fitting(reasons ...string) string {
	for _, reason := range reasons {
		if len(reason) <= maxReason {
			return reason
		}
	}
	return reasons[len(reasons)-1]
}

// named names a rule by its kind and its name, or says it has none.
func named(kind, name string) string {
	if name == "" {
		return "a " + kind + " without a name"
	}
	return fmt.Sprintf("%s %q", kind, name)
}

// tablePhrase names a table, qualified by its schema unless that is the
// default one.
func tablePhrase(table sqltext.TableName) string {
	if table.Schema == ddl.DefaultSchema {
		return fmt.Sprintf("%q", table.Table)
	}
	return fmt.Sprintf("%q.%q", table.Schema, table.Table)
}
