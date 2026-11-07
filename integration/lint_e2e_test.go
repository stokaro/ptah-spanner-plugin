//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// Each test here drives `ptah migrations lint` or `ptah sql lint` through the
// provider. The host keeps the rules every engine shares and runs them over
// the provider's split; the provider says what each statement is and checks
// the SP rules. The findings are the ones Ptah's built-in Spanner engine
// reported for the same files, measured at host d59a3b63d and on host master
// without the provider, except where a test says otherwise.

// lintMigrations is a migration directory that holds what an ordinary Spanner
// migration holds beside its tables: rows, a rename, a view in Spanner's
// spelling, a TTL drop and a suppression directive.
var lintMigrations = map[string]string{
	"1700000001_init.up.sql": `CREATE TABLE t (id bigint NOT NULL, a varchar(10), PRIMARY KEY (id));
CREATE TABLE nopk (x bigint);
CREATE TABLE ev (id bigint NOT NULL, at timestamptz NOT NULL, PRIMARY KEY (id)) TTL INTERVAL '30 days' ON at;
INSERT INTO t (id, a) VALUES (1, 'it''s; fine');
`,
	"1700000001_init.down.sql": "DROP TABLE ev;\nDROP TABLE nopk;\nDROP TABLE t;\n",
	"1700000002_risky.up.sql": `ALTER TABLE t ALTER COLUMN a SET NOT NULL;
UPDATE t SET a = 'x' WHERE id = 1;
ALTER TABLE t RENAME COLUMN a TO aa;
CREATE VIEW v SQL SECURITY INVOKER AS SELECT id FROM t;
ALTER TABLE ev DROP TTL;
-- atlas:nolint DS101 BC103
DROP TABLE nopk;
DROP TABLE t;
`,
	"1700000002_risky.down.sql": "ALTER TABLE ev ADD TTL INTERVAL '30 days' ON at;\n",
}

// lintFinding is one finding of a lint report: its rule, the base name of its
// file and its line.
type lintFinding struct {
	Rule string `json:"rule"`
	File string `json:"file"`
	Line int    `json:"line"`
}

func (f lintFinding) String() string {
	return fmt.Sprintf("%s:%d %s", filepath.Base(f.File), f.Line, f.Rule)
}

// lintReport runs one lint command of the host with this provider selected,
// in JSON, and returns its findings as file:line rule and the error it exited
// with. Standard error, which carries the notices about what a run could not
// read, is left out of the report.
func lintReport(c *qt.C, args ...string) ([]string, error) {
	c.Helper()
	host, manifest := hostManifest(c)
	ctx, cancel := context.WithTimeout(c.Context(), 2*time.Minute)
	defer cancel()
	command := testCommand(ctx, host, append(args, "--format", "json", "--provider-config", manifest, "--provider-engine", "spanner")...)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	runErr := command.Run()
	var report struct {
		Findings []lintFinding `json:"findings"`
	}
	c.Assert(json.Unmarshal(stdout.Bytes(), &report), qt.IsNil, qt.Commentf("stdout: %s\nstderr: %s", stdout.String(), stderr.String()))
	findings := make([]string, 0, len(report.Findings))
	for _, finding := range report.Findings {
		findings = append(findings, finding.String())
	}
	return findings, runErr
}

// The host lints a directory whose files hold rows, a rename and a view: the
// AnalyzeSQL split refused every statement but the table statements it
// writes, so the run failed. SET NOT NULL is SP101, in the words PG303 said
// for Spanner, in an up file. A TTL drop is SP102, where the built-in reported
// DS102 and BC104 for a column named TTL, which Spanner reserves as a word.
// The suppression directive above a statement silences what it names.
func TestHostLintsMigrationsThroughTheProviderE2E(t *testing.T) {
	c := testcheck.New(t)
	dir := writeFiles(c, lintMigrations)
	findings, err := lintReport(c, "migrations", "lint", "--dialect", "spanner", "--dir", dir, "--fail-on", "none")
	c.Assert(err, qt.IsNil, qt.Commentf("%v", findings))
	for _, want := range []string{
		"1700000002_risky.up.sql:1 SP101",
		"1700000002_risky.up.sql:3 BC101",
		"1700000002_risky.up.sql:5 SP102",
		"1700000002_risky.up.sql:8 BC103",
		"1700000002_risky.up.sql:8 DS101",
	} {
		c.Assert(findings, qt.Contains, want)
	}
	for _, unwanted := range []string{
		"1700000002_risky.up.sql:5 DS102",
		"1700000002_risky.up.sql:5 BC104",
		"1700000002_risky.up.sql:7 DS101",
		"1700000002_risky.up.sql:7 BC103",
	} {
		c.Assert(findings, qt.Not(qt.Contains), unwanted)
	}
}

// A down file is checked too, as PG303 checked one.
func TestHostLintsADownFileThroughTheProviderE2E(t *testing.T) {
	c := testcheck.New(t)
	dir := writeFiles(c, map[string]string{
		"1700000001_t.up.sql":   "CREATE TABLE t (id bigint NOT NULL, a varchar(10), PRIMARY KEY (id));\n",
		"1700000001_t.down.sql": "ALTER TABLE t ALTER COLUMN a SET NOT NULL;\nDROP TABLE t;\n",
	})
	findings, err := lintReport(c, "migrations", "lint", "--dialect", "spanner", "--dir", dir, "--fail-on", "none")
	c.Assert(err, qt.IsNil, qt.Commentf("%v", findings))
	c.Assert(findings, qt.Contains, "1700000001_t.down.sql:1 SP101")
}

// `ptah sql lint` reads a Spanner file by the statement facts of the
// provider's split: a table without a primary key is DDL001, a data statement
// is SQL002, and CREATE INDEX CONCURRENTLY is CAP001, as the built-in reported
// them. The host answers the capability from the Spanner preset it still
// builds in. A statement of Spanner's own the built-in grammar refused, such
// as a bit-reversed sequence, is a statement no rule examined (SQL004) rather
// than unsupported SQL.
func TestHostLintsASQLFileThroughTheProviderE2E(t *testing.T) {
	c := testcheck.New(t)
	file := filepath.Join(writeFiles(c, map[string]string{"wide.sql": `CREATE TABLE nopk (a bigint);
CREATE TABLE withpk (id bigint NOT NULL, c bigint, PRIMARY KEY (id));
INSERT INTO withpk (id) VALUES (1);
CREATE INDEX CONCURRENTLY withpk_c ON withpk (c);
CREATE SEQUENCE s BIT_REVERSED_POSITIVE;
`}), "wide.sql")
	findings, err := lintReport(c, "sql", "lint", "--dialect", "spanner", file)
	c.Assert(err, qt.ErrorMatches, `exit status 1`)
	c.Assert(findings, qt.DeepEquals, []string{"wide.sql:1 DDL001", "wide.sql:3 SQL002", "wide.sql:4 CAP001", "wide.sql:1 SQL004"})
}
