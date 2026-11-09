package backend_test

import (
	"context"
	"testing"

	qt "github.com/frankban/quicktest"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-spanner-plugin/internal/backend"
	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// The provider serves migration lint for the engine, which alone lists the
// statement facts, and publishes the SP rules.
func TestDescription_ServesMigrationLint(t *testing.T) {
	c := testcheck.New(t)
	description := backend.Description()
	engine := description.GetEngines()[0]
	c.Assert(description.GetFeatures(), qt.Contains, provider.LintFeature)
	c.Assert(engine.GetFeatures(), qt.Contains, provider.LintFeature)
	c.Assert(description.GetFeatures(), qt.Not(qt.Contains), provider.LintStatementFactsFeature)
	c.Assert(engine.GetFeatures(), qt.Contains, provider.LintStatementFactsFeature)
	published := make([]string, 0, len(engine.GetLintRules()))
	for _, rule := range engine.GetLintRules() {
		published = append(published, rule.GetCode())
	}
	c.Assert(published, qt.DeepEquals, []string{"SP101", "SP102"})
}

// A host that lists lint.statement-facts gets what each statement is; one
// that does not gets the statements alone, since the SDK server refuses a
// reply that uses an addition the host did not list.
func TestSplitLintStatements_HappyPath(t *testing.T) {
	request := &protocol.SplitLintRequest{Engine: "spanner", Sources: []*protocol.LintSource{{Name: "a.sql", Sql: "INSERT INTO t VALUES (1)"}}}
	tests := []struct {
		name       string
		host       []string
		wantJudged int
	}{
		{name: "a host that lists the statement facts", host: []string{provider.LintStatementFactsFeature}, wantJudged: 3},
		{name: "a host that does not", host: nil, wantJudged: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			result, err := backend.New().SplitLintStatements(provider.ContextWithHostFeatures(context.Background(), test.host), request)
			c.Assert(err, qt.IsNil)
			c.Assert(result.GetJudged(), qt.HasLen, test.wantJudged)
			c.Assert(result.GetFiles(), qt.HasLen, 1)
			c.Assert(result.GetFiles()[0].GetStatements(), qt.HasLen, 1)
		})
	}
}

// A text the provider cannot read is refused with a reason, and a canceled
// call keeps the context's error.
func TestSplitLintStatements_FailurePath(t *testing.T) {
	t.Run("an unreadable text", func(t *testing.T) {
		c := testcheck.New(t)
		result, err := backend.New().SplitLintStatements(context.Background(), &protocol.SplitLintRequest{Engine: "spanner",
			Sources: []*protocol.LintSource{{Name: "a.sql", Sql: "SELECT $$x"}}})
		c.Assert(status.Code(err), qt.Equals, codes.InvalidArgument)
		c.Assert(status.Convert(err).Message(), qt.Equals, "a dollar-quoted string $$ does not close on line 1")
		c.Assert(result, qt.IsNil)
	})
	t.Run("a canceled call", func(t *testing.T) {
		c := testcheck.New(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		result, err := backend.New().SplitLintStatements(ctx, &protocol.SplitLintRequest{Engine: "spanner",
			Sources: []*protocol.LintSource{{Name: "a.sql", Sql: "SELECT 1"}}})
		c.Assert(status.Code(err), qt.Equals, codes.Canceled)
		c.Assert(result, qt.IsNil)
	})
}
