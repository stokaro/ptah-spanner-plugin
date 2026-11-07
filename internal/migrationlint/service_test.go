package migrationlint_test

import (
	"context"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/testing/protocmp"

	"github.com/stokaro/ptah-spanner-plugin/internal/migrationlint"
	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// A host that negotiated lint.statement-facts gets every statement's facts
// and the list of facts judged; one that did not gets the statements alone.
func TestSplitRequest_HappyPath(t *testing.T) {
	request := &protocol.SplitLintRequest{Engine: "spanner", Sources: []*protocol.LintSource{
		{Name: "1_a.up.sql", Sql: "CREATE TABLE nopk (a bigint);\nINSERT INTO nopk VALUES (1);", Version: 1, Up: true},
	}}
	statements := []*protocol.LintStatement{
		{Start: 0, End: 28, Line: 1, Sql: "CREATE TABLE nopk (a bigint)", Canonical: "CREATE TABLE NOPK (A BIGINT)",
			Words: []string{"CREATE", "TABLE", "NOPK", "(", "A", "BIGINT", ")"}, SourceWords: []string{"CREATE", "TABLE", "nopk", "(", "a", "bigint", ")"}},
		{Start: 30, End: 57, Line: 2, Sql: "INSERT INTO nopk VALUES (1)", Canonical: "INSERT INTO NOPK VALUES (1)",
			Words: []string{"INSERT", "INTO", "NOPK", "VALUES", "(", "1", ")"}, SourceWords: []string{"INSERT", "INTO", "nopk", "VALUES", "(", "1", ")"}},
	}
	t.Run("without statement facts", func(t *testing.T) {
		c := testcheck.New(t)
		result, err := migrationlint.SplitRequest(context.Background(), request, nil)
		c.Assert(err, qt.IsNil)
		c.Assert(result, qt.CmpEquals(protocmp.Transform()), &protocol.SplitLint{Files: []*protocol.LintFile{{Name: "1_a.up.sql", Version: 1, Up: true, Statements: statements}}})
	})
	t.Run("with statement facts", func(t *testing.T) {
		c := testcheck.New(t)
		result, err := migrationlint.SplitRequest(context.Background(), request, migrationlint.StatementFacts)
		c.Assert(err, qt.IsNil)
		c.Assert(result.GetJudged(), qt.DeepEquals, migrationlint.StatementFacts)
		files := result.GetFiles()
		c.Assert(files, qt.HasLen, 1)
		c.Assert(files[0].GetStatements(), qt.HasLen, 2)
		c.Assert(files[0].GetStatements()[0].GetEffects(), qt.CmpEquals(protocmp.Transform()), []*protocol.LintEffect{
			{Fact: kindFact, StatementClass: createTable, Kind: "CREATE TABLE", Object: "nopk"}, {Fact: missingKey, Table: "nopk"},
		})
		c.Assert(files[0].GetStatements()[1].GetEffects(), qt.CmpEquals(protocmp.Transform()), []*protocol.LintEffect{{Fact: kindFact, StatementClass: other, Kind: "INSERT"}})
	})
}

func TestSplitRequest_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	result, err := migrationlint.SplitRequest(context.Background(), &protocol.SplitLintRequest{Engine: "spanner", Sources: []*protocol.LintSource{
		{Name: "1_a.up.sql", Sql: "SELECT 'x"},
	}}, migrationlint.StatementFacts)
	c.Assert(err, qt.ErrorMatches, `a string does not close on line 1`)
	c.Assert(result, qt.IsNil)
}

// A statement the host split at the bytes this package splits it at is
// judged for the facts asked for; any other is declined, and so is every
// statement of a source the provider's lexer cannot read.
func TestAnnotateRequest_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	request := &protocol.AnnotateLintRequest{
		Engine: "spanner",
		Facts:  []protocol.LintFact{protocol.LintFact_LINT_FACT_STATEFUL_DROP, kindFact},
		Sources: []*protocol.LintSource{
			{Name: "a.sql", Sql: "CREATE TABLE nopk (a bigint); DROP TABLE nopk"},
			{Name: "b.sql", Sql: "SELECT $$a;b$$"},
			{Name: "c.sql", Sql: "SELECT $$a"},
		},
		Files: []*protocol.LintFile{
			{Name: "a.sql", Statements: []*protocol.LintStatement{{Start: 0, End: 28}, {Start: 30, End: 40}}},
			{Name: "b.sql", Statements: []*protocol.LintStatement{{Start: 0, End: 14}}},
			{Name: "c.sql", Statements: []*protocol.LintStatement{{Start: 0, End: 10}}},
		},
	}
	result, err := migrationlint.AnnotateRequest(context.Background(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result, qt.CmpEquals(protocmp.Transform()), &protocol.AnnotatedLint{Files: []*protocol.LintAnnotations{
		{Statements: []*protocol.LintAnnotation{
			{Effects: []*protocol.LintEffect{{Fact: kindFact, StatementClass: createTable, Kind: "CREATE TABLE", Object: "nopk"}}},
			{Declined: true},
		}},
		{Statements: []*protocol.LintAnnotation{{Effects: []*protocol.LintEffect{{Fact: kindFact, StatementClass: other, Kind: "SELECT"}}}}},
		{Statements: []*protocol.LintAnnotation{{Declined: true}}},
	}})
}

func TestCheckRequest_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	result, err := migrationlint.CheckRequest(context.Background(), &protocol.CheckLintRequest{
		Engine: "spanner", Codes: []string{"SP101"},
		Files: []*protocol.LintFile{
			{Name: "1_a.up.sql", Up: true, Statements: []*protocol.LintStatement{{Words: []string{"DROP", "TABLE", "T"}}}},
			{Name: "1_a.down.sql", Down: true, Statements: []*protocol.LintStatement{
				{Words: []string{"ALTER", "TABLE", "T", "ALTER", "COLUMN", "A", "SET", "NOT", "NULL"}},
			}},
		},
	})
	c.Assert(err, qt.IsNil)
	c.Assert(result, qt.CmpEquals(protocmp.Transform()), &protocol.CheckedLint{Complete: true, Findings: []*protocol.LintFinding{
		{Code: "SP101", File: 1, Statement: 0, Message: "SET NOT NULL runs as a schema change that validates every existing row, and a row holding NULL fails it " +
			"(FAILED_PRECONDITION: Cannot specify a null value for column) and leaves the column nullable; backfill first"},
	}})
}
