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
		{name: "an index statement", text: "CREATE INDEX i ON t (a);", wantErr: `the provider analyzes CREATE TABLE, DROP TABLE and ALTER TABLE \.\.\. ADD COLUMN or DROP COLUMN only`},
		{name: "a data statement", text: "INSERT INTO t VALUES (1);", wantErr: `the provider analyzes .*`},
		{name: "an ALTER TABLE of another kind", text: "ALTER TABLE t ALTER COLUMN a SET NOT NULL;", wantErr: `the provider analyzes .*`},
		{name: "an unclosed quoted name", text: `DROP TABLE "t;`, wantErr: `a quoted name is not closed`},
		{name: "an unclosed string", text: `ALTER TABLE t ADD COLUMN c text DEFAULT 'x;`, wantErr: `a string is not closed`},
		{name: "an unclosed comment", text: `DROP TABLE t /* x`, wantErr: `a comment is not closed`},
		{name: "a dollar-quoted string", text: `ALTER TABLE t ADD COLUMN c text DEFAULT $$x$$;`, wantErr: `a dollar-quoted string is not part of the provider's SQL`},
		{name: "an empty statement", text: "DROP TABLE t;\n;", wantErr: `an empty statement ends on line 2`},
		{name: "comments alone", text: "-- nothing\n", wantErr: `the SQL text holds comments and no statement`},
		{name: "a character outside the dialect", text: "DROP TABLE t\x00;", wantErr: `the SQL text holds a character the provider does not read on line 1`},
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
