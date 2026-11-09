package migrationlint_test

import (
	"context"
	"testing"

	qt "github.com/frankban/quicktest"
	"google.golang.org/protobuf/testing/protocmp"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-spanner-plugin/internal/migrationlint"
	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// SP101 is the check the host's PG303 made for Spanner, in its words. SP102
// replaces the host's column-drop findings on a TTL drop, which drops no
// column; a quoted "TTL" is a column, which DS102 keeps.
func TestRules_PublishesTheCatalog(t *testing.T) {
	c := testcheck.New(t)
	c.Assert(migrationlint.Rules(), qt.CmpEquals(protocmp.Transform()), []*protocol.LintRule{
		{Code: "SP101", Title: "not-null validation scans existing rows", Severity: protocol.LintRule_WARNING, AppliesToDown: true},
		{Code: "SP102", Title: "row deletion policy dropped", Severity: protocol.LintRule_INFO, AppliesToDown: true, Subsumes: []string{"BC104", "DS102"}},
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
				{Code: "SP101", File: 0, Statement: 0, Message: notNull},
				{Code: "SP101", File: 0, Statement: 2, Message: notNull},
				{Code: "SP101", File: 1, Statement: 0, Message: notNull},
			},
		},
		{
			name:  "a TTL drop, and neither a quoted TTL column nor a column drop",
			files: []string{"ALTER TABLE ev DROP TTL; ALTER TABLE ev DROP \"TTL\"; ALTER TABLE ev DROP COLUMN ttl; ALTER TABLE ev ADD TTL INTERVAL '1 day' ON at"},
			want:  []migrationlint.Finding{{Code: "SP102", File: 0, Statement: 0, Message: dropTTL}},
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
			findings, err := migrationlint.Check(context.Background(), []string{"SP101", "SP102"}, files)
			c.Assert(err, qt.IsNil)
			c.Assert(findings, qt.DeepEquals, test.want)
		})
	}
}

func TestCheck_FailurePath(t *testing.T) {
	t.Run("a code the provider does not publish", func(t *testing.T) {
		c := testcheck.New(t)
		findings, err := migrationlint.Check(context.Background(), []string{"PG303"}, nil)
		c.Assert(err, qt.ErrorMatches, `the provider publishes no lint rule PG303`)
		c.Assert(findings, qt.IsNil)
	})
	t.Run("a canceled run", func(t *testing.T) {
		c := testcheck.New(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		findings, err := migrationlint.Check(ctx, []string{"SP101"}, []migrationlint.File{{}})
		c.Assert(err, qt.ErrorIs, context.Canceled)
		c.Assert(findings, qt.IsNil)
	})
}
