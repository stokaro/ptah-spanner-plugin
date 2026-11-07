//go:build integration

package integration_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// readColumn is the part of an observed column a test compares.
type readColumn struct {
	Name, Type, Nullable string
	Default              string
	Key                  bool
}

// readIndex is the part of an observed index a test compares.
type readIndex struct {
	Name    string
	Unique  bool
	Keys    []string
	Storing []string
}

// readTable is the part of an observed table a test compares.
type readTable struct {
	Schema, Name string
	Columns      []readColumn
	PrimaryKey   []string
	Indexes      []readIndex
}

func readTables(snapshot *protocol.Schema) []readTable {
	var tables []readTable
	for _, table := range snapshot.GetTables() {
		got := readTable{Schema: table.GetIdentity().GetSchema(), Name: table.GetIdentity().GetName(), PrimaryKey: table.GetPrimaryKey().GetColumnNames()}
		for _, column := range table.GetColumns() {
			observed := column.GetObserved()
			got.Columns = append(got.Columns, readColumn{Name: observed.GetName(), Type: observed.GetColumnType(), Nullable: observed.GetIsNullable(),
				Default: observed.GetDefaultExpression(), Key: observed.GetPrimaryKey()})
		}
		for _, index := range table.GetIndexes() {
			got.Indexes = append(got.Indexes, readIndex{Name: index.GetName(), Unique: index.GetUnique(), Keys: indexKeys(index), Storing: index.GetIncludeColumns()})
		}
		tables = append(tables, got)
	}
	return tables
}

// indexKeys spells each key part as column, direction and NULLS ordering.
func indexKeys(index *protocol.ObservedIndex) []string {
	keys := make([]string, 0, len(index.GetParts()))
	for _, part := range index.GetParts() {
		key := part.GetColumn() + " ASC"
		if part.GetDescending() {
			key = part.GetColumn() + " DESC"
		}
		if part.GetNulls() != "" {
			key += " NULLS " + part.GetNulls()
		}
		keys = append(keys, key)
	}
	return keys
}

// The read reports what Spanner stores: types in its spelling, the key in key
// order rather than column order, defaults as stored, and each index's keys
// with their direction and NULLS ordering and its STORING columns.
func TestInspectionSubprocess(t *testing.T) {
	c := testcheck.New(t)
	address := freshDatabase(c)
	execSQL(c, address,
		`CREATE TABLE parent (a bigint NOT NULL, b varchar(20) NOT NULL, note text, score bigint NOT NULL DEFAULT 0, seen timestamptz,
  flag bool, ratio float8, tags text[], PRIMARY KEY (b, a))`,
		`CREATE UNIQUE INDEX ux_parent_score ON parent (score DESC NULLS LAST) INCLUDE (note)`,
		`CREATE INDEX ix_parent_note ON parent (note, a DESC)`,
		`CREATE TABLE plain (id bigint PRIMARY KEY)`,
	)
	client := startProvider(c)
	session := openSession(c, client, address)
	snapshot, err := client.Inspect(c.Context(), &protocol.InspectRequest{SessionId: session.GetId()})
	c.Assert(err, qt.IsNil)
	c.Assert(snapshot.GetComplete(), qt.IsTrue)
	c.Assert(readTables(snapshot), qt.DeepEquals, []readTable{
		{
			Schema: "public", Name: "parent",
			Columns: []readColumn{
				{Name: "a", Type: "bigint", Nullable: "NO", Key: true},
				{Name: "b", Type: "character varying(20)", Nullable: "NO", Key: true},
				{Name: "note", Type: "character varying", Nullable: "YES"},
				{Name: "score", Type: "bigint", Nullable: "NO", Default: "'0'::bigint"},
				{Name: "seen", Type: "timestamp with time zone", Nullable: "YES"},
				{Name: "flag", Type: "boolean", Nullable: "YES"},
				{Name: "ratio", Type: "double precision", Nullable: "YES"},
				{Name: "tags", Type: "character varying[]", Nullable: "YES"},
			},
			PrimaryKey: []string{"b", "a"},
			Indexes: []readIndex{
				{Name: "ix_parent_note", Keys: []string{"note ASC", "a DESC"}},
				{Name: "ux_parent_score", Unique: true, Keys: []string{"score DESC NULLS LAST"}, Storing: []string{"note"}},
			},
		},
		{Schema: "public", Name: "plain", Columns: []readColumn{{Name: "id", Type: "bigint", Nullable: "NO", Key: true}}, PrimaryKey: []string{"id"}},
	})
}

// A read narrowed to a schema reads that schema and binds the selector to it.
func TestInspectionSubprocess_NamedSchema(t *testing.T) {
	c := testcheck.New(t)
	address := freshDatabase(c)
	execSQL(c, address, `CREATE SCHEMA app`, `CREATE TABLE app.items (id bigint PRIMARY KEY)`, `CREATE TABLE elsewhere (id bigint PRIMARY KEY)`)
	client := startProvider(c)
	session := openSession(c, client, address)
	snapshot, err := client.Inspect(c.Context(), &protocol.InspectRequest{SessionId: session.GetId(), Schemas: []string{"app"}})
	c.Assert(err, qt.IsNil)
	c.Assert(readTables(snapshot), qt.DeepEquals, []readTable{
		{Schema: "app", Name: "items", Columns: []readColumn{{Name: "id", Type: "bigint", Nullable: "NO", Key: true}}, PrimaryKey: []string{"id"}},
	})
	c.Assert(snapshot.GetScopeBindings(), qt.HasLen, 1)
	c.Assert(snapshot.GetScopeBindings()[0].GetSelector(), qt.Equals, "app")
}

// A read refuses an object it does not describe yet, with a reason naming it,
// rather than leaving it out: the host would plan to create what a read leaves
// out.
func TestInspectionSubprocess_FailurePath(t *testing.T) {
	tests := []struct {
		name       string
		statements []string
		wantErr    string
	}{
		{name: "a view", statements: []string{`CREATE TABLE t (id bigint PRIMARY KEY)`, `CREATE VIEW v SQL SECURITY INVOKER AS SELECT id FROM t`},
			wantErr: `.*\(FailedPrecondition\): view "public"\."v" is not described by the provider yet`},
		{name: "a foreign key", statements: []string{`CREATE TABLE p (id bigint PRIMARY KEY)`,
			`CREATE TABLE ch (id bigint PRIMARY KEY, p bigint, CONSTRAINT fk_ch_p FOREIGN KEY (p) REFERENCES p (id))`},
			wantErr: `.*\(FailedPrecondition\): table "public"\."ch" has the FOREIGN KEY constraint "fk_ch_p", which the provider does not describe yet`},
		{name: "a check constraint", statements: []string{`CREATE TABLE t (id bigint PRIMARY KEY, n bigint, CONSTRAINT n_ok CHECK (n > 0))`},
			wantErr: `.*\(FailedPrecondition\): table "public"\."t" has the CHECK constraint "n_ok", which the provider does not describe yet`},
		{name: "a row deletion policy", statements: []string{`CREATE TABLE t (id bigint PRIMARY KEY, ts timestamptz) TTL INTERVAL '7 days' ON ts`},
			wantErr: `.*\(FailedPrecondition\): table "public"\."t" has a row deletion policy, which the provider does not describe yet`},
		{name: "a generated column", statements: []string{`CREATE TABLE t (id bigint PRIMARY KEY, g bigint GENERATED ALWAYS AS (id * 2) STORED)`},
			wantErr: `.*\(FailedPrecondition\): column "g" of table "public"\."t" is generated, which the provider does not describe yet`},
		{name: "a table without a primary key", statements: []string{`CREATE TABLE t (n bigint)`},
			wantErr: `.*\(FailedPrecondition\): column "rowid" of table "public"\."t" is an identity column, which the provider does not describe yet; .*`},
		{name: "a filtered index", statements: []string{`CREATE TABLE t (id bigint PRIMARY KEY, n bigint)`, `CREATE INDEX nf ON t (n) WHERE n IS NOT NULL`},
			wantErr: `.*\(FailedPrecondition\): index "nf" of table "public"\."t" has a WHERE filter, which the provider does not describe yet`},
		{name: "an interleaved table", statements: []string{`CREATE TABLE p (id bigint PRIMARY KEY)`,
			`CREATE TABLE ch (id bigint, k bigint, PRIMARY KEY (id, k)) INTERLEAVE IN PARENT p`},
			wantErr: `.*\(FailedPrecondition\): table "public"\."ch" is interleaved in a parent table, which the provider does not describe yet`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			address := freshDatabase(c)
			execSQL(c, address, test.statements...)
			client := startProvider(c)
			session := openSession(c, client, address)
			snapshot, err := client.Inspect(c.Context(), &protocol.InspectRequest{SessionId: session.GetId()})
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(snapshot, qt.IsNil)
		})
	}
}
