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
	ended := false
	for position := 0; position < len(text); {
		if isSpace(text[position]) {
			position++
			continue
		}
		end, err := tokenEnd(text, position)
		if err != nil {
			return nil, nil, err
		}
		token := text[position:end]
		position = end
		switch {
		case strings.HasPrefix(token, "--") || strings.HasPrefix(token, "/*"):
			continue
		case ended:
			return nil, nil, ErrSeveralStatements
		case token == ";":
			ended = true
			continue
		}
		source = append(source, token)
		word := token
		if isWordStart(token[0]) {
			word = strings.ToUpper(token)
		}
		words = append(words, word)
	}
	return source, words, nil
}

// Unquote returns the name a token spells: a quoted name without its quotes
// and with each doubled quote halved, and an unquoted word in lower case, as
// Spanner's PostgreSQL interface folds one.
func Unquote(token string) string {
	if len(token) >= 2 && token[0] == '"' && token[len(token)-1] == '"' {
		return strings.ReplaceAll(token[1:len(token)-1], `""`, `"`)
	}
	return strings.ToLower(token)
}
