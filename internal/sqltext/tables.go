package sqltext

import "slices"

// TableName is a table a statement names, as Spanner resolves the name: an
// unquoted name folded to lower case, a quoted one as written. Schema is empty
// when the statement names the table without one, which puts it in the
// default schema.
type TableName struct {
	Schema, Table string
}

// ReadTableName reads the table name that starts at source[at]: a name, or a
// schema and a name joined by a dot. It returns the name and the index of the
// token after it. source are a statement's tokens as written, as Words returns
// them. A token that spells no name, such as a string or a number, is no
// table name.
func ReadTableName(source []string, at int) (TableName, int, bool) {
	if at >= len(source) {
		return TableName{}, 0, false
	}
	first, found := Identifier(source[at])
	if !found {
		return TableName{}, 0, false
	}
	if at+2 < len(source) && source[at+1] == "." {
		second, found := Identifier(source[at+2])
		return TableName{Schema: first, Table: second}, at + 3, found
	}
	return TableName{Table: first}, at + 1, true
}

// AlteredTable reads the table an ALTER TABLE names, after an optional IF
// EXISTS, and returns it and the index of the token after it. source and
// words are the statement's tokens as Words returns them.
func AlteredTable(source, words []string) (TableName, int, bool) {
	if !Leads(words, "ALTER", "TABLE") {
		return TableName{}, 0, false
	}
	at := 2
	if Leads(words[at:], "IF", "EXISTS") {
		at += 2
	}
	return ReadTableName(source, at)
}

// Identifier returns the name a token spells: a quoted name with its quoting
// undone, or an unquoted word in lower case, as Spanner's PostgreSQL interface
// folds one. A number, a string or punctuation spells no name.
func Identifier(token string) (string, bool) {
	tokens, err := Tokens(token)
	if err != nil || len(tokens) != 1 || !tokens[0].Valued || !tokens[0].Name && tokens[0].Kind != Word {
		return "", false
	}
	return tokens[0].Value, true
}

// Leads reports whether words start with leading.
func Leads(words []string, leading ...string) bool {
	return len(words) >= len(leading) && slices.Equal(words[:len(leading)], leading)
}
