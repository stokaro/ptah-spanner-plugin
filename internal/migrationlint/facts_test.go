package migrationlint_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-spanner-plugin/internal/migrationlint"
	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

const (
	createTable = protocol.LintStatementClass_LINT_STATEMENT_CLASS_CREATE_TABLE
	alterTable  = protocol.LintStatementClass_LINT_STATEMENT_CLASS_ALTER_TABLE
	schema      = protocol.LintStatementClass_LINT_STATEMENT_CLASS_SCHEMA
	other       = protocol.LintStatementClass_LINT_STATEMENT_CLASS_OTHER
	kindFact    = protocol.LintFact_LINT_FACT_STATEMENT_KIND
	missingKey  = protocol.LintFact_LINT_FACT_MISSING_PRIMARY_KEY
	requirement = protocol.LintFact_LINT_FACT_REQUIRED_CAPABILITY
)

// A statement is classified as the host's built-in Spanner path of `ptah sql
// lint` classified it: tables by name, other schema statements by their first
// two words, everything else by its first word; a table without a primary key
// is named as written, as that path's DDL001 named it.
func TestDescribe_HappyPath(t *testing.T) {
	tests := []struct {
		name, sql string
		want      []migrationlint.Effect
	}{
		{
			name: "a table with a primary key",
			sql:  "CREATE TABLE t (id bigint NOT NULL, PRIMARY KEY (id))",
			want: []migrationlint.Effect{{Fact: kindFact, Class: createTable, Kind: "CREATE TABLE", Object: "t"}},
		},
		{
			name: "a table without a primary key",
			sql:  "CREATE TABLE IF NOT EXISTS nopk (a bigint)",
			want: []migrationlint.Effect{{Fact: kindFact, Class: createTable, Kind: "CREATE TABLE", Object: "nopk"}, {Fact: missingKey, Table: "nopk"}},
		},
		{
			name: "a quoted table without a primary key keeps its quotes",
			sql:  `CREATE TABLE "Q" ("Id" bigint)`,
			want: []migrationlint.Effect{{Fact: kindFact, Class: createTable, Kind: "CREATE TABLE", Object: `"Q"`}, {Fact: missingKey, Table: `"Q"`}},
		},
		{
			name: "a qualified table without a primary key",
			sql:  "CREATE TABLE s.t2 (id bigint)",
			want: []migrationlint.Effect{{Fact: kindFact, Class: createTable, Kind: "CREATE TABLE", Object: "s.t2"}, {Fact: missingKey, Table: "s.t2"}},
		},
		{
			name: "an altered table",
			sql:  "ALTER TABLE ev ADD TTL INTERVAL '1 day' ON at",
			want: []migrationlint.Effect{{Fact: kindFact, Class: alterTable, Kind: "ALTER TABLE", Object: "ev"}},
		},
		{
			name: "a unique index",
			sql:  "CREATE UNIQUE INDEX u ON t (a)",
			want: []migrationlint.Effect{{Fact: kindFact, Class: schema, Kind: "CREATE INDEX"}},
		},
		{
			name: "an index built concurrently needs the capability",
			sql:  "CREATE INDEX CONCURRENTLY t_d ON t (d)",
			want: []migrationlint.Effect{
				{Fact: kindFact, Class: schema, Kind: "CREATE INDEX"},
				{Fact: requirement, Kind: "CREATE INDEX", Object: "t_d", Table: "t", Clause: "CONCURRENTLY", Capability: "create_index_concurrently"},
			},
		},
		{
			name: "a view Spanner requires SQL SECURITY on",
			sql:  "CREATE OR REPLACE VIEW v SQL SECURITY INVOKER AS SELECT 1",
			want: []migrationlint.Effect{{Fact: kindFact, Class: schema, Kind: "CREATE VIEW"}},
		},
		{
			name: "a change stream",
			sql:  "DROP CHANGE STREAM cs",
			want: []migrationlint.Effect{{Fact: kindFact, Class: schema, Kind: "DROP CHANGE STREAM"}},
		},
		{
			name: "a sequence option of Spanner's own",
			sql:  "ALTER SEQUENCE q SKIP RANGE 1 10",
			want: []migrationlint.Effect{{Fact: kindFact, Class: schema, Kind: "ALTER SEQUENCE"}},
		},
		{
			name: "a data statement",
			sql:  "INSERT INTO t VALUES (1)",
			want: []migrationlint.Effect{{Fact: kindFact, Class: other, Kind: "INSERT"}},
		},
		{
			name: "a grant changes no schema object",
			sql:  "GRANT SELECT ON TABLE t TO r",
			want: []migrationlint.Effect{{Fact: kindFact, Class: other, Kind: "GRANT"}},
		},
		{
			name: "a statement that starts with punctuation",
			sql:  "(SELECT 1)",
			want: []migrationlint.Effect{{Fact: kindFact, Class: other, Kind: "expression"}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			statements, err := migrationlint.Split(test.sql)
			c.Assert(err, qt.IsNil)
			c.Assert(statements, qt.HasLen, 1)
			c.Assert(migrationlint.Describe(statements[0]), qt.DeepEquals, test.want)
		})
	}
}
