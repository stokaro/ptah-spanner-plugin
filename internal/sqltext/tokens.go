package sqltext

import (
	"fmt"
	"math"
	"strings"
)

// TokenKind is what a token of a text is.
type TokenKind int

const (
	// Space is a run of white space.
	Space TokenKind = iota + 1
	// Comment is a `--` line comment or a `/* */` block comment.
	Comment
	// Word is an unquoted word or a number.
	Word
	// Quoted is a double-quoted name or a single-quoted string.
	Quoted
	// Semicolon ends a statement.
	Semicolon
	// Punctuation is an operator or one of `(),[].`.
	Punctuation
)

// Token is one token of a text: what it is, where it is, and its text.
type Token struct {
	Kind       TokenKind
	Start, End int
	Text       string
}

// Tokens returns every token of a text in order, white space and comments
// included, so the tokens cover the text with no gap. The lexing is Split's:
// a dollar-quoted string, an unclosed quote or comment, or a character the
// provider does not read fails the whole text.
func Tokens(text string) ([]Token, error) {
	if uint64(len(text)) > math.MaxUint32 {
		return nil, fmt.Errorf("a SQL text is larger than the protocol can address")
	}
	var tokens []Token
	for position := 0; position < len(text); {
		if isSpace(text[position]) {
			end := runEnd(text, position, isSpace)
			tokens = append(tokens, Token{Kind: Space, Start: position, End: end, Text: text[position:end]})
			position = end
			continue
		}
		end, err := tokenEnd(text, position)
		if err != nil {
			return nil, err
		}
		token := Token{Start: position, End: end, Text: text[position:end]}
		switch {
		case strings.HasPrefix(token.Text, "--") || strings.HasPrefix(token.Text, "/*"):
			token.Kind = Comment
		case token.Text == ";":
			token.Kind = Semicolon
		case token.Text[0] == '"' || token.Text[0] == '\'':
			token.Kind = Quoted
		case isWordStart(token.Text[0]) || token.Text[0] >= '0' && token.Text[0] <= '9':
			token.Kind = Word
		default:
			token.Kind = Punctuation
		}
		tokens = append(tokens, token)
		position = end
	}
	return tokens, nil
}
