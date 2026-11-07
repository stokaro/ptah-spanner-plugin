package rowdeletion

import (
	"fmt"
	"strings"
)

// ParseExpression reads a policy as the catalog reports it in
// information_schema.tables.row_deletion_policy_expression: the clause
// without its leading TTL keyword, such as `INTERVAL '4 WEEKS 2 DAYS' ON
// created_at`. The interval keeps the spelling the server stored.
//
// An empty expression is a table without a policy, which is not an error:
// found is false.
func ParseExpression(expression string) (policy Policy, found bool, err error) {
	trimmed := strings.TrimSpace(expression)
	if trimmed == "" {
		return Policy{}, false, nil
	}
	rest, ok := cutWord(trimmed, "INTERVAL")
	if !ok {
		return Policy{}, false, fmt.Errorf("the row deletion policy does not begin with INTERVAL")
	}
	interval, rest, ok := cutQuoted(strings.TrimSpace(rest))
	if !ok {
		return Policy{}, false, fmt.Errorf("the row deletion policy's interval is not a quoted literal")
	}
	column, ok := cutWord(strings.TrimSpace(rest), "ON")
	if !ok {
		return Policy{}, false, fmt.Errorf("the row deletion policy's interval is not followed by ON")
	}
	column = unquoteIdentifier(strings.TrimSpace(column))
	if column == "" {
		return Policy{}, false, fmt.Errorf("the row deletion policy names no column")
	}
	return Policy{Column: column, Interval: interval}, true, nil
}

// ParseClause reads the clause a SQL schema document writes after a table's
// column list, such as `TTL INTERVAL '30 days' ON created_at`. The column is
// one name, taken as written and without its double quotes, as Ptah reads a
// column name in the same document; anything after it is refused.
func ParseClause(clause string) (Policy, error) {
	rest, ok := cutWord(strings.TrimSpace(clause), "TTL")
	if !ok {
		return Policy{}, fmt.Errorf("a row deletion policy clause begins with TTL")
	}
	if rest, ok = cutWord(strings.TrimSpace(rest), "INTERVAL"); !ok {
		return Policy{}, fmt.Errorf("a row deletion policy clause is TTL INTERVAL '<interval>' ON <column>")
	}
	interval, rest, ok := cutQuoted(strings.TrimSpace(rest))
	if !ok {
		return Policy{}, fmt.Errorf("the row deletion policy's interval is not a quoted literal")
	}
	column, ok := cutWord(strings.TrimSpace(rest), "ON")
	if !ok {
		return Policy{}, fmt.Errorf("the row deletion policy's interval is not followed by ON")
	}
	name, ok := clauseColumn(strings.TrimSpace(column))
	if !ok {
		return Policy{}, fmt.Errorf("the row deletion policy names something other than one column after ON")
	}
	return Policy{Column: name, Interval: interval}, nil
}

// clauseColumn reads one column name: a double-quoted identifier with its
// doubled quotes undone, or a word of letters, digits, underscores and dollar
// signs that does not start with a digit or a dollar sign.
func clauseColumn(text string) (string, bool) {
	if strings.HasPrefix(text, `"`) {
		body := text[1:]
		var name strings.Builder
		for i := 0; i < len(body); i++ {
			switch {
			case body[i] != '"':
				name.WriteByte(body[i])
			case i+1 < len(body) && body[i+1] == '"':
				name.WriteByte('"')
				i++
			default:
				return name.String(), i == len(body)-1 && name.Len() > 0
			}
		}
		return "", false
	}
	for i, r := range text {
		letter := r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
		if !letter && (i == 0 || (r < '0' || r > '9') && r != '$') {
			return "", false
		}
	}
	return text, text != ""
}

// cutWord removes a leading keyword, whatever its letter case, when a space
// or a quote follows it.
func cutWord(text, keyword string) (string, bool) {
	if len(text) <= len(keyword) || !strings.EqualFold(text[:len(keyword)], keyword) {
		return "", false
	}
	if next := text[len(keyword)]; next != ' ' && next != '\t' && next != '\n' && next != '\'' && next != '"' {
		return "", false
	}
	return text[len(keyword):], true
}

// cutQuoted reads a single-quoted literal, undoubling its escaped quotes, and
// returns it with what follows.
func cutQuoted(text string) (literal, rest string, ok bool) {
	if !strings.HasPrefix(text, "'") {
		return "", "", false
	}
	var value strings.Builder
	for i := 1; i < len(text); i++ {
		if text[i] != '\'' {
			value.WriteByte(text[i])
			continue
		}
		if i+1 < len(text) && text[i+1] == '\'' {
			value.WriteByte('\'')
			i++
			continue
		}
		return value.String(), text[i+1:], true
	}
	return "", "", false
}

// unquoteIdentifier drops the double quotes the catalog puts around a column
// name that needs them, so the policy carries the name and not one spelling
// of it.
func unquoteIdentifier(name string) string {
	if len(name) >= 2 && strings.HasPrefix(name, `"`) && strings.HasSuffix(name, `"`) {
		return strings.ReplaceAll(name[1:len(name)-1], `""`, `"`)
	}
	return name
}
