package indexfilter_test

import (
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-spanner-plugin/internal/indexfilter"
	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// A declared clause reads into its columns in byte order without repeats: a
// quoted name as written, a bare name folded to lower case, parentheses and
// the letter case of the keywords dropped. Spelling writes them back as
// Spanner keeps the clause.
func TestParse_HappyPath(t *testing.T) {
	tests := []struct {
		name, condition string
		want            []string
		spelling        string
	}{
		{name: "one column", condition: "c IS NOT NULL", want: []string{"c"}, spelling: "c IS NOT NULL"},
		{name: "byte order", condition: `zed IS NOT NULL AND mid IS NOT NULL AND "Abc" IS NOT NULL`, want: []string{"Abc", "mid", "zed"},
			spelling: "Abc IS NOT NULL AND mid IS NOT NULL AND zed IS NOT NULL"},
		{name: "a bare name folds", condition: "ZeD is not null", want: []string{"zed"}, spelling: "zed IS NOT NULL"},
		{name: "a repeat", condition: "c IS NOT NULL AND c IS NOT NULL", want: []string{"c"}, spelling: "c IS NOT NULL"},
		{name: "parentheses", condition: "((a IS NOT NULL) AND (b IS NOT NULL))", want: []string{"a", "b"}, spelling: "a IS NOT NULL AND b IS NOT NULL"},
		{name: "a quoted name with a space and a quote", condition: `"my col" IS NOT NULL AND "a""b" IS NOT NULL`, want: []string{`a"b`, "my col"},
			spelling: `a"b IS NOT NULL AND my col IS NOT NULL`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			columns, err := indexfilter.Parse(test.condition)
			c.Assert(err, qt.IsNil)
			c.Assert(columns, qt.DeepEquals, test.want)
			c.Assert(indexfilter.Spelling(columns), qt.Equals, test.spelling)
		})
	}
}

// A clause Spanner creates no index with is refused, measured on the emulator
// behind PGAdapter 0.56.2: a comparison and NOT (c IS NULL) answer the
// conjunction rule.
func TestParse_FailurePath(t *testing.T) {
	tests := []struct{ name, condition, wantErr string }{
		{name: "empty", condition: " ", wantErr: `the WHERE clause is empty`},
		{name: "a comparison", condition: "c > 0", wantErr: `Spanner takes only <column> IS NOT NULL tests joined by AND`},
		{name: "a negation", condition: "NOT (c IS NULL)", wantErr: `Spanner takes only <column> IS NOT NULL tests joined by AND`},
		{name: "IS NULL", condition: "c IS NULL", wantErr: `Spanner takes only <column> IS NOT NULL tests joined by AND`},
		{name: "OR", condition: "a IS NOT NULL OR b IS NOT NULL", wantErr: `Spanner takes only <column> IS NOT NULL tests joined by AND`},
		{name: "a trailing AND", condition: "a IS NOT NULL AND", wantErr: `Spanner takes only <column> IS NOT NULL tests joined by AND`},
		{name: "an open parenthesis", condition: "(a IS NOT NULL", wantErr: `Spanner takes only <column> IS NOT NULL tests joined by AND`},
		{name: "a closing parenthesis too many", condition: "a IS NOT NULL)", wantErr: `Spanner takes only <column> IS NOT NULL tests joined by AND`},
		{name: "an open quote", condition: `"a IS NOT NULL`, wantErr: `the WHERE clause leaves a quoted name open`},
		{name: "an empty quoted name", condition: `"" IS NOT NULL`, wantErr: `the WHERE clause names an empty column`},
		{name: "a word that only starts with NULL", condition: "a IS NOT NULLS", wantErr: `Spanner takes only <column> IS NOT NULL tests joined by AND`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			columns, err := indexfilter.Parse(test.condition)
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(columns, qt.IsNil)
		})
	}
}

// A held filter reads back into its columns when it is exactly the spelling
// Spanner keeps for columns of the index.
func TestRead_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	columns, err := indexfilter.Read("Abc IS NOT NULL AND my col IS NOT NULL", []string{"my col", "Abc", "z"})
	c.Assert(err, qt.IsNil)
	c.Assert(columns, qt.DeepEquals, []string{"Abc", "my col"})
}

// A filter on a column the index does not hold, or one out of Spanner's
// order, is refused rather than guessed at.
func TestRead_FailurePath(t *testing.T) {
	tests := []struct{ name, filter, wantErr string }{
		{name: "a column outside the index", filter: "b IS NOT NULL", wantErr: `the filter "b IS NOT NULL" is not a conjunction of NOT NULL tests on the index's columns`},
		{name: "another test", filter: "a > 0", wantErr: `the filter "a > 0" is not a conjunction of NOT NULL tests on the index's columns`},
		{name: "out of order", filter: "z IS NOT NULL AND a IS NOT NULL", wantErr: `the filter "z IS NOT NULL AND a IS NOT NULL" is not in the spelling Spanner keeps`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			columns, err := indexfilter.Read(test.filter, []string{"a", "z"})
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(columns, qt.IsNil)
		})
	}
}
