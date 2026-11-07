//go:build integration

package integration_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// The tests in this file declare a table's row deletion policy in each source
// format Ptah's built-in Spanner engine read it from, and drive the host
// binary through the provider against the emulator. The statements each plan
// holds are the ones the built-in wrote for the same declarations, measured
// at host d59a3b63d on the emulator behind PGAdapter 0.56.2. Each test takes
// one database and runs its whole sequence in it, because the emulator holds
// at most 100 databases.

// policyVersions are the three states of a declaration: events keeps a
// 30-day policy on created_at and plain has none; then events moves its
// policy to a new column seen_at at 60 days, and plain gains a column ts and
// a 7-day policy on it; then events drops seen_at with its policy, and plain
// drops its policy and keeps ts.
type policyVersions struct {
	// file names the file each version is written in: a schema file, or a
	// Go file in a directory the host scans.
	file string
	v1   string
	v2   string
	v3   string
}

var goPolicies = policyVersions{file: "entities.go",
	v1: goEntities(`platform.spanner.row_deletion_column="created_at" platform.spanner.row_deletion_interval="30 days"`, []string{"created_at"}, "", false),
	v2: goEntities(`platform.spanner.row_deletion_column="seen_at" platform.spanner.row_deletion_interval="60 days"`, []string{"created_at", "seen_at"},
		`platform.spanner.row_deletion_column="ts" platform.spanner.row_deletion_interval="7 days"`, true),
	v3: goEntities("", []string{"created_at"}, "", true),
}

// goEntities declares events with its directive's attributes and the
// timestamptz columns it names, and plain with its attributes and, when ts
// is set, a timestamptz column ts.
func goEntities(events string, columns []string, plain string, ts bool) string {
	var fields strings.Builder
	for _, column := range columns {
		fields.WriteString("\t//ptah:schema:field name=\"" + column + "\" type=\"TIMESTAMPTZ\"\n\tF" + column + " string\n")
	}
	plainFields := map[bool]string{true: "\t//ptah:schema:field name=\"ts\" type=\"TIMESTAMPTZ\"\n\tTS string\n"}[ts]
	return "package entities\n\n//ptah:schema:table name=\"events\" " + events + "\ntype Event struct {\n" +
		"\t//ptah:schema:field name=\"id\" type=\"BIGINT\" primary=\"true\"\n\tID int64\n" + fields.String() + "}\n\n" +
		"//ptah:schema:table name=\"plain\" " + plain + "\ntype Plain struct {\n\t//ptah:schema:field name=\"id\" type=\"BIGINT\" primary=\"true\"\n\tID int64\n" +
		plainFields + "}\n"
}

var yamlPolicies = policyVersions{file: "schema.yaml",
	v1: `tables:
  events:
    platform:
      spanner: {row_deletion_column: created_at, row_deletion_interval: 30 days}
    columns:
      id: {type: bigint, primary: true}
      created_at: {type: timestamptz}
  plain:
    columns:
      id: {type: bigint, primary: true}
`,
	v2: `tables:
  events:
    platform:
      spanner: {row_deletion_column: seen_at, row_deletion_interval: 60 days}
    columns:
      id: {type: bigint, primary: true}
      created_at: {type: timestamptz}
      seen_at: {type: timestamptz}
  plain:
    platform:
      spanner: {row_deletion_column: ts, row_deletion_interval: 7 days}
    columns:
      id: {type: bigint, primary: true}
      ts: {type: timestamptz}
`,
	v3: `tables:
  events:
    columns:
      id: {type: bigint, primary: true}
      created_at: {type: timestamptz}
  plain:
    columns:
      id: {type: bigint, primary: true}
      ts: {type: timestamptz}
`,
}

// The modes of policyApply: apply the plan, or print it.
const (
	apply  = "--auto-approve"
	dryRun = "--dry-run"
)

// policyApply runs `ptah schema apply` of a declaration in mode and returns
// what the host wrote and its error. A Go source is a directory the host
// scans; any other is a schema file.
func policyApply(c *qt.C, address, file, body, mode string) (string, error) {
	c.Helper()
	return hostCommand(c, append([]string{"schema", "apply", "--db-url", address, mode}, sourceArgs(c, file, body)...)...)
}

// sourceArgs writes body into a new directory under file and returns the
// flags that name it as the desired source.
func sourceArgs(c *qt.C, file, body string) []string {
	c.Helper()
	dir := writeFiles(c, map[string]string{file: body})
	if strings.HasSuffix(file, ".go") {
		return []string{"--root-dir", dir}
	}
	return []string{"--schema-file", filepath.Join(dir, file)}
}

// The policy is created with its table, converges against the server's
// spelling of the interval, moves to a column the same plan adds, is added to
// a kept table and dropped, each in place and in the built-in's order, from a
// Go source and from a YAML one. A read writes it back in HCL and in SQL with
// the stored interval, `schema stats` counts it, and a generated migration
// carries the change and its reverse with the built-in's recovery note. The
// SQL read needs a host with stokaro/ptah#5559 and runs last.
func TestHostRowDeletionPolicyLifecycleE2E(t *testing.T) {
	tests := []struct {
		name     string
		versions policyVersions
	}{
		{name: "Go", versions: goPolicies},
		{name: "YAML", versions: yamlPolicies},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			address := withSearchPath(c, freshDatabase(c))
			versions := test.versions
			output, err := policyApply(c, address, versions.file, versions.v1, apply)
			c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
			c.Assert(output, qt.Matches, `(?s).*CREATE TABLE "events" \(\n  "id" (?i:bigint) PRIMARY KEY NOT NULL,\n  "created_at" (?i:timestamptz)\n\) `+
				`TTL INTERVAL '30 days' ON "created_at";\n.*`)
			c.Assert(queryRows(c, address, `SELECT table_name, row_deletion_policy_expression FROM information_schema.tables
WHERE table_schema = 'public' AND table_name IN ('events', 'plain') ORDER BY table_name`), qt.DeepEquals,
				[]string{"events INTERVAL '4 WEEKS 2 DAYS' ON created_at", "plain <nil>"})
			output, err = policyApply(c, address, versions.file, versions.v1, dryRun)
			c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
			c.Assert(output, qt.Contains, synced)

			output, err = hostCommand(c, "schema", "inspect", "--db-url", address, "--format", "hcl")
			c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
			c.Assert(output, qt.Contains, "  platform \"spanner\" {\n    override \"row_deletion_column\" {\n      value = \"created_at\"\n    }\n"+
				"    override \"row_deletion_interval\" {\n      value = \"4 WEEKS 2 DAYS\"\n    }\n  }\n")
			output, err = hostCommand(c, "schema", "stats", "--db-url", address)
			c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
			c.Assert(output, qt.Contains, "ptah_schema_spanner_row_deletion_policy_tables{dialect=\"spanner\"} 1\n")

			migrations := c.TempDir()
			output, err = hostCommand(c, append([]string{"migrations", "generate", "--db-url", address, "--migrations-dir", migrations, "--name", "second"},
				sourceArgs(c, versions.file, versions.v2)...)...)
			c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
			down, err := filepath.Glob(filepath.Join(migrations, "*_second.down.sql"))
			c.Assert(err, qt.IsNil)
			c.Assert(down, qt.HasLen, 1)
			reverse, err := os.ReadFile(down[0])
			c.Assert(err, qt.IsNil)
			c.Assert(string(reverse), qt.Contains, `-- Recovery limit: "Restoring the prior row deletion policy cannot recover rows the server deleted under the forward policy."`+"\n")
			c.Assert(string(reverse), qt.Contains, "-- Row deletion policy on table: events\nALTER TABLE \"events\" ALTER TTL INTERVAL '4 WEEKS 2 DAYS' ON \"created_at\";\n"+
				"-- Row deletion policy on table: plain\nALTER TABLE \"plain\" DROP TTL;\n")

			output, err = policyApply(c, address, versions.file, versions.v2, apply)
			c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
			c.Assert(output, qt.Matches, `(?s).*ALTER TABLE "events" ADD COLUMN "seen_at" (?i:timestamptz);\nALTER TABLE "plain" ADD COLUMN "ts" (?i:timestamptz);\n`+
				`-- Row deletion policy on table: events\nALTER TABLE "events" ALTER TTL INTERVAL '60 days' ON "seen_at";\n`+
				`-- Row deletion policy on table: plain\nALTER TABLE "plain" ADD TTL INTERVAL '7 days' ON "ts";\n.*`)
			output, err = policyApply(c, address, versions.file, versions.v2, dryRun)
			c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
			c.Assert(output, qt.Contains, synced)

			output, err = policyApply(c, address, versions.file, versions.v3, apply)
			c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
			c.Assert(output, qt.Contains, "-- Row deletion policy on table: events\nALTER TABLE \"events\" DROP TTL;\n"+
				"-- Row deletion policy on table: plain\nALTER TABLE \"plain\" DROP TTL;\nALTER TABLE \"events\" DROP COLUMN \"seen_at\";\n")
			c.Assert(queryRows(c, address, `SELECT table_name, row_deletion_policy_expression FROM information_schema.tables
WHERE table_schema = 'public' AND table_name IN ('events', 'plain') ORDER BY table_name`), qt.DeepEquals, []string{"events <nil>", "plain <nil>"})
			output, err = policyApply(c, address, versions.file, versions.v3, dryRun)
			c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
			c.Assert(output, qt.Contains, synced)

			output, err = policyApply(c, address, versions.file, versions.v1, apply)
			c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
			output, err = hostCommand(c, "db", "read", "--db-url", address)
			skipOnOldHost(c, output, err, "has no schema rendering service", hostChange)
			c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
			c.Assert(output, qt.Contains, ") TTL INTERVAL '4 WEEKS 2 DAYS' ON \"created_at\";\n")
		})
	}
}

const hclPolicyTable = `schema "public" {}
table "events" {
  schema = schema.public
  column "id" {
    null = false
    type = bigint
  }
  column "created_at" {
    null = true
    type = timestamptz
  }
%s  primary_key {
    columns = [column.id]
  }
%s}
`

const hclSeenAt = `  column "seen_at" {
    null = true
    type = timestamptz
  }
`

// hclPolicy is the platform "spanner" block that declares a policy.
func hclPolicy(column, interval string) string {
	return "  platform \"spanner\" {\n    override \"row_deletion_column\" {\n      value = \"" + column + "\"\n    }\n" +
		"    override \"row_deletion_interval\" {\n      value = \"" + interval + "\"\n    }\n  }\n"
}

// An HCL table declares its policy in its platform "spanner" block, as the
// built-in read it, and a table that writes no block leaves the policy it
// holds alone. Dropping the column that policy counts from is refused before
// a statement is sent: the built-in planned the DROP COLUMN, which Spanner
// refused at apply (`ROW DELETION POLICY will break if drop/alter column
// named seen_at`), after the statements before it had run.
func TestHostRowDeletionPolicyInHCLE2E(t *testing.T) {
	c := testcheck.New(t)
	address := withSearchPath(c, freshDatabase(c))
	output, err := policyApply(c, address, "schema.hcl", sprintfHCL("", hclPolicy("created_at", "30 days")), apply)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, ") TTL INTERVAL '30 days' ON \"created_at\";\n")
	output, err = policyApply(c, address, "schema.hcl", sprintfHCL("", hclPolicy("created_at", "4 WEEKS 2 DAYS")), dryRun)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, synced)

	output, err = policyApply(c, address, "schema.hcl", sprintfHCL(hclSeenAt, hclPolicy("seen_at", "60 days")), apply)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, "ALTER TABLE \"events\" ADD COLUMN \"seen_at\" timestamptz;\n-- Row deletion policy on table: events\n"+
		"ALTER TABLE \"events\" ALTER TTL INTERVAL '60 days' ON \"seen_at\";\n")

	output, err = policyApply(c, address, "schema.hcl", sprintfHCL(hclSeenAt, ""), dryRun)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, synced)
	output, err = policyApply(c, address, "schema.hcl", sprintfHCL("", ""), apply)
	c.Assert(err, qt.IsNotNil)
	c.Assert(output, qt.Contains, `dropping column "seen_at" of table "events": the table's row deletion policy counts from it, and Spanner refuses to drop that column`)
	c.Assert(queryRows(c, address, `SELECT column_name FROM information_schema.columns WHERE table_name = 'events' ORDER BY ordinal_position`), qt.DeepEquals,
		[]string{"id", "created_at", "seen_at"})
}

// sprintfHCL fills the events table with the extra column and the platform
// block.
func sprintfHCL(columns, platform string) string {
	return strings.Replace(strings.Replace(hclPolicyTable, "%s", columns, 1), "%s", platform, 1)
}

// A policy Spanner refuses is refused before a statement is sent: a unit,
// which Spanner's clause has no place for, an interval that is not a whole
// number of days, and a column that is not timestamptz. A SQL document's TTL
// clause is refused by the host, which carries no provider's table clause yet
// (stokaro/ptah#4204 S42): the table is not created without its policy.
func TestHostRefusesRowDeletionPoliciesE2E(t *testing.T) {
	c := testcheck.New(t)
	address := withSearchPath(c, freshDatabase(c))
	const table = "tables:\n  events:\n    platform:\n      spanner: {%s}\n    columns:\n      id: {type: bigint, primary: true}\n      n: {type: bigint}\n      ts: {type: timestamptz}\n"
	for _, group := range []string{
		"row_deletion_column: ts, row_deletion_interval: 30 days, row_deletion_unit: seconds",
		"row_deletion_column: ts, row_deletion_interval: 36 hours",
	} {
		output, err := policyApply(c, address, "schema.yaml", strings.Replace(table, "%s", group, 1), dryRun)
		c.Assert(err, qt.IsNotNil, qt.Commentf("%s", group))
		c.Assert(output, qt.Contains, "provider decode source failed (InvalidArgument)", qt.Commentf("%s", group))
	}
	output, err := policyApply(c, address, "schema.yaml", strings.Replace(table, "%s", "row_deletion_column: n, row_deletion_interval: 30 days", 1), dryRun)
	c.Assert(err, qt.IsNotNil)
	c.Assert(output, qt.Contains, `the row deletion policy of table "events" counts from column "n", which is not timestamptz`)

	output, err = applySQL(c, address, "CREATE TABLE events (id bigint PRIMARY KEY, ts timestamptz) TTL INTERVAL '30 days' ON ts;\n")
	c.Assert(err, qt.IsNotNil)
	c.Assert(output, qt.Contains, "Table.Overrides")
	c.Assert(queryRows(c, address, `SELECT table_name FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'events'`), qt.HasLen, 0)
}

// rowsPolicy is a YAML declaration of events, a table with a key and the
// timestamptz columns named, and the platform group of its row deletion
// policy when the group is not empty.
func rowsPolicy(group string, columns ...string) string {
	var body strings.Builder
	body.WriteString("tables:\n  events:\n")
	if group != "" {
		body.WriteString("    platform:\n      spanner: {" + group + "}\n")
	}
	body.WriteString("    columns:\n      id: {type: bigint, primary: true}\n")
	for _, column := range columns {
		body.WriteString("      " + column + ": {type: timestamptz}\n")
	}
	return body.String()
}

// A policy walks through each change on a table holding rows: added, given
// another interval on the same column, moved to a column the same plan adds
// while the column it counted from is dropped, and removed. Each plan is the
// statements in the order Spanner takes them: ADD and ALTER are not
// interchangeable, a policy cannot name a column that does not exist yet, and
// a column a policy names cannot be dropped. Each step converges and the rows
// stay.
func TestHostChangesARowDeletionPolicyOnATableWithRowsE2E(t *testing.T) {
	c := testcheck.New(t)
	address := withSearchPath(c, freshDatabase(c))
	output, err := policyApply(c, address, "schema.yaml", rowsPolicy("", "created_at"), apply)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	execSQL(c, address, "INSERT INTO events (id, created_at) VALUES (1, NULL), (2, now())")

	steps := []struct {
		name, declared, plan, policy string
	}{
		{
			name: "a policy added", declared: rowsPolicy("row_deletion_column: created_at, row_deletion_interval: 30 days", "created_at"),
			plan:   "-- Row deletion policy on table: events\nALTER TABLE \"events\" ADD TTL INTERVAL '30 days' ON \"created_at\";\n",
			policy: "INTERVAL '4 WEEKS 2 DAYS' ON created_at",
		},
		{
			name: "another interval", declared: rowsPolicy("row_deletion_column: created_at, row_deletion_interval: 7 days", "created_at"),
			plan:   "-- Row deletion policy on table: events\nALTER TABLE \"events\" ALTER TTL INTERVAL '7 days' ON \"created_at\";\n",
			policy: "INTERVAL '7 DAYS' ON created_at",
		},
		{
			name: "another column, added in the same plan, and the old one dropped", declared: rowsPolicy("row_deletion_column: expires_at, row_deletion_interval: 7 days", "expires_at"),
			plan: "ALTER TABLE \"events\" ADD COLUMN \"expires_at\" timestamptz;\n-- Row deletion policy on table: events\n" +
				"ALTER TABLE \"events\" ALTER TTL INTERVAL '7 days' ON \"expires_at\";\nALTER TABLE \"events\" DROP COLUMN \"created_at\";\n",
			policy: "INTERVAL '7 DAYS' ON expires_at",
		},
		{
			name: "no policy", declared: rowsPolicy("", "expires_at"),
			plan:   "-- Row deletion policy on table: events\nALTER TABLE \"events\" DROP TTL;\n",
			policy: "<nil>",
		},
	}
	for _, step := range steps {
		output, err := policyApply(c, address, "schema.yaml", step.declared, apply)
		c.Assert(err, qt.IsNil, qt.Commentf("step %q: %s", step.name, output))
		c.Assert(output, qt.Contains, step.plan, qt.Commentf("step %q", step.name))
		c.Assert(queryRows(c, address, `SELECT row_deletion_policy_expression FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'events'`),
			qt.DeepEquals, []string{step.policy}, qt.Commentf("step %q", step.name))
		c.Assert(queryRows(c, address, `SELECT count(*) FROM events`), qt.DeepEquals, []string{"2"}, qt.Commentf("step %q", step.name))
		output, err = policyApply(c, address, "schema.yaml", step.declared, dryRun)
		c.Assert(err, qt.IsNil, qt.Commentf("step %q: %s", step.name, output))
		c.Assert(output, qt.Contains, synced, qt.Commentf("step %q", step.name))
	}
}
