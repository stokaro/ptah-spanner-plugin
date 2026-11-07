package sqltext_test

import (
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-spanner-plugin/internal/sqltext"
	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// Spans splits any statement, keeps the leading words in upper case, and
// places each statement by its first token.
func TestSpans_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	text := "/* a */ insert into t values ('a;b');\n\n  ;\n  Select \"x\" from t -- end\n"
	got, err := sqltext.Spans(text)
	c.Assert(err, qt.IsNil)
	c.Assert(got, qt.DeepEquals, []sqltext.Span{
		{Start: 8, End: 37, Line: 1, Column: 9, Words: []string{"INSERT", "INTO", "T"}, Body: "insert into t values ('a;b')"},
		{Start: 45, End: len(text), Line: 4, Column: 3, Words: []string{"SELECT", `"x"`, "FROM"}, Body: `Select "x" from t`},
	})
}

func TestSpans_FailurePath(t *testing.T) {
	tests := []struct {
		name, text string
		offset     int
		wantErr    string
	}{
		{name: "an open string", text: "SELECT 'a", offset: 7, wantErr: `a string is not closed`},
		{name: "an open comment", text: "SELECT 1; /* x", offset: 10, wantErr: `a comment is not closed`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			got, err := sqltext.Spans(test.text)
			var lexError *sqltext.LexError
			c.Assert(err, qt.ErrorAs, &lexError)
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(lexError.Offset, qt.Equals, test.offset)
			c.Assert(got, qt.IsNil)
		})
	}
}
