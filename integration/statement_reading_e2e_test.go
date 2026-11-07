//go:build integration

package integration_test

import (
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// The tests in this file hold what Ptah reads of a Spanner statement to what
// the emulator does with it. `ptah sql lint` reads a file through the
// provider's split, which names each statement by its opening words and does
// not parse Spanner's grammar; a schema document is read by the host's shared
// grammar. Ptah's built-in Spanner engine read both with a PostgreSQL grammar.

// readStatements are statements Spanner takes, in the order they run on the
// fixture: a view dropped, a view dropped only if it exists, a sequence
// dropped only if it exists, and a table emptied of its rows.
var readStatements = []string{"DROP VIEW v1_r", "DROP VIEW IF EXISTS v2_r", "DROP SEQUENCE IF EXISTS q_r", "TRUNCATE t_r"}

// `ptah sql lint` reads each drop statement Spanner takes without an error,
// reporting only that no rule examined its kind, and the server takes each in
// turn. TRUNCATE changes no schema object, so the provider's split says so,
// and the host reports it as a statement it does not lint, as it reports
// INSERT. Ptah's built-in Spanner engine read TRUNCATE with the host's
// PostgreSQL grammar and listed it among the statements no rule examined.
func TestHostLintsTheStatementsSpannerTakesE2E(t *testing.T) {
	c := testcheck.New(t)
	address := withSearchPath(c, freshDatabase(c))
	execSQL(c, address, "CREATE TABLE t_r (id bigint PRIMARY KEY)", "CREATE VIEW v1_r AS SELECT id FROM t_r",
		"CREATE VIEW v2_r AS SELECT id FROM t_r", "CREATE SEQUENCE q_r BIT_REVERSED_POSITIVE")
	file := filepath.Join(writeFiles(c, map[string]string{"read.sql": strings.Join(readStatements, ";\n") + ";\n"}), "read.sql")
	findings, err := lintMessages(c, "sql", "lint", "--dialect", "spanner", file)
	c.Assert(err, qt.ErrorMatches, `exit status 1`)
	c.Assert(findings, qt.DeepEquals, []string{
		"read.sql:4 SQL002 error: ptah sql lint does not lint TRUNCATE statements yet",
		"read.sql:1 SQL004 info: no rule examined DROP VIEW, DROP SEQUENCE in this file",
	})

	execSQL(c, address, readStatements...)
	c.Assert(queryRows(c, address, `SELECT table_name FROM information_schema.views WHERE table_schema = 'public'`), qt.HasLen, 0)
	c.Assert(queryRows(c, address, `SELECT sequence_name FROM "information_schema".sequences`), qt.HasLen, 0)
}

// refusedStatements are statements Spanner refuses whatever the database
// holds: a list of views, a trigger and a materialized view, which Spanner
// does not have, and a misspelled keyword.
var refusedStatements = []string{"DROP VIEW v1_f, v2_f", "DROP TRIGGER g_f", "DROP MATERIALIZED VIEW m_f", "DROP TABEL t_f"}

// The emulator refuses each of these statements, and `ptah sql lint` lists
// them among the statements no rule examined, an info finding that names each
// by its opening words, without an error. The provider's split does not parse
// Spanner's grammar, so it cannot tell a statement Spanner refuses from one
// it takes, and a clean result says the file was read, not that Spanner will
// run it. Ptah's built-in Spanner engine reported an error for each, SQL001
// or SQL002, from the host's PostgreSQL grammar, which also refused
// statements Spanner takes, such as a bit-reversed sequence (stokaro/ptah#4204
// S69).
func TestHostLintsTheStatementsSpannerRefusesE2E(t *testing.T) {
	c := testcheck.New(t)
	address := withSearchPath(c, freshDatabase(c))
	execSQL(c, address, "CREATE TABLE t_f (id bigint PRIMARY KEY)", "CREATE VIEW v1_f AS SELECT id FROM t_f", "CREATE VIEW v2_f AS SELECT id FROM t_f")
	conn := connect(c, address)
	for _, statement := range refusedStatements {
		_, err := conn.Exec(c.Context(), statement)
		c.Assert(err, qt.IsNotNil, qt.Commentf("%s", statement))
	}
	file := filepath.Join(writeFiles(c, map[string]string{"refused.sql": strings.Join(refusedStatements, ";\n") + ";\n"}), "refused.sql")
	findings, err := lintMessages(c, "sql", "lint", "--dialect", "spanner", file)
	c.Assert(err, qt.IsNil, qt.Commentf("%v", findings))
	c.Assert(findings, qt.DeepEquals, []string{
		"refused.sql:1 SQL004 info: no rule examined DROP VIEW, DROP TRIGGER, DROP MATERIALIZED, DROP TABEL in this file",
	})
	c.Assert(queryRows(c, address, `SELECT table_name FROM information_schema.views WHERE table_schema = 'public' ORDER BY table_name`), qt.DeepEquals,
		[]string{"v1_f", "v2_f"})
}

// viewScript is what a schema document may do with a view after it declares
// table {t} and views {a} and {b}; {c} names nothing.
type viewScript struct {
	name       string
	statements []string
}

// viewScripts are the scripts a host reads as the server runs them.
var viewScripts = []viewScript{
	{name: "a dropped view", statements: []string{"DROP VIEW {a}"}},
	{name: "a view created again", statements: []string{"DROP VIEW {a}", "CREATE VIEW {a} AS SELECT id FROM {t}"}},
	{name: "IF EXISTS of nothing", statements: []string{"DROP VIEW IF EXISTS {c}"}},
	{name: "a name nothing holds", statements: []string{"DROP VIEW {c}"}},
	{name: "a name a table holds", statements: []string{"DROP VIEW {t}"}},
}

// divergentViewScripts are the scripts Ptah's built-in Spanner engine read
// as the server runs them and the host reads otherwise for an engine a
// provider serves: it refuses DROP VIEW IF EXISTS of a name a table holds,
// which the server takes and changes nothing with, and it drops a view
// another view reads, which the server refuses. The words are what such a
// host answers: a refusal, or the view a plan creates although the server
// would refuse the script.
var divergentViewScripts = []struct {
	viewScript
	oldRefusal, oldPlan string
}{
	{viewScript: viewScript{name: "a name a table holds, IF EXISTS", statements: []string{"DROP VIEW IF EXISTS {t}"}},
		oldRefusal: "DROP VIEW t_s6: the schema declares t_s6 as another kind of object"},
	{viewScript: viewScript{name: "a view another view reads", statements: []string{"CREATE VIEW {c} AS SELECT id FROM {a}", "DROP VIEW {a}"}},
		oldPlan: `CREATE VIEW "c_s7" AS`},
}

// viewReadingChange names the host change the divergent scripts need.
const viewReadingChange = "reading DROP VIEW for an engine a provider serves as Ptah's built-in Spanner engine read it"

// createdView finds the view a plan creates and the name it gives it.
var createdView = regexp.MustCompile(`(?m)^CREATE VIEW "([a-z0-9_]+)" AS$`)

// A schema document that declares views and then drops, recreates or reads
// them ends with the views the emulator ends with after the same statements,
// and is refused where the emulator refuses one of them. Each script runs on
// the emulator under names of its own, and the document is planned against
// an empty database, whose plan creates the views the read ends with. A host
// that reads no DROP VIEW in a schema document refuses the first script, and
// a host that reads the divergent scripts otherwise skips after the others
// are checked.
func TestHostReadsADroppedViewAsTheServerEndsE2E(t *testing.T) {
	c := testcheck.New(t)
	server, empty := withSearchPath(c, freshDatabase(c)), withSearchPath(c, freshDatabase(c))
	for i, script := range viewScripts {
		want, output, err := runViewScript(c, server, empty, script, i+1)
		skipOnOldHost(c, output, err, "unsupported DROP target: VIEW", "reading DROP VIEW in a schema document")
		c.Assert(viewsThePlanCreates(output, err), qt.DeepEquals, want, qt.Commentf("script %q: %s", script.name, output))
	}
	for i, script := range divergentViewScripts {
		want, output, err := runViewScript(c, server, empty, script.viewScript, len(viewScripts)+i+1)
		skipOnOldReading(c, output, err, script.oldRefusal, script.oldPlan, viewReadingChange)
		c.Assert(viewsThePlanCreates(output, err), qt.DeepEquals, want, qt.Commentf("script %q: %s", script.name, output))
	}
}

// runViewScript runs script, the nth, on the server database under names of
// its own and returns the views the server ends with, and what the host
// wrote and its error for a plan of the same document against the empty
// database.
func runViewScript(c *qt.C, server, empty string, script viewScript, n int) ([]string, string, error) {
	c.Helper()
	suffix := "_s" + strconv.Itoa(n)
	names := strings.NewReplacer("{t}", "t"+suffix, "{a}", "a"+suffix, "{b}", "b"+suffix, "{c}", "c"+suffix)
	declared := []string{names.Replace("CREATE TABLE {t} (id bigint PRIMARY KEY)"), names.Replace("CREATE VIEW {a} AS SELECT id FROM {t}"),
		names.Replace("CREATE VIEW {b} AS SELECT id FROM {t}")}
	execSQL(c, server, declared...)
	written := make([]string, 0, len(script.statements))
	for _, statement := range script.statements {
		written = append(written, names.Replace(statement))
	}
	want := viewsTheServerEndsWith(c, server, written, suffix)
	output, err := planSQL(c, empty, strings.Join(append(declared, written...), ";\n")+";\n")
	return want, output, err
}

// skipOnOldReading skips the test when the host answered as a host that
// lacks change: with refusal, when it is not empty, or with a plan that holds
// plan, when it is not empty. Any other answer is the test's to judge.
func skipOnOldReading(c *qt.C, output string, err error, refusal, plan, change string) {
	c.Helper()
	if refusal != "" {
		skipOnOldHost(c, output, err, refusal, change)
	}
	if plan != "" && err == nil && strings.Contains(output, plan) {
		c.Skipf("the host predates %s: it plans %s", change, plan)
	}
}

// viewsTheServerEndsWith runs statements on the database address names and
// returns the views whose names end in suffix, in name order, or nil where
// the server refuses a statement. No view is an empty list, not nil, so a
// script that ends with none differs from a refused one.
func viewsTheServerEndsWith(c *qt.C, address string, statements []string, suffix string) []string {
	c.Helper()
	conn := connect(c, address)
	for _, statement := range statements {
		if _, err := conn.Exec(c.Context(), statement); err != nil {
			return nil
		}
	}
	views := queryRows(c, address, `SELECT table_name FROM information_schema.views WHERE table_schema = 'public' AND table_name LIKE '%`+suffix+`' ORDER BY table_name`)
	return append(make([]string, 0, len(views)), views...)
}

// viewsThePlanCreates returns the views a plan of an empty database creates,
// in name order, or nil where the host refused the document; a plan that
// creates none is an empty list.
func viewsThePlanCreates(output string, err error) []string {
	if err != nil {
		return nil
	}
	views := make([]string, 0)
	for _, match := range createdView.FindAllStringSubmatch(output, -1) {
		views = append(views, match[1])
	}
	slices.Sort(views)
	return views
}
