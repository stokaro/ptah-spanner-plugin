// Package sqltext splits a text of Spanner's PostgreSQL dialect into
// statements and tokens, and says what each statement does to the schema.
//
// The lexing follows PostgreSQL's: double-quoted identifiers with doubled
// quotes, single-quoted strings with doubled quotes, `--` and `/* */`
// comments, and a semicolon that ends a statement outside a quote or a
// comment. A dollar-quoted string is refused rather than guessed at.
package sqltext

import (
	"errors"
	"fmt"
	"math"
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
var ErrUnknownStatement = errors.New("the provider analyzes CREATE TABLE, DROP TABLE and ALTER TABLE ... ADD COLUMN or DROP COLUMN only")

// Split splits a text into statements. Every byte of the text belongs to a
// statement or is white space between statements: a comment before a
// statement starts it, and a comment after the last one ends it. A statement
// the provider does not know fails the whole text.
func Split(text string) ([]Statement, error) {
	if uint64(len(text)) > math.MaxUint32 {
		return nil, fmt.Errorf("a SQL text is larger than the protocol can address")
	}
	var statements []Statement
	start := -1
	var source []string
	finish := func(end int) error {
		statement, err := classify(text, start, end, source)
		if err != nil {
			return err
		}
		statements = append(statements, statement)
		start, source = -1, nil
		return nil
	}
	for position := 0; position < len(text); {
		character := text[position]
		if isSpace(character) {
			position++
			continue
		}
		if start < 0 {
			start = position
		}
		end, err := tokenEnd(text, position)
		if err != nil {
			return nil, err
		}
		token := text[position:end]
		position = end
		switch {
		case strings.HasPrefix(token, "--") || strings.HasPrefix(token, "/*"):
			continue
		case token == ";":
			if len(source) == 0 {
				return nil, fmt.Errorf("an empty statement ends on line %d", 1+strings.Count(text[:end], "\n"))
			}
			if err := finish(end); err != nil {
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

// operators are the characters an operator token is made of.
const operators = "+-*/<>=!~%^&|?:#@"

// tokenEnd returns where the token that starts at position ends.
func tokenEnd(text string, position int) (int, error) {
	character := text[position]
	switch {
	case strings.HasPrefix(text[position:], "--") || strings.HasPrefix(text[position:], "/*"):
		return commentEnd(text, position)
	case character == '"' || character == '\'':
		return quotedEnd(text, position, character)
	case character == '$':
		return 0, fmt.Errorf("a dollar-quoted string is not part of the provider's SQL")
	case strings.IndexByte("(),;[].", character) >= 0:
		return position + 1, nil
	case isWordStart(character):
		return runEnd(text, position, isWordPart), nil
	case character >= '0' && character <= '9':
		return runEnd(text, position, isNumberPart), nil
	case strings.IndexByte(operators, character) >= 0:
		return operatorEnd(text, position), nil
	default:
		return 0, fmt.Errorf("the SQL text holds a character the provider does not read on line %d", 1+strings.Count(text[:position], "\n"))
	}
}

// commentEnd returns the end of the comment at position: a line comment ends
// before its newline, a block comment after its closing mark.
func commentEnd(text string, position int) (int, error) {
	if strings.HasPrefix(text[position:], "--") {
		if newline := strings.IndexByte(text[position:], '\n'); newline >= 0 {
			return position + newline, nil
		}
		return len(text), nil
	}
	end := strings.Index(text[position+2:], "*/")
	if end < 0 {
		return 0, fmt.Errorf("a comment is not closed")
	}
	return position + 2 + end + 2, nil
}

// runEnd returns the end of the run of characters part accepts that starts
// at position.
func runEnd(text string, position int, part func(byte) bool) int {
	end := position + 1
	for end < len(text) && part(text[end]) {
		end++
	}
	return end
}

// operatorEnd returns the end of an operator, which stops before a comment.
func operatorEnd(text string, position int) int {
	end := position + 1
	for end < len(text) && strings.IndexByte(operators, text[end]) >= 0 && !strings.HasPrefix(text[end:], "--") && !strings.HasPrefix(text[end:], "/*") {
		end++
	}
	return end
}

func isNumberPart(character byte) bool {
	return character >= '0' && character <= '9' || character == '.' || character == 'e' || character == 'E'
}

// quotedEnd returns the end of a quoted identifier or string, where a doubled
// quote stands for one.
func quotedEnd(text string, position int, quote byte) (int, error) {
	for end := position + 1; end < len(text); end++ {
		if text[end] != quote {
			continue
		}
		if end+1 < len(text) && text[end+1] == quote {
			end++
			continue
		}
		return end + 1, nil
	}
	if quote == '"' {
		return 0, fmt.Errorf("a quoted name is not closed")
	}
	return 0, fmt.Errorf("a string is not closed")
}

// classify describes the statement spanning text[start:end].
func classify(text string, start, end int, source []string) (Statement, error) {
	words := make([]string, len(source))
	for i, word := range source {
		words[i] = word
		if isWordStart(word[0]) {
			words[i] = strings.ToUpper(word)
		}
	}
	kind, err := kindOf(words)
	if err != nil {
		return Statement{}, err
	}
	return Statement{Start: start, End: end, Line: 1 + strings.Count(text[:start], "\n"), SourceWords: source, Words: words, Kind: kind}, nil
}

// kindOf reads the statement's leading words. The table name is one name or
// a schema and a name joined by a dot.
func kindOf(words []string) (Kind, error) {
	if len(words) < 3 || words[1] != "TABLE" {
		return 0, ErrUnknownStatement
	}
	switch words[0] {
	case "CREATE":
		return CreateTable, nil
	case "DROP":
		return DropTable, nil
	case "ALTER":
		rest := words[3:]
		if len(rest) >= 2 && rest[0] == "." {
			rest = rest[2:]
		}
		if len(rest) >= 3 && rest[1] == "COLUMN" {
			switch rest[0] {
			case "ADD":
				return AddColumn, nil
			case "DROP":
				return DropColumn, nil
			}
		}
	}
	return 0, ErrUnknownStatement
}

func isSpace(character byte) bool { return strings.IndexByte(" \t\r\n\f\v", character) >= 0 }

func isWordStart(character byte) bool {
	return character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character == '_'
}

func isWordPart(character byte) bool {
	return isWordStart(character) || character >= '0' && character <= '9' || character == '$'
}
