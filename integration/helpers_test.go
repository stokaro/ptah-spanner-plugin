//go:build integration

package integration_test

import (
	"context"
	"crypto/rand"
	"debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
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

// testInstanceVariable names the emulator's REST address of the instance
// behind testURLVariable, such as
// http://127.0.0.1:9020/v1/projects/emulator-project/instances/test-instance.
// The instance holds at most 100 databases, and a run of the suite takes
// nearly all of them, so where the variable is set freshDatabase drops each
// database when its test ends. Without it, every database stays until the
// emulator stops.
const testInstanceVariable = "PTAH_SPANNER_TEST_INSTANCE_URL"

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
// endpoint testURLVariable names. Where testInstanceVariable is set, the
// database is dropped when the test ends, after the cleanups registered later,
// such as a connection's.
func freshDatabase(c *qt.C) string {
	c.Helper()
	base := os.Getenv(testURLVariable)
	c.Assert(base, qt.Not(qt.Equals), "", qt.Commentf("%s must name the Spanner emulator the live tests run against", testURLVariable))
	parsed, err := url.Parse(base)
	c.Assert(err, qt.IsNil)
	name := "t" + hex.EncodeToString([]byte(rand.Text())[:6])
	parsed.Path = "/" + name
	if instance := os.Getenv(testInstanceVariable); instance != "" {
		c.Cleanup(func() { dropDatabase(c, instance, name) })
	}
	return parsed.String()
}

// dropDatabase drops the database name of the emulator instance at the REST
// address instance. A database nothing connected to was never created, so
// the emulator does not find it, which is not a failure. It runs as a
// cleanup, so it reports a failure and returns rather than stopping. The
// name is never used again: PGAdapter keeps its sessions of a dropped
// database, and a connection to the name would reach them.
func dropDatabase(c *qt.C, instance, name string) {
	c.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// #nosec G704 -- instance is the emulator's REST address the test environment names, and name a database this test generated
	request, err := http.NewRequestWithContext(ctx, http.MethodDelete, instance+"/databases/"+name, nil)
	if !c.Check(err, qt.IsNil) {
		return
	}
	// #nosec G704 -- the request goes to the emulator the test environment names; see above
	response, err := (&http.Client{Timeout: 30 * time.Second}).Do(request)
	if !c.Check(err, qt.IsNil, qt.Commentf("dropping database %s", name)) {
		return
	}
	defer func() { _ = response.Body.Close() }()
	c.Check([]int{http.StatusOK, http.StatusNotFound}, qt.Contains, response.StatusCode,
		qt.Commentf("dropping database %s answered %s", name, response.Status))
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

// statementFactsCommit is the commit time, as a pseudo-version writes it, of
// stokaro/ptah 64dc845e9, which added contract 25.33 to provider-sdk. A
// provider-sdk pseudo-version on v0.30.0 at or after it speaks 25.33, as does
// provider-sdk/v0.31.0 and every later release.
const statementFactsCommit = "20261107163439"

// hostPredatesStatementFacts reports whether the host pins a provider-sdk
// older than contract 25.33, whose source.sql-statement-facts lets the host
// read a SQL schema document as the provider's engine runs it. A host that
// replaces the module builds it from its own tree and is not older.
func hostPredatesStatementFacts(c *qt.C) bool {
	c.Helper()
	host, _ := hostManifest(c)
	info, err := buildinfo.ReadFile(host)
	c.Assert(err, qt.IsNil)
	for _, dependency := range info.Deps {
		if dependency.Path != "github.com/stokaro/ptah/provider-sdk" || dependency.Replace != nil {
			continue
		}
		var major, minor, patch int
		if _, err := fmt.Sscanf(dependency.Version, "v%d.%d.%d", &major, &minor, &patch); err != nil || major != 0 {
			return false
		}
		_, pseudo, _ := strings.Cut(dependency.Version, "-0.")
		return minor < 30 || minor == 30 && (patch == 0 || pseudo < statementFactsCommit)
	}
	return false
}

// viewQueryContextCommit is the commit time, as a pseudo-version writes it, of
// stokaro/ptah a09ccee4d, which added contract 25.37 to provider-sdk. A
// provider-sdk pseudo-version on v0.34.1 at or after it speaks 25.37, as does
// every release after provider-sdk/v0.34.0.
const viewQueryContextCommit = "20261107221847"

// hostViewQueries is how the host compares a view's query on this engine:
// "text" for a host older than contract 25.33, which compares the texts by
// its own reading; "provider" for a host of 25.33 to 25.36, which asks the
// provider (schema.view-query-equivalence) without the declared columns; and
// "context" for a host of 25.37 or later, which sends the columns and reads a
// pair the provider leaves undecided by its own reading
// (schema.view-query-context).
func hostViewQueries(c *qt.C) string {
	c.Helper()
	if hostPredatesStatementFacts(c) {
		return "text"
	}
	host, _ := hostManifest(c)
	info, err := buildinfo.ReadFile(host)
	c.Assert(err, qt.IsNil)
	for _, dependency := range info.Deps {
		if dependency.Path != "github.com/stokaro/ptah/provider-sdk" || dependency.Replace != nil {
			continue
		}
		var major, minor, patch int
		if _, err := fmt.Sscanf(dependency.Version, "v%d.%d.%d", &major, &minor, &patch); err != nil || major != 0 {
			return "context"
		}
		_, pseudo, _ := strings.Cut(dependency.Version, "-0.")
		if minor < 34 || minor == 34 && (patch == 0 || pseudo < viewQueryContextCommit) {
			return "provider"
		}
	}
	return "context"
}

// removalBlockersCommit is the commit time, as a pseudo-version writes it, of
// stokaro/ptah 939402c22, which added contract 25.39 to provider-sdk. A
// provider-sdk pseudo-version on v0.36.0 at or after it speaks 25.39, as does
// every release after provider-sdk/v0.36.0.
const removalBlockersCommit = "20261108033205"

// skipBeforeRemovalBlockers skips the test when the host pins a provider-sdk
// older than contract 25.39, whose schema.removal-blockers carries the change
// streams a read reports to the table service: such a host plans a drop a
// stream names, and the executor refuses it when it runs. A host that
// replaces the module builds it from its own tree and is not older.
func skipBeforeRemovalBlockers(c *qt.C) {
	c.Helper()
	host, _ := hostManifest(c)
	info, err := buildinfo.ReadFile(host)
	c.Assert(err, qt.IsNil)
	for _, dependency := range info.Deps {
		if dependency.Path != "github.com/stokaro/ptah/provider-sdk" || dependency.Replace != nil {
			continue
		}
		var major, minor, patch int
		if _, err := fmt.Sscanf(dependency.Version, "v%d.%d.%d", &major, &minor, &patch); err != nil || major != 0 {
			return
		}
		_, pseudo, _ := strings.Cut(dependency.Version, "-0.")
		if minor < 36 || minor == 36 && (patch == 0 || pseudo < removalBlockersCommit) {
			c.Skipf("the host pins provider-sdk %s, which predates contract 25.39", dependency.Version)
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
