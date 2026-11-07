package devclean_test

import (
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-spanner-plugin/internal/devclean"
	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// richCatalog is a database holding every kind of object the cleanup drops,
// read as the emulator answers: a schema, a sequence and a locality group, a
// table interleaved in another, two foreign keys in a cycle across schemas,
// an index, a search index (whose DDL the emulator prints as CREATE INDEX),
// a view another view reads, a change stream, the index Spanner keeps for
// each foreign key, and the provider's lock table.
func richCatalog() devclean.Catalog {
	return devclean.Catalog{
		Tables: []devclean.Table{
			{Schema: "app", Name: "other"},
			{Schema: "public", Name: "child", Parent: "parent"},
			{Schema: "public", Name: "grandchild", Parent: "child"},
			{Schema: "public", Name: "parent"},
			{Schema: "public", Name: "ptah_locks"},
			{Schema: "public", Name: "v1", View: true},
			{Schema: "public", Name: "v2", View: true},
		},
		Indexes: []devclean.Index{
			{Schema: "app", Table: "other", Name: "IDX_other_pid_N_CE4DB5578FA153A6", Type: "INDEX", Managed: true},
			{Schema: "app", Table: "other", Name: "PRIMARY_KEY", Type: "PRIMARY_KEY"},
			{Schema: "public", Table: "child", Name: "child_v", Type: "INDEX"},
			{Schema: "public", Table: "parent", Name: "si", Type: "SEARCH"},
			{Schema: "public", Table: "parent", Name: "PRIMARY_KEY", Type: "PRIMARY_KEY"},
		},
		ForeignKeys: []devclean.ForeignKey{
			{Schema: "public", Table: "parent", Name: "fk_parent_other", RefSchema: "app", RefTable: "other"},
			{Schema: "app", Table: "other", Name: "fk_other_parent", RefSchema: "public", RefTable: "parent"},
		},
		ChangeStreams: []devclean.Object{{Schema: "public", Name: "cs1"}},
		Schemas:       []string{"information_schema", "pg_catalog", "public", "spanner_sys", "app"},
		DDL: []string{
			"CREATE SCHEMA app;",
			"CREATE SEQUENCE seq1 BIT_REVERSED_POSITIVE START COUNTER WITH 1;",
			"CREATE LOCALITY GROUP lg;",
			"ALTER LOCALITY GROUP default STORAGE 'ssd';",
			"CREATE TABLE ptah_locks (\n  lock_name character varying(1024) NOT NULL,\n  PRIMARY KEY(lock_name)\n);",
			"CREATE TABLE parent (\n  id bigint NOT NULL,\n  oid bigint,\n  PRIMARY KEY(id),\n  CONSTRAINT fk_parent_other FOREIGN KEY (oid) REFERENCES app.other(id)\n);",
			"CREATE TABLE child (\n  id bigint NOT NULL,\n  cid bigint NOT NULL,\n  PRIMARY KEY(id, cid)\n) INTERLEAVE IN PARENT parent ON DELETE CASCADE;",
			"CREATE TABLE grandchild (\n  id bigint NOT NULL,\n  PRIMARY KEY(id)\n) INTERLEAVE IN PARENT child;",
			"CREATE TABLE app.other (\n  id bigint NOT NULL,\n  pid bigint,\n  PRIMARY KEY(id)\n);",
			"CREATE INDEX child_v ON child (v);",
			"CREATE INDEX si ON parent (id NULLS FIRST);",
			"CREATE VIEW v1 WITH (security_invoker) AS SELECT id FROM parent;",
			"CREATE VIEW v2 WITH (security_invoker) AS SELECT id FROM v1;",
			"CREATE CHANGE STREAM cs1\nFOR parent;",
		},
		Locks: true,
	}
}

// what is an object as a test compares it: its kind, schema and name.
type what struct{ Kind, Schema, Name string }

func whats(objects []devclean.Object) []what {
	result := make([]what, len(objects))
	for i, object := range objects {
		result[i] = what{object.Kind, object.Schema, object.Name}
	}
	return result
}

func TestObjects_HappyPath(t *testing.T) {
	tests := []struct {
		name  string
		scope []string
		want  []what
	}{
		{
			name: "a whole-database cleanup drops dependents first and every schema but public",
			want: []what{
				{"change stream", "public", "cs1"},
				{"view", "public", "v1"},
				{"view", "public", "v2"},
				{"foreign key", "app", "fk_other_parent"},
				{"foreign key", "public", "fk_parent_other"},
				{"search index", "public", "si"},
				{"index", "public", "child_v"},
				{"table", "public", "grandchild"},
				{"table", "public", "child"},
				{"table", "app", "other"},
				{"table", "public", "parent"},
				{"sequence", "public", "seq1"},
				{"locality group", "", "lg"},
				{"schema", "app", "app"},
			},
		},
		{
			name:  "a cleanup of named schemas keeps them and what belongs to the database",
			scope: []string{"public", "app"},
			want: []what{
				{"change stream", "public", "cs1"},
				{"view", "public", "v1"},
				{"view", "public", "v2"},
				{"foreign key", "app", "fk_other_parent"},
				{"foreign key", "public", "fk_parent_other"},
				{"search index", "public", "si"},
				{"index", "public", "child_v"},
				{"table", "public", "grandchild"},
				{"table", "public", "child"},
				{"table", "app", "other"},
				{"table", "public", "parent"},
				{"sequence", "public", "seq1"},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			objects, err := devclean.Objects(richCatalog(), test.scope)
			c.Assert(err, qt.IsNil)
			c.Assert(whats(objects), qt.DeepEquals, test.want)
			c.Assert(devclean.Blocking(objects), qt.HasLen, 0)
		})
	}
}

// The lock table is the user's when it is not in the provider's layout, and
// a cleanup drops it as any table.
func TestObjects_AForeignLockTableIsDropped(t *testing.T) {
	c := testcheck.New(t)
	snapshot := devclean.Catalog{
		Tables:  []devclean.Table{{Schema: "public", Name: "ptah_locks"}},
		Schemas: []string{"public"},
		DDL:     []string{"CREATE TABLE ptah_locks (id bigint NOT NULL, PRIMARY KEY(id));"},
	}
	objects, err := devclean.Objects(snapshot, nil)
	c.Assert(err, qt.IsNil)
	c.Assert(whats(objects), qt.DeepEquals, []what{{"table", "public", "ptah_locks"}})
}

// An object the provider has no statement for is listed with a refusal and
// stops the cleanup: Spanner's roles, privileges and property graphs, which
// the emulator does not take, so their DDL is written as managed Spanner
// prints it. Two privileges granted to one role are one object.
func TestObjects_AKindWithoutADropStopsTheCleanup(t *testing.T) {
	c := testcheck.New(t)
	snapshot := devclean.Catalog{
		Tables:  []devclean.Table{{Schema: "public", Name: "t"}},
		Schemas: []string{"public"},
		DDL: []string{
			"CREATE TABLE t (id bigint NOT NULL, PRIMARY KEY(id));",
			"CREATE ROLE reader;",
			"GRANT SELECT ON TABLE t TO reader;",
			"GRANT SELECT ON TABLE t TO reader;",
			`CREATE PROPERTY GRAPH "Social" NODE TABLES (t);`,
		},
	}
	objects, err := devclean.Objects(snapshot, nil)
	c.Assert(err, qt.IsNil)
	c.Assert(whats(devclean.Blocking(objects)), qt.DeepEquals, []what{
		{"role", "public", "reader"},
		{"grant", "public", "reader"},
		{"property graph", "public", "Social"},
	})
	c.Assert(devclean.Blocking(objects)[0].Refusal, qt.Equals, "the provider has no statement that removes this kind of object from a dev database")
}

// A table of the scope that a foreign key outside it references cannot be
// dropped without changing a table the cleanup must leave alone.
func TestObjects_AReferenceFromOutsideTheScopeStopsTheCleanup(t *testing.T) {
	c := testcheck.New(t)
	objects, err := devclean.Objects(richCatalog(), []string{"public"})
	c.Assert(err, qt.IsNil)
	c.Assert(whats(devclean.Blocking(objects)), qt.DeepEquals, []what{{"table", "public", "parent"}})
	c.Assert(devclean.Blocking(objects)[0].Refusal, qt.Equals, "a foreign key of a table outside the cleanup's schemas references it")
}

func TestObjects_FailurePath(t *testing.T) {
	t.Run("a table information_schema lists and the DDL does not create", func(t *testing.T) {
		c := testcheck.New(t)
		snapshot := devclean.Catalog{Tables: []devclean.Table{{Schema: "public", Name: "t"}}, Schemas: []string{"public"}}
		objects, err := devclean.Objects(snapshot, nil)
		c.Assert(err, qt.ErrorIs, devclean.ErrInconsistent)
		c.Assert(objects, qt.IsNil)
	})
	t.Run("an index the DDL creates and information_schema does not list", func(t *testing.T) {
		c := testcheck.New(t)
		snapshot := devclean.Catalog{
			Tables:  []devclean.Table{{Schema: "public", Name: "t"}},
			Schemas: []string{"public"},
			DDL:     []string{"CREATE TABLE t (id bigint NOT NULL, PRIMARY KEY(id));", "CREATE UNIQUE INDEX t_id ON t (id);"},
		}
		objects, err := devclean.Objects(snapshot, nil)
		c.Assert(err, qt.ErrorIs, devclean.ErrInconsistent)
		c.Assert(objects, qt.IsNil)
	})
	t.Run("DDL the provider cannot read", func(t *testing.T) {
		c := testcheck.New(t)
		snapshot := devclean.Catalog{Schemas: []string{"public"}, DDL: []string{"CREATE VIEW v AS SELECT $$x$$;"}}
		objects, err := devclean.Objects(snapshot, nil)
		c.Assert(err, qt.ErrorMatches, `the provider cannot read the database's DDL: .*`)
		c.Assert(objects, qt.IsNil)
	})
}

func TestStatement_HappyPath(t *testing.T) {
	tests := []struct {
		name   string
		object devclean.Object
		want   string
	}{
		{"change stream", devclean.Object{Kind: "change stream", Schema: "public", Name: "cs1"}, `DROP CHANGE STREAM "cs1"`},
		{"view in a schema", devclean.Object{Kind: "view", Schema: "app", Name: "v"}, `DROP VIEW "app"."v"`},
		{"foreign key", devclean.Object{Kind: "foreign key", Schema: "app", Name: "fk", Table: "other"}, `ALTER TABLE "app"."other" DROP CONSTRAINT "fk"`},
		{"search index", devclean.Object{Kind: "search index", Schema: "public", Name: "si"}, `DROP SEARCH INDEX "si"`},
		{"index", devclean.Object{Kind: "index", Schema: "public", Name: "i", Table: "t"}, `DROP INDEX "i"`},
		{"table", devclean.Object{Kind: "table", Schema: "public", Name: `a"b`}, `DROP TABLE "a""b"`},
		{"sequence", devclean.Object{Kind: "sequence", Schema: "app", Name: "s"}, `DROP SEQUENCE "app"."s"`},
		{"locality group", devclean.Object{Kind: "locality group", Name: "lg"}, `DROP LOCALITY GROUP "lg"`},
		{"schema", devclean.Object{Kind: "schema", Schema: "app", Name: "app"}, `DROP SCHEMA "app"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			statement, err := devclean.Statement(test.object)
			c.Assert(err, qt.IsNil)
			c.Assert(statement, qt.Equals, test.want)
		})
	}
}

func TestStatement_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	statement, err := devclean.Statement(devclean.Object{Kind: "role", Name: "reader"})
	c.Assert(err, qt.ErrorMatches, `the provider has no statement that removes a role`)
	c.Assert(statement, qt.Equals, "")
}
