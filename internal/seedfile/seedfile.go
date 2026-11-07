// Package seedfile reads a seed file the way the provider runs it: the file
// runs in one read-write transaction of the provider's, statement by
// statement, and every statement is checked before the first one runs, so a
// file the provider cannot run whole runs not at all.
package seedfile

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/stokaro/ptah-spanner-plugin/internal/sqltext"
)

// Statement is one statement of a seed file.
type Statement struct {
	// Body is the statement's text without its semicolon.
	Body string
	// Number counts the statement from 1, and Line is the line it starts on.
	Number, Line int
}

// Refusal is a file the provider runs nothing of: Offset is where the
// statement it stops at starts, in bytes, and Reason says why in one line
// that quotes nothing of the file.
type Refusal struct {
	Offset int
	Reason string
}

func (r *Refusal) Error() string { return r.Reason }

// runnable are the statements a seed file runs: the row writes and the
// queries Spanner runs inside a read-write transaction.
var runnable = map[string]bool{"INSERT": true, "UPDATE": true, "DELETE": true, "SELECT": true, "WITH": true, "VALUES": true}

// schemaWords start a schema statement, which Spanner runs only outside a
// transaction: `DDL statements are only allowed outside explicit
// transactions` (SQLSTATE 25000).
var schemaWords = map[string]bool{
	"CREATE": true, "ALTER": true, "DROP": true, "TRUNCATE": true, "GRANT": true, "REVOKE": true, "ANALYZE": true, "COMMENT": true, "RENAME": true,
}

// transactionWords start a statement that would open, end or mark the
// transaction the file runs in.
var transactionWords = map[string]bool{
	"BEGIN": true, "START": true, "COMMIT": true, "END": true, "ROLLBACK": true, "ABORT": true, "SAVEPOINT": true, "RELEASE": true, "PREPARE": true,
}

// Statements returns the statements of a seed file in the order they run, or
// a *Refusal at the first statement the provider does not run inside its
// transaction: a schema statement, a transaction statement, or any statement
// that is not a row write or a query, such as a SET, which would change the
// session beyond the file. A file of no statement has none.
func Statements(text string) ([]Statement, error) {
	spans, err := sqltext.Spans(text)
	if unreadable, ok := errors.AsType[*sqltext.LexError](err); ok {
		return nil, &Refusal{Offset: unreadable.Offset, Reason: "the provider cannot split the file into statements: " + unreadable.Reason}
	}
	if err != nil {
		return nil, err
	}
	statements := make([]Statement, len(spans))
	for i, span := range spans {
		first := span.Words[0]
		switch {
		case runnable[first]:
		case schemaWords[first]:
			return nil, &Refusal{Offset: span.Start, Reason: fmt.Sprintf(
				"statement %d is a schema statement, which Spanner runs only outside a transaction; a seed file runs in one", i+1)}
		case transactionWords[first]:
			return nil, &Refusal{Offset: span.Start, Reason: fmt.Sprintf(
				"statement %d controls a transaction; the provider runs the file in a transaction of its own, which the file cannot open, end or mark", i+1)}
		default:
			return nil, &Refusal{Offset: span.Start, Reason: fmt.Sprintf(
				"statement %d is not a row write or a query; a seed file runs INSERT, UPDATE, DELETE and queries", i+1)}
		}
		statements[i] = Statement{Body: span.Body, Number: i + 1, Line: span.Line}
	}
	return statements, nil
}

// Position is the line and the column, in characters, of a byte offset of
// text, each counted from 1.
func Position(text string, offset int) (line, column int) {
	offset = min(max(offset, 0), len(text))
	line = strings.Count(text[:offset], "\n") + 1
	column = utf8.RuneCountInString(text[strings.LastIndexByte(text[:offset], '\n')+1:offset]) + 1
	return line, column
}
