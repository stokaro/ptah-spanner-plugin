//go:build integration

package integration_test

import (
	"debug/buildinfo"
	"fmt"
	"path/filepath"
	"slices"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// skipBeforeEnumColumns skips the test when the host speaks a contract older
// than 25.31, which builds an enum column in the form the engine states
// (schema.enum-columns): the host pins a provider-sdk older than the commit
// that added it, a0dd6e40e, tagged provider-sdk/v0.29.0. Such a host keeps the
// enum a type of its own and refuses it as `SchemaDiff.EnumsAdded`
// (stokaro/ptah#5658, S84 of stokaro/ptah#4204). The decision reads the host
// binary, not its answer, so a provider that fails to state the form fails
// the test on a host that speaks 25.31 instead of skipping it.
func skipBeforeEnumColumns(c *qt.C) {
	c.Helper()
	host, _ := hostManifest(c)
	info, err := buildinfo.ReadFile(host)
	c.Assert(err, qt.IsNil)
	for _, dependency := range info.Deps {
		if !slices.Contains(providerSDKModules, dependency.Path) || dependency.Replace != nil {
			continue
		}
		var major, minor int
		if _, err := fmt.Sscanf(dependency.Version, "v%d.%d.", &major, &minor); err == nil && major == 0 && (minor < 28 || dependency.Version == "v0.28.0") {
			c.Skipf("the host pins provider-sdk %s, which predates contract 25.31", dependency.Version)
		}
	}
}

// valuedEnumTable is a YAML schema document of table t with a nullable enum
// column s and a NOT NULL enum column n, each carrying its values; one of s's
// values holds a quote.
const valuedEnumTable = `tables:
  t:
    columns:
      id: {type: bigint, primary: true}
      s: {type: ENUM, enum: [draft, "it's"]}
      n: {type: ENUM, enum: [a, b], nullable: false}
`

// declaredEnumTable is valuedEnumTable with s typed by a declared enum.
const declaredEnumTable = `enums:
  t_status: [draft, "it's"]
tables:
  t:
    columns:
      id: {type: bigint, primary: true}
      s: {type: t_status}
      n: {type: ENUM, enum: [a, b], nullable: false}
`

// An enum column, typed by a declared enum or carrying its values, is created
// as TEXT under a CHECK named `<table>_<column>_check` that lists its values,
// as Ptah's built-in Spanner engine created it; a quote in a value is doubled.
// The next plan is synced, and Spanner refuses a value the CHECK does not
// list. The host tests TestInlineEnumCreateConvergesLive/spanner declared enum
// and /spanner values on the column (stokaro/ptah-spanner-plugin#16).
func TestHostCreatesEnumColumnsE2E(t *testing.T) {
	tests := []struct{ name, schema string }{
		{name: "a declared enum", schema: declaredEnumTable},
		{name: "values on the column", schema: valuedEnumTable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			skipBeforeEnumColumns(c)
			address := withSearchPath(c, freshDatabase(c))
			path := filepath.Join(writeFiles(c, map[string]string{"schema.yaml": test.schema}), "schema.yaml")
			output, err := hostCommand(c, "schema", "apply", "--db-url", address, "--schema-file", path, "--auto-approve")
			c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
			c.Assert(output, qt.Contains, `"s" TEXT CONSTRAINT "t_s_check" CHECK (s IN ('draft', 'it''s')),`)
			c.Assert(output, qt.Contains, `"n" TEXT NOT NULL CONSTRAINT "t_n_check" CHECK (n IN ('a', 'b'))`)
			output, err = hostCommand(c, "schema", "apply", "--db-url", address, "--schema-file", path, "--dry-run")
			c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
			c.Assert(output, qt.Contains, synced)
			_, err = connect(c, address).Exec(c.Context(), "INSERT INTO t (id, s, n) VALUES (1, 'draft', 'c')")
			c.Assert(err, qt.ErrorMatches, `.*Check constraint .*t_n_check.* is violated.*`)
		})
	}
}

// Enum columns added to a kept table are added without their CHECK, which
// Spanner refuses inside ADD COLUMN, and the CHECK follows each as a
// constraint of its own, named `<table>_<column>_check`; the next plan is
// synced. The host test TestInlineEnumAddColumnConvergesLive/spanner
// (stokaro/ptah-spanner-plugin#16).
func TestHostAddsEnumColumnsToAKeptTableE2E(t *testing.T) {
	c := testcheck.New(t)
	skipBeforeEnumColumns(c)
	address := withSearchPath(c, freshDatabase(c))
	output, err := applySQL(c, address, "CREATE TABLE t (id bigint NOT NULL, PRIMARY KEY (id));\n")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	declared := "enums:\n  t_status: [draft, \"it's\"]\ntables:\n  t:\n    columns:\n      id: {type: bigint, primary: true}\n" +
		"      s: {type: t_status}\n      n: {type: ENUM, enum: [a, b]}\n"
	path := filepath.Join(writeFiles(c, map[string]string{"schema.yaml": declared}), "schema.yaml")
	output, err = hostCommand(c, "schema", "apply", "--db-url", address, "--schema-file", path, "--auto-approve")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	for _, statement := range []string{
		`ALTER TABLE "t" ADD COLUMN "s" TEXT;`,
		`ALTER TABLE "t" ADD COLUMN "n" TEXT;`,
		`ALTER TABLE "t" ADD CONSTRAINT "t_s_check" CHECK (s IN ('draft', 'it''s'));`,
		`ALTER TABLE "t" ADD CONSTRAINT "t_n_check" CHECK (n IN ('a', 'b'));`,
	} {
		c.Assert(output, qt.Contains, statement)
	}
	output, err = hostCommand(c, "schema", "apply", "--db-url", address, "--schema-file", path, "--dry-run")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, synced)
}

// skipBeforeEnumCheckDecisions skips the test on a host that may compare the
// CHECK of an enum column by its own reading of the two texts: one that asks
// the provider (schema.check-equivalence) about the declared CHECKs before it
// builds the enum columns, so the CHECK an enum column is built under never
// reaches the provider, and the host reads Spanner's `= ANY (ARRAY[...])`
// against the declared IN list as a rewrite it cannot judge and plans nothing
// (stokaro/ptah#5791, S81 of stokaro/ptah#4204). The change carries no
// contract feature, so the decision reads the provider-sdk version the host
// pins: every Ptah release that predates the change, up to v0.23.0, pins
// provider-sdk v0.30.0 or older. A host master between that pin and the change
// fails the test, which only a master leg builds.
func skipBeforeEnumCheckDecisions(c *qt.C) {
	c.Helper()
	host, _ := hostManifest(c)
	info, err := buildinfo.ReadFile(host)
	c.Assert(err, qt.IsNil)
	for _, dependency := range info.Deps {
		if !slices.Contains(providerSDKModules, dependency.Path) || dependency.Replace != nil {
			continue
		}
		var major, minor, patch int
		if _, err := fmt.Sscanf(dependency.Version, "v%d.%d.%d", &major, &minor, &patch); err == nil && major == 0 && (minor < 30 || minor == 30 && patch == 0) {
			c.Skipf("the host pins provider-sdk %s: it may ask about a CHECK before it builds the enum columns", dependency.Version)
		}
	}
}

// enumValues is a YAML schema document of table t with the enum column k of
// the values listed.
func enumValues(values string) string {
	return "tables:\n  t:\n    columns:\n      id: {type: bigint, primary: true}\n      k: {type: ENUM, enum: [" + values + "]}\n"
}

// A changed list of an enum column's values replaces the column's CHECK. The
// host asks the provider whether the CHECK Spanner keeps,
// `(k = ANY (ARRAY['a'::text, 'b'::text]))`, is the declared one, and the
// provider says it is not, so the CHECK is dropped and added under its name,
// without IF EXISTS; the next plan is synced, and Spanner takes a value of the
// new list and refuses one outside it. Ptah's built-in Spanner engine planned
// nothing for the change, and neither did a host that asked about the CHECKs
// before it built the enum columns (stokaro/ptah#4204 S81).
func TestHostReplacesTheCheckOfAnEnumColumnWhoseValuesChangeE2E(t *testing.T) {
	tests := []struct{ name, values, check, accepted, refused string }{
		{name: "a value added", values: "a, b, c", check: "k IN ('a', 'b', 'c')", accepted: "c", refused: "d"},
		{name: "a value removed", values: "a", check: "k IN ('a')", accepted: "a", refused: "b"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			skipBeforeEnumColumns(c)
			skipBeforeEnumCheckDecisions(c)
			address := withSearchPath(c, freshDatabase(c))
			held := filepath.Join(writeFiles(c, map[string]string{"schema.yaml": enumValues("a, b")}), "schema.yaml")
			output, err := hostCommand(c, "schema", "apply", "--db-url", address, "--schema-file", held, "--auto-approve")
			c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
			declared := filepath.Join(writeFiles(c, map[string]string{"schema.yaml": enumValues(test.values)}), "schema.yaml")
			output, err = hostCommand(c, "schema", "apply", "--db-url", address, "--schema-file", declared, "--auto-approve")
			c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
			c.Assert(output, qt.Contains, "ALTER TABLE \"t\" DROP CONSTRAINT \"t_k_check\";\n"+
				"ALTER TABLE \"t\" ADD CONSTRAINT \"t_k_check\" CHECK ("+test.check+");\n")
			output, err = hostCommand(c, "schema", "apply", "--db-url", address, "--schema-file", declared, "--dry-run")
			c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
			c.Assert(output, qt.Contains, synced)
			conn := connect(c, address)
			_, err = conn.Exec(c.Context(), "INSERT INTO t (id, k) VALUES (1, '"+test.accepted+"')")
			c.Assert(err, qt.IsNil)
			_, err = conn.Exec(c.Context(), "INSERT INTO t (id, k) VALUES (2, '"+test.refused+"')")
			c.Assert(err, qt.ErrorMatches, `.*Check constraint .*t_k_check.* is violated.*`)
		})
	}
}
