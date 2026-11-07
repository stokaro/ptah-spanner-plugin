package sqltext_test

import (
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-spanner-plugin/internal/sqltext"
	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

func TestWords_HappyPath(t *testing.T) {
	tests := []struct {
		name, text          string
		wantSource, wantUpp []string
	}{
		{
			name:       "comments and a final semicolon are left out",
			text:       "-- set up\nalter database /* the one */ db set x = 1;\n",
			wantSource: []string{"alter", "database", "db", "set", "x", "=", "1"},
			wantUpp:    []string{"ALTER", "DATABASE", "DB", "SET", "X", "=", "1"},
		},
		{
			name:       "a quoted name and a string keep their spelling",
			text:       `CREATE VIEW "V" AS SELECT 'a;b' FROM t`,
			wantSource: []string{"CREATE", "VIEW", `"V"`, "AS", "SELECT", "'a;b'", "FROM", "t"},
			wantUpp:    []string{"CREATE", "VIEW", `"V"`, "AS", "SELECT", "'a;b'", "FROM", "T"},
		},
		{
			name:       "a dollar-quoted string, a continued escape string and a nested comment are read as PostgreSQL reads them",
			text:       "SELECT $$x;y$$, E'a\\'b;'\n'c' /* d /* e */ f; g */ FROM t",
			wantSource: []string{"SELECT", "$$x;y$$", ",", "E'a\\'b;'\n'c'", "FROM", "t"},
			wantUpp:    []string{"SELECT", "$$x;y$$", ",", "E'a\\'b;'\n'c'", "FROM", "T"},
		},
		{
			name: "a semicolon alone has no words",
			text: " ; ",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			source, words, err := sqltext.Words(test.text)
			c.Assert(err, qt.IsNil)
			c.Assert(source, qt.DeepEquals, test.wantSource)
			c.Assert(words, qt.DeepEquals, test.wantUpp)
		})
	}
}

func TestWords_FailurePath(t *testing.T) {
	t.Run("two statements", func(t *testing.T) {
		c := testcheck.New(t)
		source, words, err := sqltext.Words("CREATE TABLE t (id bigint); DROP TABLE t")
		c.Assert(err, qt.ErrorIs, sqltext.ErrSeveralStatements)
		c.Assert(source, qt.IsNil)
		c.Assert(words, qt.IsNil)
	})
	t.Run("an unclosed dollar-quoted body", func(t *testing.T) {
		c := testcheck.New(t)
		source, words, err := sqltext.Words("SELECT $$x")
		c.Assert(err, qt.ErrorMatches, `a dollar-quoted string \$\$ does not close on line 1`)
		c.Assert(source, qt.IsNil)
		c.Assert(words, qt.IsNil)
	})
}

func TestUnquote(t *testing.T) {
	tests := []struct{ name, written, want string }{
		{name: "an unquoted word folds to lower case", written: "Parent", want: "parent"},
		{name: "a quoted name keeps its case", written: `"Parent"`, want: "Parent"},
		{name: "a doubled quote stands for one", written: `"a""b"`, want: `a"b`},
		{name: "a Unicode escape name", written: `U&"a\0062"`, want: "ab"},
		{name: "a Unicode escape name with a UESCAPE clause", written: `U&"X!0079" UESCAPE '!'`, want: "Xy"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(sqltext.Unquote(test.written), qt.Equals, test.want)
		})
	}
}
