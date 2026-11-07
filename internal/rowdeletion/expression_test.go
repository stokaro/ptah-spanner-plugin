package rowdeletion_test

import (
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-spanner-plugin/internal/rowdeletion"
	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// The catalog reports the clause without its TTL keyword and keeps the
// interval in the server's spelling.
func TestParseExpression_HappyPath(t *testing.T) {
	tests := []struct {
		name, expression string
		want             rowdeletion.Policy
	}{
		{name: "as the emulator reports it", expression: "INTERVAL '4 WEEKS 2 DAYS' ON created_at", want: rowdeletion.Policy{Column: "created_at", Interval: "4 WEEKS 2 DAYS"}},
		{name: "a mixed-case column", expression: "INTERVAL '7 DAYS' ON CreatedAt", want: rowdeletion.Policy{Column: "CreatedAt", Interval: "7 DAYS"}},
		{name: "a quoted column", expression: `interval '2 MONTHS' on "a ""b"""`, want: rowdeletion.Policy{Column: `a "b"`, Interval: "2 MONTHS"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			policy, found, err := rowdeletion.ParseExpression(test.expression)
			c.Assert(err, qt.IsNil)
			c.Assert(found, qt.IsTrue)
			c.Assert(policy, qt.Equals, test.want)
		})
	}
}

// A table without a policy reports an empty expression, which is no policy
// and no error.
func TestParseExpression_NoPolicy(t *testing.T) {
	c := testcheck.New(t)
	policy, found, err := rowdeletion.ParseExpression("  ")
	c.Assert(err, qt.IsNil)
	c.Assert(found, qt.IsFalse)
	c.Assert(policy, qt.Equals, rowdeletion.Policy{})
}

func TestParseExpression_FailurePath(t *testing.T) {
	tests := []struct{ name, expression, wantErr string }{
		{name: "no INTERVAL", expression: "'30 days' ON c", wantErr: `the row deletion policy does not begin with INTERVAL`},
		{name: "a word that only starts with INTERVAL", expression: "INTERVALS '30 days' ON c", wantErr: `the row deletion policy does not begin with INTERVAL`},
		{name: "an unquoted interval", expression: "INTERVAL 30 ON c", wantErr: `the row deletion policy's interval is not a quoted literal`},
		{name: "an unclosed interval", expression: "INTERVAL '30 days ON c", wantErr: `the row deletion policy's interval is not a quoted literal`},
		{name: "no ON", expression: "INTERVAL '30 days' c", wantErr: `the row deletion policy's interval is not followed by ON`},
		{name: "no column", expression: `INTERVAL '30 days' ON ""`, wantErr: `the row deletion policy names no column`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			policy, found, err := rowdeletion.ParseExpression(test.expression)
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(found, qt.IsFalse)
			c.Assert(policy, qt.Equals, rowdeletion.Policy{})
		})
	}
}

// A SQL schema document writes the clause with its TTL keyword after the
// column list; the column is one name, taken as written.
func TestParseClause_HappyPath(t *testing.T) {
	tests := []struct {
		name, clause string
		want         rowdeletion.Policy
	}{
		{name: "a bare column", clause: "TTL INTERVAL '30 days' ON created_at", want: rowdeletion.Policy{Column: "created_at", Interval: "30 days"}},
		{name: "a mixed-case column as written", clause: "ttl interval '1 day' on CreatedAt", want: rowdeletion.Policy{Column: "CreatedAt", Interval: "1 day"}},
		{name: "a quoted column", clause: "TTL INTERVAL '2 MONTHS'\n  ON \"a \"\"b\"\"\"", want: rowdeletion.Policy{Column: `a "b"`, Interval: "2 MONTHS"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			policy, err := rowdeletion.ParseClause(test.clause)
			c.Assert(err, qt.IsNil)
			c.Assert(policy, qt.Equals, test.want)
		})
	}
}

func TestParseClause_FailurePath(t *testing.T) {
	tests := []struct{ name, clause, wantErr string }{
		{name: "no TTL", clause: "INTERVAL '30 days' ON c", wantErr: `a row deletion policy clause begins with TTL`},
		{name: "no INTERVAL", clause: "TTL '30 days' ON c", wantErr: `a row deletion policy clause is TTL INTERVAL '<interval>' ON <column>`},
		{name: "an unquoted interval", clause: "TTL INTERVAL 30 ON c", wantErr: `the row deletion policy's interval is not a quoted literal`},
		{name: "no ON", clause: "TTL INTERVAL '30 days' c", wantErr: `the row deletion policy's interval is not followed by ON`},
		{name: "two words after ON", clause: "TTL INTERVAL '30 days' ON c d", wantErr: `the row deletion policy names something other than one column after ON`},
		{name: "text after a quoted column", clause: `TTL INTERVAL '30 days' ON "c" x`, wantErr: `the row deletion policy names something other than one column after ON`},
		{name: "an empty quoted column", clause: `TTL INTERVAL '30 days' ON ""`, wantErr: `the row deletion policy names something other than one column after ON`},
		{name: "a column starting with a digit", clause: "TTL INTERVAL '30 days' ON 1c", wantErr: `the row deletion policy names something other than one column after ON`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			policy, err := rowdeletion.ParseClause(test.clause)
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(policy, qt.Equals, rowdeletion.Policy{})
		})
	}
}
