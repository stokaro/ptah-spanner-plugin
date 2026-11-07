package migrationunits_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-spanner-plugin/internal/migrationunits"
	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// unit is a migration unit as the tests compare it.
type unit struct {
	SQL, Source string
	Data        bool
}

func units(split []*protocol.MigrationUnit) []unit {
	result := make([]unit, len(split))
	for i, current := range split {
		result[i] = unit{SQL: current.GetSql(), Source: current.GetSource(), Data: current.GetPlacement() == protocol.MigrationUnit_WITH_CHECKPOINT}
	}
	return result
}

// Each statement is a unit. A schema statement runs outside a transaction and
// a statement that changes or reads rows commits with its checkpoint. The
// source runs from the first token through the semicolon, as Ptah's built-in
// engine read a statement for its partial hashes: leading comments and white
// space are left out, comments inside the statement and the bytes before the
// semicolon stay, and a last statement no semicolon ends runs to the end of
// the body.
func TestSplit_HappyPath(t *testing.T) {
	tests := []struct {
		name string
		body string
		want []unit
	}{
		{
			name: "schema and data statements",
			body: "CREATE TABLE t (id bigint PRIMARY KEY);\nINSERT INTO t (id) VALUES (1);\n",
			want: []unit{
				{SQL: "CREATE TABLE t (id bigint PRIMARY KEY)", Source: "CREATE TABLE t (id bigint PRIMARY KEY);"},
				{SQL: "INSERT INTO t (id) VALUES (1)", Source: "INSERT INTO t (id) VALUES (1);", Data: true},
			},
		},
		{
			name: "comments before a statement are not its source",
			body: "-- create the table\n/* block */ CREATE INDEX i ON t (a);",
			want: []unit{{SQL: "CREATE INDEX i ON t (a)", Source: "CREATE INDEX i ON t (a);"}},
		},
		{
			name: "a comment inside and a newline before the semicolon stay",
			body: "UPDATE t /* every row */ SET a = 'x;y'\n;",
			want: []unit{{SQL: "UPDATE t /* every row */ SET a = 'x;y'", Source: "UPDATE t /* every row */ SET a = 'x;y'\n;", Data: true}},
		},
		{
			name: "a last statement without a semicolon runs to the end",
			body: "DELETE FROM t;\nDROP TABLE t\n",
			want: []unit{
				{SQL: "DELETE FROM t", Source: "DELETE FROM t;", Data: true},
				{SQL: "DROP TABLE t", Source: "DROP TABLE t\n"},
			},
		},
		{
			name: "stray semicolons are no statement",
			body: ";\n;CREATE TABLE \"T\" (\"id\" bigint PRIMARY KEY);;",
			want: []unit{{SQL: "CREATE TABLE \"T\" (\"id\" bigint PRIMARY KEY)", Source: "CREATE TABLE \"T\" (\"id\" bigint PRIMARY KEY);"}},
		},
		{
			name: "queries read rows",
			body: "SELECT 1; WITH x AS (SELECT 1) SELECT * FROM x; (SELECT 2); VALUES (3); SHOW search_path;",
			want: []unit{
				{SQL: "SELECT 1", Source: "SELECT 1;", Data: true},
				{SQL: "WITH x AS (SELECT 1) SELECT * FROM x", Source: "WITH x AS (SELECT 1) SELECT * FROM x;", Data: true},
				{SQL: "(SELECT 2)", Source: "(SELECT 2);", Data: true},
				{SQL: "VALUES (3)", Source: "VALUES (3);", Data: true},
				{SQL: "SHOW search_path", Source: "SHOW search_path;", Data: true},
			},
		},
		{
			name: "other statements run outside a transaction",
			body: "alter table t add column b bigint; GRANT SELECT ON t TO r; ANALYZE;",
			want: []unit{
				{SQL: "alter table t add column b bigint", Source: "alter table t add column b bigint;"},
				{SQL: "GRANT SELECT ON t TO r", Source: "GRANT SELECT ON t TO r;"},
				{SQL: "ANALYZE", Source: "ANALYZE;"},
			},
		},
		{name: "a body of comments has no unit", body: "-- nothing\n/* here */\n", want: make([]unit, 0)},
		{name: "an empty body has no unit", body: "", want: make([]unit, 0)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			split, refusal := migrationunits.Split(test.body)
			c.Assert(refusal, qt.IsNil)
			c.Assert(units(split), qt.DeepEquals, test.want)
		})
	}
}

// A statement that controls a transaction or a batch, or changes a session
// setting, refuses the whole body with its byte range; a body the provider
// cannot tokenize is refused whole.
func TestSplit_FailurePath(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		start, end uint32
		reason     string
	}{
		{name: "begin", body: "CREATE TABLE t (id bigint PRIMARY KEY);\n  BEGIN;\nINSERT INTO t VALUES (1);", start: 42, end: 48, reason: migrationunits.TransactionControlReason},
		{name: "commit", body: "COMMIT", start: 0, end: 6, reason: migrationunits.TransactionControlReason},
		{name: "start transaction", body: "start transaction;", start: 0, end: 18, reason: migrationunits.TransactionControlReason},
		{name: "start batch", body: "START BATCH DDL;", start: 0, end: 16, reason: migrationunits.TransactionControlReason},
		{name: "run batch", body: "RUN BATCH;", start: 0, end: 10, reason: migrationunits.TransactionControlReason},
		{name: "savepoint", body: "SAVEPOINT s;", start: 0, end: 12, reason: migrationunits.TransactionControlReason},
		{name: "set", body: "SET statement_timeout = '1s';", start: 0, end: 29, reason: migrationunits.SettingReason},
		{name: "reset", body: "-- note\nRESET ALL;", start: 8, end: 18, reason: migrationunits.SettingReason},
		{name: "dollar quote", body: "SELECT $$x$$;", start: 0, end: 13, reason: migrationunits.LexingReason},
		{name: "unclosed string", body: "SELECT 'x;", start: 0, end: 10, reason: migrationunits.LexingReason},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			split, refusal := migrationunits.Split(test.body)
			c.Assert(split, qt.IsNil)
			c.Assert(refusal.GetStart(), qt.Equals, test.start)
			c.Assert(refusal.GetEnd(), qt.Equals, test.end)
			c.Assert(refusal.GetReason(), qt.Equals, test.reason)
		})
	}
}

// A check assertion is read only when it is one query that changes no row.
func TestReadOnly_HappyPath(t *testing.T) {
	for _, assertion := range []string{"SELECT count(*) = 0 FROM t", "SELECT 1;", "WITH x AS (SELECT 1) SELECT * FROM x", "VALUES (true)"} {
		t.Run(assertion, func(t *testing.T) {
			c := testcheck.New(t)
			readOnly, reason := migrationunits.ReadOnly(assertion)
			c.Assert(readOnly, qt.IsTrue)
			c.Assert(reason, qt.Equals, "")
		})
	}
}

func TestReadOnly_FailurePath(t *testing.T) {
	tests := []struct{ name, assertion, reason string }{
		{name: "two statements", assertion: "SELECT 1; SELECT 2", reason: "the text is not exactly one statement"},
		{name: "no statement", assertion: "-- empty", reason: "the text is not exactly one statement"},
		{name: "a change", assertion: "DELETE FROM t", reason: "the statement is not a query that only reads rows"},
		{name: "a change in a query", assertion: "WITH d AS (DELETE FROM t RETURNING id) SELECT * FROM d", reason: "the statement changes rows"},
		{name: "unreadable", assertion: "SELECT $$x$$", reason: migrationunits.LexingReason},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			readOnly, reason := migrationunits.ReadOnly(test.assertion)
			c.Assert(readOnly, qt.IsFalse)
			c.Assert(reason, qt.Equals, test.reason)
		})
	}
}
