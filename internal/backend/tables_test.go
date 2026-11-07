package backend_test

import (
	"context"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"

	"github.com/stokaro/ptah-spanner-plugin/internal/backend"
	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// predicted is the part of a predicted column a test compares.
type predicted struct {
	Name, DataType, ColumnType, IsNullable string
	PrimaryKey                             bool
	Ordinal                                int64
	Default                                *string
}

// A creation's prediction is what the read of the emulator reports for the
// same table: types in Spanner's spelling, key columns NOT NULL, a literal
// default stored as a quoted value cast to the column's type.
func TestProjectTableCreations_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	request := &protocol.ProjectTableCreationsRequest{Engine: "spanner", Tables: []*protocol.TableCreationInput{{
		Declaration: &protocol.DeclaredTable{Name: "t", PrimaryKey: []string{"id"}, Columns: []*protocol.DeclaredColumn{
			{Name: "id", Type: "INTEGER", Nullable: true},
			{Name: "name", Type: "VARCHAR(20)"},
			{Name: "tags", Type: "text[]", Nullable: true},
			{Name: "status", Type: "TEXT", DefaultSet: true, DefaultValue: "it's"},
			{Name: "mood", Type: "mood", Nullable: true, EnumValues: []string{"a"}},
			{Name: "seen", Type: "timestamptz", Nullable: true, DefaultExpression: "CURRENT_TIMESTAMP"},
		}},
	}}}
	result, err := backend.New().ProjectTableCreations(context.Background(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.GetTables(), qt.HasLen, 1)
	c.Assert(result.GetTables()[0].GetColumnPrimaryKeys(), qt.DeepEquals, []string{"id"})
	var got []predicted
	for _, column := range result.GetTables()[0].GetColumns() {
		got = append(got, predicted{Name: column.GetName(), DataType: column.GetDataType(), ColumnType: column.GetColumnType(), IsNullable: column.GetIsNullable(),
			PrimaryKey: column.GetPrimaryKey(), Ordinal: column.GetOrdinalPosition(), Default: column.DefaultExpression})
	}
	c.Assert(got, qt.DeepEquals, []predicted{
		{Name: "id", DataType: "bigint", ColumnType: "bigint", IsNullable: "NO", PrimaryKey: true, Ordinal: 1},
		{Name: "name", DataType: "character varying", ColumnType: "character varying(20)", IsNullable: "NO", Ordinal: 2},
		{Name: "tags", DataType: "ARRAY", ColumnType: "character varying[]", IsNullable: "YES", Ordinal: 3},
		{Name: "status", DataType: "character varying", ColumnType: "character varying", IsNullable: "NO", Ordinal: 4, Default: new("'it''s'::character varying")},
		{Name: "mood", DataType: "character varying", ColumnType: "character varying", IsNullable: "YES", Ordinal: 5},
		{Name: "seen", DataType: "timestamp with time zone", ColumnType: "timestamp with time zone", IsNullable: "YES", Ordinal: 6, Default: new("CURRENT_TIMESTAMP")},
	})
}

func TestProjectTableCreations_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	request := &protocol.ProjectTableCreationsRequest{Engine: "spanner", Tables: []*protocol.TableCreationInput{{
		Declaration: &protocol.DeclaredTable{Name: "t", Columns: []*protocol.DeclaredColumn{{Name: "id", Type: "smallint"}}},
	}}}
	result, err := backend.New().ProjectTableCreations(context.Background(), request)
	c.Assert(err, qt.ErrorMatches, `.*column "id" of table "t": type "smallint": Spanner stores integers as bigint only.*`)
	c.Assert(result, qt.IsNil)
}

// A table's key columns are its key list, then each column marked primary,
// in key order.
func TestPrepareTables(t *testing.T) {
	c := testcheck.New(t)
	request := &protocol.PrepareTablesRequest{Engine: "spanner", Tables: []*protocol.TablePreparationInput{
		{Desired: &protocol.DeclaredTable{Name: "edges", PrimaryKey: []string{"b", "a"}, Columns: []*protocol.DeclaredColumn{{Name: "a"}, {Name: "b"}}}},
		{Desired: &protocol.DeclaredTable{Name: "t", Columns: []*protocol.DeclaredColumn{{Name: "x"}, {Name: "id", Primary: true}}}},
	}}
	result, err := backend.New().PrepareTables(context.Background(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.GetTables(), qt.HasLen, 2)
	c.Assert(result.GetTables()[0].GetColumnPrimaryKeys(), qt.DeepEquals, []string{"b", "a"})
	c.Assert(result.GetTables()[1].GetColumnPrimaryKeys(), qt.DeepEquals, []string{"id"})
	c.Assert(result.GetTables()[1].GetColumnPrimaryKeysPrepared(), qt.IsTrue)
}

// Under the index NULLS order, a prepared table states each declared index's
// key parts in the form a read states them: FIRST or LAST only where the
// order is not Spanner's default for the direction, NULLS LAST ascending and
// NULLS FIRST descending. A key listed as fields is in the default order.
func TestPrepareTables_StatesIndexNullsOrders(t *testing.T) {
	c := testcheck.New(t)
	request := &protocol.PrepareTablesRequest{Engine: "spanner", TargetFeatures: backend.Description().GetEngines()[0].GetFeatures(),
		Tables: []*protocol.TablePreparationInput{{Desired: &protocol.DeclaredTable{Name: "t", Columns: []*protocol.DeclaredColumn{{Name: "a"}, {Name: "b"}},
			Indexes: []*protocol.DeclaredIndex{
				{Name: "t_fields", Fields: []string{"a", "b"}},
				{Name: "t_parts", Parts: []*protocol.CapturedIndexPart{
					{Column: "a", Nulls: "first"}, {Column: "b", Descending: true, Nulls: "LAST"},
					{Column: "a", Nulls: "LAST"}, {Column: "b", Descending: true, Nulls: "FIRST"}, {Column: "a"},
				}},
			}}}}}
	result, err := backend.New().PrepareTables(context.Background(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(provider.ValidatePreparedTablesReply(backend.Description(), request, result), qt.IsNil)
	indexes := result.GetTables()[0].GetIndexes()
	c.Assert(indexes, qt.HasLen, 2)
	c.Assert(indexes[0].GetName(), qt.Equals, "t_fields")
	c.Assert(indexes[0].GetNulls(), qt.DeepEquals, []string{"", ""})
	c.Assert(indexes[1].GetName(), qt.Equals, "t_parts")
	c.Assert(indexes[1].GetNulls(), qt.DeepEquals, []string{"FIRST", "LAST", "", "", ""})
}

// A host that did not negotiate the NULLS order gets no prepared index.
func TestPrepareTables_StatesNoIndexWithoutTheFeature(t *testing.T) {
	c := testcheck.New(t)
	request := &protocol.PrepareTablesRequest{Engine: "spanner", Tables: []*protocol.TablePreparationInput{{Desired: &protocol.DeclaredTable{Name: "t",
		Columns: []*protocol.DeclaredColumn{{Name: "a"}}, Indexes: []*protocol.DeclaredIndex{{Name: "t_a", Fields: []string{"a"}}}}}}}
	result, err := backend.New().PrepareTables(context.Background(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.GetTables()[0].GetIndexes(), qt.HasLen, 0)
}

// The declarations Spanner cannot hold are reported before anything runs,
// each with its reason; a table Spanner can hold reports nothing.
func TestValidateSchema(t *testing.T) {
	c := testcheck.New(t)
	table := func(name string, declared *protocol.DeclaredTable) *protocol.ValidationTable {
		declared.Name = name
		return &protocol.ValidationTable{Subject: &protocol.ComparisonIdentity{Kind: "table", Schema: &protocol.ComparisonPart{Source: "public", Normalized: "public"},
			Name: &protocol.ComparisonPart{Source: name, Normalized: name}}, Declaration: declared}
	}
	request := &protocol.ValidateSchemaRequest{Engine: "spanner", Tables: []*protocol.ValidationTable{
		table("good", &protocol.DeclaredTable{Columns: []*protocol.DeclaredColumn{{Name: "id", Type: "bigint", Primary: true}, {Name: "j", Type: "jsonb", Nullable: true}}}),
		table("keyless", &protocol.DeclaredTable{Columns: []*protocol.DeclaredColumn{{Name: "id", Type: "bigint"}}}),
		table("typed", &protocol.DeclaredTable{Columns: []*protocol.DeclaredColumn{{Name: "id", Type: "bigint", Primary: true}, {Name: "n", Type: "int4"}}}),
		table("serial", &protocol.DeclaredTable{Columns: []*protocol.DeclaredColumn{{Name: "id", Type: "bigint", Primary: true, AutoIncrement: true}}}),
		table("filtered", &protocol.DeclaredTable{Columns: []*protocol.DeclaredColumn{{Name: "id", Type: "bigint", Primary: true}},
			Indexes: []*protocol.DeclaredIndex{{Name: "f", Fields: []string{"id"}, Condition: "id > 0"}}}),
		table("refs", &protocol.DeclaredTable{Columns: []*protocol.DeclaredColumn{{Name: "id", Type: "bigint", Primary: true},
			{Name: "j", Type: "jsonb", Nullable: true, ForeignReference: "good(j)"}, {Name: "g", Type: "bigint", Nullable: true, ForeignReference: "good(id)", OnUpdate: "CASCADE"}}}),
	}}
	result, err := backend.New().ValidateSchema(context.Background(), request)
	c.Assert(err, qt.IsNil)
	messages := make([]string, len(result.GetDiagnostics()))
	for i, diagnostic := range result.GetDiagnostics() {
		messages[i] = diagnostic.GetMessage()
	}
	c.Assert(messages, qt.DeepEquals, []string{
		`column "n" of table "typed": type "int4": Spanner stores integers as bigint only`,
		`column "id" of table "serial" is an identity column, which the provider does not describe yet`,
		`index "f" of table "filtered" has a WHERE filter, which the provider does not create yet`,
		`foreign key "fk_refs_g" of table "refs" has ON UPDATE CASCADE; Spanner has no ON UPDATE action`,
	})
}

// A foreign key on a type Spanner keeps out of keys is refused as Ptah's
// built-in Spanner engine refused it.
func TestValidateSchema_NonKeyTypeInAForeignKey(t *testing.T) {
	c := testcheck.New(t)
	subject := func(name string) *protocol.ComparisonIdentity {
		return &protocol.ComparisonIdentity{Kind: "table", Schema: &protocol.ComparisonPart{Source: "public", Normalized: "public"}, Name: &protocol.ComparisonPart{Source: name, Normalized: name}}
	}
	request := &protocol.ValidateSchemaRequest{Engine: "spanner", Tables: []*protocol.ValidationTable{
		{Subject: subject("pj"), Declaration: &protocol.DeclaredTable{Name: "pj", Columns: []*protocol.DeclaredColumn{{Name: "id", Type: "bigint", Primary: true}, {Name: "j", Type: "jsonb", Nullable: true}}}},
		{Subject: subject("cj"), Declaration: &protocol.DeclaredTable{Name: "cj", Columns: []*protocol.DeclaredColumn{{Name: "id", Type: "bigint", Primary: true}, {Name: "j", Type: "jsonb", Nullable: true}},
			Constraints: []*protocol.DeclaredConstraint{{Name: "cj_fk", Type: "FOREIGN KEY", Columns: []string{"j"}, ForeignTable: "pj", ForeignColumns: []string{"j"}}}}},
	}}
	result, err := backend.New().ValidateSchema(context.Background(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.GetDiagnostics(), qt.HasLen, 1)
	c.Assert(result.GetDiagnostics()[0].GetMessage(), qt.Equals, `invalid foreign key: Spanner type jsonb cannot participate in foreign keys: "cj"."j" references "pj"."j"`)
	c.Assert(result.GetDiagnostics()[0].GetCode(), qt.Equals, protocol.SchemaDiagnostic_INVALID_SCHEMA)
}

// An index comment is refused as a table's and a column's are: Spanner's
// PostgreSQL interface answers COMMENT ON INDEX with `Unknown statement`, so a
// comment the declaration states could never be read back.
func TestValidateSchema_AnIndexComment(t *testing.T) {
	c := testcheck.New(t)
	request := &protocol.ValidateSchemaRequest{Engine: "spanner", Tables: []*protocol.ValidationTable{{
		Subject: &protocol.ComparisonIdentity{Kind: "table", Schema: &protocol.ComparisonPart{Source: "public", Normalized: "public"},
			Name: &protocol.ComparisonPart{Source: "u", Normalized: "u"}},
		Declaration: &protocol.DeclaredTable{Name: "u", Columns: []*protocol.DeclaredColumn{{Name: "id", Type: "bigint", Primary: true}, {Name: "n", Type: "bigint", Nullable: true}},
			Indexes: []*protocol.DeclaredIndex{{Name: "u_n_ix", Fields: []string{"n"}, Comment: "first"}}},
	}}}
	result, err := backend.New().ValidateSchema(context.Background(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.GetDiagnostics(), qt.HasLen, 1)
	c.Assert(result.GetDiagnostics()[0].GetMessage(), qt.Equals, `index "u_n_ix" of table "u" has a comment; Spanner's PostgreSQL interface stores no comment`)
	c.Assert(result.GetDiagnostics()[0].GetCode(), qt.Equals, protocol.SchemaDiagnostic_UNSUPPORTED_FEATURE)
}

func TestNormalizeValues_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	result, err := backend.New().NormalizeValues(context.Background(), &protocol.NormalizeValuesRequest{Engine: "spanner", Values: []*protocol.ValueInput{
		{Id: "x", Representation: protocol.ValueInput_DESIRED, Value: &protocol.Envelope{Kind: "example.org/other"}},
	}})
	c.Assert(err, qt.ErrorMatches, `.*the provider normalizes its table operations and row deletion policies only.*`)
	c.Assert(result, qt.IsNil)
}
