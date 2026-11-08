// Package sqltext splits a text of Spanner's PostgreSQL dialect into
// statements and tokens, and says what each statement does to the schema.
//
// The lexing is PostgreSQL's, as Spanner's PostgreSQL interface reads text,
// and comes from github.com/stokaro/ptah/sqlsyntax/pgsyntax, the module the
// host reads the PostgreSQL family with: quoted names, string constants in
// every form, dollar-quoted strings, nested block comments, and a semicolon
// that ends a statement outside all of them.
package sqltext

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Kind is what a statement does to the schema.
type Kind int

const (
	// CreateTable creates a table.
	CreateTable Kind = iota + 1
	// DropTable drops a table.
	DropTable
	// AddColumn adds a column to a table.
	AddColumn
	// DropColumn drops a column from a table.
	DropColumn
	// AlterColumn changes a column's type, default or nullability.
	AlterColumn
	// AddConstraint adds a foreign key or CHECK constraint to a table.
	AddConstraint
	// CreateIndex creates an index.
	CreateIndex
	// DropIndex drops an index.
	DropIndex
	// SetRowDeletion gives a table a row deletion policy or replaces it:
	// ALTER TABLE ... ADD TTL or ALTER TTL.
	SetRowDeletion
	// DropRowDeletion removes a table's row deletion policy: ALTER TABLE ...
	// DROP TTL.
	DropRowDeletion
	// CreateView creates a view or replaces its query.
	CreateView
	// DropView drops a view.
	DropView
	// CreateSequence creates a sequence.
	CreateSequence
	// AlterSequence restarts a sequence's counter.
	AlterSequence
	// DropSequence drops a sequence.
	DropSequence
	// Insert, Update and Delete change rows, and Select reads them. A
	// migration body holds them beside its schema statements.
	Insert
	Update
	Delete
	Select
)

// Statement is one statement of a text.
type Statement struct {
	// Start and End delimit the statement in the text, from its first token or
	// the comment before it to its semicolon, or to the end of the text.
	Start, End int
	// Line is the line Start is on, counted from 1.
	Line int
	// SourceWords are the tokens as written, and Words the same tokens with
	// every unquoted word in upper case.
	SourceWords, Words []string
	Kind               Kind
}

// ErrUnknownStatement reports a statement the provider does not know.
var ErrUnknownStatement = errors.New("the provider analyzes CREATE and DROP of a TABLE, an INDEX, a VIEW or a SEQUENCE, " +
	"CREATE OR REPLACE VIEW, ALTER SEQUENCE, ALTER TABLE ... ADD COLUMN, DROP COLUMN, ALTER COLUMN, ADD CONSTRAINT, ADD TTL, ALTER TTL or DROP TTL, " +
	"INSERT, UPDATE, DELETE and SELECT only")

// Split splits a text into statements. Every byte of the text belongs to a
// statement or is white space between statements: a comment before a
// statement starts it, and a comment after the last one ends it. A statement
// the provider does not know fails the whole text.
func Split(text string) ([]Statement, error) {
	tokens, err := Tokens(text)
	if err != nil {
		return nil, err
	}
	var statements []Statement
	start := -1
	var source []Token
	finish := func(end int) error {
		statement, err := classify(text, start, end, source)
		if err != nil {
			return err
		}
		statements = append(statements, statement)
		start, source = -1, nil
		return nil
	}
	for _, token := range tokens {
		if token.Kind == Space {
			continue
		}
		if start < 0 {
			start = token.Start
		}
		switch token.Kind {
		case Comment:
			continue
		case Semicolon:
			if len(source) == 0 {
				return nil, fmt.Errorf("an empty statement ends on line %d", 1+strings.Count(text[:token.End], "\n"))
			}
			if err := finish(token.End); err != nil {
				return nil, err
			}
		default:
			source = append(source, token)
		}
	}
	switch {
	case start >= 0 && len(source) > 0:
		if err := finish(len(text)); err != nil {
			return nil, err
		}
	case start >= 0 && len(statements) == 0:
		return nil, fmt.Errorf("the SQL text holds comments and no statement")
	case start >= 0:
		// Comments after the last statement end it.
		statements[len(statements)-1].End = len(text)
	}
	return statements, nil
}

// alterKind reads what an ALTER TABLE does from the words after the table's
// first name.
func alterKind(rest []string) (Kind, error) {
	if len(rest) >= 2 && rest[0] == "." {
		rest = rest[2:]
	}
	if len(rest) == 2 && rest[0] == "DROP" && rest[1] == "TTL" {
		return DropRowDeletion, nil
	}
	if len(rest) < 3 {
		return 0, ErrUnknownStatement
	}
	switch rest[0] + " " + rest[1] {
	case "ADD TTL", "ALTER TTL":
		return SetRowDeletion, nil
	case "ADD COLUMN":
		return AddColumn, nil
	case "DROP COLUMN":
		return DropColumn, nil
	case "ALTER COLUMN":
		return AlterColumn, nil
	case "ADD CONSTRAINT":
		return AddConstraint, nil
	}
	return 0, ErrUnknownStatement
}

// classify describes the statement spanning text[start:end], whose tokens
// are source with white space and comments left out.
func classify(text string, start, end int, source []Token) (Statement, error) {
	written := make([]string, len(source))
	words := make([]string, len(source))
	for i, token := range source {
		written[i], words[i] = token.Text, token.Upper()
	}
	kind, err := kindOf(words)
	if err != nil {
		return Statement{}, err
	}
	return Statement{Start: start, End: end, Line: 1 + strings.Count(text[:start], "\n"), SourceWords: written, Words: words, Kind: kind}, nil
}

// statementWords are the statements a kind is told by its leading words. The
// table name of an ALTER TABLE is one name or a schema and a name joined by a
// dot, and alterKind reads what follows it.
var statementWords = []struct {
	words []string
	kind  Kind
}{
	{words: []string{"CREATE", "TABLE"}, kind: CreateTable},
	{words: []string{"DROP", "TABLE"}, kind: DropTable},
	{words: []string{"CREATE", "INDEX"}, kind: CreateIndex},
	{words: []string{"CREATE", "UNIQUE", "INDEX"}, kind: CreateIndex},
	{words: []string{"DROP", "INDEX"}, kind: DropIndex},
	{words: []string{"CREATE", "VIEW"}, kind: CreateView},
	{words: []string{"CREATE", "OR", "REPLACE", "VIEW"}, kind: CreateView},
	{words: []string{"DROP", "VIEW"}, kind: DropView},
	{words: []string{"CREATE", "SEQUENCE"}, kind: CreateSequence},
	{words: []string{"ALTER", "SEQUENCE"}, kind: AlterSequence},
	{words: []string{"DROP", "SEQUENCE"}, kind: DropSequence},
}

// kindOf reads the statement's leading words.
func kindOf(words []string) (Kind, error) {
	if kind, found := dataKinds[words[0]]; found && len(words) > 1 {
		return kind, nil
	}
	if len(words) < 3 {
		return 0, ErrUnknownStatement
	}
	if words[0] == "ALTER" && words[1] == "TABLE" {
		return alterKind(words[3:])
	}
	for _, leading := range statementWords {
		if len(words) > len(leading.words) && slices.Equal(words[:len(leading.words)], leading.words) {
			return leading.kind, nil
		}
	}
	return 0, ErrUnknownStatement
}

// dataKinds are the statements that change or read rows, by their first
// word.
var dataKinds = map[string]Kind{"INSERT": Insert, "UPDATE": Update, "DELETE": Delete, "SELECT": Select}
