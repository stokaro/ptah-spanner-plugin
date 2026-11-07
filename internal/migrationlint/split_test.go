package migrationlint_test

import (
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-spanner-plugin/internal/migrationlint"
	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// The word forms are the ones the host's own scanner gives a
// PostgreSQL-family text, so the host's rules read a statement of this split
// as they read it while Spanner was built in: a bare word with its digits in
// upper case, every other character of punctuation alone, strings and quoted
// names as written.
func TestSplit_HappyPath(t *testing.T) {
	tests := []struct {
		name, text string
		want       []migrationlint.Statement
	}{
		{
			name: "words, punctuation and numbers as the host's scanner cuts them",
			text: `ALTER TABLE "t" ADD CONSTRAINT c CHECK (b >= 1.5 AND x$1 <> 'a;b');`,
			want: []migrationlint.Statement{{
				Start: 0, End: 66, Line: 1, SQL: `ALTER TABLE "t" ADD CONSTRAINT c CHECK (b >= 1.5 AND x$1 <> 'a;b')`,
				Canonical:   `ALTER TABLE "t" ADD CONSTRAINT C CHECK (B >= 1.5 AND X$1 <> 'a;b')`,
				Words:       []string{"ALTER", "TABLE", `"t"`, "ADD", "CONSTRAINT", "C", "CHECK", "(", "B", ">", "=", "1", ".", "5", "AND", "X", "$", "1", "<", ">", "'a;b'", ")"},
				SourceWords: []string{"ALTER", "TABLE", `"t"`, "ADD", "CONSTRAINT", "c", "CHECK", "(", "b", ">", "=", "1", ".", "5", "AND", "x", "$", "1", "<", ">", "'a;b'", ")"},
			}},
		},
		{
			name: "the comments directly above a statement, and none detached by a blank line or ending the line before",
			text: "-- detached\n\n-- atlas:nolint DS101\n/* kept */\nDROP TABLE a; -- belongs to the line before\nINSERT INTO b VALUES (1)",
			want: []migrationlint.Statement{
				{
					Start: 46, End: 58, Line: 5, SQL: "DROP TABLE a", Canonical: "DROP TABLE A", Words: []string{"DROP", "TABLE", "A"},
					SourceWords: []string{"DROP", "TABLE", "a"}, Comments: []string{"-- atlas:nolint DS101", "/* kept */"},
				},
				{
					Start: 90, End: 114, Line: 6, SQL: "INSERT INTO b VALUES (1)", Canonical: "INSERT INTO B VALUES (1)",
					Words: []string{"INSERT", "INTO", "B", "VALUES", "(", "1", ")"}, SourceWords: []string{"INSERT", "INTO", "b", "VALUES", "(", "1", ")"},
				},
			},
		},
		{
			name: "a comment inside a statement separates two words in the canonical form",
			text: "DROP/*x*/TABLE\n  t ;;",
			want: []migrationlint.Statement{{
				Start: 0, End: 18, Line: 1, SQL: "DROP/*x*/TABLE\n  t", Canonical: "DROP TABLE T",
				Words: []string{"DROP", "TABLE", "T"}, SourceWords: []string{"DROP", "TABLE", "t"},
			}},
		},
		{
			name: "only comments and white space",
			text: "-- nothing\n\n",
			want: nil,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			statements, err := migrationlint.Split(test.text)
			c.Assert(err, qt.IsNil)
			c.Assert(statements, qt.DeepEquals, test.want)
		})
	}
}

// A text the provider's lexer cannot read fails whole, and the reason names
// what failed without quoting the text.
func TestSplit_FailurePath(t *testing.T) {
	tests := []struct {
		name, text, wantErr string
	}{
		{name: "an unclosed string", text: "INSERT INTO t VALUES ('x", wantErr: `a string is not closed`},
		{name: "an unclosed quoted name", text: `DROP TABLE "t`, wantErr: `a quoted name is not closed`},
		{name: "a dollar-quoted string", text: "SELECT $$x$$", wantErr: `a dollar-quoted string is not part of the provider's SQL`},
		{name: "a character the lexer does not read", text: "SELECT 1 \\", wantErr: `the SQL text holds a character the provider does not read on line 1`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			statements, err := migrationlint.Split(test.text)
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(statements, qt.IsNil)
		})
	}
}
