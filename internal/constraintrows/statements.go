package constraintrows

import (
	"slices"
	"strings"

	"github.com/stokaro/ptah-spanner-plugin/internal/coltype"
	"github.com/stokaro/ptah-spanner-plugin/internal/ddl"
	"github.com/stokaro/ptah-spanner-plugin/internal/sqltext"
)

// statementRead is what one statement means for the additions after it.
type statementRead struct {
	// stops reports a statement after which no count holds.
	stops bool
	// table is a table the statement changes, nil for none, and column the
	// column of it, empty when it changes the whole table. unfilled reports
	// a column the statement adds without a default.
	table    *sqltext.TableName
	column   string
	unfilled bool
	// addition is the rule the statement adds, nil for none.
	addition *Addition
}

// leavesRowsAlone are the leading words of statements that change no row and
// no column of a table. CREATE UNIQUE INDEX is read before them.
var leavesRowsAlone = [][]string{
	{"CREATE", "INDEX"}, {"CREATE", "UNIQUE", "INDEX"}, {"ALTER", "INDEX"}, {"DROP", "INDEX"},
	{"CREATE", "SEARCH", "INDEX"}, {"ALTER", "SEARCH", "INDEX"}, {"DROP", "SEARCH", "INDEX"},
	{"CREATE", "VIEW"}, {"CREATE", "OR", "REPLACE", "VIEW"}, {"DROP", "VIEW"},
	{"CREATE", "SEQUENCE"}, {"ALTER", "SEQUENCE"}, {"DROP", "SEQUENCE"},
	{"CREATE", "CHANGE", "STREAM"}, {"ALTER", "CHANGE", "STREAM"}, {"DROP", "CHANGE", "STREAM"},
	{"CREATE", "SCHEMA"}, {"DROP", "SCHEMA"}, {"ANALYZE"},
}

// statement is one statement's text and its significant tokens, as written
// (source) and as the provider matches them (words).
type statement struct {
	body          string
	tokens        []sqltext.Token
	source, words []string
}

// text returns the statement's text from tokens[from] to the end.
func (s statement) text(from int) string {
	return strings.TrimSpace(s.body[s.tokens[from].Start:])
}

// between returns the statement's text after tokens[open] and before
// tokens[closing].
func (s statement) between(open, closing int) string {
	return strings.TrimSpace(s.body[s.tokens[open].End:s.tokens[closing].Start])
}

// readStatement reads one statement, whose significant tokens are tokens.
func readStatement(body string, tokens []sqltext.Token) statementRead {
	s := statement{body: body, tokens: tokens, source: make([]string, len(tokens)), words: make([]string, len(tokens))}
	for i, token := range tokens {
		s.source[i], s.words[i] = token.Text, token.Upper()
	}
	switch {
	case sqltext.Leads(s.words, "CREATE", "UNIQUE", "INDEX"):
		return statementRead{addition: uniqueIndex(s)}
	case slices.ContainsFunc(leavesRowsAlone, func(leading []string) bool { return sqltext.Leads(s.words, leading...) }):
		return statementRead{}
	case sqltext.Leads(s.words, "CREATE", "TABLE"):
		return wholeTable(s, "NOT")
	case sqltext.Leads(s.words, "DROP", "TABLE"):
		return wholeTable(s, "")
	case sqltext.Leads(s.words, "ALTER", "TABLE"):
		return alteredTable(s)
	}
	return statementRead{stops: true}
}

// significant returns the tokens of a statement other than white space and
// comments.
func significant(body string) ([]sqltext.Token, error) {
	tokens, err := sqltext.Tokens(body)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(tokens, func(token sqltext.Token) bool {
		return token.Kind == sqltext.Space || token.Kind == sqltext.Comment
	}), nil
}

// wholeTable reads `CREATE TABLE [IF NOT EXISTS] t ...` (guard "NOT") or
// `DROP TABLE [IF EXISTS] t` (guard ""), which change table t whole.
func wholeTable(s statement, guard string) statementRead {
	at := 2
	switch {
	case guard != "" && sqltext.Leads(s.words[at:], "IF", guard, "EXISTS"):
		at += 3
	case guard == "" && sqltext.Leads(s.words[at:], "IF", "EXISTS"):
		at += 2
	}
	name, next, found := sqltext.ReadTableName(s.source, at)
	if !found || guard == "" && next < len(s.words) && !slices.Equal(s.words[next:], []string{"RESTRICT"}) && !slices.Equal(s.words[next:], []string{"CASCADE"}) {
		return statementRead{stops: true}
	}
	table := resolved(name)
	return statementRead{table: &table}
}

// alteredTable reads an ALTER TABLE: a rule it adds, and the table or the
// column it changes.
func alteredTable(s statement) statementRead {
	name, next, found := sqltext.AlteredTable(s.source, s.words)
	if !found || next+1 >= len(s.words) {
		return statementRead{stops: true}
	}
	table := resolved(name)
	action, object := s.words[next], s.words[next+1]
	switch {
	case action == "ADD" && slices.Contains([]string{"CONSTRAINT", "CHECK", "FOREIGN", "UNIQUE", "PRIMARY"}, object):
		return statementRead{addition: addedConstraint(s, table, next+1)}
	case slices.Contains([]string{"ADD", "ALTER", "DROP"}, action) && object == "TTL", action == "DROP" && object == "CONSTRAINT":
		return statementRead{}
	case action == "ADD" && object == "COLUMN":
		column, at := changedColumn(s, next+2)
		return statementRead{table: &table, column: column, unfilled: column != "" && withoutValue(s.words[at:])}
	case action == "ALTER" && object == "COLUMN":
		column, at := changedColumn(s, next+2)
		return statementRead{table: &table, column: column, addition: alteredColumn(s, table, column, at)}
	case action == "DROP" && object == "COLUMN":
		column, _ := changedColumn(s, next+2)
		return statementRead{table: &table, column: column}
	case slices.Contains([]string{"ADD", "ALTER", "DROP"}, action):
		return statementRead{table: &table}
	}
	return statementRead{stops: true}
}

// changedColumn reads the column an ADD, ALTER or DROP COLUMN names at
// words[at], after an optional IF [NOT] EXISTS, and returns it and the index
// of the token after it. It returns the empty string, which stands for the
// whole table, when it finds no name.
func changedColumn(s statement, at int) (string, int) {
	switch {
	case sqltext.Leads(s.words[at:], "IF", "NOT", "EXISTS"):
		at += 3
	case sqltext.Leads(s.words[at:], "IF", "EXISTS"):
		at += 2
	}
	if at >= len(s.source) {
		return "", at
	}
	column, found := sqltext.Identifier(s.source[at])
	if !found {
		return "", at
	}
	return column, at + 1
}

// withoutValue reports whether the column definition words, from its type on,
// gives the column no value in the rows a table holds: no DEFAULT, and no
// generated expression.
func withoutValue(words []string) bool {
	return len(words) > 0 && !slices.ContainsFunc(words, func(word string) bool { return word == "DEFAULT" || word == "AS" || word == "GENERATED" })
}

// alteredColumn reads what `ALTER COLUMN column` asks of every row, from
// words[at] to the end of the statement: `SET NOT NULL`, or `[SET DATA] TYPE`
// a character varying, with or without a length, or an array of it. It
// returns nil for any other change.
func alteredColumn(s statement, table sqltext.TableName, column string, at int) *Addition {
	if column == "" {
		return nil
	}
	if slices.Equal(s.words[at:], []string{"SET", "NOT", "NULL"}) {
		return &Addition{Kind: NotNull, Table: table, Columns: []string{column}}
	}
	if sqltext.Leads(s.words[at:], "SET", "DATA") {
		at += 2
	}
	if at+1 >= len(s.words) || s.words[at] != "TYPE" {
		return nil
	}
	canonical, err := coltype.Canonical(s.text(at + 1))
	if err != nil {
		return nil
	}
	limit, array, found := coltype.CharacterLimit(canonical)
	if !found {
		return nil
	}
	return &Addition{Kind: CharacterLimit, Table: table, Columns: []string{column}, Limit: limit, Array: array}
}

// uniqueIndex reads `CREATE UNIQUE INDEX i ON t (a [ASC|DESC] [NULLS
// FIRST|LAST], ...) [INCLUDE (b, ...)] [WHERE e]`. It returns nil for an index
// of another shape.
func uniqueIndex(s statement) *Addition {
	if len(s.words) < 6 || s.words[4] != "ON" {
		return nil
	}
	name, found := sqltext.Identifier(s.source[3])
	if !found {
		return nil
	}
	table, next, found := sqltext.ReadTableName(s.source, 5)
	if !found {
		return nil
	}
	columns, next, found := indexKey(s, next)
	if !found {
		return nil
	}
	addition := &Addition{Kind: UniqueIndex, Table: resolved(table), Name: name, Columns: columns}
	if sqltext.Leads(s.words[next:], "INCLUDE") {
		if _, next, found = nameList(s.source, s.words, next+1); !found {
			return nil
		}
	}
	switch {
	case next == len(s.words):
		return addition
	case s.words[next] == "WHERE" && next+1 < len(s.words):
		addition.Expression = s.text(next + 1)
		return addition
	}
	return nil
}

// indexKey reads an index's parenthesized key at words[at]: columns, each with
// an optional direction and NULLS ordering. It returns the columns and the
// index of the token after the key.
func indexKey(s statement, at int) ([]string, int, bool) {
	if at >= len(s.words) || s.words[at] != "(" {
		return nil, 0, false
	}
	var columns []string
	for at++; at < len(s.words); at++ {
		column, found := sqltext.Identifier(s.source[at])
		if !found {
			return nil, 0, false
		}
		columns = append(columns, column)
		at++
		if at < len(s.words) && slices.Contains([]string{"ASC", "DESC"}, s.words[at]) {
			at++
		}
		if sqltext.Leads(s.words[at:], "NULLS", "FIRST") || sqltext.Leads(s.words[at:], "NULLS", "LAST") {
			at += 2
		}
		if at >= len(s.words) {
			return nil, 0, false
		}
		switch s.words[at] {
		case ")":
			return columns, at + 1, true
		case ",":
		default:
			return nil, 0, false
		}
	}
	return nil, 0, false
}

// addedConstraint reads `[CONSTRAINT c] CHECK (e)` or `[CONSTRAINT c] FOREIGN
// KEY (...) REFERENCES r (...) ...` from words[at] to the end of the
// statement. It returns nil for a constraint of another shape.
func addedConstraint(s statement, table sqltext.TableName, at int) *Addition {
	addition := &Addition{Table: table}
	if s.words[at] == "CONSTRAINT" {
		if at+2 >= len(s.words) {
			return nil
		}
		name, found := sqltext.Identifier(s.source[at+1])
		if !found {
			return nil
		}
		addition.Name = name
		at += 2
	}
	switch {
	case s.words[at] == "CHECK":
		closing, found := closingParenthesis(s.words, at+1)
		if !found || closing != len(s.words)-1 || closing == at+2 {
			return nil
		}
		addition.Kind, addition.Expression = Check, s.between(at+1, closing)
		return addition
	case sqltext.Leads(s.words[at:], "FOREIGN", "KEY"):
		return foreignKey(s, addition, at+2)
	}
	return nil
}

// foreignKey reads `(a, ...) REFERENCES r (b, ...)` and the referential
// actions after it, from words[at] to the end of the statement, into
// addition. It returns nil for a key of another shape.
func foreignKey(s statement, addition *Addition, at int) *Addition {
	columns, next, found := nameList(s.source, s.words, at)
	if !found || next >= len(s.words) || s.words[next] != "REFERENCES" {
		return nil
	}
	name, next, found := sqltext.ReadTableName(s.source, next+1)
	if !found {
		return nil
	}
	referenced, next, found := nameList(s.source, s.words, next)
	if !found || len(referenced) != len(columns) || !referentialActions(s.words[next:]) {
		return nil
	}
	addition.Kind, addition.Columns, addition.Referenced, addition.ReferencedColumns = ForeignKey, columns, resolved(name), referenced
	return addition
}

// referentialActions reports whether words are actions Spanner enforces a
// foreign key under, each at most once: ON DELETE CASCADE or NO ACTION, and
// ON UPDATE NO ACTION. Another clause, such as NOT ENFORCED or a MATCH type,
// leaves the key to Spanner.
func referentialActions(words []string) bool {
	var seen []string
	for len(words) > 0 {
		var clause string
		switch {
		case sqltext.Leads(words, "ON", "DELETE", "CASCADE"):
			clause, words = "ON DELETE", words[3:]
		case sqltext.Leads(words, "ON", "DELETE", "NO", "ACTION"):
			clause, words = "ON DELETE", words[4:]
		case sqltext.Leads(words, "ON", "UPDATE", "NO", "ACTION"):
			clause, words = "ON UPDATE", words[4:]
		default:
			return false
		}
		if slices.Contains(seen, clause) {
			return false
		}
		seen = append(seen, clause)
	}
	return true
}

// nameList reads a parenthesized list of one name or more at words[at], and
// returns the names and the index of the token after the list.
func nameList(source, words []string, at int) ([]string, int, bool) {
	if at >= len(words) || words[at] != "(" {
		return nil, 0, false
	}
	var names []string
	for at++; at+1 < len(words); at += 2 {
		name, found := sqltext.Identifier(source[at])
		if !found {
			return nil, 0, false
		}
		names = append(names, name)
		switch words[at+1] {
		case ")":
			return names, at + 2, true
		case ",":
		default:
			return nil, 0, false
		}
	}
	return nil, 0, false
}

// closingParenthesis returns the index of the parenthesis that closes the one
// at words[at].
func closingParenthesis(words []string, at int) (int, bool) {
	if at >= len(words) || words[at] != "(" {
		return 0, false
	}
	depth := 0
	for i := at; i < len(words); i++ {
		switch words[i] {
		case "(":
			depth++
		case ")":
			depth--
			if depth == 0 {
				return i, true
			}
		}
	}
	return 0, false
}

// resolved names a table by its schema and name, a name without a schema
// being in the default one.
func resolved(name sqltext.TableName) sqltext.TableName {
	if name.Schema == "" {
		name.Schema = ddl.DefaultSchema
	}
	return name
}
