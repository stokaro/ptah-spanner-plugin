//go:build integration

package integration_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// Each test here runs Ptah's migration commands through the provider against
// the emulator. The revision tables, the rows they hold and the checksums are
// the ones Ptah's built-in Spanner engine wrote for the same directories,
// measured at host d59a3b63d on the emulator behind PGAdapter 0.56.2; where
// the built-in failed, the test says what the provider does instead.

// migrations are two migrations: one that creates a table and an index, and
// one that creates a table and writes a row.
var migrations = map[string]string{
	"1700000001_users.up.sql":   "CREATE TABLE users (\n  id bigint NOT NULL,\n  name varchar(100) NOT NULL,\n  PRIMARY KEY (id)\n);\nCREATE INDEX users_name ON users (name);\n",
	"1700000001_users.down.sql": "DROP INDEX users_name;\nDROP TABLE users;\n",
	"1700000002_posts.up.sql":   "CREATE TABLE posts (\n  id bigint NOT NULL,\n  author_id bigint NOT NULL,\n  title varchar(200),\n  PRIMARY KEY (id)\n);\nINSERT INTO users (id, name) VALUES (1, 'ann');\n",
	"1700000002_posts.down.sql": "DELETE FROM users WHERE id = 1;\nDROP TABLE posts;\n",
}

// The checksums the built-in engine recorded for the two migrations.
const (
	usersChecksum = "f19070269412ff55b1723199f8c939376ad0137ed186330393d94c808e62d271"
	postsChecksum = "624156e0ef2834447b3ab24844cc0e6d7daaa8dd6e26d43d6fde4dd9e915702e"
)

// migrate runs one `ptah migrations` command through the provider.
func migrate(c *qt.C, args ...string) (string, error) {
	c.Helper()
	return hostCommand(c, append([]string{"migrations"}, args...)...)
}

// revisions reads the native revision table's rows in version order.
func revisions(c *qt.C, address string) []string {
	c.Helper()
	return queryRows(c, address, `SELECT version, description, state, applied, total, error IS NULL, error_stmt IS NULL, checksum
FROM schema_migrations ORDER BY version`)
}

// tables lists the base tables of the default schema.
func tables(c *qt.C, address string) []string {
	c.Helper()
	return queryRows(c, address, `SELECT table_name FROM information_schema.tables WHERE table_schema = 'public' AND table_type = 'BASE TABLE' ORDER BY table_name`)
}

// Migrations apply and roll back through the provider and keep their records
// in the built-in engine's revision table and log, column for column. A body
// runs statement by statement, each committing as it runs, and its rows
// commit with it. --tx-mode all is refused, as the built-in refused it:
// Spanner commits a schema change as it runs.
func TestHostMigratesUpAndDownE2E(t *testing.T) {
	c := testcheck.New(t)
	address := withSearchPath(c, freshDatabase(c))
	dir := writeFiles(c, migrations)

	output, err := migrate(c, "status", "--db-url", address, "--migrations-dir", dir)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, "Pending Migrations: 2")
	c.Assert(tables(c, address), qt.HasLen, 0)

	output, err = migrate(c, "up", "--db-url", address, "--migrations-dir", dir, "--tx-mode", "all")
	c.Assert(err, qt.ErrorMatches, "exit status 2")
	c.Assert(output, qt.Contains, `tx-mode all is not supported for dialect "spanner": this target commits schema changes as they run`)

	output, err = migrate(c, "up", "--db-url", address, "--migrations-dir", dir)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(revisions(c, address), qt.DeepEquals, []string{
		"1700000001 Users applied 2 2 true true " + usersChecksum,
		"1700000002 Posts applied 2 2 true true " + postsChecksum,
	})
	c.Assert(queryRows(c, address, `SELECT column_name, spanner_type, is_nullable FROM information_schema.columns
WHERE table_schema = 'public' AND table_name = 'schema_migrations' ORDER BY ordinal_position`), qt.DeepEquals, []string{
		"version bigint NO", "description character varying NO", "applied_at timestamp with time zone NO", "state character varying(32) NO",
		"applied bigint NO", "total bigint NO", "error character varying YES", "error_stmt character varying YES", "execution_time_ms bigint NO",
		"checksum character varying(64) NO",
	})
	c.Assert(queryRows(c, address, `SELECT operation, version, state FROM schema_migrations_log ORDER BY run_id, seq`), qt.DeepEquals, []string{
		"up 1700000001 started", "up 1700000001 applied", "up 1700000002 started", "up 1700000002 applied",
	})
	c.Assert(queryRows(c, address, `SELECT name FROM users`), qt.DeepEquals, []string{"ann"})

	output, err = migrate(c, "status", "--db-url", address, "--migrations-dir", dir)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, "Current Version: 1700000002")
	c.Assert(output, qt.Contains, "Pending Migrations: 0")

	output, err = migrate(c, "down", "--db-url", address, "--migrations-dir", dir, "--target", "1700000001", "--confirm")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(revisions(c, address), qt.DeepEquals, []string{"1700000001 Users applied 2 2 true true " + usersChecksum})
	c.Assert(queryRows(c, address, `SELECT count(*) FROM users`), qt.DeepEquals, []string{"0"})

	output, err = migrate(c, "down", "--db-url", address, "--migrations-dir", dir, "--target", "0", "--confirm")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(revisions(c, address), qt.HasLen, 0)
	c.Assert(tables(c, address), qt.DeepEquals, []string{"ptah_locks", "schema_migrations", "schema_migrations_log"})
}

// A migration that fails part way keeps the statements that ran, and its
// revision row records that prefix with the partial checksum the built-in
// engine recorded for the same body, so a retry resumes over the same digest.
// The record names the server's SQLSTATE. A rollback that fails is recorded
// in the direction it ran, as `failed:down`.
func TestHostRecordsAFailedMigrationE2E(t *testing.T) {
	c := testcheck.New(t)
	address := withSearchPath(c, freshDatabase(c))
	failing := map[string]string{
		"1700000003_bad.up.sql":   "CREATE TABLE tags (\n  id bigint NOT NULL,\n  PRIMARY KEY (id)\n);\nALTER TABLE tags ADD COLUMN x nosuchtype;\nCREATE TABLE never (\n  id bigint NOT NULL,\n  PRIMARY KEY (id)\n);\n",
		"1700000003_bad.down.sql": "DROP TABLE tags;\n",
	}
	dir := writeFiles(c, failing)
	output, err := migrate(c, "up", "--db-url", address, "--migrations-dir", dir)
	c.Assert(err, qt.ErrorMatches, "exit status 2")
	c.Assert(output, qt.Contains, "failed to apply migration 1700000003")
	c.Assert(queryRows(c, address, `SELECT version, state, applied, total, error_stmt, checksum FROM schema_migrations`), qt.DeepEquals,
		[]string{"1700000003 failed 1 3 ALTER TABLE tags ADD COLUMN x nosuchtype partial:h1:O12xwzQ1zs+7YyBpfm75BcWkrpFQ5KP/1EPLoW2skj0="})
	c.Assert(queryRows(c, address, `SELECT error FROM schema_migrations`)[0], qt.Matches, `(?s).*Spanner refused unit 1 of 1 with SQLSTATE P0001.*`)
	c.Assert(tables(c, address), qt.DeepEquals, []string{"ptah_locks", "schema_migrations", "schema_migrations_log", "tags"})

	output, err = migrate(c, "status", "--db-url", address, "--migrations-dir", dir)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, "Dirty Migration: version=1700000003 state=failed direction=up applied=1/3")

	rollback := writeFiles(c, map[string]string{
		"1700000001_two.up.sql":   "CREATE TABLE a (id bigint NOT NULL, PRIMARY KEY (id));\nCREATE TABLE b (id bigint NOT NULL, PRIMARY KEY (id));\n",
		"1700000001_two.down.sql": "DROP TABLE b;\nDROP TABLE nosuch;\nDROP TABLE a;\n",
	})
	other := withSearchPath(c, freshDatabase(c))
	output, err = migrate(c, "up", "--db-url", other, "--migrations-dir", rollback)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	output, err = migrate(c, "down", "--db-url", other, "--migrations-dir", rollback, "--target", "0", "--confirm")
	c.Assert(err, qt.ErrorMatches, "exit status 2", qt.Commentf("%s", output))
	c.Assert(queryRows(c, other, `SELECT version, state, applied, total, error_stmt, checksum FROM schema_migrations`), qt.DeepEquals,
		[]string{"1700000001 failed:down 1 3 DROP TABLE nosuch partial:h1:nVfaVNhzhABPZCoviR1gBh8xd5cjEH7SUsLe9QHjPBw="})
	output, err = migrate(c, "status", "--db-url", other, "--migrations-dir", rollback)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, "Dirty Migration: version=1700000001 state=failed direction=down applied=1/3")
}

// `migrations set` moves the revision boundary in both directions without
// running a migration, in one serializable transaction over the revision
// table.
func TestHostSetsTheRevisionBoundaryE2E(t *testing.T) {
	c := testcheck.New(t)
	address := withSearchPath(c, freshDatabase(c))
	dir := writeFiles(c, migrations)
	output, err := migrate(c, "up", "--db-url", address, "--migrations-dir", dir, "--limit", "1")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))

	output, err = migrate(c, "set", "--db-url", address, "--migrations-dir", dir, "--version", "1700000002")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, "+ 1700000002 (Posts)")
	c.Assert(revisions(c, address), qt.DeepEquals, []string{
		"1700000001 Users applied 2 2 true true " + usersChecksum,
		"1700000002 Posts applied 2 2 true true " + postsChecksum,
	})
	c.Assert(queryRows(c, address, `SELECT execution_time_ms FROM schema_migrations WHERE version = 1700000002`), qt.DeepEquals, []string{"0"})
	c.Assert(tables(c, address), qt.Not(qt.Contains), "posts")

	output, err = migrate(c, "set", "--db-url", address, "--migrations-dir", dir, "--version", "1700000001")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, "- 1700000002 (Posts)")
	c.Assert(revisions(c, address), qt.DeepEquals, []string{"1700000001 Users applied 2 2 true true " + usersChecksum})
}

// A tag names the version a database reached and a rollback can return to
// it. The built-in engine could not record one on Spanner: it created the
// tag table with TIMESTAMP, which Spanner refuses (`Type <timestamp> is not
// supported`); the provider creates it with TIMESTAMPTZ.
func TestHostTagsAVersionE2E(t *testing.T) {
	c := testcheck.New(t)
	address := withSearchPath(c, freshDatabase(c))
	dir := writeFiles(c, migrations)
	output, err := migrate(c, "up", "--db-url", address, "--migrations-dir", dir, "--limit", "1")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	output, err = migrate(c, "tag", "release-1", "--db-url", address)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, "Tagged version 1700000001 as release-1.")
	c.Assert(queryRows(c, address, `SELECT tag, version FROM ptah_migration_tags`), qt.DeepEquals, []string{"release-1 1700000001"})
	c.Assert(queryRows(c, address, `SELECT column_name, spanner_type, is_nullable FROM information_schema.columns
WHERE table_schema = 'public' AND table_name = 'ptah_migration_tags' ORDER BY ordinal_position`), qt.DeepEquals, []string{
		"tag character varying(255) NO", "version bigint NO", "recorded_at timestamp with time zone NO",
	})

	output, err = migrate(c, "up", "--db-url", address, "--migrations-dir", dir)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	output, err = migrate(c, "down", "--db-url", address, "--migrations-dir", dir, "--to-tag", "release-1", "--confirm")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(revisions(c, address), qt.DeepEquals, []string{"1700000001 Users applied 2 2 true true " + usersChecksum})

	output, err = migrate(c, "log", "--db-url", address)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Matches, `(?s)STARTED +VERSION +OPERATION +OUTCOME .*1700000002 +down +rolled_back.*1700000002 +up +applied.*1700000001 +up +applied.*`)

	output, err = migrate(c, "tag", "release-1", "--delete", "--db-url", address)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(queryRows(c, address, `SELECT tag FROM ptah_migration_tags`), qt.HasLen, 0)
}

// The Atlas-compatible revision format keeps its rows in the built-in
// engine's Atlas layout, written as that engine wrote them: no partial hashes
// as the JSON null, an empty error as an empty string.
func TestHostKeepsAtlasRevisionsE2E(t *testing.T) {
	c := testcheck.New(t)
	address := withSearchPath(c, freshDatabase(c))
	dir := writeFiles(c, migrations)
	output, err := migrate(c, "up", "--db-url", address, "--migrations-dir", dir, "--revision-format", "atlas")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(queryRows(c, address, `SELECT table_schema FROM information_schema.tables WHERE table_name = 'atlas_schema_revisions'`), qt.DeepEquals, []string{"public"})
	c.Assert(queryRows(c, address, `SELECT version, description, type, applied, total, error, error_stmt, hash, partial_hashes::text, operator_version
FROM atlas_schema_revisions ORDER BY version`), qt.DeepEquals, []string{
		"1700000001 Users 2 2 2   " + usersChecksum + " null Ptah",
		"1700000002 Posts 2 2 2   " + postsChecksum + " null Ptah",
	})
	output, err = migrate(c, "status", "--db-url", address, "--migrations-dir", dir, "--revision-format", "atlas")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, "Current Version: 1700000002")
	output, err = migrate(c, "down", "--db-url", address, "--migrations-dir", dir, "--target", "1700000001", "--confirm", "--revision-format", "atlas")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(queryRows(c, address, `SELECT version FROM atlas_schema_revisions`), qt.DeepEquals, []string{"1700000001"})
}

// A database the built-in engine migrated keeps its history: the provider
// reads the revision table the engine created and applies what is pending.
func TestHostContinuesTheBuiltinHistoryE2E(t *testing.T) {
	c := testcheck.New(t)
	address := withSearchPath(c, freshDatabase(c))
	// The engine's statement and the row it wrote for the first migration,
	// as measured.
	execSQL(c, address, `CREATE TABLE IF NOT EXISTS schema_migrations (
    version BIGINT PRIMARY KEY,
    description TEXT NOT NULL,
    applied_at TIMESTAMPTZ NOT NULL,
    state VARCHAR(32) NOT NULL DEFAULT 'applied',
    applied INTEGER NOT NULL DEFAULT 1,
    total INTEGER NOT NULL DEFAULT 1,
    error TEXT NULL,
    error_stmt TEXT NULL,
    execution_time_ms BIGINT NOT NULL DEFAULT 0,
    checksum VARCHAR(64) NOT NULL DEFAULT ''
)`, `CREATE TABLE users (id bigint NOT NULL, name varchar(100) NOT NULL, PRIMARY KEY (id))`, `CREATE INDEX users_name ON users (name)`,
		`INSERT INTO schema_migrations (version, description, applied_at, state, applied, total, error, error_stmt, execution_time_ms, checksum)
VALUES (1700000001, 'Users', '2026-11-07T02:02:13.262468Z', 'applied', 2, 2, NULL, NULL, 54, '`+usersChecksum+`')`)
	dir := writeFiles(c, migrations)

	output, err := migrate(c, "status", "--db-url", address, "--migrations-dir", dir)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, "Current Version: 1700000001")
	c.Assert(output, qt.Contains, "Pending Migrations: 1")
	output, err = migrate(c, "up", "--db-url", address, "--migrations-dir", dir)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(revisions(c, address), qt.DeepEquals, []string{
		"1700000001 Users applied 2 2 true true " + usersChecksum,
		"1700000002 Posts applied 2 2 true true " + postsChecksum,
	})
	c.Assert(queryRows(c, address, `SELECT applied_at::text, execution_time_ms FROM schema_migrations WHERE version = 1700000001`), qt.DeepEquals,
		queryRows(c, address, `SELECT '2026-11-07T02:02:13.262468Z'::timestamptz::text, 54`))
}

// A table of the revision table's name in another layout is someone else's:
// the provider refuses it and changes nothing, rather than adopting it.
func TestHostRefusesAForeignRevisionTableE2E(t *testing.T) {
	c := testcheck.New(t)
	address := withSearchPath(c, freshDatabase(c))
	execSQL(c, address, `CREATE TABLE schema_migrations (version bigint PRIMARY KEY, note varchar(10))`)
	dir := writeFiles(c, migrations)
	output, err := migrate(c, "up", "--db-url", address, "--migrations-dir", dir)
	c.Assert(err, qt.ErrorMatches, "exit status 2")
	c.Assert(output, qt.Contains, "a table of the revision table's name exists and does not hold the layout the provider keeps it in: its columns differ")
	c.Assert(tables(c, address), qt.DeepEquals, []string{"schema_migrations"})
	c.Assert(queryRows(c, address, `SELECT column_name FROM information_schema.columns WHERE table_name = 'schema_migrations' ORDER BY ordinal_position`),
		qt.DeepEquals, []string{"version", "note"})
}

// A read leaves the revision table, the log and the tag table out of the
// schema and reserves them, as the built-in engine's reader left them out, so
// a schema apply after migrations plans no drop of the records.
func TestHostLeavesMigrationRecordsOutOfASchemaE2E(t *testing.T) {
	c := testcheck.New(t)
	address := withSearchPath(c, freshDatabase(c))
	dir := writeFiles(c, migrations)
	output, err := migrate(c, "up", "--db-url", address, "--migrations-dir", dir)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	output, err = migrate(c, "tag", "release-1", "--db-url", address)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))

	output, err = planSQL(c, address, "CREATE TABLE users (id bigint PRIMARY KEY, name varchar(100) NOT NULL);\nCREATE INDEX users_name ON users (name);\n"+
		"CREATE TABLE posts (id bigint PRIMARY KEY, author_id bigint NOT NULL, title varchar(200));\n")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, synced)

	client := startProvider(c)
	session := openSession(c, client, address)
	snapshot, err := client.Inspect(c.Context(), &protocol.InspectRequest{SessionId: session.GetId()})
	c.Assert(err, qt.IsNil)
	reserved := make(map[string]protocol.ReservedObject_Purpose)
	for _, object := range snapshot.GetReservedObjects() {
		reserved[object.GetObject().GetName()] = object.GetPurpose()
	}
	c.Assert(reserved, qt.DeepEquals, map[string]protocol.ReservedObject_Purpose{
		"ptah_locks": protocol.ReservedObject_LOCK, "schema_migrations": protocol.ReservedObject_HISTORY,
		"schema_migrations_log": protocol.ReservedObject_LOG, "ptah_migration_tags": protocol.ReservedObject_TAG,
	})
}

// A migration run takes the provider's lock, so a run another session holds
// it against waits and gives up rather than running beside it. The built-in
// engine took no lock on Spanner and refused --migration-lock-timeout.
func TestHostMigrationsTakeTheLockE2E(t *testing.T) {
	c := testcheck.New(t)
	address := withSearchPath(c, freshDatabase(c))
	dir := writeFiles(c, migrations)
	client := startProvider(c)
	holder := openSession(c, client, address)
	held, err := client.TryLock(c.Context(), &protocol.LockRequest{Scope: &protocol.LockScope{SessionId: holder.GetId(), LockId: "h1", Name: "ptah_migrate"}, RequestId: "r1"})
	c.Assert(err, qt.IsNil)
	c.Assert(held.GetOutcome(), qt.Equals, protocol.LockResult_ACQUIRED)

	output, err := migrate(c, "up", "--db-url", address, "--migrations-dir", dir, "--migration-lock-timeout", "2s")
	c.Assert(err, qt.ErrorMatches, "exit status 2")
	c.Assert(output, qt.Contains, "ptah_migrate")
	c.Assert(tables(c, address), qt.DeepEquals, []string{"ptah_locks"})

	released, err := client.ReleaseLock(c.Context(), &protocol.LockRequest{Scope: &protocol.LockScope{SessionId: holder.GetId(), LockId: "h1", Name: "ptah_migrate"}, RequestId: "r2"})
	c.Assert(err, qt.IsNil)
	c.Assert(released.GetOutcome(), qt.Equals, protocol.LockResult_RELEASED)
	output, err = migrate(c, "up", "--db-url", address, "--migrations-dir", dir, "--migration-lock-timeout", "2s")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(revisions(c, address), qt.HasLen, 2)
}

// A migration's `-- +ptah check` assertion runs through the provider as a
// read-only query before the migration: a satisfied one lets it run, and one
// that is not stops the run before the migration touches anything, in the
// built-in engine's words. Without the provider's statements the host refused
// every migration that holds one.
func TestHostRunsCheckAssertionsE2E(t *testing.T) {
	c := testcheck.New(t)
	address := withSearchPath(c, freshDatabase(c))
	dir := writeFiles(c, map[string]string{
		"1700000001_users.up.sql":   "CREATE TABLE users (id bigint NOT NULL, PRIMARY KEY (id));\n",
		"1700000001_users.down.sql": "DROP TABLE users;\n",
		"1700000002_notes.up.sql": "-- +ptah check name=\"users_empty\" assert=\"SELECT count(*) = 0 FROM users\" on_fail=abort\n" +
			"CREATE TABLE notes (id bigint NOT NULL, PRIMARY KEY (id));\n",
		"1700000002_notes.down.sql": "DROP TABLE notes;\n",
	})
	output, err := migrate(c, "up", "--db-url", address, "--migrations-dir", dir, "--limit", "1")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	execSQL(c, address, "INSERT INTO users (id) VALUES (1)")
	output, err = migrate(c, "up", "--db-url", address, "--migrations-dir", dir)
	c.Assert(err, qt.ErrorMatches, "exit status 2", qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, "pre-migration check users_empty for migration 1700000002 was not satisfied (assert: SELECT count(*) = 0 FROM users)")
	c.Assert(queryRows(c, address, "SELECT version, state FROM schema_migrations ORDER BY version"), qt.DeepEquals, []string{"1700000001 applied"})
	c.Assert(tables(c, address), qt.Not(qt.Contains), "notes")

	execSQL(c, address, "DELETE FROM users WHERE true")
	output, err = migrate(c, "up", "--db-url", address, "--migrations-dir", dir)
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(queryRows(c, address, "SELECT version, state FROM schema_migrations ORDER BY version"), qt.DeepEquals,
		[]string{"1700000001 applied", "1700000002 applied"})
}
