// Package constraintrows keeps a batch of schema statements from adding a
// CHECK or a foreign key that rows already in a table break, before any
// statement of the batch runs.
//
// Spanner checks a constraint added to a table against every row the table
// holds, and refuses the statement when a row breaks it. Measured on the
// emulator behind PGAdapter 0.56.2: "Check constraint `items`.`k1` is violated
// for key {Int64(1)}" (SQLSTATE P0001), and "Foreign key `ca_fk` constraint
// violation on table `ca`. Cannot find referenced key ... in table `pa`."
// (SQLSTATE 23503). Spanner runs each schema statement on its own and commits
// it, so the statements before the refused one stay applied. A changed CHECK
// or foreign key is replaced by DROP CONSTRAINT and then ADD CONSTRAINT, and
// such a run leaves the table without the constraint (stokaro/ptah#4204 S93).
//
// So the executor finds the constraints a batch adds (Additions), counts the
// rows of each table that break its constraint (Addition.Query), and refuses
// the batch before its first unit when a row does (Refusal). A constraint the
// batch's first statement adds is not counted: when Spanner refuses it,
// nothing of the batch has run, and the count would read the whole table once
// more for nothing.
//
// The count reads the rows as they are before the batch runs. A statement
// before the addition that changes those rows, or the columns the constraint
// reads, makes the count wrong, so Additions leaves such an addition to
// Spanner; see Additions.
package constraintrows

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/stokaro/ptah-spanner-plugin/internal/catalog"
	"github.com/stokaro/ptah-spanner-plugin/internal/ddl"
	"github.com/stokaro/ptah-spanner-plugin/internal/sqltext"
)

// Addition is a CHECK or a foreign key that a statement of a batch adds to a
// table.
type Addition struct {
	// Unit is the index of the statement in the batch.
	Unit int
	// Table names the table the constraint is added to. A statement that
	// names no schema names the default one.
	Table sqltext.TableName
	// Name is the constraint's name, empty when the statement gives none.
	Name string
	// Check is the CHECK expression as the statement writes it, and empty for
	// a foreign key.
	Check string
	// ForeignKey is the foreign key, and nil for a CHECK.
	ForeignKey *ForeignKey
}

// ForeignKey is what a row of a table must match for a foreign key the
// statement adds.
type ForeignKey struct {
	// Columns are the table's columns the key reads, in order.
	Columns []string
	// Referenced names the referenced table, and ReferencedColumns its
	// columns, in the order they pair with Columns.
	Referenced        sqltext.TableName
	ReferencedColumns []string
}

// Additions returns the CHECKs and foreign keys the statements add to a table,
// in statement order, except one the batch's first statement adds. A
// statement may hold more than one SQL statement.
//
// Spanner takes `ALTER TABLE [IF EXISTS] t ADD [CONSTRAINT c] CHECK (e)` and
// `ALTER TABLE [IF EXISTS] t ADD [CONSTRAINT c] FOREIGN KEY (a, ...)
// REFERENCES r (b, ...)` with ON DELETE CASCADE or NO ACTION and ON UPDATE NO
// ACTION, a table qualified by its schema or not. An addition of another
// shape, such as a CHECK with a clause after its expression or a foreign key
// without the referenced columns, is not counted: Spanner decides it.
//
// The count reads the rows before the batch runs, so an addition is left to
// Spanner, and not returned, when a statement before it may change the rows
// or the columns its constraint reads:
//
//   - A statement that changes rows (INSERT, UPDATE, DELETE), a text the
//     lexer cannot read, an ALTER TABLE that renames, and any statement not
//     known to leave every table's rows alone end the reading. Only an index,
//     a view, a sequence, a change stream, a schema and ANALYZE are known to.
//   - CREATE TABLE, DROP TABLE, and an ALTER TABLE action other than one that
//     adds or drops a constraint or a row deletion policy, or changes a named
//     column, leave the additions after it to that table to Spanner, and the
//     foreign keys after it that reference the table.
//   - ADD, DROP and ALTER COLUMN leave to Spanner the additions after them
//     whose constraint reads the column: Spanner fills a column added with a
//     default in every row, and a CHECK that reads a column the batch adds
//     cannot be counted before the column exists.
func Additions(statements []string) []Addition {
	var additions []Addition
	changed := make(map[sqltext.TableName]*change)
	seen := 0
	for unit, statement := range statements {
		spans, err := sqltext.Spans(statement)
		if err != nil {
			return additions
		}
		for _, span := range spans {
			tokens, err := significant(span.Body)
			if err != nil {
				return additions
			}
			read := readStatement(span.Body, tokens)
			if read.stops {
				return additions
			}
			if read.addition != nil && seen > 0 && !blocked(*read.addition, changed) {
				read.addition.Unit = unit
				additions = append(additions, *read.addition)
			}
			if read.table != nil {
				changedTable(changed, *read.table).record(read.column)
			}
			seen++
		}
	}
	return additions
}

// change is what statements of a batch change of one table: the whole table,
// or some of its columns.
type change struct {
	whole   bool
	columns []string
}

// record notes a change of column, or of the whole table for the empty name.
func (c *change) record(column string) {
	if column == "" {
		c.whole = true
		return
	}
	if !slices.Contains(c.columns, column) {
		c.columns = append(c.columns, column)
	}
}

// changedTable returns what the batch changes of table, empty until a
// statement changes something.
func changedTable(changed map[sqltext.TableName]*change, table sqltext.TableName) *change {
	if found, ok := changed[table]; ok {
		return found
	}
	created := &change{}
	changed[table] = created
	return created
}

// blocked reports whether a statement before addition changed the rows or
// the columns its constraint reads: its table or the table a foreign key
// references, whole, or a column the constraint names.
func blocked(addition Addition, changed map[sqltext.TableName]*change) bool {
	table := changed[addition.Table]
	if addition.ForeignKey == nil {
		return table != nil && (table.whole || mentions(addition.Check, table.columns))
	}
	key := addition.ForeignKey
	return table.covers(key.Columns) || changed[key.Referenced].covers(key.ReferencedColumns)
}

// covers reports whether c changes the whole table or one of columns. A nil
// change covers nothing.
func (c *change) covers(columns []string) bool {
	return c != nil && (c.whole || slices.ContainsFunc(columns, func(column string) bool { return slices.Contains(c.columns, column) }))
}

// mentions reports whether expression holds a name of one of columns: a
// quoted name or an unquoted word that spells it. A function or a keyword of
// the same spelling counts too, which only leaves the addition to Spanner.
func mentions(expression string, columns []string) bool {
	if len(columns) == 0 {
		return false
	}
	tokens, err := sqltext.Tokens(expression)
	if err != nil {
		return true
	}
	return slices.ContainsFunc(tokens, func(token sqltext.Token) bool {
		return token.Valued && (token.Name || token.Kind == sqltext.Word) && slices.Contains(columns, token.Value)
	})
}

// significant returns the tokens of a statement other than white space and
// comments.
func significant(body string) ([]sqltext.Token, error) {
	tokens, err := sqltext.Tokens(body)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(tokens, func(token sqltext.Token) bool {
		return token.Kind == sqltext.Space || token.Kind == sqltext.Comment
	}), nil
}

// statementRead is what one statement means for the additions after it.
type statementRead struct {
	// stops reports a statement after which no count holds.
	stops bool
	// table is a table the statement changes, nil for none, and column the
	// column of it, empty when it changes the whole table.
	table  *sqltext.TableName
	column string
	// addition is the constraint the statement adds, nil for none.
	addition *Addition
}

// leavesRowsAlone are the leading words of statements that change no row and
// no column of a table.
var leavesRowsAlone = [][]string{
	{"CREATE", "INDEX"}, {"CREATE", "UNIQUE", "INDEX"}, {"ALTER", "INDEX"}, {"DROP", "INDEX"},
	{"CREATE", "SEARCH", "INDEX"}, {"ALTER", "SEARCH", "INDEX"}, {"DROP", "SEARCH", "INDEX"},
	{"CREATE", "VIEW"}, {"CREATE", "OR", "REPLACE", "VIEW"}, {"DROP", "VIEW"},
	{"CREATE", "SEQUENCE"}, {"ALTER", "SEQUENCE"}, {"DROP", "SEQUENCE"},
	{"CREATE", "CHANGE", "STREAM"}, {"ALTER", "CHANGE", "STREAM"}, {"DROP", "CHANGE", "STREAM"},
	{"CREATE", "SCHEMA"}, {"DROP", "SCHEMA"}, {"ANALYZE"},
}

// readStatement reads one statement, whose significant tokens are tokens.
func readStatement(body string, tokens []sqltext.Token) statementRead {
	source := make([]string, len(tokens))
	words := make([]string, len(tokens))
	for i, token := range tokens {
		source[i], words[i] = token.Text, token.Upper()
	}
	switch {
	case slices.ContainsFunc(leavesRowsAlone, func(leading []string) bool { return sqltext.Leads(words, leading...) }):
		return statementRead{}
	case sqltext.Leads(words, "CREATE", "TABLE"):
		return wholeTable(source, words, "NOT")
	case sqltext.Leads(words, "DROP", "TABLE"):
		return wholeTable(source, words, "")
	case sqltext.Leads(words, "ALTER", "TABLE"):
		return alteredTable(body, tokens, source, words)
	}
	return statementRead{stops: true}
}

// wholeTable reads `CREATE TABLE [IF NOT EXISTS] t ...` (guard "NOT") or
// `DROP TABLE [IF EXISTS] t` (guard ""), which change table t whole.
func wholeTable(source, words []string, guard string) statementRead {
	at := 2
	switch {
	case guard != "" && sqltext.Leads(words[at:], "IF", guard, "EXISTS"):
		at += 3
	case guard == "" && sqltext.Leads(words[at:], "IF", "EXISTS"):
		at += 2
	}
	name, next, found := sqltext.ReadTableName(source, at)
	if !found || guard == "" && next < len(words) && !slices.Equal(words[next:], []string{"RESTRICT"}) && !slices.Equal(words[next:], []string{"CASCADE"}) {
		return statementRead{stops: true}
	}
	table := resolved(name)
	return statementRead{table: &table}
}

// alteredTable reads an ALTER TABLE: a constraint it adds, or the table or the
// column it changes.
func alteredTable(body string, tokens []sqltext.Token, source, words []string) statementRead {
	name, next, found := sqltext.AlteredTable(source, words)
	if !found || next+1 >= len(words) {
		return statementRead{stops: true}
	}
	table := resolved(name)
	action, object := words[next], words[next+1]
	switch {
	case action == "ADD" && slices.Contains([]string{"CONSTRAINT", "CHECK", "FOREIGN", "UNIQUE", "PRIMARY"}, object):
		return statementRead{addition: addedConstraint(body, tokens, source, words, table, next+1)}
	case slices.Contains([]string{"ADD", "ALTER", "DROP"}, action) && object == "TTL", action == "DROP" && object == "CONSTRAINT":
		return statementRead{}
	case slices.Contains([]string{"ADD", "ALTER", "DROP"}, action) && object == "COLUMN":
		return statementRead{table: &table, column: changedColumn(source, words, next+2)}
	case slices.Contains([]string{"ADD", "ALTER", "DROP"}, action):
		return statementRead{table: &table}
	}
	return statementRead{stops: true}
}

// changedColumn reads the column an ADD, ALTER or DROP COLUMN names at
// words[at], after an optional IF [NOT] EXISTS. It returns the empty string,
// which stands for the whole table, when it finds no name.
func changedColumn(source, words []string, at int) string {
	switch {
	case sqltext.Leads(words[at:], "IF", "NOT", "EXISTS"):
		at += 3
	case sqltext.Leads(words[at:], "IF", "EXISTS"):
		at += 2
	}
	if at >= len(source) {
		return ""
	}
	column, found := sqltext.Identifier(source[at])
	if !found {
		return ""
	}
	return column
}

// addedConstraint reads `[CONSTRAINT c] CHECK (e)` or `[CONSTRAINT c] FOREIGN
// KEY (...) REFERENCES r (...) ...` from words[at] to the end of the
// statement. It returns nil for a constraint of another shape.
func addedConstraint(body string, tokens []sqltext.Token, source, words []string, table sqltext.TableName, at int) *Addition {
	addition := &Addition{Table: table}
	if words[at] == "CONSTRAINT" {
		if at+2 >= len(words) {
			return nil
		}
		name, found := sqltext.Identifier(source[at+1])
		if !found {
			return nil
		}
		addition.Name = name
		at += 2
	}
	switch {
	case words[at] == "CHECK":
		closing, found := closingParenthesis(words, at+1)
		if !found || closing != len(words)-1 || closing == at+2 {
			return nil
		}
		addition.Check = strings.TrimSpace(body[tokens[at+1].End:tokens[closing].Start])
		return addition
	case sqltext.Leads(words[at:], "FOREIGN", "KEY"):
		key, found := foreignKey(source, words, at+2)
		if !found {
			return nil
		}
		addition.ForeignKey = key
		return addition
	}
	return nil
}

// foreignKey reads `(a, ...) REFERENCES r (b, ...)` and the referential
// actions after it, from words[at] to the end of the statement.
func foreignKey(source, words []string, at int) (*ForeignKey, bool) {
	columns, next, found := nameList(source, words, at)
	if !found || next >= len(words) || words[next] != "REFERENCES" {
		return nil, false
	}
	name, next, found := sqltext.ReadTableName(source, next+1)
	if !found {
		return nil, false
	}
	referenced, next, found := nameList(source, words, next)
	if !found || len(referenced) != len(columns) || !referentialActions(words[next:]) {
		return nil, false
	}
	return &ForeignKey{Columns: columns, Referenced: resolved(name), ReferencedColumns: referenced}, true
}

// referentialActions reports whether words are actions Spanner enforces a
// foreign key under, each at most once: ON DELETE CASCADE or NO ACTION, and
// ON UPDATE NO ACTION. Another clause, such as NOT ENFORCED or a MATCH type,
// leaves the key to Spanner.
func referentialActions(words []string) bool {
	var seen []string
	for len(words) > 0 {
		var clause string
		switch {
		case sqltext.Leads(words, "ON", "DELETE", "CASCADE"):
			clause, words = "ON DELETE", words[3:]
		case sqltext.Leads(words, "ON", "DELETE", "NO", "ACTION"):
			clause, words = "ON DELETE", words[4:]
		case sqltext.Leads(words, "ON", "UPDATE", "NO", "ACTION"):
			clause, words = "ON UPDATE", words[4:]
		default:
			return false
		}
		if slices.Contains(seen, clause) {
			return false
		}
		seen = append(seen, clause)
	}
	return true
}

// nameList reads a parenthesized list of one name or more at words[at], and
// returns the names and the index of the token after the list.
func nameList(source, words []string, at int) ([]string, int, bool) {
	if at >= len(words) || words[at] != "(" {
		return nil, 0, false
	}
	var names []string
	for at++; at+1 < len(words); at += 2 {
		name, found := sqltext.Identifier(source[at])
		if !found {
			return nil, 0, false
		}
		names = append(names, name)
		switch words[at+1] {
		case ")":
			return names, at + 2, true
		case ",":
		default:
			return nil, 0, false
		}
	}
	return nil, 0, false
}

// closingParenthesis returns the index of the parenthesis that closes the one
// at words[at].
func closingParenthesis(words []string, at int) (int, bool) {
	if at >= len(words) || words[at] != "(" {
		return 0, false
	}
	depth := 0
	for i := at; i < len(words); i++ {
		switch words[i] {
		case "(":
			depth++
		case ")":
			depth--
			if depth == 0 {
				return i, true
			}
		}
	}
	return 0, false
}

// resolved names a table by its schema and name, a name without a schema
// being in the default one.
func resolved(name sqltext.TableName) sqltext.TableName {
	if name.Schema == "" {
		name.Schema = ddl.DefaultSchema
	}
	return name
}

// Query is the statement that counts the rows of the table that break the
// constraint.
//
// A row breaks a CHECK when its expression is false; a CHECK takes a row for
// which it is null, and so does `WHERE NOT (e)`. A row breaks a foreign key
// when none of its key columns is null and no row of the referenced table
// holds its values; Spanner takes a row with a null key column (measured: a
// composite key adds over a row of (5, NULL) with no referenced row).
func (a Addition) Query() string {
	table := ddl.Name(a.Table.Schema, a.Table.Table)
	if a.ForeignKey == nil {
		return "SELECT COUNT(*) FROM " + table + " WHERE NOT (" + a.Check + ")"
	}
	key := a.ForeignKey
	present := make([]string, len(key.Columns))
	pairs := make([]string, len(key.Columns))
	for i, column := range key.Columns {
		present[i] = "child." + ddl.Quote(column) + " IS NOT NULL"
		pairs[i] = "parent." + ddl.Quote(key.ReferencedColumns[i]) + " = child." + ddl.Quote(column)
	}
	return "SELECT COUNT(*) FROM " + table + " AS child WHERE " + strings.Join(present, " AND ") +
		" AND NOT EXISTS (SELECT 1 FROM " + ddl.Name(key.Referenced.Schema, key.Referenced.Table) + " AS parent WHERE " + strings.Join(pairs, " AND ") + ")"
}

// Count counts the rows of the table that break the constraint, with Query.
func (a Addition) Count(ctx context.Context, q catalog.Queryer) (int64, error) {
	rows, err := q.Query(ctx, a.Query())
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return 0, err
		}
		return 0, errors.New("the count returned no row")
	}
	var count int64
	if err := rows.Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

// maxReason is the longest reason a host shows: one line of at most 512
// bytes (provider.ValidateDiagnostic).
const maxReason = 512

// remedy is what to do about the rows that break the constraint addition
// adds.
func remedy(addition Addition) string {
	if addition.ForeignKey != nil {
		return "update or delete the rows that break it, or add the rows they reference, before adding the constraint"
	}
	return "update or delete the rows that break it before adding the constraint"
}

// Refusal returns why a batch of total units cannot run when rows of the table
// break the constraint addition adds: it names the unit, the constraint, the
// table and how many rows break it, and what to do, in one line a host shows.
func Refusal(addition Addition, rows int64, total int) string {
	counted := "1 row breaks"
	if rows != 1 {
		counted = fmt.Sprintf("%d rows break", rows)
	}
	reason := func(what string) string {
		return fmt.Sprintf("unit %d of %d adds %s, which %s, so no unit ran: Spanner refuses to add a constraint a row breaks; %s",
			addition.Unit+1, total, what, counted, remedy(addition))
	}
	return fitting(reason(constraintPhrase(addition)+" to table "+tablePhrase(addition.Table)), reason(kindPhrase(addition)))
}

// Uncounted returns why a batch of total units cannot run when the provider
// could not count the rows that break the constraint addition adds. Spanner
// checks the same rows when the unit runs, so a count that fails, such as on a
// row where the expression divides by zero, may stand for a refusal that comes
// after the units before it. The reason names the server's SQLSTATE, never
// its message, which may quote a value.
func Uncounted(addition Addition, err error, total int) string {
	failure := "the count failed"
	if refused, found := errors.AsType[*pgconn.PgError](err); found {
		failure = "Spanner refused the count with SQLSTATE " + refused.Code
	}
	reason := func(what string) string {
		return fmt.Sprintf("the provider could not count the rows that break %s, which unit %d of %d adds, so no unit ran: %s",
			what, addition.Unit+1, total, failure)
	}
	return fitting(reason(constraintPhrase(addition)+" of table "+tablePhrase(addition.Table)), reason(kindPhrase(addition)))
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

// constraintPhrase names the constraint addition adds by its kind and name.
func constraintPhrase(addition Addition) string {
	kind := "CHECK"
	if addition.ForeignKey != nil {
		kind = "foreign key"
	}
	if addition.Name == "" {
		return "a " + kind + " without a name"
	}
	return fmt.Sprintf("%s %q", kind, addition.Name)
}

// kindPhrase names the kind of constraint addition adds and nothing else.
func kindPhrase(addition Addition) string {
	if addition.ForeignKey != nil {
		return "a foreign key"
	}
	return "a CHECK"
}

// tablePhrase names a table, qualified by its schema unless that is the
// default one.
func tablePhrase(table sqltext.TableName) string {
	if table.Schema == ddl.DefaultSchema {
		return fmt.Sprintf("%q", table.Table)
	}
	return fmt.Sprintf("%q.%q", table.Schema, table.Table)
}
