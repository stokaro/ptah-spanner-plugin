// Package changestream keeps a batch of schema statements from dropping a
// table or a column that a Spanner change stream names, before any statement
// of the batch runs.
//
// Spanner refuses to drop a table that a change stream names in its FOR
// clause, and a column that one names in the column list of its FOR clause.
// Measured on the emulator behind PGAdapter 0.56.2: `Table t is tracked by 1
// Change Stream: cs1. Tables explicitly tracked by a Change Stream cannot be
// dropped`, and the same for a column. A stream created FOR ALL names
// nothing, and a stream FOR t names no column of t, so neither stops a drop.
//
// The provider does not model change streams. A read reports the streams that
// name each table as the table's removal blockers (Blockers), for a host that
// carries them (schema.removal-blockers), and the table service refuses a plan
// that drops what one names. A plan from a host that does not carry them, and
// a migration someone wrote, may still drop what a stream names, and such a
// run would stop at the drop after the statements before it. So the executor
// reads what the streams name from information_schema (Read), finds the drops
// among the units it is about to run (Drops), and refuses the batch before its
// first unit when a stream names one of them (Refusal).
package changestream

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/stokaro/ptah-spanner-plugin/internal/catalog"
	"github.com/stokaro/ptah-spanner-plugin/internal/ddl"
	"github.com/stokaro/ptah-spanner-plugin/internal/sqltext"
)

// Drop is a table, or a column of a table, that a statement of a batch drops.
type Drop struct {
	// Unit is the index of the statement in the batch.
	Unit int
	// Schema and Table name the table. A statement that names no schema
	// names the default one.
	Schema, Table string
	// Column is the column a DROP COLUMN drops, and empty for DROP TABLE.
	Column string
}

// Drops returns the tables and columns the statements drop, in statement
// order. A statement may hold more than one SQL statement.
//
// It reads the statements up to the first one that may change what a change
// stream names: CREATE, ALTER or DROP CHANGE STREAM, a table's RENAME, or a
// text the lexer cannot read. What the catalog says before the batch runs no
// longer holds after such a statement, so the drops that follow it are left
// to Spanner. The usual remedy for a refusal is that shape: drop the stream,
// then drop the table, in one migration.
//
// Spanner takes `DROP TABLE [IF EXISTS] t` and `ALTER TABLE [IF EXISTS] t
// DROP [COLUMN] c`, each with an optional RESTRICT, and a table qualified by
// its schema. A statement of another shape drops nothing here; if it is
// malformed, Spanner refuses it when it runs.
func Drops(statements []string) []Drop {
	var drops []Drop
	for unit, statement := range statements {
		spans, err := sqltext.Spans(statement)
		if err != nil {
			return drops
		}
		for _, span := range spans {
			source, words, err := sqltext.Words(span.Body)
			if err != nil || changesStreams(words) {
				return drops
			}
			if drop, found := readDrop(source, words); found {
				drop.Unit = unit
				drops = append(drops, drop)
			}
		}
	}
	return drops
}

// changesStreams reports a statement that may change what a change stream
// names: a CHANGE STREAM statement, or an ALTER TABLE that renames the table
// (Spanner renames a table a stream names, and the stream then names the new
// name; it renames no column).
func changesStreams(words []string) bool {
	if len(words) >= 3 && slices.Contains([]string{"CREATE", "ALTER", "DROP"}, words[0]) && words[1] == "CHANGE" && words[2] == "STREAM" {
		return true
	}
	_, next, found := sqltext.AlteredTable(words, words)
	return found && next < len(words) && words[next] == "RENAME"
}

// readDrop reads a DROP TABLE or an ALTER TABLE ... DROP COLUMN. source are
// the statement's tokens as written and words the same tokens as the
// provider matches them.
func readDrop(source, words []string) (Drop, bool) {
	switch {
	case sqltext.Leads(words, "DROP", "TABLE"):
		return droppedTable(source, words)
	case sqltext.Leads(words, "ALTER", "TABLE"):
		return droppedColumn(source, words)
	}
	return Drop{}, false
}

// droppedTable reads `DROP TABLE [IF EXISTS] t [RESTRICT]`.
func droppedTable(source, words []string) (Drop, bool) {
	at := 2
	if sqltext.Leads(words[at:], "IF", "EXISTS") {
		at += 2
	}
	name, next, found := sqltext.ReadTableName(source, at)
	if !found || !ends(words, next) {
		return Drop{}, false
	}
	schema, table := resolved(name)
	return Drop{Schema: schema, Table: table}, true
}

// droppedColumn reads `ALTER TABLE [IF EXISTS] t DROP [COLUMN] c
// [RESTRICT]`. DROP CONSTRAINT and DROP TTL drop no column.
func droppedColumn(source, words []string) (Drop, bool) {
	name, next, found := sqltext.AlteredTable(source, words)
	if !found || next+1 >= len(words) || words[next] != "DROP" || slices.Contains([]string{"CONSTRAINT", "TTL"}, words[next+1]) {
		return Drop{}, false
	}
	next++
	if words[next] == "COLUMN" {
		next++
	}
	if next >= len(words) || !ends(words, next+1) {
		return Drop{}, false
	}
	column, found := sqltext.Identifier(source[next])
	if !found {
		return Drop{}, false
	}
	schema, table := resolved(name)
	return Drop{Schema: schema, Table: table, Column: column}, true
}

// ends reports whether the statement ends at words[at], or holds only
// RESTRICT from there.
func ends(words []string, at int) bool {
	return at == len(words) || at == len(words)-1 && words[at] == "RESTRICT"
}

// resolved names the table a statement names by its schema and name, a name
// without a schema being in the default one.
func resolved(name sqltext.TableName) (schema, table string) {
	if name.Schema == "" {
		return ddl.DefaultSchema, name.Table
	}
	return name.Schema, name.Table
}

// Tracking is what the change streams of a database name.
type Tracking struct {
	// Tables are the tables a stream names in its FOR clause, with or
	// without a column list.
	Tables []Tracked
	// Columns are the columns a stream names in the column list of its FOR
	// clause.
	Columns []Tracked
}

// Tracked is one table, or one column of a table, that a change stream names.
type Tracked struct {
	// Stream is the stream's name. Spanner keeps every change stream in the
	// default schema.
	Stream string
	// Schema and Table name the table, and Column the column, empty for a
	// table.
	Schema, Table, Column string
}

// Read reads what the database's change streams name, from
// information_schema.change_stream_tables and change_stream_columns. A stream
// FOR ALL has no rows in either.
func Read(ctx context.Context, q catalog.Queryer) (Tracking, error) {
	var tracking Tracking
	tables, err := q.Query(ctx, "SELECT change_stream_name, table_schema, table_name FROM information_schema.change_stream_tables ORDER BY 1, 2, 3")
	if err != nil {
		return Tracking{}, err
	}
	for tables.Next() {
		var row Tracked
		if err := tables.Scan(&row.Stream, &row.Schema, &row.Table); err != nil {
			tables.Close()
			return Tracking{}, err
		}
		tracking.Tables = append(tracking.Tables, row)
	}
	tables.Close()
	if err := tables.Err(); err != nil {
		return Tracking{}, err
	}
	columns, err := q.Query(ctx, "SELECT change_stream_name, table_schema, table_name, column_name FROM information_schema.change_stream_columns ORDER BY 1, 2, 3, 4")
	if err != nil {
		return Tracking{}, err
	}
	defer columns.Close()
	for columns.Next() {
		var row Tracked
		if err := columns.Scan(&row.Stream, &row.Schema, &row.Table, &row.Column); err != nil {
			return Tracking{}, err
		}
		tracking.Columns = append(tracking.Columns, row)
	}
	return tracking, columns.Err()
}

// Blocker is a change stream that names a table, with the table's columns it
// names in the column list of its FOR clause, in name order; none for a
// stream that names the table whole.
type Blocker struct {
	Stream  string
	Columns []string
}

// Blockers are the change streams that name table schema.table, each once and
// in name order: what a read reports as the table's removal blockers.
func (t Tracking) Blockers(schema, table string) []Blocker {
	var result []Blocker
	for _, row := range t.Tables {
		if row.Schema != schema || row.Table != table || slices.ContainsFunc(result, func(seen Blocker) bool { return seen.Stream == row.Stream }) {
			continue
		}
		blocker := Blocker{Stream: row.Stream}
		for _, column := range t.Columns {
			if column.Stream == row.Stream && column.Schema == schema && column.Table == table && !slices.Contains(blocker.Columns, column.Column) {
				blocker.Columns = append(blocker.Columns, column.Column)
			}
		}
		slices.Sort(blocker.Columns)
		result = append(result, blocker)
	}
	slices.SortFunc(result, func(a, b Blocker) int { return strings.Compare(a.Stream, b.Stream) })
	return result
}

// maxReason is the longest reason a host shows: one line of at most 512
// bytes (provider.ValidateDiagnostic).
const maxReason = 512

// Refusal returns why a batch of total units that makes drops cannot run when
// a change stream names one of them: it names the first such drop, the
// streams that name it, and what to do instead, in one line a host shows. It
// returns the empty string when no stream names any of the drops.
func Refusal(drops []Drop, tracking Tracking, total int) string {
	for _, drop := range drops {
		streams := namedBy(drop, tracking)
		if len(streams) == 0 {
			continue
		}
		for _, reason := range reasons(drop, streams, total) {
			if len(reason) <= maxReason {
				return reason
			}
		}
	}
	return ""
}

// namedBy returns the streams that name what drop drops, each once and in
// name order: for a table, every stream that names the table; for a column,
// every stream that names the column in its column list.
func namedBy(drop Drop, tracking Tracking) []string {
	rows := tracking.Tables
	if drop.Column != "" {
		rows = tracking.Columns
	}
	var streams []string
	for _, row := range rows {
		if row.Schema == drop.Schema && row.Table == drop.Table && row.Column == drop.Column {
			streams = append(streams, row.Stream)
		}
	}
	slices.Sort(streams)
	return slices.Compact(streams)
}

// reasons are the ways to say why drop cannot run, longest first: with the
// names of the streams, with their number, and with neither the streams nor
// the table.
func reasons(drop Drop, streams []string, total int) []string {
	what, kind := "table "+tablePhrase(drop), "table"
	remedy := "drop the change stream, or alter it to stop tracking the table, before dropping the table"
	if drop.Column != "" {
		what, kind = fmt.Sprintf("column %q of table %s", drop.Column, tablePhrase(drop)), "column"
		remedy = "alter the change stream to stop tracking the column, or drop it, before dropping the column"
	}
	counted, verb := "a change stream", "names"
	if len(streams) > 1 {
		counted, verb = fmt.Sprintf("%d change streams", len(streams)), "name"
	}
	reason := func(what, streams, verb string) string {
		return fmt.Sprintf("unit %d of %d drops %s, which %s %s, so no unit ran: Spanner refuses to drop a %s a change stream names; %s",
			drop.Unit+1, total, what, streams, verb, kind, remedy)
	}
	return []string{reason(what, streamPhrase(streams), verb), reason(what, counted, verb), reason("a "+kind, "a change stream", "names")}
}

// tablePhrase names the table of drop, qualified by its schema unless that is
// the default one.
func tablePhrase(drop Drop) string {
	if drop.Schema == ddl.DefaultSchema {
		return fmt.Sprintf("%q", drop.Table)
	}
	return fmt.Sprintf("%q.%q", drop.Schema, drop.Table)
}

// streamPhrase names one stream or several.
func streamPhrase(streams []string) string {
	quoted := make([]string, len(streams))
	for i, stream := range streams {
		quoted[i] = fmt.Sprintf("%q", stream)
	}
	if len(quoted) == 1 {
		return "change stream " + quoted[0]
	}
	return "change streams " + strings.Join(quoted[:len(quoted)-1], ", ") + " and " + quoted[len(quoted)-1]
}
