//go:build integration

package integration_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"

	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// namespaceNames is the name of each namespace a read lists, in order.
func namespaceNames(snapshot *protocol.Schema) []string {
	names := make([]string, 0, len(snapshot.GetCommonObjects().GetNamespaces()))
	for _, namespace := range snapshot.GetCommonObjects().GetNamespaces() {
		names = append(names, namespace.GetIdentity().GetName())
	}
	return names
}

// A read for a host that carries schemas lists them completely: a read that
// requests schemas lists the requested ones the database holds, and a read
// that requests none lists every schema but Spanner's own catalogs, while its
// tables stay the default schema's. Each is a schema reference of the
// session's database.
func TestInspectionSubprocess_ListsSchemas(t *testing.T) {
	tests := []struct {
		name      string
		requested []string
		want      []string
	}{
		{name: "a read of the default schema", want: []string{"app", "es_app", "public"}},
		{name: "a read of named schemas", requested: []string{"public", "es_app", "missing"}, want: []string{"es_app", "public"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			address := freshDatabase(c)
			execSQL(c, address, "CREATE SCHEMA es_app", "CREATE SCHEMA app", "CREATE TABLE app.t (id bigint NOT NULL, PRIMARY KEY (id))")
			client := startProviderWith(c, append(hostFeatures(), provider.CommonObjectsFeature, provider.CommonNamespacesFeature)...)
			session := openSession(c, client, address)
			snapshot, err := client.Inspect(c.Context(), &protocol.InspectRequest{SessionId: session.GetId(), Schemas: test.requested})
			c.Assert(err, qt.IsNil)
			c.Assert(namespaceNames(snapshot), qt.DeepEquals, test.want)
			c.Assert(snapshot.GetCommonObjects().GetNamespaces()[0].GetIdentity().GetKind(), qt.Equals, provider.NamespaceKind)
			c.Assert(snapshot.GetCommonObjects().GetNamespaces()[0].GetIdentity().GetDatabase(), qt.Equals, session.GetDatabase())
			c.Assert(snapshot.GetCommonObjects().GetCoverage(), qt.HasLen, 3)
			c.Assert(snapshot.GetCommonObjects().GetCoverage()[2].GetKind(), qt.Equals, protocol.CommonObjectSupport_SCHEMA)
			c.Assert(snapshot.GetCommonObjects().GetCoverage()[2].GetKnowledge().GetState(), qt.Equals, protocol.ModelKnowledge_COMPLETE)
		})
	}
}

// A host that carries common objects without schemas gets no schema and no
// schema coverage.
func TestInspectionSubprocess_ListsNoSchemaWithoutTheFeature(t *testing.T) {
	c := testcheck.New(t)
	address := freshDatabase(c)
	execSQL(c, address, "CREATE SCHEMA es_app")
	client := startCommonObjectsProvider(c)
	snapshot, err := client.Inspect(c.Context(), &protocol.InspectRequest{SessionId: openSession(c, client, address).GetId()})
	c.Assert(err, qt.IsNil)
	c.Assert(snapshot.GetCommonObjects().GetNamespaces(), qt.HasLen, 0)
	c.Assert(snapshot.GetCommonObjects().GetCoverage(), qt.HasLen, 2)
}

// A read for a host that compares index predicates states each index's WHERE
// clause as Spanner keeps it, and leaves out the NOT NULL tests Spanner gives
// a unique index's keys by itself: a plain unique index states none, and a
// unique index filtered on a stored column states that test alone.
func TestInspectionSubprocess_ReadsIndexPredicates(t *testing.T) {
	c := testcheck.New(t)
	address := freshDatabase(c)
	execSQL(c, address, `CREATE TABLE t (id bigint NOT NULL, zed bigint, "Abc" bigint, s varchar, PRIMARY KEY (id))`,
		`CREATE INDEX t_f ON t (zed, "Abc") WHERE zed IS NOT NULL AND "Abc" IS NOT NULL`,
		`CREATE UNIQUE INDEX t_u ON t (zed)`,
		`CREATE UNIQUE INDEX t_us ON t ("Abc") INCLUDE (s) WHERE s IS NOT NULL`)
	client := startProviderWith(c, append(hostFeatures(), provider.IndexPredicatesFeature)...)
	snapshot, err := client.Inspect(c.Context(), &protocol.InspectRequest{SessionId: openSession(c, client, address).GetId()})
	c.Assert(err, qt.IsNil)
	conditions := make(map[string]string)
	for _, index := range snapshot.GetTables()[0].GetIndexes() {
		conditions[index.GetName()] = index.GetCondition()
	}
	c.Assert(conditions, qt.DeepEquals, map[string]string{"t_f": "Abc IS NOT NULL AND zed IS NOT NULL", "t_u": "", "t_us": "s IS NOT NULL"})
}

// A read for a host that negotiated the reserved seed ledger reports
// schema_seeds as SEEDS, in its layout alone, and leaves it out of the tables;
// a host that did not reads it as a table.
func TestInspectionSubprocess_ReservesTheSeedLedger(t *testing.T) {
	tests := []struct {
		name         string
		features     []string
		wantTables   int
		wantReserved []protocol.ReservedObject_Purpose
	}{
		{name: "a host that negotiated the ledger", features: append(hostFeatures(), provider.ReservedSeedsFeature),
			wantReserved: []protocol.ReservedObject_Purpose{protocol.ReservedObject_SEEDS}},
		{name: "a host that did not", features: hostFeatures(), wantTables: 1, wantReserved: make([]protocol.ReservedObject_Purpose, 0)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			address := freshDatabase(c)
			execSQL(c, address, `CREATE TABLE schema_seeds (seed_path VARCHAR(512) PRIMARY KEY, env VARCHAR(128) NOT NULL, checksum TEXT NOT NULL,
applied_at TIMESTAMPTZ NOT NULL)`)
			client := startProviderWith(c, test.features...)
			snapshot, err := client.Inspect(c.Context(), &protocol.InspectRequest{SessionId: openSession(c, client, address).GetId()})
			c.Assert(err, qt.IsNil)
			c.Assert(snapshot.GetTables(), qt.HasLen, test.wantTables)
			purposes := make([]protocol.ReservedObject_Purpose, 0)
			for _, reserved := range snapshot.GetReservedObjects() {
				purposes = append(purposes, reserved.GetPurpose())
			}
			c.Assert(purposes, qt.DeepEquals, test.wantReserved)
		})
	}
}
