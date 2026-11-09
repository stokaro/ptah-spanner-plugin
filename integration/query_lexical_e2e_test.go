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

// skipBeforeQueryLexical skips the test when the host speaks a contract older
// than 25.34, which reads an engine's text by the lexical rules its query
// dialect states (query.lexical): the host pins a provider-sdk older than
// provider-sdk/v0.32.0. Such a host reads Spanner's schema documents by the
// SQL standard alone (stokaro/ptah#5754). The decision reads the host binary,
// not its answer, so a provider that fails to state the rules fails the test
// on a host that speaks 25.34 instead of skipping it.
func skipBeforeQueryLexical(c *qt.C) {
	c.Helper()
	host, _ := hostManifest(c)
	info, err := buildinfo.ReadFile(host)
	c.Assert(err, qt.IsNil)
	for _, dependency := range info.Deps {
		if !slices.Contains(providerSDKModules, dependency.Path) || dependency.Replace != nil {
			continue
		}
		var major, minor int
		if _, err := fmt.Sscanf(dependency.Version, "v%d.%d.", &major, &minor); err == nil && major == 0 && minor < 32 {
			c.Skipf("the host pins provider-sdk %s, which predates contract 25.34", dependency.Version)
		}
	}
}

// A SQL schema file holds a nested block comment with a semicolon after its
// inner comment, and a column default written as an escape string. The host
// reads the file by the lexical rules the provider states, so the comment is
// one comment, and hands the default to the provider in the standard
// single-quoted spelling of it's, which Spanner stores as that text cast to
// text, and the next plan is synced. A host that reads the file by the SQL
// standard alone refuses it (unsupported column attribute: S), and one that
// hands the default over as written has the provider store the text
// E'it\'s' (stokaro/ptah#5754).
func TestHostReadsASpannerSchemaFileByItsLexicalRulesE2E(t *testing.T) {
	c := testcheck.New(t)
	skipBeforeQueryLexical(c)
	address := withSearchPath(c, freshDatabase(c))
	path := filepath.Join(writeFiles(c, map[string]string{"schema.sql": "/* a /* nested */ it's; a comment */\n" +
		"CREATE TABLE notes (id bigint NOT NULL, note varchar DEFAULT E'it\\'s', PRIMARY KEY (id));\n"}), "schema.sql")

	output, err := hostCommand(c, "schema", "apply", "--db-url", address, "--schema-file", path, "--auto-approve")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	again, err := hostCommand(c, "schema", "apply", "--db-url", address, "--schema-file", path, "--dry-run")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", again))

	c.Assert(again, qt.Contains, synced)
	c.Assert(queryRows(c, address, `SELECT column_default FROM information_schema.columns WHERE table_name = 'notes' AND column_name = 'note'`),
		qt.DeepEquals, []string{"'it''s'::text"})
}
