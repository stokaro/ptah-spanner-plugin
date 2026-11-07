//go:build integration

package integration_test

import (
	"context"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	providerruntime "github.com/stokaro/ptah/provider-sdk/runtime"

	"github.com/stokaro/ptah-spanner-plugin/internal/backend"
	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// startDevProvider starts the provider for a host that also lists the dev
// database cleanup and the replay guard.
func startDevProvider(c *qt.C) *providerruntime.Client {
	c.Helper()
	binary, digest := providerBinary(c)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client, err := providerruntime.Start(ctx, providerruntime.Options{
		Executable: binary, SHA256: digest, ProviderID: backend.ProviderID,
		HostFeatures: append(hostFeatures(), provider.LocksFeature, provider.DevCleanupFeature, provider.ReplayGuardFeature),
	})
	c.Assert(err, qt.IsNil)
	c.Cleanup(client.Close)
	return client
}

// devObjectNames are the objects of a list as kind, schema and name.
func devObjectNames(objects []*protocol.DevObject) []string {
	names := make([]string, len(objects))
	for i, object := range objects {
		ref := object.GetObject()
		names[i] = ref.GetKind() + " " + ref.GetSchema() + "." + ref.GetName()
	}
	return names
}

// databaseDDL is the database's DDL, one statement a row, as PGAdapter
// prints it.
func databaseDDL(c *qt.C, address string) []string {
	c.Helper()
	return queryRows(c, address, "SHOW DATABASE DDL")
}

// A cleanup of the whole database drops every kind of object the emulator
// creates, each after what depends on it: a change stream before the table it
// watches, a view before the view it reads, a cycle of foreign keys across
// schemas before the tables, an interleaved table before its parent, a table
// before the sequence its default reads, and the schema last. The lock the
// session holds, and the table it lives in, stay. The interleaved table's
// name sorts after its parent's, so an order by name alone would drop the
// parent first, which Spanner refuses.
func TestDevCleanupLive_RemovesEveryObject(t *testing.T) {
	c := testcheck.New(t)
	address := freshDatabase(c)
	execSQL(c, address,
		"CREATE SCHEMA app",
		"CREATE SEQUENCE seq1 BIT_REVERSED_POSITIVE",
		"CREATE LOCALITY GROUP lg",
		"CREATE TABLE parent (id bigint NOT NULL, oid bigint, PRIMARY KEY (id))",
		"CREATE TABLE z_child (id bigint NOT NULL, cid bigint NOT NULL, v varchar(10), PRIMARY KEY (id, cid)) INTERLEAVE IN PARENT parent ON DELETE CASCADE",
		"CREATE TABLE app.other (id bigint NOT NULL, pid bigint, PRIMARY KEY (id))",
		"ALTER TABLE app.other ADD CONSTRAINT fk_other_parent FOREIGN KEY (pid) REFERENCES parent (id)",
		"ALTER TABLE parent ADD CONSTRAINT fk_parent_other FOREIGN KEY (oid) REFERENCES app.other (id)",
		"CREATE INDEX z_child_v ON z_child (v)",
		"CREATE SEARCH INDEX si ON parent (id)",
		"CREATE TABLE withseq (id bigint NOT NULL DEFAULT nextval('seq1'), PRIMARY KEY (id))",
		"CREATE VIEW v1 SQL SECURITY INVOKER AS SELECT id FROM parent",
		"CREATE VIEW v2 SQL SECURITY INVOKER AS SELECT id FROM v1",
		"CREATE CHANGE STREAM cs1 FOR parent",
		"INSERT INTO parent (id) VALUES (1)",
	)
	client := startDevProvider(c)
	session := openSession(c, client, address)
	c.Assert(tryLock(c, client, session.GetId(), "dev-lock", "r1"), qt.Equals, protocol.LockResult_ACQUIRED)

	listed, err := client.ListDevObjects(c.Context(), &protocol.ListDevObjectsRequest{SessionId: session.GetId()})
	c.Assert(err, qt.IsNil)

	cleaned, err := client.CleanDevDatabase(c.Context(), &protocol.CleanDevRequest{SessionId: session.GetId(), RequestId: "clean-1"})
	c.Assert(err, qt.IsNil)
	c.Assert(cleaned.GetOutcome(), qt.Equals, protocol.DevCleanup_CLEANED)
	remaining := databaseDDL(c, address)
	c.Assert(remaining, qt.HasLen, 1)
	c.Assert(strings.HasPrefix(remaining[0], "CREATE TABLE ptah_locks ("), qt.IsTrue, qt.Commentf("DDL left: %q", remaining))
	checked, err := client.CheckLock(c.Context(), lockRequest(session.GetId(), "dev-lock", "r2"))
	c.Assert(err, qt.IsNil)
	c.Assert(checked.GetOutcome(), qt.Equals, protocol.LockResult_ACQUIRED)

	// The list names the objects in the order the cleanup dropped them. It is
	// compared after the cleanup, so a cleanup that drops them in an order
	// Spanner refuses fails on its outcome first.
	c.Assert(devObjectNames(listed.GetObjects()), qt.DeepEquals, []string{
		"change stream public.cs1",
		"view public.v1",
		"view public.v2",
		"foreign key app.fk_other_parent",
		"foreign key public.fk_parent_other",
		"search index public.si",
		"index public.z_child_v",
		"table public.z_child",
		"table app.other",
		"table public.parent",
		"table public.withseq",
		"sequence public.seq1",
		"locality group .lg",
		"schema app.app",
	})

	again, err := client.ListDevObjects(c.Context(), &protocol.ListDevObjectsRequest{SessionId: session.GetId()})
	c.Assert(err, qt.IsNil)
	c.Assert(again.GetObjects(), qt.HasLen, 0)
}

// A cleanup of the public schema stops before it drops anything when a
// table of another schema holds a foreign key to one of its tables: dropping
// that table would need a change outside the schemas the cleanup may touch.
func TestDevCleanupLive_RefusesAReferenceFromOutsideItsScope(t *testing.T) {
	c := testcheck.New(t)
	address := freshDatabase(c)
	execSQL(c, address,
		"CREATE SCHEMA app",
		"CREATE TABLE p (id bigint NOT NULL, PRIMARY KEY (id))",
		"CREATE TABLE q (id bigint NOT NULL, PRIMARY KEY (id))",
		"CREATE TABLE app.c (id bigint NOT NULL, pid bigint, PRIMARY KEY (id))",
		"ALTER TABLE app.c ADD CONSTRAINT fk_c_p FOREIGN KEY (pid) REFERENCES p (id)",
	)
	client := startDevProvider(c)
	session := openSession(c, client, address)

	cleaned, err := client.CleanDevDatabase(c.Context(), &protocol.CleanDevRequest{SessionId: session.GetId(), RequestId: "clean-1", Schemas: []string{"public"}})
	c.Assert(err, qt.ErrorAs, new(*provider.DevCleanupError))
	c.Assert(cleaned.GetOutcome(), qt.Equals, protocol.DevCleanup_REFUSED)
	c.Assert(devObjectNames(cleaned.GetBlocking()), qt.DeepEquals, []string{"table public.p"})
	c.Assert(cleaned.GetBlocking()[0].GetRefusal(), qt.Equals, "a foreign key of a table outside the cleanup's schemas references it")
	c.Assert(queryRows(c, address, "SELECT table_schema, table_name FROM information_schema.tables WHERE table_schema IN ('public', 'app') ORDER BY 1, 2"),
		qt.DeepEquals, []string{"app c", "public p", "public q"})
}
