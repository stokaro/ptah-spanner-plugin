// Package indexfilter is the WHERE clause of a Spanner index. Spanner's
// PostgreSQL interface creates an index with one kind of predicate only: a
// conjunction of `<column> IS NOT NULL` on columns of the index, its keys or
// the columns it stores. Measured on the emulator behind PGAdapter 0.56.2,
// `CREATE INDEX i ON t (c) WHERE c > 0` answers `<WHERE> clause of <CREATE
// INDEX> statement supports only conjunction of <column IS NOT NULL>
// expressions using <AND>, where <column> is part of an index`, and a column
// outside the index answers `Cannot null-filter column d that is not in the
// index`.
//
// Spanner keeps the clause in information_schema.indexes.filter in a spelling
// of its own: each column once, unquoted, in byte order of the names, joined
// by AND. `WHERE mid IS NOT NULL AND "Abc" IS NOT NULL AND zed IS NOT NULL`
// reads back as `Abc IS NOT NULL AND mid IS NOT NULL AND zed IS NOT NULL`. A
// unique index carries `<key> IS NOT NULL` for every key column whatever its
// declaration says, so a read cannot tell a unique index declared with such a
// clause from one declared without it.
//
// The package reads a declared clause into its columns, spells columns the
// way Spanner keeps them, and reads a held filter back into its columns.
package indexfilter

import (
	"fmt"
	"slices"
	"strings"
)

// notNull is the test each conjunct of a clause makes.
const notNull = " IS NOT NULL"

// Parse reads a declared WHERE clause, without the WHERE keyword, into the
// columns it requires to be NOT NULL, in byte order without repeats. A name
// in double quotes is taken as written, and a bare name in lower case, as
// PostgreSQL folds it: the emulator answers `WHERE Mixed IS NOT NULL` with
// `a null filter on a nonexistent column mixed`. Parentheses around the
// clause or around a conjunct are dropped. Anything else is refused, since
// Spanner creates no index with it.
func Parse(condition string) ([]string, error) {
	text := strings.TrimSpace(condition)
	if text == "" {
		return nil, fmt.Errorf("the WHERE clause is empty")
	}
	var columns []string
	depth := 0
	for {
		var opened int
		text, opened = trimParentheses(text)
		depth += opened
		name, rest, err := column(text)
		if err != nil {
			return nil, err
		}
		rest = strings.TrimSpace(rest)
		for _, word := range []string{"IS", "NOT", "NULL"} {
			var found bool
			if rest, found = cutKeyword(rest, word); !found {
				return nil, errOnlyNotNull
			}
		}
		columns = append(columns, name)
		rest = strings.TrimSpace(rest)
		for strings.HasPrefix(rest, ")") && depth > 0 {
			rest = strings.TrimSpace(rest[1:])
			depth--
		}
		if rest == "" && depth == 0 {
			break
		}
		var found bool
		if text, found = cutKeyword(rest, "AND"); !found {
			return nil, errOnlyNotNull
		}
	}
	slices.Sort(columns)
	return slices.Compact(columns), nil
}

// errOnlyNotNull refuses a clause that is not a conjunction of NOT NULL
// tests, in the words of Spanner's rule.
var errOnlyNotNull = fmt.Errorf("Spanner takes only <column> IS NOT NULL tests joined by AND")

// trimParentheses drops the opening parentheses before a conjunct and says
// how many it dropped. Parse drops the closing ones after a conjunct, as many
// as are open, so `(a IS NOT NULL) AND (b IS NOT NULL)` reads like
// `a IS NOT NULL AND b IS NOT NULL` and an unbalanced clause is refused.
func trimParentheses(text string) (string, int) {
	text = strings.TrimSpace(text)
	opened := 0
	for strings.HasPrefix(text, "(") {
		text = strings.TrimSpace(text[1:])
		opened++
	}
	return text, opened
}

// column reads the name a conjunct opens with and returns what follows it.
func column(text string) (name, rest string, err error) {
	if strings.HasPrefix(text, `"`) {
		var quoted strings.Builder
		for i := 1; i < len(text); i++ {
			switch {
			case text[i] != '"':
				quoted.WriteByte(text[i])
			case i+1 < len(text) && text[i+1] == '"':
				quoted.WriteByte('"')
				i++
			default:
				if quoted.Len() == 0 {
					return "", "", fmt.Errorf("the WHERE clause names an empty column")
				}
				return quoted.String(), text[i+1:], nil
			}
		}
		return "", "", fmt.Errorf("the WHERE clause leaves a quoted name open")
	}
	end := 0
	for end < len(text) && bareByte(text[end], end == 0) {
		end++
	}
	if end == 0 {
		return "", "", errOnlyNotNull
	}
	return strings.ToLower(text[:end]), text[end:], nil
}

// bareByte reports whether b continues an unquoted name, or starts one.
func bareByte(b byte, first bool) bool {
	letter := b == '_' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
	return letter || !first && (b >= '0' && b <= '9' || b == '$')
}

// cutKeyword removes a leading keyword, whatever its letter case, when white
// space, a parenthesis or the end of the text follows it.
func cutKeyword(text, keyword string) (string, bool) {
	if len(text) < len(keyword) || !strings.EqualFold(text[:len(keyword)], keyword) {
		return "", false
	}
	rest := text[len(keyword):]
	if rest != "" && bareByte(rest[0], false) {
		return "", false
	}
	return strings.TrimSpace(rest), true
}

// Spelling is the clause that requires columns to be NOT NULL, as Spanner
// keeps it: each column once, unquoted, in byte order, joined by AND. It is
// empty for no column.
func Spelling(columns []string) string {
	sorted := slices.Compact(slices.Sorted(slices.Values(columns)))
	conditions := make([]string, len(sorted))
	for i, name := range sorted {
		conditions[i] = name + notNull
	}
	return strings.Join(conditions, " AND ")
}

// Read reads a filter as information_schema.indexes reports it back into the
// columns it requires to be NOT NULL. Each must be a column of the index,
// given in columns, and the filter must be exactly their spelling; a filter
// the provider cannot read back that way is refused rather than guessed at.
func Read(filter string, columns []string) ([]string, error) {
	var named []string
	for conjunct := range strings.SplitSeq(filter, " AND ") {
		name, found := strings.CutSuffix(conjunct, notNull)
		if !found || !slices.Contains(columns, name) {
			return nil, fmt.Errorf("the filter %q is not a conjunction of NOT NULL tests on the index's columns", filter)
		}
		named = append(named, name)
	}
	if Spelling(named) != filter {
		return nil, fmt.Errorf("the filter %q is not in the spelling Spanner keeps", filter)
	}
	return slices.Compact(slices.Sorted(slices.Values(named))), nil
}
