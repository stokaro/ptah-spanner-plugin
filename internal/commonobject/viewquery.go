package commonobject

import (
	"regexp"
	"strings"
)

// keywords are the words Spanner writes in upper case when it writes a view's
// query back. Spanner parses the query and stores its own rendering of it:
// measured on the emulator behind PGAdapter 0.56.2, `select "id" from t` reads
// back as `SELECT id FROM t`.
var keywords = buildKeywords()

func buildKeywords() map[string]bool {
	words := make(map[string]bool)
	for word := range strings.FieldsSeq(`ALL AND ANY AS ASC BETWEEN BY CASE CAST CROSS CURRENT_DATE CURRENT_TIMESTAMP DESC DISTINCT ELSE END
		EXCEPT EXISTS FALSE FETCH FIRST FOR FROM FULL GROUP HAVING ILIKE IN INNER INTERSECT IS JOIN LAST LEFT LIKE LIMIT NOT NULL NULLS
		OFFSET ON OR ORDER OUTER RIGHT ROWS SELECT SOME THEN TRUE UNION USING WHEN WHERE WITH`) {
		words[word] = true
	}
	return words
}

var simpleName = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// ViewQuery writes a view's query in the spelling Spanner stores it in, as
// far as that follows from the text alone: comments dropped, one space
// between tokens and none inside a parenthesis or before a comma, keywords in
// upper case, a quoted name that needs no quotes unquoted, and no terminator.
// The host compares the result with the query a read reports, so a view
// declared in any of these spellings is not planned again once it holds.
//
// Spanner also rewrites a condition it parses, which this does not follow:
// `WHERE id > 0` reads back as `WHERE (id > '0'::bigint)`. A view whose
// query Spanner rewrites that way is replaced on every plan, as Ptah's
// built-in Spanner engine replaced it; declaring the query in the stored
// spelling ends it.
func ViewQuery(query string) string {
	var out strings.Builder
	previous := ""
	for _, token := range tokens(query) {
		if token == ";" {
			continue
		}
		if out.Len() > 0 && spaceBetween(previous, token) {
			out.WriteByte(' ')
		}
		out.WriteString(token)
		previous = token
	}
	return out.String()
}

// spaceBetween reports whether Spanner writes a space between two tokens.
func spaceBetween(previous, token string) bool {
	switch {
	case token == "," || token == ")" || token == "." || strings.HasPrefix(token, "::"):
		return false
	case previous == "(" || previous == ".":
		return false
	case token == "(":
		// A call writes its parenthesis against its name; a keyword or an
		// operator before a parenthesis is followed by a space.
		return keywords[strings.ToUpper(previous)] || !isWordToken(previous)
	}
	return true
}

func isWordToken(token string) bool {
	return token != "" && (isWordStart(token[0]) || token[0] == '"')
}

// tokens splits a query into the tokens Spanner's rendering spaces apart, in
// their stored spelling. A string, a quoted name and a number keep their text;
// comments and white space are dropped.
func tokens(query string) []string {
	var result []string
	for i := 0; i < len(query); {
		token, end := next(query, i)
		if token != "" {
			result = append(result, token)
		}
		i = end
	}
	return result
}

// next reads the token at i and returns it with where it ends; white space
// and a comment return no token.
func next(query string, i int) (string, int) {
	c := query[i]
	switch {
	case strings.IndexByte(" \t\n\r\f\v", c) >= 0:
		return "", i + 1
	case strings.HasPrefix(query[i:], "--"):
		if end := strings.IndexByte(query[i:], '\n'); end >= 0 {
			return "", i + end
		}
		return "", len(query)
	case strings.HasPrefix(query[i:], "/*"):
		if end := strings.Index(query[i+2:], "*/"); end >= 0 {
			return "", i + 2 + end + 2
		}
		return "", len(query)
	case c == '\'' || c == '"':
		end := quotedEnd(query, i)
		return word(query[i:end]), end
	case strings.HasPrefix(query[i:], "::"):
		end := run(query, i+2, isWordPart)
		return "::" + strings.ToLower(query[i+2:end]), end
	case isWordStart(c):
		end := run(query, i+1, isWordPart)
		return word(query[i:end]), end
	case c >= '0' && c <= '9':
		end := run(query, i+1, func(b byte) bool { return b >= '0' && b <= '9' || b == '.' || b == 'e' || b == 'E' })
		return query[i:end], end
	case strings.IndexByte("(),;.[]", c) >= 0:
		return string(c), i + 1
	default:
		end := run(query, i+1, func(b byte) bool { return strings.IndexByte("+-*/<>=!~%^&|?#@", b) >= 0 })
		return query[i:end], end
	}
}

// run returns where the run of bytes part accepts, starting at i, ends.
func run(query string, i int, part func(byte) bool) int {
	for i < len(query) && part(query[i]) {
		i++
	}
	return i
}

// word writes a keyword in upper case, and a quoted name that needs no
// quotes without them. Every other word keeps its spelling: Spanner resolves
// a name by its exact spelling.
func word(token string) string {
	if strings.HasPrefix(token, `"`) {
		name := strings.ReplaceAll(token[1:len(token)-1], `""`, `"`)
		if simpleName.MatchString(name) && !keywords[strings.ToUpper(name)] {
			return name
		}
		return token
	}
	if strings.HasPrefix(token, "'") {
		return token
	}
	if keywords[strings.ToUpper(token)] {
		return strings.ToUpper(token)
	}
	return token
}

// quotedEnd returns the end of the quoted token at start, a doubled quote
// staying inside it. An unclosed quote runs to the end of the text.
func quotedEnd(text string, start int) int {
	quote := text[start]
	for i := start + 1; i < len(text); i++ {
		if text[i] != quote {
			continue
		}
		if i+1 < len(text) && text[i+1] == quote {
			i++
			continue
		}
		return i + 1
	}
	return len(text)
}

func isWordStart(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' }

func isWordPart(c byte) bool { return isWordStart(c) || c >= '0' && c <= '9' || c == '$' }
