package migrationlint_test

import (
	"context"
	"testing"

	qt "github.com/frankban/quicktest"
	"google.golang.org/protobuf/testing/protocmp"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-spanner-plugin/internal/migrationlint"
	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// data-dependent/set-not-null is the check the host's PostgreSQL owner made for
// Spanner, in its words. side-effect/drop-row-deletion-policy replaces the
// host's column-drop findings on a TTL drop, which drops no column; a quoted
// "TTL" is a column, which destructive/drop-column keeps.
func TestRules_PublishesTheCatalog(t *testing.T) {
	c := testcheck.New(t)
	c.Assert(migrationlint.Rules(), qt.CmpEquals(protocmp.Transform()), []*protocol.LintRule{
		{Code: "data-dependent/set-not-null", Title: "not-null validation scans existing rows", Severity: protocol.LintRule_WARNING, AppliesToDown: true},
		{Code: "side-effect/drop-row-deletion-policy", Title: "row deletion policy dropped", Severity: protocol.LintRule_INFO, AppliesToDown: true, Subsumes: []string{"rollout/drop-column", "destructive/drop-column"}},
	})
}

func TestCheck_HappyPath(t *testing.T) {
	const notNull = "SET NOT NULL runs as a schema change that validates every existing row, and a row holding NULL fails it " +
		"(FAILED_PRECONDITION: Cannot specify a null value for column) and leaves the column nullable; backfill first"
	const dropTTL = "DROP TTL removes the table's row deletion policy and drops no column; rows the policy would have deleted are kept from now on"
	tests := []struct {
		name  string
		files []string
		want  []migrationlint.Finding
	}{
		{
			name: "SET NOT NULL in either spelling, in every file",
			files: []string{
				"ALTER TABLE t ALTER COLUMN a SET NOT NULL; ALTER TABLE t ADD COLUMN b bigint NOT NULL; ALTER TABLE \"s\".\"t\" ALTER b SET NOT NULL",
				"ALTER TABLE t ADD COLUMN c bigint, ALTER COLUMN c SET NOT NULL",
			},
			want: []migrationlint.Finding{
				{Code: "data-dependent/set-not-null", File: 0, Statement: 0, Message: notNull},
				{Code: "data-dependent/set-not-null", File: 0, Statement: 2, Message: notNull},
				{Code: "data-dependent/set-not-null", File: 1, Statement: 0, Message: notNull},
			},
		},
		{
			name:  "a TTL drop, and neither a quoted TTL column nor a column drop",
			files: []string{"ALTER TABLE ev DROP TTL; ALTER TABLE ev DROP \"TTL\"; ALTER TABLE ev DROP COLUMN ttl; ALTER TABLE ev ADD TTL INTERVAL '1 day' ON at"},
			want:  []migrationlint.Finding{{Code: "side-effect/drop-row-deletion-policy", File: 0, Statement: 0, Message: dropTTL}},
		},
		{
			name:  "nothing in a file of data statements",
			files: []string{"INSERT INTO t (a) VALUES ('SET NOT NULL'); UPDATE t SET a = 'DROP TTL'"},
			want:  nil,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			files := make([]migrationlint.File, 0, len(test.files))
			for _, text := range test.files {
				statements, err := migrationlint.Split(text)
				c.Assert(err, qt.IsNil)
				files = append(files, migrationlint.File{Statements: statements})
			}
			findings, err := migrationlint.Check(context.Background(), []string{"data-dependent/set-not-null", "side-effect/drop-row-deletion-policy"}, files)
			c.Assert(err, qt.IsNil)
			c.Assert(findings, qt.DeepEquals, test.want)
		})
	}
}

func TestCheck_FailurePath(t *testing.T) {
	t.Run("a code the provider does not publish", func(t *testing.T) {
		c := testcheck.New(t)
		findings, err := migrationlint.Check(context.Background(), []string{"locking/set-not-null"}, nil)
		c.Assert(err, qt.ErrorMatches, `the provider publishes no lint rule locking/set-not-null`)
		c.Assert(findings, qt.IsNil)
	})
	t.Run("a canceled run", func(t *testing.T) {
		c := testcheck.New(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		findings, err := migrationlint.Check(ctx, []string{"data-dependent/set-not-null"}, []migrationlint.File{{}})
		c.Assert(err, qt.ErrorIs, context.Canceled)
		c.Assert(findings, qt.IsNil)
	})
}

// TestRules_AreNamedByTheSDKGrammar_HappyPath holds each published rule and each rule it
// subsumes to the grammar a contract 26 host accepts.
func TestRules_AreNamedByTheSDKGrammar_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	rules := migrationlint.Rules()
	c.Assert(len(rules) > 0, qt.IsTrue)
	for _, rule := range rules {
		c.Assert(provider.ValidLintRuleName(rule.GetCode()), qt.IsTrue, qt.Commentf("rule %q", rule.GetCode()))
		for _, subsumed := range rule.GetSubsumes() {
			c.Assert(provider.ValidLintRuleName(subsumed), qt.IsTrue, qt.Commentf("rule %q subsumes %q", rule.GetCode(), subsumed))
		}
	}
}

// TestRules_AreNamedByTheSDKGrammar_FailurePath is the control: the grammar
// refuses the codes the provider published on contract 25, so the happy-path
// test cannot pass because the check accepts anything.
func TestRules_AreNamedByTheSDKGrammar_FailurePath(t *testing.T) {
	for _, name := range []string{"SP101", "SP102", "PG303", "locking", "locking/", "/set-not-null", "Locking/set-not-null", "locking/set_not_null"} {
		t.Run(name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(provider.ValidLintRuleName(name), qt.IsFalse)
		})
	}
}
