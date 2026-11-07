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
