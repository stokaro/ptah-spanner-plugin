package sqltext

import (
	"strings"
	"unicode/utf8"
)

// Span is one statement of a text, whatever it does.
type Span struct {
	// Start is where the statement's first token starts, and End where the
	// statement ends: after its semicolon, or at the end of the text.
	Start, End int
	// Line and Column place Start, each counted from 1, the column in
	// characters.
	Line, Column int
	// Words are the statement's first tokens, at most three, with every
	// unquoted word in upper case.
	Words []string
	// Body is the statement's text from its first token to the end of its
	// last one, without the semicolon.
	Body string
}

// LexError is a text the lexer cannot read, with the offset of the construct
// that does not close.
type LexError struct {
	Offset int
	Reason string
}

func (e *LexError) Error() string { return e.Reason }

// leadingWords is how many tokens a Span keeps.
const leadingWords = 3

// Spans splits a text into its statements with the lexing Split uses, but
// for any statement, not only the ones the provider plans: a seed file runs
// INSERT and UPDATE statements Split refuses. An empty statement, a lone
// semicolon, is skipped, and comments belong to no statement. A text the
// lexer cannot read fails with a [*LexError].
func Spans(text string) ([]Span, error) {
	tokens, err := Tokens(text)
	if err != nil {
		return nil, err
	}
	var spans []Span
	current := Span{Start: -1}
	last := -1
	finish := func(end int) {
		if current.Start >= 0 {
			current.End = end
			current.Body = text[current.Start:last]
			spans = append(spans, current)
		}
		current, last = Span{Start: -1}, -1
	}
	for _, token := range tokens {
		switch token.Kind {
		case Space, Comment:
		case Semicolon:
			finish(token.End)
		default:
			if current.Start < 0 {
				current.Start = token.Start
				current.Line = 1 + strings.Count(text[:token.Start], "\n")
				current.Column = 1 + utf8.RuneCountInString(text[strings.LastIndexByte(text[:token.Start], '\n')+1:token.Start])
			}
			if len(current.Words) < leadingWords {
				current.Words = append(current.Words, upper(token))
			}
			last = token.End
		}
	}
	finish(len(text))
	return spans, nil
}
