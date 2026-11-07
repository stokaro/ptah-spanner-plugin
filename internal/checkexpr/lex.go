package checkexpr

import (
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// tokenKind is what a token is.
type tokenKind int

const (
	tokenEnd tokenKind = iota
	// tokenWord is an unquoted name or keyword, folded to lower case as the
	// server folds it.
	tokenWord
	// tokenName is a double-quoted name, kept as spelled.
	tokenName
	tokenNumber
	tokenString
	tokenOperator
	// tokenPunct is one of ( ) [ ] , and the cast operator ::.
	tokenPunct
)

type token struct {
	kind tokenKind
	text string
}

// operatorChars are the characters a PostgreSQL operator is made of.
const operatorChars = "+-*/<>=~!@#%^&|`?"

// lex splits an expression into tokens. It refuses what it cannot read:
// comments, escape and bit strings, and characters no expression holds.
func lex(input string) ([]token, error) {
	var tokens []token
	for i := 0; i < len(input); {
		r, size := utf8.DecodeRuneInString(input[i:])
		if unicode.IsSpace(r) {
			i += size
			continue
		}
		t, next, err := nextToken(input, i, r)
		if err != nil {
			return nil, err
		}
		tokens = append(tokens, t)
		i = next
	}
	return append(tokens, token{kind: tokenEnd}), nil
}

// nextToken reads the token that starts at input[i] with the rune r, and
// returns where the next one starts.
func nextToken(input string, i int, r rune) (token, int, error) {
	switch {
	case strings.HasPrefix(input[i:], "--") || strings.HasPrefix(input[i:], "/*"):
		return token{}, 0, fmt.Errorf("a comment")
	case r == '\'' || r == '"':
		return quotedToken(input, i)
	case strings.HasPrefix(input[i:], "::"):
		return token{kind: tokenPunct, text: "::"}, i + 2, nil
	case strings.ContainsRune("()[],", r):
		return token{kind: tokenPunct, text: string(r)}, i + 1, nil
	case r >= '0' && r <= '9' || r == '.' && i+1 < len(input) && input[i+1] >= '0' && input[i+1] <= '9':
		text, next := number(input, i)
		return token{kind: tokenNumber, text: text}, next, nil
	case r == '_' || unicode.IsLetter(r):
		return wordToken(input, i)
	case strings.ContainsRune(operatorChars, r):
		text := operator(input[i:])
		return token{kind: tokenOperator, text: text}, i + len(text), nil
	}
	return token{}, 0, fmt.Errorf("the character %q", r)
}

// quotedToken reads a string or a quoted name that starts at input[start].
func quotedToken(input string, start int) (token, int, error) {
	kind := tokenString
	if input[start] == '"' {
		kind = tokenName
	}
	text, next, err := quoted(input, start, input[start])
	if err != nil {
		return token{}, 0, err
	}
	if kind == tokenName && text == "" {
		return token{}, 0, fmt.Errorf("an empty quoted name")
	}
	return token{kind: kind, text: text}, next, nil
}

// wordToken reads an unquoted name or keyword that starts at input[start],
// folded to lower case. A prefix that makes the string after it an escape,
// bit or national string is refused.
func wordToken(input string, start int) (token, int, error) {
	i := start
	for i < len(input) {
		r, size := utf8.DecodeRuneInString(input[i:])
		if r != '_' && r != '$' && !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			break
		}
		i += size
	}
	word := strings.ToLower(input[start:i])
	if i < len(input) && input[i] == '\'' && slices.Contains([]string{"e", "b", "x", "n"}, word) {
		return token{}, 0, fmt.Errorf("a string with the prefix %s", word)
	}
	return token{kind: tokenWord, text: word}, i, nil
}

// quoted reads a string or a quoted name that starts at input[start], with a
// doubled quote standing for one.
func quoted(input string, start int, quote byte) (string, int, error) {
	var b strings.Builder
	for i := start + 1; i < len(input); i++ {
		if input[i] != quote {
			b.WriteByte(input[i])
			continue
		}
		if i+1 < len(input) && input[i+1] == quote {
			b.WriteByte(quote)
			i++
			continue
		}
		return b.String(), i + 1, nil
	}
	return "", 0, fmt.Errorf("an unterminated quote")
}

// number reads a numeric literal: digits, a fraction and an exponent.
func number(input string, start int) (string, int) {
	i := start
	digits := func() {
		for i < len(input) && input[i] >= '0' && input[i] <= '9' {
			i++
		}
	}
	digits()
	if i < len(input) && input[i] == '.' {
		i++
		digits()
	}
	if i < len(input) && (input[i] == 'e' || input[i] == 'E') {
		j := i + 1
		if j < len(input) && (input[j] == '+' || input[j] == '-') {
			j++
		}
		if j < len(input) && input[j] >= '0' && input[j] <= '9' {
			i = j
			digits()
		}
	}
	return input[start:i], i
}

// operator reads the longest operator at the start of input, by PostgreSQL's
// rule: an operator of several characters ends in + or - only when it holds
// one of ~ ! @ # % ^ & | ` ?, so `>-5` is `>` followed by `-5`. A comment
// start ends it.
func operator(input string) string {
	end := 0
	for end < len(input) && strings.IndexByte(operatorChars, input[end]) >= 0 {
		if strings.HasPrefix(input[end:], "--") || strings.HasPrefix(input[end:], "/*") {
			break
		}
		end++
	}
	if end == 0 {
		return input[:1]
	}
	text := input[:end]
	for len(text) > 1 && strings.ContainsAny(text[len(text)-1:], "+-") && !strings.ContainsAny(text, "~!@#%^&|`?") {
		text = text[:len(text)-1]
	}
	return text
}
