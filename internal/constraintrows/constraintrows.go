// Package constraintrows keeps a batch of schema statements from adding a rule
// that rows already in a table break, before any statement of the batch runs:
// a CHECK, a foreign key, a unique index, NOT NULL, or a shorter limit on a
// column's characters.
//
// Spanner checks such a rule against every row the table holds, and refuses
// the statement when a row breaks it. Measured on the emulator behind
// PGAdapter 0.56.2, each with SQLSTATE P0001 unless named:
//
//   - "Check constraint `items`.`k1` is violated for key {Int64(1)}";
//   - "Foreign key `ca_fk` constraint violation on table `ca`. Cannot find
//     referenced key ... in table `pa`." (SQLSTATE 23503);
//   - "Found uniqueness violation on index u_a, duplicate key: {Int64(5)}";
//   - "Cannot specify a null value for column: a in table: n referenced by
//     key: {Int64(1)}", also for a column with a default;
//   - "Reducing the length of column s to 3 is not allowed because it has a
//     value of length 8", and for a bytea column "Alteration of column y is
//     not allowed because it has an invalid UTF-8 character".
//
// Spanner runs each schema statement on its own and commits it, so the
// statements before the refused one stay applied. A changed CHECK or foreign
// key is replaced by DROP CONSTRAINT and then ADD CONSTRAINT, an index that
// becomes unique by DROP INDEX and then CREATE UNIQUE INDEX, and such a run
// leaves the table without the constraint or the index (stokaro/ptah#4204 S93,
// S94).
//
// So the executor finds the rules a batch adds (Additions), counts the rows of
// each table that break its rule (Addition.Count), and refuses the batch
// before its first unit when a row does (Refusal). A rule the batch's first
// statement adds is not counted: when Spanner refuses it, nothing of the batch
// has run, and the count would read the whole table once more for nothing.
//
// The count reads the rows as they are before the batch runs. A statement
// before the addition that changes those rows, or the columns the rule reads,
// makes the count wrong, so Additions leaves such an addition to Spanner; see
// Additions.
package constraintrows

import (
	"slices"

	"github.com/stokaro/ptah-spanner-plugin/internal/sqltext"
)

// Kind is the rule an addition asks every row of its table to keep.
type Kind int

const (
	// Check is a CHECK constraint, whose expression is Expression.
	Check Kind = iota + 1
	// ForeignKey is a foreign key: Columns are the table's key columns, and
	// Referenced and ReferencedColumns the table and the columns they match.
	ForeignKey
	// UniqueIndex is a unique index: Name is the index, Columns its key
	// columns and Expression its WHERE filter, empty for none.
	UniqueIndex
	// NotNull makes the column Columns names NOT NULL.
	NotNull
	// CharacterLimit changes the column Columns names to character varying
	// of at most Limit characters, or an array of such values when Array is
	// set.
	CharacterLimit
)

// Addition is a rule a statement of a batch adds to a table, which every row
// of the table has to keep.
type Addition struct {
	// Unit is the index of the statement in the batch.
	Unit int
	Kind Kind
	// Table names the table. A statement that names no schema names the
	// default one.
	Table sqltext.TableName
	// Name is the constraint's or the index's name, empty when the statement
	// gives none and for NotNull and CharacterLimit.
	Name string
	// Columns are the columns the rule names, in order: the key of a foreign
	// key or a unique index, or the one column of NotNull and CharacterLimit.
	Columns []string
	// Expression is a CHECK's expression or a unique index's filter, as the
	// statement writes it.
	Expression string
	// Referenced and ReferencedColumns are what a foreign key references.
	Referenced        sqltext.TableName
	ReferencedColumns []string
	// Limit and Array are what CharacterLimit changes the column to.
	Limit int64
	Array bool
	// Unfilled reports a NotNull column a statement before it adds without
	// a default, so that every row holds a null in it.
	Unfilled bool
}

// Additions returns the rules the statements add to a table, in statement
// order, except one the batch's first statement adds. A statement may hold
// more than one SQL statement.
//
// The statements read are the ones Spanner takes, with a table qualified by
// its schema or not:
//
//   - `ALTER TABLE [IF EXISTS] t ADD [CONSTRAINT c] CHECK (e)`;
//   - `ALTER TABLE [IF EXISTS] t ADD [CONSTRAINT c] FOREIGN KEY (a, ...)
//     REFERENCES r (b, ...)`, with ON DELETE CASCADE or NO ACTION and ON
//     UPDATE NO ACTION;
//   - `CREATE UNIQUE INDEX i ON t (a [ASC|DESC] [NULLS FIRST|LAST], ...)`,
//     with INCLUDE and WHERE;
//   - `ALTER TABLE [IF EXISTS] t ALTER COLUMN a SET NOT NULL`;
//   - `ALTER TABLE [IF EXISTS] t ALTER COLUMN a [SET DATA] TYPE varchar(n)`,
//     any spelling of character varying with or without a length, or an
//     array of it.
//
// A statement of another shape, such as a CHECK with a clause after its
// expression, a foreign key without the referenced columns, or a unique index
// with IF NOT EXISTS or INTERLEAVE IN, is not counted: Spanner decides it.
//
// The count reads the rows before the batch runs, so an addition is left to
// Spanner, and not returned, when a statement before it may change the rows
// or the columns its rule reads:
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
//     whose rule reads the column: Spanner fills a column added with a default
//     in every row, and a rule that reads a column the batch adds cannot be
//     counted before the column exists. One case is counted: NOT NULL on a
//     column a statement before it adds without a default, which holds a null
//     in every row, so every row breaks it.
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
			if addition := read.addition; addition != nil && seen > 0 {
				table := changed[addition.Table]
				addition.Unfilled = addition.Kind == NotNull && table.unfilled(addition.Columns[0])
				if addition.Unfilled || !blocked(*addition, changed) {
					addition.Unit = unit
					additions = append(additions, *addition)
				}
			}
			if read.table != nil {
				table := changedTable(changed, *read.table)
				table.record(read.column)
				if read.unfilled {
					table.nulls = append(table.nulls, read.column)
				}
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
	// nulls are the columns the batch adds without a default, which hold a
	// null in every row while no later statement changes them.
	nulls []string
}

// record notes a change of column, or of the whole table for the empty name.
// A changed column no longer holds the nulls an addition left in it.
func (c *change) record(column string) {
	if column == "" {
		c.whole = true
		return
	}
	c.nulls = slices.DeleteFunc(c.nulls, func(added string) bool { return added == column })
	if !slices.Contains(c.columns, column) {
		c.columns = append(c.columns, column)
	}
}

// unfilled reports whether column is one the batch adds without a default,
// unchanged since, in a table it does not change whole. A nil change has none.
func (c *change) unfilled(column string) bool {
	return c != nil && !c.whole && slices.Contains(c.nulls, column)
}

// covers reports whether c changes the whole table or one of columns. A nil
// change covers nothing.
func (c *change) covers(columns []string) bool {
	return c != nil && (c.whole || slices.ContainsFunc(columns, func(column string) bool { return slices.Contains(c.columns, column) }))
}

// mentioned reports whether c changes the whole table or a column expression
// names. A nil change mentions nothing.
func (c *change) mentioned(expression string) bool {
	return c != nil && (c.whole || mentions(expression, c.columns))
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
// the columns its rule reads: its table, or the table a foreign key
// references, whole, or a column the rule names.
func blocked(addition Addition, changed map[sqltext.TableName]*change) bool {
	table := changed[addition.Table]
	switch addition.Kind {
	case Check:
		return table.mentioned(addition.Expression)
	case ForeignKey:
		return table.covers(addition.Columns) || changed[addition.Referenced].covers(addition.ReferencedColumns)
	case UniqueIndex:
		return table.covers(addition.Columns) || addition.Expression != "" && table.mentioned(addition.Expression)
	}
	return table.covers(addition.Columns)
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
