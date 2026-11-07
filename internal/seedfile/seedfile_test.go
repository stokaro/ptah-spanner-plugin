package seedfile_test

import (
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-spanner-plugin/internal/seedfile"
	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// A seed file's row writes and queries run in order. Comments belong to no
// statement, a lone semicolon is no statement, and a semicolon inside a
// string or a quoted name does not end one.
func TestStatements_HappyPath(t *testing.T) {
	tests := []struct {
		name, text string
		want       []seedfile.Statement
	}{
		{name: "no statement", text: "-- nothing\n", want: make([]seedfile.Statement, 0)},
		{name: "two inserts", text: "INSERT INTO t (a) VALUES ('x;y');\n-- next\ninsert into \"t;\" (a) values (1);;\n",
			want: []seedfile.Statement{
				{Body: "INSERT INTO t (a) VALUES ('x;y')", Number: 1, Line: 1},
				{Body: `insert into "t;" (a) values (1)`, Number: 2, Line: 3},
			}},
		{name: "a last statement without a semicolon", text: "UPDATE t SET a = 1 WHERE b = 2\n",
			want: []seedfile.Statement{{Body: "UPDATE t SET a = 1 WHERE b = 2", Number: 1, Line: 1}}},
		{name: "a delete, a query and a common table expression", text: "DELETE FROM t WHERE a = 1; SELECT 1; WITH x AS (SELECT 1) SELECT * FROM x",
			want: []seedfile.Statement{
				{Body: "DELETE FROM t WHERE a = 1", Number: 1, Line: 1},
				{Body: "SELECT 1", Number: 2, Line: 1},
				{Body: "WITH x AS (SELECT 1) SELECT * FROM x", Number: 3, Line: 1},
			}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			got, err := seedfile.Statements(test.text)
			c.Assert(err, qt.IsNil)
			c.Assert(got, qt.DeepEquals, test.want)
		})
	}
}

// A file holding a statement the provider does not run inside its
// transaction is refused at that statement before anything runs.
func TestStatements_FailurePath(t *testing.T) {
	tests := []struct {
		name, text string
		offset     int
		reason     string
	}{
		{name: "a schema statement", text: "INSERT INTO t (a) VALUES (1);\n  create table u (id bigint primary key);", offset: 32,
			reason: `statement 2 is a schema statement, which Spanner runs only outside a transaction; a seed file runs in one`},
		{name: "a commit", text: "INSERT INTO t (a) VALUES (1); COMMIT;", offset: 30,
			reason: `statement 2 controls a transaction; the provider runs the file in a transaction of its own, which the file cannot open, end or mark`},
		{name: "a savepoint", text: "SAVEPOINT s;", offset: 0,
			reason: `statement 1 controls a transaction; .*`},
		{name: "a setting", text: "-- c\nSET spanner.autocommit_dml_mode = 'PARTITIONED_NON_ATOMIC';", offset: 5,
			reason: `statement 1 is not a row write or a query; a seed file runs INSERT, UPDATE, DELETE and queries`},
		{name: "an unclosed dollar-quoted string", text: "INSERT INTO t (a) VALUES ($$x);", offset: 26,
			reason: `the provider cannot split the file into statements: a dollar-quoted string \$\$ does not close on line 1`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			got, err := seedfile.Statements(test.text)
			var refusal *seedfile.Refusal
			c.Assert(err, qt.ErrorAs, &refusal)
			c.Assert(err, qt.ErrorMatches, test.reason)
			c.Assert(refusal.Offset, qt.Equals, test.offset)
			c.Assert(got, qt.IsNil)
		})
	}
}

// A position counts lines and characters from 1.
func TestPosition(t *testing.T) {
	tests := []struct {
		name, text   string
		offset       int
		line, column int
	}{
		{name: "the start", text: "abc", offset: 0, line: 1, column: 1},
		{name: "the second line", text: "a;\n  b", offset: 5, line: 2, column: 3},
		{name: "after characters outside ASCII", text: "'é'; x", offset: 6, line: 1, column: 6},
		{name: "past the end", text: "ab", offset: 9, line: 1, column: 3},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			line, column := seedfile.Position(test.text, test.offset)
			c.Assert(line, qt.Equals, test.line)
			c.Assert(column, qt.Equals, test.column)
		})
	}
}
