package sqltext_test

import (
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-spanner-plugin/internal/sqltext"
	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// span is the part of a statement a test compares.
type span struct {
	Start, End, Line int
	Kind             sqltext.Kind
	Words            []string
}

func spans(statements []sqltext.Statement) []span {
	result := make([]span, len(statements))
	for i, statement := range statements {
		result[i] = span{Start: statement.Start, End: statement.End, Line: statement.Line, Kind: statement.Kind, Words: statement.Words}
	}
	return result
}

func TestSplit_HappyPath(t *testing.T) {
	tests := []struct {
		name, text string
		want       []span
	}{
		{
			name: "the four statements the provider renders",
			text: "CREATE TABLE \"t\" (\n  \"id\" bigint NOT NULL,\n  PRIMARY KEY (\"id\")\n);\nALTER TABLE \"t\" ADD COLUMN \"n\" bigint NOT NULL DEFAULT 0;\n" +
				"ALTER TABLE \"app\".\"t\" DROP COLUMN \"n\";\ndrop table \"t\";\n",
			want: []span{
				{Start: 0, End: 66, Line: 1, Kind: sqltext.CreateTable, Words: []string{"CREATE", "TABLE", `"t"`, "(", `"id"`, "BIGINT", "NOT", "NULL", ",", "PRIMARY", "KEY", "(", `"id"`, ")", ")"}},
				{Start: 67, End: 124, Line: 5, Kind: sqltext.AddColumn, Words: []string{"ALTER", "TABLE", `"t"`, "ADD", "COLUMN", `"n"`, "BIGINT", "NOT", "NULL", "DEFAULT", "0"}},
				{Start: 125, End: 163, Line: 6, Kind: sqltext.DropColumn, Words: []string{"ALTER", "TABLE", `"app"`, ".", `"t"`, "DROP", "COLUMN", `"n"`}},
				{Start: 164, End: 179, Line: 7, Kind: sqltext.DropTable, Words: []string{"DROP", "TABLE", `"t"`}},
			},
		},
		{
			name: "a semicolon inside a string, a quoted name and a comment",
			text: "-- leading;\nALTER TABLE \"a;b\" ADD COLUMN c text DEFAULT 'x;y' /* ; */;",
			want: []span{{Start: 0, End: 70, Line: 1, Kind: sqltext.AddColumn,
				Words: []string{"ALTER", "TABLE", `"a;b"`, "ADD", "COLUMN", "C", "TEXT", "DEFAULT", "'x;y'"}}},
		},
		{
			name: "a trailing comment ends the last statement",
			text: "DROP TABLE t;\n-- done\n",
			want: []span{{Start: 0, End: 22, Line: 1, Kind: sqltext.DropTable, Words: []string{"DROP", "TABLE", "T"}}},
		},
		{
			name: "a last statement without a semicolon",
			text: "  DROP TABLE t",
			want: []span{{Start: 2, End: 14, Line: 1, Kind: sqltext.DropTable, Words: []string{"DROP", "TABLE", "T"}}},
		},
		{
			name: "the index and column statements the provider renders",
			text: "CREATE UNIQUE INDEX \"u\" ON \"t\" (\"a\" DESC) INCLUDE (\"b\");\nDROP INDEX \"u\";\nALTER TABLE \"t\" ALTER COLUMN \"a\" SET NOT NULL;\n" +
				"ALTER TABLE \"t\" ADD CONSTRAINT \"f\" FOREIGN KEY (\"a\") REFERENCES \"p\"(\"id\");",
			want: []span{
				{Start: 0, End: 56, Line: 1, Kind: sqltext.CreateIndex, Words: []string{"CREATE", "UNIQUE", "INDEX", `"u"`, "ON", `"t"`, "(", `"a"`, "DESC", ")", "INCLUDE", "(", `"b"`, ")"}},
				{Start: 57, End: 72, Line: 2, Kind: sqltext.DropIndex, Words: []string{"DROP", "INDEX", `"u"`}},
				{Start: 73, End: 119, Line: 3, Kind: sqltext.AlterColumn, Words: []string{"ALTER", "TABLE", `"t"`, "ALTER", "COLUMN", `"a"`, "SET", "NOT", "NULL"}},
				{Start: 120, End: 194, Line: 4, Kind: sqltext.AddConstraint, Words: []string{"ALTER", "TABLE", `"t"`, "ADD", "CONSTRAINT", `"f"`, "FOREIGN", "KEY", "(", `"a"`, ")",
					"REFERENCES", `"p"`, "(", `"id"`, ")"}},
			},
		},
		{
			name: "the row deletion policy statements the provider renders",
			text: "ALTER TABLE \"t\" ADD TTL INTERVAL '7 days' ON \"ts\";\nALTER TABLE \"app\".\"t\" ALTER TTL INTERVAL '60 days' ON \"ts\";\nALTER TABLE \"t\" DROP TTL;",
			want: []span{
				{Start: 0, End: 50, Line: 1, Kind: sqltext.SetRowDeletion, Words: []string{"ALTER", "TABLE", `"t"`, "ADD", "TTL", "INTERVAL", "'7 days'", "ON", `"ts"`}},
				{Start: 51, End: 110, Line: 2, Kind: sqltext.SetRowDeletion, Words: []string{"ALTER", "TABLE", `"app"`, ".", `"t"`, "ALTER", "TTL", "INTERVAL", "'60 days'", "ON", `"ts"`}},
				{Start: 111, End: 136, Line: 3, Kind: sqltext.DropRowDeletion, Words: []string{"ALTER", "TABLE", `"t"`, "DROP", "TTL"}},
			},
		},
		{
			name: "the view and sequence statements the provider renders",
			text: "CREATE VIEW \"v\" AS\nSELECT id FROM t;\nCREATE OR REPLACE VIEW \"v\" AS\nSELECT id, a FROM t;\nDROP VIEW \"v\";\n" +
				"CREATE SEQUENCE \"s\" START COUNTER WITH 10;\nALTER SEQUENCE \"s\" RESTART COUNTER WITH 20;\nDROP SEQUENCE \"s\";",
			want: []span{
				{Start: 0, End: 36, Line: 1, Kind: sqltext.CreateView, Words: []string{"CREATE", "VIEW", `"v"`, "AS", "SELECT", "ID", "FROM", "T"}},
				{Start: 37, End: 87, Line: 3, Kind: sqltext.CreateView, Words: []string{"CREATE", "OR", "REPLACE", "VIEW", `"v"`, "AS", "SELECT", "ID", ",", "A", "FROM", "T"}},
				{Start: 88, End: 102, Line: 5, Kind: sqltext.DropView, Words: []string{"DROP", "VIEW", `"v"`}},
				{Start: 103, End: 145, Line: 6, Kind: sqltext.CreateSequence, Words: []string{"CREATE", "SEQUENCE", `"s"`, "START", "COUNTER", "WITH", "10"}},
				{Start: 146, End: 189, Line: 7, Kind: sqltext.AlterSequence, Words: []string{"ALTER", "SEQUENCE", `"s"`, "RESTART", "COUNTER", "WITH", "20"}},
				{Start: 190, End: 208, Line: 8, Kind: sqltext.DropSequence, Words: []string{"DROP", "SEQUENCE", `"s"`}},
			},
		},
		{
			name: "the data statements of a migration body",
			text: "INSERT INTO t VALUES (1);\nupdate t set a = 2;\nDELETE FROM t;\nSELECT 1;",
			want: []span{
				{Start: 0, End: 25, Line: 1, Kind: sqltext.Insert, Words: []string{"INSERT", "INTO", "T", "VALUES", "(", "1", ")"}},
				{Start: 26, End: 45, Line: 2, Kind: sqltext.Update, Words: []string{"UPDATE", "T", "SET", "A", "=", "2"}},
				{Start: 46, End: 60, Line: 3, Kind: sqltext.Delete, Words: []string{"DELETE", "FROM", "T"}},
				{Start: 61, End: 70, Line: 4, Kind: sqltext.Select, Words: []string{"SELECT", "1"}},
			},
		},
		{
			name: "only white space",
			text: " \n\t",
			want: make([]span, 0),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			statements, err := sqltext.Split(test.text)
			c.Assert(err, qt.IsNil)
			c.Assert(spans(statements), qt.DeepEquals, test.want)
		})
	}
}

func TestSplit_FailurePath(t *testing.T) {
	tests := []struct {
		name, text, wantErr string
	}{
		{name: "a schema", text: "CREATE SCHEMA app;", wantErr: `the provider analyzes CREATE and DROP of a TABLE, an INDEX, a VIEW or a SEQUENCE, ` +
			`CREATE OR REPLACE VIEW, ALTER SEQUENCE, ALTER TABLE \.\.\. ADD COLUMN, DROP COLUMN, ALTER COLUMN, ADD CONSTRAINT, ADD TTL, ALTER TTL or DROP TTL, ` +
			`INSERT, UPDATE, DELETE and SELECT only`},
		{name: "a data word alone", text: "DELETE;", wantErr: `the provider analyzes .*`},
		{name: "an ALTER TABLE of another kind", text: "ALTER TABLE t RENAME TO u;", wantErr: `the provider analyzes .*`},
		{name: "a short statement", text: "DROP INDEX;", wantErr: `the provider analyzes .*`},
		{name: "an unclosed quoted name", text: `DROP TABLE "t;`, wantErr: `a quoted identifier does not close on line 1`},
		{name: "an unclosed string", text: `ALTER TABLE t ADD COLUMN c text DEFAULT 'x;`, wantErr: `a string does not close on line 1`},
		{name: "an unclosed comment", text: `DROP TABLE t /* x`, wantErr: `a block comment does not close on line 1`},
		{name: "an unclosed dollar-quoted string", text: `ALTER TABLE t ADD COLUMN c text DEFAULT $d$x$$;`, wantErr: `a dollar-quoted string \$d\$ does not close on line 1`},
		{name: "an empty statement", text: "DROP TABLE t;\n;", wantErr: `an empty statement ends on line 2`},
		{name: "comments alone", text: "-- nothing\n", wantErr: `the SQL text holds comments and no statement`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			statements, err := sqltext.Split(test.text)
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(statements, qt.IsNil)
		})
	}
}

// Tokens cover the text with no gap, white space and comments included, and
// say what each token is.
func TestTokens_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	text := "-- note\nINSERT INTO \"t\" (a) VALUES ('x;y'); /* end */"
	tokens, err := sqltext.Tokens(text)
	c.Assert(err, qt.IsNil)
	var kinds []sqltext.TokenKind
	var rebuilt string
	end := 0
	for _, token := range tokens {
		c.Assert(token.Start, qt.Equals, end)
		c.Assert(token.Text, qt.Equals, text[token.Start:token.End])
		end = token.End
		rebuilt += token.Text
		kinds = append(kinds, token.Kind)
	}
	c.Assert(rebuilt, qt.Equals, text)
	c.Assert(kinds, qt.DeepEquals, []sqltext.TokenKind{
		sqltext.Comment, sqltext.Space, sqltext.Word, sqltext.Space, sqltext.Word, sqltext.Space, sqltext.Quoted, sqltext.Space,
		sqltext.Punctuation, sqltext.Word, sqltext.Punctuation, sqltext.Space, sqltext.Word, sqltext.Space, sqltext.Punctuation,
		sqltext.Quoted, sqltext.Punctuation, sqltext.Semicolon, sqltext.Space, sqltext.Comment,
	})
}

func TestTokens_FailurePath(t *testing.T) {
	for _, text := range []string{"SELECT $$x", "SELECT 'x", `SELECT "x`, "SELECT 1 /* x /* y */"} {
		t.Run(text, func(t *testing.T) {
			c := testcheck.New(t)
			tokens, err := sqltext.Tokens(text)
			c.Assert(err, qt.IsNotNil)
			c.Assert(tokens, qt.IsNil)
		})
	}
}

// tokenValue is what a test compares of a token.
type tokenValue struct {
	Text   string
	Kind   sqltext.TokenKind
	Name   bool
	Value  string
	Valued bool
}

// tokenValues returns what each token but white space stands for.
func tokenValues(tokens []sqltext.Token) []tokenValue {
	var values []tokenValue
	for _, token := range tokens {
		if token.Kind != sqltext.Space {
			values = append(values, tokenValue{Text: token.Text, Kind: token.Kind, Name: token.Name, Value: token.Value, Valued: token.Valued})
		}
	}
	return values
}

// A quoted token is one constant: a string continued on the next line and a
// UESCAPE clause belong to it. Each token says what it stands for where
// Spanner reads a value: the text of a string, the name of a quoted name, and
// an unquoted word folded to lower case.
func TestTokens_Values(t *testing.T) {
	c := testcheck.New(t)
	text := "SELECT E'it\\'s'\n'!' AS \"Mixed\", U&\"x!0079\" UESCAPE '!', $f$a;b$f$, 1, B'101' FROM t"
	tokens, err := sqltext.Tokens(text)
	c.Assert(err, qt.IsNil)
	c.Assert(tokenValues(tokens), qt.DeepEquals, []tokenValue{
		{Text: "SELECT", Kind: sqltext.Word, Value: "select", Valued: true},
		{Text: "E'it\\'s'\n'!'", Kind: sqltext.Quoted, Value: "it's!", Valued: true},
		{Text: "AS", Kind: sqltext.Word, Value: "as", Valued: true},
		{Text: `"Mixed"`, Kind: sqltext.Quoted, Name: true, Value: "Mixed", Valued: true},
		{Text: ",", Kind: sqltext.Punctuation},
		{Text: `U&"x!0079" UESCAPE '!'`, Kind: sqltext.Quoted, Name: true, Value: "xy", Valued: true},
		{Text: ",", Kind: sqltext.Punctuation},
		{Text: "$f$a;b$f$", Kind: sqltext.Quoted, Value: "a;b", Valued: true},
		{Text: ",", Kind: sqltext.Punctuation},
		{Text: "1", Kind: sqltext.Word},
		{Text: ",", Kind: sqltext.Punctuation},
		{Text: "B'101'", Kind: sqltext.Quoted},
		{Text: "FROM", Kind: sqltext.Word, Value: "from", Valued: true},
		{Text: "t", Kind: sqltext.Word, Value: "t", Valued: true},
	})
}
