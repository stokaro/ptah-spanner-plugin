// Package migrationlint answers the host's migration lint for Spanner: it
// splits a migration into statements as Spanner reads them, says what each
// statement is (its kind, a table it creates without a primary key, a
// capability it needs), and checks the provider's own rules.
//
// The host keeps its rules that hold on every engine, such as the DS and BC
// families, and runs them over this package's split. So a statement's word
// forms are the ones the host's own scanner gives a PostgreSQL-family text
// (ptah.run/internal/lintscan): a bare word in upper case with its digits,
// each other character of punctuation on its own, and a string or a quoted
// name as written. The host's rules then read a Spanner migration exactly as
// they read it while Spanner was an engine Ptah built in.
package migrationlint

import (
	"strings"

	"github.com/stokaro/ptah-spanner-plugin/internal/sqltext"
)

// Statement is one statement of a migration source, in the forms the host's
// lint rules read.
type Statement struct {
	// Start and End delimit the statement in its source, from its first token
	// to its last; the semicolon that ends it and the comments before it are
	// outside.
	Start, End int
	// Line is the line Start is on, counted from 1.
	Line int
	// SQL is the source's text from Start to End.
	SQL string
	// Canonical is the statement without comments, every run of white space
	// one space, and everything but strings and quoted names in upper case.
	Canonical string
	// Words are the tokens rules scan, and SourceWords the same tokens as
	// written: see the package documentation.
	Words, SourceWords []string
	// Comments are the comments directly above the statement, where the host
	// reads a suppression directive: those after the line the statement
	// before it ends on, with no blank line between them and the statement.
	Comments []string
}

// Split splits a migration source into its statements. A text the provider's
// lexer cannot read fails, and returns no statement; a statement of any kind
// is part of the split, because a migration holds data statements, views and
// sequences beside the tables the provider writes.
func Split(text string) ([]Statement, error) {
	tokens, err := sqltext.Tokens(text)
	if err != nil {
		return nil, err
	}
	var statements []Statement
	var pending, active []string
	start, end, lastEndLine := -1, 0, 0
	emit := func() {
		statements = append(statements, statement(text, tokens, start, end, active))
		start, active = -1, nil
	}
	for _, token := range tokens {
		switch token.Kind {
		case sqltext.Semicolon:
			if start >= 0 {
				emit()
				lastEndLine = lineAt(text, token.Start)
			}
		case sqltext.Space:
			// A blank line detaches the comments above it from the
			// statement below.
			if start < 0 && strings.Count(token.Text, "\n") > 1 {
				pending = nil
			}
		case sqltext.Comment:
			if start < 0 && lineAt(text, token.Start) != lastEndLine {
				pending = append(pending, token.Text)
			}
		default:
			if start < 0 {
				start, active, pending = token.Start, pending, nil
			}
			end = token.End
		}
	}
	if start >= 0 {
		emit()
	}
	return statements, nil
}

// statement renders the statement text[start:end] with the comments above
// it.
func statement(text string, tokens []sqltext.Token, start, end int, comments []string) Statement {
	result := Statement{Start: start, End: end, Line: lineAt(text, start), SQL: text[start:end], Comments: comments}
	var canonical strings.Builder
	space := false
	for _, token := range tokens {
		if token.Start < start || token.End > end {
			continue
		}
		switch token.Kind {
		case sqltext.Space, sqltext.Comment:
			// A comment separates two tokens as white space does.
			space = canonical.Len() > 0
			continue
		case sqltext.Quoted:
			result.Words = append(result.Words, token.Text)
			result.SourceWords = append(result.SourceWords, token.Text)
		default:
			for _, word := range lintWords(token.Text) {
				result.Words = append(result.Words, strings.ToUpper(word))
				result.SourceWords = append(result.SourceWords, word)
			}
		}
		if space {
			canonical.WriteByte(' ')
			space = false
		}
		if token.Kind == sqltext.Quoted {
			canonical.WriteString(token.Text)
		} else {
			canonical.WriteString(strings.ToUpper(token.Text))
		}
	}
	result.Canonical = canonical.String()
	return result
}

// lintWords cuts an unquoted token into the words the host's scanner reads:
// each run of letters, digits and underscores is one word, and every other
// character is one of its own. The provider's lexer keeps `1.5`, `x$1` and
// `>=` whole; the host's scanner reads them as `1 . 5`, `x $ 1` and `> =`.
func lintWords(token string) []string {
	var words []string
	for position := 0; position < len(token); {
		end := position + 1
		if wordPart(token[position]) {
			for end < len(token) && wordPart(token[end]) {
				end++
			}
		}
		words = append(words, token[position:end])
		position = end
	}
	return words
}

// wordPart reports a character that continues a word in the host's scanner.
func wordPart(character byte) bool {
	return character == '_' || character >= '0' && character <= '9' || character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z'
}

// lineAt is the line of text that offset is on, counted from 1.
func lineAt(text string, offset int) int { return 1 + strings.Count(text[:offset], "\n") }
