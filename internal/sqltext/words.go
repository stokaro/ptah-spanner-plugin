package sqltext

import (
	"errors"
	"strings"
)

// ErrSeveralStatements reports a text that holds more than one statement
// where one is expected.
var ErrSeveralStatements = errors.New("the text holds more than one statement")

// Words reads the one statement of text, whatever it does: source holds its
// tokens as written and words the same tokens with every unquoted word in
// upper case, with comments and a final semicolon left out. A semicolon
// before another token is refused with ErrSeveralStatements, and a text the
// lexer cannot read is refused with the lexer's error. A text with no token,
// or a semicolon alone, has no words.
func Words(text string) (source, words []string, err error) {
	tokens, err := Tokens(text)
	if err != nil {
		return nil, nil, err
	}
	ended := false
	for _, token := range tokens {
		switch {
		case token.Kind == Space || token.Kind == Comment:
			continue
		case ended:
			return nil, nil, ErrSeveralStatements
		case token.Kind == Semicolon:
			ended = true
			continue
		}
		source = append(source, token.Text)
		words = append(words, upper(token))
	}
	return source, words, nil
}

// Unquote returns the name a token spells: a quoted name, `"..."` or
// `U&"..."`, with its quoting undone, and an unquoted word in lower case, as
// Spanner's PostgreSQL interface folds one.
func Unquote(token string) string {
	tokens, err := Tokens(token)
	if err == nil && len(tokens) == 1 && tokens[0].Valued && (tokens[0].Name || tokens[0].Kind == Word) {
		return tokens[0].Value
	}
	return strings.ToLower(token)
}
