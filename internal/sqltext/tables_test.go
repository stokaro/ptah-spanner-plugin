package sqltext_test

import (
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-spanner-plugin/internal/sqltext"
	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// An ALTER TABLE names its table after an optional IF EXISTS: a name, or a
// schema and a name, each folded to lower case unless quoted. The index after
// the name is where the statement's action starts.
func TestAlteredTable_HappyPath(t *testing.T) {
	tests := []struct {
		name      string
		statement string
		want      sqltext.TableName
		next      int
	}{
		{name: "a quoted name", statement: `ALTER TABLE "Items" ADD COLUMN c bigint`, want: sqltext.TableName{Table: "Items"}, next: 3},
		{name: "a schema and a name", statement: `alter table App.T drop column c`, want: sqltext.TableName{Schema: "app", Table: "t"}, next: 5},
		{name: "IF EXISTS", statement: `ALTER TABLE IF EXISTS t DROP TTL`, want: sqltext.TableName{Table: "t"}, next: 5},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			source, words, err := sqltext.Words(test.statement)
			c.Assert(err, qt.IsNil)
			got, next, found := sqltext.AlteredTable(source, words)
			c.Assert(found, qt.IsTrue)
			c.Assert(got, qt.Equals, test.want)
			c.Assert(next, qt.Equals, test.next)
		})
	}
}

// A statement that is no ALTER TABLE, or names its table with a token that
// spells no name, names no table.
func TestAlteredTable_FailurePath(t *testing.T) {
	tests := []struct {
		name      string
		statement string
	}{
		{name: "another statement", statement: `DROP TABLE t`},
		{name: "a string for a name", statement: `ALTER TABLE 't' DROP COLUMN c`},
		{name: "a number for a name", statement: `ALTER TABLE 12 DROP COLUMN c`},
		{name: "no name", statement: `ALTER TABLE IF EXISTS`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			source, words, err := sqltext.Words(test.statement)
			c.Assert(err, qt.IsNil)
			got, next, found := sqltext.AlteredTable(source, words)
			c.Assert(found, qt.IsFalse)
			c.Assert(got, qt.Equals, sqltext.TableName{})
			c.Assert(next, qt.Equals, 0)
		})
	}
}

// A token spells a name when it is an unquoted word or a quoted name, and
// nothing else does.
func TestIdentifier(t *testing.T) {
	tests := []struct {
		name, token, want string
		found             bool
	}{
		{name: "an unquoted word folds to lower case", token: "Kind", want: "kind", found: true},
		{name: "a quoted name keeps its case", token: `"Kind"`, want: "Kind", found: true},
		{name: "a string", token: `'kind'`},
		{name: "a number", token: `12`},
		{name: "punctuation", token: `(`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			got, found := sqltext.Identifier(test.token)
			c.Assert(got, qt.Equals, test.want)
			c.Assert(found, qt.Equals, test.found)
		})
	}
}
