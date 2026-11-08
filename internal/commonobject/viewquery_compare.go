package commonobject

import (
	"regexp"
	"slices"
	"strings"

	"github.com/stokaro/ptah-spanner-plugin/internal/checkexpr"
)

// Column is one declared column of a table, with its declared type, as a view
// query comparison reads it.
type Column struct {
	Name string
	Type string
}

// SameViewQuery reports whether a view's declared query and the query Spanner
// keeps for it are one query. Spanner parses a view's query and stores its
// own rendering of it. Measured on the emulator behind PGAdapter 0.56.2:
//
//	SELECT id FROM t WHERE id > 0     SELECT id FROM t WHERE (id > '0'::bigint)
//	WHERE name IN ('a', 'b')          WHERE (name = ANY (ARRAY['a'::text, 'b'::text]))
//	FROM t JOIN u ON u.t_id = t.id    FROM (t JOIN u ON ((u.t_id = t.id)))
//	ORDER BY id LIMIT 10              ORDER BY id LIMIT '10'::bigint
//	SELECT x.id FROM t AS x           SELECT id FROM t x
//	SELECT * FROM t                   SELECT id, name, flag FROM t
//	SELECT id FROM t UNION ALL ...    SELECT t.id FROM t UNION ALL SELECT u.id FROM u
//
// Both sides are read into one canonical form: a SELECT of items, one table
// or a chain of joins, and an optional WHERE, ORDER BY and LIMIT, or such
// SELECTs joined by UNION or UNION ALL. A `*` stands for the declared columns
// of the one table it reads, a column is named without the qualifier the
// table's name or alias gives it where one table is read, and each expression
// is written in the canonical form of internal/checkexpr, typed by the
// columns tables declares. tables maps a table's name to its columns in the
// order it declares them.
//
// A nil tables reads a query of any table, whose columns have no known type
// and whose `*` it cannot expand, for a caller that has no declared columns.
//
// It answers true only where it reads both sides and the two forms are one;
// a query it cannot read, such as one with a subquery, a CASE, an aggregate
// call or GROUP BY, is never the same query here. A caller that leaves such a
// pair to another reading loses nothing.
func SameViewQuery(declared, held string, tables map[string][]Column) bool {
	left, ok := canonicalQuery(declared, tables)
	if !ok {
		return false
	}
	right, ok := canonicalQuery(held, tables)
	return ok && left == right
}

// canonicalQuery reads a view's query into its canonical form, and false
// where it cannot read it.
func canonicalQuery(query string, tables map[string][]Column) (string, bool) {
	r := &queryReader{tokens: slices.DeleteFunc(tokens(query), func(token string) bool { return token == ";" }), tables: tables}
	var parts []string
	for {
		part, ok := r.selection()
		if !ok {
			return "", false
		}
		parts = append(parts, part)
		if r.done() {
			break
		}
		if !r.keyword("UNION") {
			return "", false
		}
		operator := "UNION"
		if r.keyword("ALL") {
			operator += " ALL"
		}
		parts = append(parts, operator)
	}
	if len(parts) > 1 && slices.ContainsFunc(parts, func(part string) bool {
		return strings.Contains(part, " ORDER BY ") || strings.Contains(part, " LIMIT ")
	}) {
		// ORDER BY and LIMIT after a UNION apply to the whole query, which
		// this reading does not tell from the last SELECT's.
		return "", false
	}
	return strings.Join(parts, " "), true
}

// queryReader reads the tokens of a query, a SELECT at a time.
type queryReader struct {
	tokens []string
	pos    int
	tables map[string][]Column
}

func (r *queryReader) done() bool { return r.pos >= len(r.tokens) }

func (r *queryReader) peek() string {
	if r.done() {
		return ""
	}
	return r.tokens[r.pos]
}

// keyword consumes the next token if it is the keyword word.
func (r *queryReader) keyword(word string) bool {
	if strings.EqualFold(r.peek(), word) {
		r.pos++
		return true
	}
	return false
}

// clauseWords end a clause of a SELECT at the top level.
var clauseWords = map[string]bool{"FROM": true, "WHERE": true, "GROUP": true, "HAVING": true, "WINDOW": true, "ORDER": true, "LIMIT": true,
	"OFFSET": true, "FETCH": true, "UNION": true, "INTERSECT": true, "EXCEPT": true, "FOR": true}

// clause returns the tokens up to the next top-level clause word, and false
// when the parentheses do not balance.
func (r *queryReader) clause() ([]string, bool) {
	start, depth := r.pos, 0
	for ; !r.done(); r.pos++ {
		token := r.tokens[r.pos]
		switch {
		case token == "(":
			depth++
		case token == ")":
			depth--
			if depth < 0 {
				return nil, false
			}
		case depth == 0 && clauseWords[strings.ToUpper(token)]:
			return r.tokens[start:r.pos], true
		}
	}
	return r.tokens[start:r.pos], depth == 0
}

// selection reads one SELECT.
func (r *queryReader) selection() (string, bool) {
	if !r.keyword("SELECT") {
		return "", false
	}
	head := "SELECT "
	if r.keyword("DISTINCT") {
		head += "DISTINCT "
	}
	items, ok := r.clause()
	if !ok || !r.keyword("FROM") {
		return "", false
	}
	fromTokens, ok := r.clause()
	if !ok {
		return "", false
	}
	scope, from, ok := r.from(fromTokens)
	if !ok {
		return "", false
	}
	written, ok := scope.items(items)
	if !ok {
		return "", false
	}
	out := head + written + " FROM " + from
	if r.keyword("WHERE") {
		condition, ok := r.clause()
		if !ok {
			return "", false
		}
		canonical, ok := scope.expression(condition)
		if !ok {
			return "", false
		}
		out += " WHERE " + canonical
	}
	if r.keyword("ORDER") {
		if !r.keyword("BY") {
			return "", false
		}
		order, ok := r.clause()
		if !ok {
			return "", false
		}
		canonical, ok := scope.order(order)
		if !ok {
			return "", false
		}
		out += " ORDER BY " + canonical
	}
	if r.keyword("LIMIT") {
		limit, ok := r.clause()
		if !ok {
			return "", false
		}
		count, ok := limitCount(limit)
		if !ok {
			return "", false
		}
		out += " LIMIT " + count
	}
	if !r.done() && !strings.EqualFold(r.peek(), "UNION") {
		// GROUP BY, HAVING, OFFSET and the rest are not read here.
		return "", false
	}
	return out, true
}

// relation is one table a SELECT reads: its name and the name the query
// gives it, its alias where it has one.
type relation struct {
	table, name string
	columns     []Column
	// untyped is a table whose columns nothing declares: any name is one of
	// its columns, of no known type.
	untyped bool
}

// scope is what a SELECT's expressions can name: the tables it reads.
type scope struct{ relations []relation }

// from reads a FROM clause: a table, or a chain of joins, each table with an
// optional alias and each join with its ON condition, with parentheses
// around any part of it, as Spanner writes a join.
func (r *queryReader) from(tokens []string) (scope, string, bool) {
	var s scope
	var parts []string
	var conditions [][]string
	tokens = flattenParentheses(tokens)
	for len(tokens) > 0 {
		if len(parts) > 0 {
			switch {
			case strings.EqualFold(tokens[0], "JOIN"):
				tokens = tokens[1:]
			case len(tokens) > 1 && strings.EqualFold(tokens[0], "INNER") && strings.EqualFold(tokens[1], "JOIN"):
				tokens = tokens[2:]
			default:
				return scope{}, "", false
			}
		}
		rel, rest, ok := r.relation(tokens)
		if !ok {
			return scope{}, "", false
		}
		s.relations = append(s.relations, rel)
		written := rel.table
		if rel.name != rel.table {
			written += " " + rel.name
		}
		tokens = rest
		if len(parts) > 0 {
			if len(tokens) == 0 || !strings.EqualFold(tokens[0], "ON") {
				return scope{}, "", false
			}
			end := topLevelJoin(tokens[1:]) + 1
			conditions = append(conditions, tokens[1:end])
			tokens = tokens[end:]
		}
		parts = append(parts, written)
	}
	if len(parts) == 0 {
		return scope{}, "", false
	}
	var written strings.Builder
	written.WriteString(parts[0])
	for i, condition := range conditions {
		canonical, ok := s.expression(condition)
		if !ok {
			return scope{}, "", false
		}
		written.WriteString(" JOIN " + parts[i+1] + " ON " + canonical)
	}
	return s, written.String(), true
}

// flattenParentheses drops the parentheses Spanner writes around a join, and
// any around a whole table name: in a FROM clause they group nothing this
// reading keeps apart. The parentheses of an ON condition stay.
func flattenParentheses(tokens []string) []string {
	var out []string
	depth, condition := 0, -1
	for _, token := range tokens {
		switch {
		case strings.EqualFold(token, "ON") && condition < 0:
			condition = depth
			out = append(out, token)
		case strings.EqualFold(token, "JOIN") && condition >= 0 && depth <= condition:
			condition = -1
			out = append(out, token)
		case token == "(":
			depth++
			if condition >= 0 {
				out = append(out, token)
			}
		case token == ")":
			depth--
			if condition >= 0 && depth >= condition {
				out = append(out, token)
			} else if condition >= 0 {
				condition = -1
			}
		default:
			out = append(out, token)
		}
	}
	return out
}

// topLevelJoin returns where the next top-level JOIN or INNER JOIN starts in
// tokens, or len(tokens).
func topLevelJoin(tokens []string) int {
	depth := 0
	for i, token := range tokens {
		switch {
		case token == "(":
			depth++
		case token == ")":
			depth--
		case depth == 0 && (strings.EqualFold(token, "JOIN") || strings.EqualFold(token, "INNER")):
			return i
		}
	}
	return len(tokens)
}

var plainName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_$]*$`)

// relation reads a table name, schema-qualified or not, and its alias.
func (r *queryReader) relation(tokens []string) (relation, []string, bool) {
	if len(tokens) == 0 || !isName(tokens[0]) {
		return relation{}, nil, false
	}
	table := identifier(tokens[0])
	rest := tokens[1:]
	if len(rest) > 1 && rest[0] == "." && isName(rest[1]) {
		if table != "public" {
			return relation{}, nil, false
		}
		table, rest = identifier(rest[1]), rest[2:]
	}
	columns, declared := r.tables[table]
	if !declared && r.tables != nil {
		return relation{}, nil, false
	}
	rel := relation{table: table, name: table, columns: columns, untyped: r.tables == nil}
	if len(rest) > 1 && strings.EqualFold(rest[0], "AS") && isName(rest[1]) {
		rel.name, rest = identifier(rest[1]), rest[2:]
	} else if len(rest) > 0 && isName(rest[0]) && !keywords[strings.ToUpper(rest[0])] {
		rel.name, rest = identifier(rest[0]), rest[1:]
	}
	return rel, rest, true
}

// isName reports whether token is a name: a word that is no keyword, or a
// quoted name.
func isName(token string) bool {
	return strings.HasPrefix(token, `"`) || plainName.MatchString(token) && !keywords[strings.ToUpper(token)]
}

// identifier writes a name as the server resolves it: an unquoted word in
// lower case, a quoted name as spelled.
func identifier(token string) string {
	if strings.HasPrefix(token, `"`) {
		return strings.ReplaceAll(token[1:len(token)-1], `""`, `"`)
	}
	return strings.ToLower(token)
}

// items reads the select list.
func (s scope) items(tokens []string) (string, bool) {
	var written []string
	for _, item := range splitTopLevel(tokens) {
		if len(item) == 1 && item[0] == "*" {
			if len(s.relations) != 1 || len(s.relations[0].columns) == 0 {
				return "", false
			}
			for _, column := range s.relations[0].columns {
				canonical, ok := s.expression([]string{quoteName(column.Name)})
				if !ok {
					return "", false
				}
				written = append(written, canonical)
			}
			continue
		}
		expression, alias := splitAlias(item)
		canonical, ok := s.expression(expression)
		if !ok {
			return "", false
		}
		if alias != "" {
			canonical += " AS " + alias
		}
		written = append(written, canonical)
	}
	return strings.Join(written, ", "), len(written) > 0
}

// splitAlias splits a select item into its expression and its alias, written
// as the server resolves it: after AS, or a name right after a closing
// parenthesis or an operand.
func splitAlias(item []string) ([]string, string) {
	n := len(item)
	if n >= 3 && strings.EqualFold(item[n-2], "AS") && isName(item[n-1]) {
		return item[:n-2], quoteAlias(identifier(item[n-1]))
	}
	if n >= 2 && isName(item[n-1]) && item[n-2] != "." && (item[n-2] == ")" || isName(item[n-2]) || strings.HasPrefix(item[n-2], "'")) {
		return item[:n-1], quoteAlias(identifier(item[n-1]))
	}
	return item, ""
}

// quoteAlias writes an alias that needs quotes with them, so "Id" and id stay
// two aliases.
func quoteAlias(alias string) string {
	if simpleName.MatchString(alias) {
		return alias
	}
	return quoteName(alias)
}

func quoteName(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// order reads an ORDER BY list: each expression with its direction.
func (s scope) order(tokens []string) (string, bool) {
	var written []string
	for _, item := range splitTopLevel(tokens) {
		suffix := ""
	trim:
		for len(item) > 1 {
			last := strings.ToUpper(item[len(item)-1])
			switch {
			case last == "ASC":
				item = item[:len(item)-1]
			case last == "DESC":
				suffix = " DESC" + suffix
				item = item[:len(item)-1]
			case len(item) > 2 && strings.EqualFold(item[len(item)-2], "NULLS") && (last == "FIRST" || last == "LAST"):
				suffix = " NULLS " + last + suffix
				item = item[:len(item)-2]
			default:
				break trim
			}
		}
		canonical, ok := s.expression(item)
		if !ok {
			return "", false
		}
		written = append(written, canonical+suffix)
	}
	return strings.Join(written, ", "), len(written) > 0
}

// limitDigits matches a LIMIT count as the declaration writes it and as
// Spanner keeps it: 10, '10'::bigint.
var limitDigits = regexp.MustCompile(`^'?([0-9]+)'?$`)

// limitCount reads a LIMIT count: a whole number, alone or cast to bigint.
func limitCount(tokens []string) (string, bool) {
	if len(tokens) == 2 && tokens[1] == "::bigint" {
		tokens = tokens[:1]
	}
	if len(tokens) != 1 {
		return "", false
	}
	match := limitDigits.FindStringSubmatch(tokens[0])
	if match == nil || strings.HasPrefix(tokens[0], "'") != strings.HasSuffix(tokens[0], "'") {
		return "", false
	}
	count := strings.TrimLeft(match[1], "0")
	if count == "" {
		count = "0"
	}
	return count, true
}

// splitTopLevel splits tokens at the commas outside parentheses.
func splitTopLevel(tokens []string) [][]string {
	var items [][]string
	depth, start := 0, 0
	for i, token := range tokens {
		switch {
		case token == "(" || token == "[":
			depth++
		case token == ")" || token == "]":
			depth--
		case token == "," && depth == 0:
			items = append(items, tokens[start:i])
			start = i + 1
		}
	}
	if start < len(tokens) {
		items = append(items, tokens[start:])
	}
	return items
}

// expression writes an expression in the canonical form of
// internal/checkexpr. Each column is named as the scope resolves it: by its
// name where the SELECT reads one table, and by its table's name in the query
// and its own, joined by "__", where it reads more than one.
func (s scope) expression(tokens []string) (string, bool) {
	if len(tokens) == 0 {
		return "", false
	}
	var text []string
	types := make(map[string]string)
	for i := 0; i < len(tokens); i++ {
		token := tokens[i]
		switch {
		case i+2 < len(tokens) && isName(token) && tokens[i+1] == "." && isName(tokens[i+2]):
			name, columnType, ok := s.qualified(identifier(token), identifier(tokens[i+2]))
			if !ok {
				return "", false
			}
			types[name] = columnType
			text = append(text, quoteName(name))
			i += 2
		case token == ".":
			return "", false
		case strings.EqualFold(token, "ARRAY"):
			text = append(text, token)
		case isName(token) && (i+1 >= len(tokens) || tokens[i+1] != "("):
			name, columnType, ok := s.unqualified(identifier(token))
			if !ok {
				return "", false
			}
			types[name] = columnType
			text = append(text, quoteName(name))
		case strings.HasPrefix(token, "::"):
			text = append(text, "::", token[2:])
			for i+1 < len(tokens) && typeWords[strings.ToLower(tokens[i+1])] {
				i++
				text = append(text, tokens[i])
			}
		default:
			text = append(text, token)
		}
	}
	return checkexpr.Canonical(strings.Join(text, " "), types)
}

// typeWords are the words after the first of a type name of several words,
// such as double precision, which the tokens split after the cast.
var typeWords = map[string]bool{"precision": true, "varying": true, "with": true, "without": true, "time": true, "zone": true}

// qualified resolves table.column: the column of the table the query names
// table, by its alias or its name.
func (s scope) qualified(table, column string) (name, columnType string, ok bool) {
	for _, rel := range s.relations {
		if rel.name != table {
			continue
		}
		columnType, ok = columnOf(rel, column)
		if !ok {
			return "", "", false
		}
		if len(s.relations) == 1 {
			return column, columnType, true
		}
		return rel.name + "__" + column, columnType, true
	}
	return "", "", false
}

// unqualified resolves a column name: the column of the one table that
// declares it.
func (s scope) unqualified(column string) (name, columnType string, ok bool) {
	var found []relation
	for _, rel := range s.relations {
		if _, declared := columnOf(rel, column); declared {
			found = append(found, rel)
		}
	}
	if len(found) != 1 {
		return "", "", false
	}
	columnType, _ = columnOf(found[0], column)
	if len(s.relations) == 1 {
		return column, columnType, true
	}
	return found[0].name + "__" + column, columnType, true
}

func columnOf(rel relation, column string) (string, bool) {
	if rel.untyped {
		return "", true
	}
	for _, declared := range rel.columns {
		if declared.Name == column {
			return declared.Type, true
		}
	}
	return "", false
}
