package sqltext

import (
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/stokaro/ptah/sqlsyntax/pgsyntax"
)

// TokenKind is what a token of a text is.
type TokenKind int

const (
	// Space is a run of white space.
	Space TokenKind = iota + 1
	// Comment is a `--` line comment or a `/* */` block comment, which may
	// hold nested comments.
	Comment
	// Word is an unquoted word or a number.
	Word
	// Quoted is a quoted name or a string constant: `"..."`, `U&"..."`,
	// `'...'`, `E'...'`, `U&'...'`, `N'...'`, `B'...'`, `X'...'` or a
	// dollar-quoted string. A string continued on a later line, and the
	// UESCAPE clause of a Unicode escape string or name, belong to its token.
	Quoted
	// Semicolon ends a statement.
	Semicolon
	// Punctuation is an operator, a positional parameter or one of `(),[].:`.
	Punctuation
)

// Token is one token of a text: what it is, where it is, and its text.
type Token struct {
	Kind       TokenKind
	Start, End int
	Text       string
	// Name reports a Quoted token that is a quoted name rather than a string.
	Name bool
	// Value is the text a Quoted token stands for, or the name an unquoted
	// word stands for in lower case, as Spanner's PostgreSQL interface folds
	// one. Valued reports whether there is one: a number, a bit string and a
	// constant whose escapes the server refuses have none.
	Value  string
	Valued bool
}

// options are the lexical rules Spanner's PostgreSQL interface reads text
// with: standard_conforming_strings is on and cannot be turned off.
var options = pgsyntax.Options{}

// Tokens returns every token of a text in order, white space and comments
// included, so the tokens cover the text with no gap. The lexing is
// PostgreSQL's (github.com/stokaro/ptah/sqlsyntax/pgsyntax). A string, a
// quoted name, a dollar-quoted string or a block comment that does not close
// fails the whole text with a [*LexError], naming the line it opens on.
func Tokens(text string) ([]Token, error) {
	if uint64(len(text)) > math.MaxUint32 {
		return nil, fmt.Errorf("a SQL text is larger than the protocol can address")
	}
	scanned, err := pgsyntax.Scan(text, options)
	if err != nil {
		return nil, lexError(text, err)
	}
	tokens := make([]Token, 0, len(scanned))
	for i := 0; i < len(scanned); {
		token, next := readToken(text, scanned, i)
		tokens = append(tokens, token)
		i = next
	}
	return tokens, nil
}

// readToken reads the token that starts at scanned[i], and returns it and the
// index of the scanned token after it.
func readToken(text string, scanned []pgsyntax.Token, i int) (Token, int) {
	first := scanned[i]
	token := Token{Kind: tokenKind(text, first), Start: first.Start, End: first.End, Text: first.Text(text)}
	switch first.Kind {
	case pgsyntax.String, pgsyntax.QuotedIdentifier, pgsyntax.DollarString:
		token.Name = first.Kind == pgsyntax.QuotedIdentifier
		value, next, ok := pgsyntax.Value(text, scanned, i, options)
		if !ok {
			return token, i + 1
		}
		token.End = scanned[next-1].End
		token.Text = text[token.Start:token.End]
		token.Value, token.Valued = value, true
		return token, next
	case pgsyntax.Word:
		token.Value, token.Valued = strings.ToLower(token.Text), true
	}
	return token, i + 1
}

// tokenKind is the kind of a scanned token.
func tokenKind(text string, token pgsyntax.Token) TokenKind {
	switch token.Kind {
	case pgsyntax.Whitespace:
		return Space
	case pgsyntax.LineComment, pgsyntax.BlockComment:
		return Comment
	case pgsyntax.Word, pgsyntax.Number:
		return Word
	case pgsyntax.String, pgsyntax.QuotedIdentifier, pgsyntax.DollarString:
		return Quoted
	case pgsyntax.Punctuation:
		if text[token.Start] == ';' {
			return Semicolon
		}
	}
	return Punctuation
}

// lexError places a scanner's refusal on the line its construct opens on.
func lexError(text string, err error) error {
	syntax, ok := errors.AsType[*pgsyntax.SyntaxError](err)
	if !ok {
		return err
	}
	line := 1 + strings.Count(text[:syntax.Offset], "\n")
	return &LexError{Offset: syntax.Offset, Reason: fmt.Sprintf("%s on line %d", syntax.Reason, line)}
}

// Upper returns the token as the provider matches it: an unquoted word in
// upper case, and any other token as written.
func (t Token) Upper() string {
	if t.Kind == Word && t.Valued {
		return strings.ToUpper(t.Text)
	}
	return t.Text
}
