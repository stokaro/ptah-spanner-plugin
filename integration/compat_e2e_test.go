//go:build integration

package integration_test

import (
	"context"
	"os"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// compatRefusal is the words of a ptah-compat that builds in a Spanner engine
// and refuses a provider registration for it (`invalid provider registration:
// target name "cloudspanner" is claimed by "ptah.run/spanner" and ...`).
const compatRefusal = `is claimed by "ptah.run/spanner"`

// compatChange names the host change that lets ptah-compat hand a Spanner
// address to this provider.
const compatChange = "stokaro/ptah#5657"

// compatCommand runs one command of the ptah-compat binary the live job built
// beside the host, with this provider's manifest named the way the
// compatibility surface takes it, PTAH_PROVIDER_CONFIG, since it registers no
// provider flag. It returns what the command wrote to both streams and its
// error. The test is skipped without the binary, which only the live job
// builds.
func compatCommand(c *qt.C, args ...string) (string, error) {
	c.Helper()
	compat := os.Getenv("PTAH_HOST_COMPAT_TEST_BIN")
	if compat == "" {
		c.Skip("PTAH_HOST_COMPAT_TEST_BIN names no ptah-compat binary")
	}
	_, manifest := hostManifest(c)
	ctx, cancel := context.WithTimeout(c.Context(), 5*time.Minute)
	defer cancel()
	command := testCommand(ctx, compat, args...)
	command.Env = append(os.Environ(), "PTAH_PROVIDER_CONFIG="+manifest)
	output, err := command.CombinedOutput()
	return string(output), err
}

// `ptah-compat schema inspect` of an address that pins no search_path reads
// the database's schemas through the provider and writes the user's schema,
// public, and not spanner_sys, the server's own: listed, it made an empty
// Spanner dev database read as not clean, and the inspection wrote a block
// for it.
func TestCompatSchemaInspectLeavesSpannerSysOutE2E(t *testing.T) {
	c := testcheck.New(t)
	address := freshDatabase(c)
	execSQL(c, address, "CREATE TABLE t (id bigint PRIMARY KEY)")
	output, err := compatCommand(c, "schema", "inspect", "-u", address)
	skipOnOldHost(c, output, err, compatRefusal, compatChange)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, `schema "public"`)
	c.Assert(output, qt.Contains, `table "t"`)
	c.Assert(output, qt.Not(qt.Contains), "spanner_sys")
}
