//go:build integration

package integration_test

import (
	"context"
	"crypto/rand"
	"debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/jackc/pgx/v5"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	providerruntime "github.com/stokaro/ptah/provider-sdk/runtime"

	"github.com/stokaro/ptah-spanner-plugin/internal/backend"
)

// testURLVariable names the Spanner endpoint the live tests run against: the
// emulator behind PGAdapter, as spanner://host:port/database. Each test takes
// a database of its own on that endpoint, which the emulator creates on the
// first connection, so tests share no state.
const testURLVariable = "PTAH_SPANNER_TEST_URL"

// hostFeatures are the features startProvider's host negotiates.
func hostFeatures() []string {
	return []string{provider.ObservedColumnsFeature, provider.ObservedPrimaryKeysFeature, provider.ObservedIndexesFeature, provider.ErrorReasonsFeature}
}

func startProvider(c *qt.C) *providerruntime.Client {
	c.Helper()
	return startProviderWith(c, hostFeatures()...)
}

// startProviderWith starts the provider for a host that lists features.
func startProviderWith(c *qt.C, features ...string) *providerruntime.Client {
	c.Helper()
	binary, digest := providerBinary(c)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client, err := providerruntime.Start(ctx, providerruntime.Options{
		Executable: binary, SHA256: digest, ProviderID: backend.ProviderID, HostFeatures: features,
	})
	c.Assert(err, qt.IsNil)
	c.Cleanup(client.Close)
	return client
}

// startCommonObjectsProvider starts the provider for a host that also
// carries views and sequences on the common-object wire.
func startCommonObjectsProvider(c *qt.C) *providerruntime.Client {
	c.Helper()
	binary, digest := providerBinary(c)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client, err := providerruntime.Start(ctx, providerruntime.Options{
		Executable: binary, SHA256: digest, ProviderID: backend.ProviderID, HostFeatures: append(hostFeatures(), provider.CommonObjectsFeature),
	})
	c.Assert(err, qt.IsNil)
	c.Cleanup(client.Close)
	return client
}

// freshDatabase returns the address of a database no other test uses, on the
// endpoint testURLVariable names.
func freshDatabase(c *qt.C) string {
	c.Helper()
	base := os.Getenv(testURLVariable)
	c.Assert(base, qt.Not(qt.Equals), "", qt.Commentf("%s must name the Spanner emulator the live tests run against", testURLVariable))
	parsed, err := url.Parse(base)
	c.Assert(err, qt.IsNil)
	parsed.Path = "/t" + hex.EncodeToString([]byte(rand.Text())[:6])
	return parsed.String()
}

// withSearchPath pins the address to the public schema. A host that still
// builds in a Spanner engine reads an address that pins no search_path as a
// read of every schema, through its built-in engine's catalog, which a provider
// does not serve.
func withSearchPath(c *qt.C, address string) string {
	c.Helper()
	parsed, err := url.Parse(address)
	c.Assert(err, qt.IsNil)
	query := parsed.Query()
	query.Set("search_path", "public")
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

// execSQL runs setup statements on the database address names, each outside a
// transaction, as Spanner runs a schema change.
func execSQL(c *qt.C, address string, statements ...string) {
	c.Helper()
	conn := connect(c, address)
	for _, statement := range statements {
		_, err := conn.Exec(c.Context(), statement)
		c.Assert(err, qt.IsNil, qt.Commentf("statement: %s", statement))
	}
}

// connect opens a PostgreSQL-wire connection to the database address names,
// closed when the test ends.
func connect(c *qt.C, address string) *pgx.Conn {
	c.Helper()
	parsed, err := url.Parse(address)
	c.Assert(err, qt.IsNil)
	parsed.Scheme = "postgres"
	config, err := pgx.ParseConfig(parsed.String())
	c.Assert(err, qt.IsNil)
	config.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	conn, err := pgx.ConnectConfig(c.Context(), config)
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

// openSession opens a provider session on address.
func openSession(c *qt.C, client *providerruntime.Client, address string) *protocol.Session {
	c.Helper()
	session, err := client.Open(c.Context(), &protocol.OpenRequest{Engine: "spanner", Address: address})
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { _ = client.CloseSession(context.Background(), session.GetId()) })
	return session
}

// hostManifest returns the host binary the live job built and a provider
// manifest that selects this provider for spanner. The test is skipped without
// the host binary, which only the live job builds.
func hostManifest(c *qt.C) (host, manifest string) {
	c.Helper()
	host = os.Getenv("PTAH_HOST_TEST_BIN")
	if host == "" {
		c.Skip("PTAH_HOST_TEST_BIN names no host binary")
	}
	binary, digest := providerBinary(c)
	configuration := map[string]any{"format_version": 2, "providers": []any{map[string]any{
		"executable": binary, "sha256": digest, "provider_id": backend.ProviderID,
		"engines": []any{map[string]any{"id": "spanner", "aliases": []string{"cloudspanner", "google-spanner", "google_spanner"}}},
	}}}
	encoded, err := json.Marshal(configuration)
	c.Assert(err, qt.IsNil)
	manifest = filepath.Join(c.TempDir(), "providers.json")
	c.Assert(os.WriteFile(manifest, encoded, 0o600), qt.IsNil)
	return host, manifest
}

// hostCommand runs one command of the host with this provider selected for
// spanner, and returns what it wrote to both streams and its error.
func hostCommand(c *qt.C, args ...string) (string, error) {
	c.Helper()
	host, manifest := hostManifest(c)
	ctx, cancel := context.WithTimeout(c.Context(), 5*time.Minute)
	defer cancel()
	output, err := testCommand(ctx, host, append(args, "--provider-config", manifest, "--provider-engine", "spanner")...).CombinedOutput()
	return string(output), err
}

// testCommand is the process that runs binary with args, killed when ctx ends.
func testCommand(ctx context.Context, binary string, args ...string) *exec.Cmd {
	// #nosec G702 -- binary is the host the test environment names or the go tool, and args are the test's own flags, fixture paths and database addresses; exec passes them as argv, not through a shell
	return exec.CommandContext(ctx, binary, args...)
}

// writeFiles writes each body into a new directory under its name and returns
// the directory.
func writeFiles(c *qt.C, files map[string]string) string {
	c.Helper()
	dir := c.TempDir()
	for name, body := range files {
		c.Assert(os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600), qt.IsNil)
	}
	return dir
}

// applySQL runs `ptah schema apply` of the SQL schema document schema on
// address, approving it, and returns what the host wrote and its error.
func applySQL(c *qt.C, address, schema string) (string, error) {
	c.Helper()
	path := filepath.Join(writeFiles(c, map[string]string{"schema.sql": schema}), "schema.sql")
	return hostCommand(c, "schema", "apply", "--db-url", address, "--schema-file", path, "--auto-approve")
}

// planSQL runs `ptah schema apply --dry-run` of the SQL schema document
// schema on address and returns what the host wrote and its error.
func planSQL(c *qt.C, address, schema string) (string, error) {
	c.Helper()
	path := filepath.Join(writeFiles(c, map[string]string{"schema.sql": schema}), "schema.sql")
	return hostCommand(c, "schema", "apply", "--db-url", address, "--schema-file", path, "--dry-run")
}

// skipOnOldHost skips the test when the host answered with refusal, the words
// of a host that lacks change, which the test needs. Any other error fails
// later, at the test's own assertion, so a host with the change cannot pass by
// skipping.
func skipOnOldHost(c *qt.C, output string, err error, refusal, change string) {
	c.Helper()
	if err != nil && strings.Contains(output, refusal) {
		c.Skipf("the host predates %s: %s", change, refusal)
	}
}

// skipBeforeConstraints skips the test when the host speaks a contract older
// than 25.28, which carries a table's foreign keys and CHECKs, their changes,
// CHECK equivalence and index NULLS orders: the host pins a provider-sdk
// older than v0.25.0. A host of 25.27 reads no constraint, so the provider
// refuses the read of a table that holds one, as it did before 25.28. The
// release leg runs such a host until a Ptah release speaks 25.28.
func skipBeforeConstraints(c *qt.C) {
	c.Helper()
	host, _ := hostManifest(c)
	info, err := buildinfo.ReadFile(host)
	c.Assert(err, qt.IsNil)
	for _, dependency := range info.Deps {
		if dependency.Path != "github.com/stokaro/ptah/provider-sdk" || dependency.Replace != nil {
			continue
		}
		var major, minor int
		if _, err := fmt.Sscanf(dependency.Version, "v%d.%d.", &major, &minor); err == nil && major == 0 && minor < 25 {
			c.Skipf("the host pins provider-sdk %s, which predates contract 25.28", dependency.Version)
		}
	}
}

// queryRows runs a catalog query on the database address names and returns
// each row as its fields' text joined by spaces.
func queryRows(c *qt.C, address, query string) []string {
	c.Helper()
	rows, err := connect(c, address).Query(c.Context(), query)
	c.Assert(err, qt.IsNil)
	defer rows.Close()
	var result []string
	for rows.Next() {
		values, err := rows.Values()
		c.Assert(err, qt.IsNil)
		fields := make([]string, len(values))
		for i, value := range values {
			fields[i] = fmt.Sprint(value)
		}
		result = append(result, strings.Join(fields, " "))
	}
	c.Assert(rows.Err(), qt.IsNil)
	return result
}

// synced is what `ptah schema apply` writes when the database already holds
// the declaration.
const synced = "Schema is synced, no changes to be made."
