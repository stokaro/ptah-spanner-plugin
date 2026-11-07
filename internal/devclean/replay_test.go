package devclean_test

import (
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-spanner-plugin/internal/devclean"
	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// A statement whose effects a cleanup removes is accepted.
func TestReplay_Accepted(t *testing.T) {
	statements := []string{
		"CREATE TABLE t (id bigint NOT NULL, PRIMARY KEY (id))",
		"create table if not exists app.t (id bigint not null, primary key (id));",
		"ALTER TABLE t ADD COLUMN v varchar(10)",
		"DROP TABLE t",
		"CREATE UNIQUE INDEX t_v ON t (v)",
		"CREATE SEARCH INDEX si ON t (id)",
		"DROP SEARCH INDEX si",
		"CREATE OR REPLACE VIEW v SQL SECURITY INVOKER AS SELECT id FROM t",
		"CREATE SEQUENCE s BIT_REVERSED_POSITIVE",
		"ALTER SEQUENCE s SKIP RANGE 1 10",
		"CREATE CHANGE STREAM cs FOR ALL",
		"CREATE LOCALITY GROUP lg",
		"CREATE SCHEMA app",
		"-- seed a row\nINSERT INTO t (id) VALUES (1)",
		"UPDATE t SET v = 'x' WHERE id = 1",
		"DELETE FROM t WHERE id = 1",
		"SELECT nextval('s')",
		"WITH x AS (SELECT 1) SELECT * FROM x",
		"ANALYZE",
	}
	for _, statement := range statements {
		t.Run(statement, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(devclean.Replay(statement), qt.Equals, "")
		})
	}
}

// A statement whose effects outlive a cleanup, or reach the statements the
// run sends after it, is refused, and so is one the provider cannot read.
func TestReplay_Refused(t *testing.T) {
	tests := []struct{ statement, operation string }{
		{"ALTER DATABASE db SET spanner.default_leader = 'us-east1'", "ALTER DATABASE"},
		{"DROP DATABASE db", "DROP DATABASE"},
		{"CREATE ROLE reader", "CREATE ROLE"},
		{"DROP ROLE reader", "DROP ROLE"},
		{"GRANT SELECT ON TABLE t TO reader", "GRANT"},
		{"REVOKE SELECT ON TABLE t FROM reader", "REVOKE"},
		{"SET spanner.read_only_staleness = 'strong'", "SET session or transaction state"},
		{"RESET ALL", "RESET session or transaction state"},
		{"BEGIN", "BEGIN session or transaction state"},
		{"COMMIT", "COMMIT session or transaction state"},
		{"CALL p()", "CALL statement"},
		{"CREATE", "CREATE statement"},
		{"SELECT $$x$$", "a statement the provider cannot read"},
		{"CREATE TABLE a (id bigint); DROP TABLE b", "a statement the provider cannot read"},
		{"-- only a comment", "an empty statement"},
	}
	for _, test := range tests {
		t.Run(test.statement, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(devclean.Replay(test.statement), qt.Equals, test.operation)
		})
	}
}
