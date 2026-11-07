package tableplan_test

import (
	"context"
	"slices"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-spanner-plugin/internal/backend"
	"github.com/stokaro/ptah-spanner-plugin/internal/rowdeletion"
	"github.com/stokaro/ptah-spanner-plugin/internal/tableop"
	"github.com/stokaro/ptah-spanner-plugin/internal/tableplan"
	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// subject is a table identity as the host builds one for the spanner engine.
func subject(name string) *protocol.ComparisonIdentity {
	return &protocol.ComparisonIdentity{Kind: "table", Database: &protocol.ComparisonPart{}, Catalog: &protocol.ComparisonPart{},
		Schema: &protocol.ComparisonPart{Source: "public", Normalized: "public", Defaulted: true}, Parent: &protocol.ComparisonPart{},
		Name: &protocol.ComparisonPart{Source: name, Normalized: name}}
}

// child is the identity of a column or an index of a table, as the host
// builds one: a column names its table as its parent, an index does not.
func child(table, kind, name string) *protocol.ComparisonIdentity {
	identity := proto.CloneOf(subject(table))
	identity.Kind, identity.Name = kind, &protocol.ComparisonPart{Source: name, Normalized: name}
	if kind == "column" {
		identity.Parent = proto.CloneOf(subject(table).GetName())
	}
	return identity
}

func effect(identity *protocol.ComparisonIdentity, action protocol.PlanningEffect_Action) *protocol.PlanningEffect {
	return &protocol.PlanningEffect{Subject: identity, Action: action}
}

// common is a host step on table with effects.
func common(name, table string, effects ...*protocol.PlanningEffect) *protocol.CommonPlanningStep {
	return &protocol.CommonPlanningStep{Id: &protocol.StepIdentity{Owner: "ptah.run/provider-common", Name: name}, Parent: subject(table),
		Metadata: &protocol.PlanningMetadata{Impact: protocol.ChangeSemantics_UNKNOWN, Effects: effects}}
}

func request(tables []*protocol.PlanningTable, steps ...*protocol.CommonPlanningStep) *protocol.PlanFeaturesRequest {
	return &protocol.PlanFeaturesRequest{Engine: "spanner", PlanningId: tableplan.ServiceID, ParentKinds: []string{rowdeletion.Kind},
		TargetFeatures: backend.Description().GetEngines()[0].GetFeatures(),
		Identifiers: &protocol.ComparisonIdentifiers{DefaultSchema: "public", Rules: &protocol.IdentifierRules{TableNames: protocol.IdentifierRules_EXACT,
			ColumnNames: protocol.IdentifierRules_EXACT, IndexNames: protocol.IdentifierRules_EXACT, IndexNamespace: protocol.IdentifierRules_SCHEMA}},
		Capabilities: map[string]bool{"transactional_ddl": false}, Tables: tables, CommonSteps: steps}
}

// planned is a planned step as a test reads it: its name, its statement and
// the notes it carries.
type planned struct {
	Name      string
	Statement string
	Notes     []string
}

// plan plans r, checks the reply as the SDK checks it, and returns the steps
// in the order the planner wrote them and the dependencies by step name.
func plan(c *qt.C, r *protocol.PlanFeaturesRequest) ([]planned, [][2]string, *protocol.PlannedFeatures) {
	c.Helper()
	result, err := tableplan.Plan(context.Background(), r)
	c.Assert(err, qt.IsNil)
	c.Assert(provider.ValidatePlannedFeaturesReply(backend.Description(), r, result), qt.IsNil)
	var steps []planned
	var dependencies [][2]string
	for _, contribution := range result.GetContributions() {
		for _, step := range contribution.GetSteps() {
			statement, err := tableop.Statement(step.GetOperation().GetValue())
			c.Assert(err, qt.IsNil)
			steps = append(steps, planned{Name: step.GetId().GetName(), Statement: statement, Notes: step.GetOperation().GetNotes()})
		}
		for _, dependency := range contribution.GetDependencies() {
			dependencies = append(dependencies, [2]string{dependency.GetBefore().GetName(), dependency.GetAfter().GetName()})
		}
	}
	return steps, dependencies, result
}

func statements(steps []planned) []string {
	result := make([]string, len(steps))
	for i, step := range steps {
		result[i] = step.Statement
	}
	return result
}

// A created table is one CREATE TABLE in place of the host's step, with its
// key, defaults, checks and enum columns, and an index is a step after it.
func TestPlan_CreatesATableAndItsIndexes(t *testing.T) {
	c := testcheck.New(t)
	desired := &protocol.DeclaredTable{Name: "authors", Columns: []*protocol.DeclaredColumn{
		{Name: "id", Type: "BIGINT", Nullable: true, Primary: true},
		{Name: "email", Type: "VARCHAR(255)"},
		{Name: "status", Type: "TEXT", DefaultSet: true, DefaultValue: "active", EnumValues: []string{"active", "retired"}},
		{Name: "score", Type: "bigint", DefaultSet: true, DefaultValue: "0", Check: "score >= 0"},
		{Name: "seen", Type: "timestamptz", Nullable: true, DefaultExpression: "CURRENT_TIMESTAMP"},
	}, Indexes: []*protocol.DeclaredIndex{
		{Name: "authors_email", Fields: []string{"email"}, Unique: true},
		{Name: "authors_score", Parts: []*protocol.CapturedIndexPart{{Column: "score", Descending: true}, {Column: "seen", Nulls: "first"}}, IncludeColumns: []string{"status"}},
	}}
	r := request([]*protocol.PlanningTable{{Subject: subject("authors"), Action: protocol.PlanningTable_CREATE, Desired: desired}},
		common("common/000000", "authors", effect(subject("authors"), protocol.PlanningEffect_CREATE)))
	steps, dependencies, result := plan(c, r)
	c.Assert(statements(steps), qt.DeepEquals, []string{
		"CREATE TABLE \"authors\" (\n  \"id\" BIGINT PRIMARY KEY NOT NULL,\n  \"email\" VARCHAR(255) NOT NULL,\n" +
			"  \"status\" TEXT NOT NULL DEFAULT 'active' CONSTRAINT \"authors_status_check\" CHECK (status IN ('active', 'retired')),\n" +
			"  \"score\" bigint NOT NULL DEFAULT 0 CONSTRAINT \"authors_score_check\" CHECK (score >= 0),\n" +
			"  \"seen\" timestamptz DEFAULT CURRENT_TIMESTAMP\n);",
		`CREATE UNIQUE INDEX "authors_email" ON "authors" ("email");`,
		`CREATE INDEX "authors_score" ON "authors" ("score" DESC, "seen" NULLS FIRST) INCLUDE ("status");`,
	})
	c.Assert(dependencies, qt.DeepEquals, [][2]string{{steps[0].Name, steps[1].Name}, {steps[0].Name, steps[2].Name}})
	c.Assert(result.GetRewrites(), qt.HasLen, 1)
	c.Assert(result.GetRewrites()[0].GetSources()[0].GetName(), qt.Equals, "common/000000")
	c.Assert(result.GetRewrites()[0].GetReplacement().GetName(), qt.Equals, steps[0].Name)
}

// A composite key is a clause in key order, and its columns keep the
// nullability they declare.
func TestPlan_CreatesACompositeKeyInKeyOrder(t *testing.T) {
	c := testcheck.New(t)
	desired := &protocol.DeclaredTable{Name: "edges", PrimaryKey: []string{"b", "a"}, Columns: []*protocol.DeclaredColumn{
		{Name: "a", Type: "bigint", Nullable: true}, {Name: "b", Type: "bigint", Nullable: true}, {Name: "note", Type: "text", Nullable: true},
	}}
	r := request([]*protocol.PlanningTable{{Subject: subject("edges"), Action: protocol.PlanningTable_CREATE, Desired: desired}},
		common("common/000000", "edges", effect(subject("edges"), protocol.PlanningEffect_CREATE)))
	steps, _, _ := plan(c, r)
	c.Assert(statements(steps), qt.DeepEquals, []string{"CREATE TABLE \"edges\" (\n  \"a\" bigint,\n  \"b\" bigint,\n  \"note\" text,\n  PRIMARY KEY (\"b\", \"a\")\n);"})
}

// Every foreign key is a statement of its own after the creation of both its
// tables, so two created tables may reference each other; a table CHECK
// follows its table.
func TestPlan_CreatesForeignKeysAfterBothTables(t *testing.T) {
	c := testcheck.New(t)
	x := &protocol.DeclaredTable{Name: "x", Columns: []*protocol.DeclaredColumn{
		{Name: "id", Type: "bigint", Primary: true}, {Name: "y_id", Type: "bigint", Nullable: true, ForeignReference: "y(id)", OnDelete: "CASCADE"},
	}, Constraints: []*protocol.DeclaredConstraint{
		{Name: "fk_x_y_id", Type: "FOREIGN KEY", Columns: []string{"y_id"}, ForeignTable: "y", ForeignColumns: []string{"id"}},
		{Name: "x_id_check", Type: "CHECK", CheckExpression: "id > 0"},
	}}
	y := &protocol.DeclaredTable{Name: "y", Columns: []*protocol.DeclaredColumn{
		{Name: "id", Type: "bigint", Primary: true}, {Name: "x_id", Type: "bigint", Nullable: true},
	}, Constraints: []*protocol.DeclaredConstraint{
		{Name: "y_x_fk", Type: "FOREIGN KEY", Columns: []string{"x_id"}, ForeignTable: "x", ForeignColumns: []string{"id"}},
	}}
	r := request([]*protocol.PlanningTable{
		{Subject: subject("x"), Action: protocol.PlanningTable_CREATE, Desired: x},
		{Subject: subject("y"), Action: protocol.PlanningTable_CREATE, Desired: y},
	}, common("common/000000", "x", effect(subject("x"), protocol.PlanningEffect_CREATE)), common("common/000001", "y", effect(subject("y"), protocol.PlanningEffect_CREATE)))
	steps, dependencies, _ := plan(c, r)
	c.Assert(statements(steps), qt.DeepEquals, []string{
		"CREATE TABLE \"x\" (\n  \"id\" bigint PRIMARY KEY NOT NULL,\n  \"y_id\" bigint\n);",
		"CREATE TABLE \"y\" (\n  \"id\" bigint PRIMARY KEY NOT NULL,\n  \"x_id\" bigint\n);",
		`ALTER TABLE "x" ADD CONSTRAINT "fk_x_y_id" FOREIGN KEY ("y_id") REFERENCES "y"("id") ON DELETE CASCADE;`,
		`ALTER TABLE "x" ADD CONSTRAINT "x_id_check" CHECK (id > 0);`,
		`ALTER TABLE "y" ADD CONSTRAINT "y_x_fk" FOREIGN KEY ("x_id") REFERENCES "x"("id");`,
	})
	c.Assert(dependencies, qt.DeepEquals, [][2]string{
		{steps[0].Name, steps[2].Name}, {steps[1].Name, steps[2].Name},
		{steps[0].Name, steps[3].Name},
		{steps[1].Name, steps[4].Name}, {steps[0].Name, steps[4].Name},
	})
}

// unqualified is a table identity without a schema, as the host builds one
// for an offline render, which has no session to read the search path from.
func unqualified(name string) *protocol.ComparisonIdentity {
	identity := subject(name)
	identity.Schema = &protocol.ComparisonPart{}
	return identity
}

// unqualifiedCreation is the host's step that creates table, named without a
// schema.
func unqualifiedCreation(name, table string) *protocol.CommonPlanningStep {
	step := common(name, table, effect(unqualified(table), protocol.PlanningEffect_CREATE))
	step.Parent = unqualified(table)
	return step
}

// A table an offline render names without a schema is a table of the default
// schema: its creation, its index and its foreign key are planned as for a
// table of public, and each operation names public as its table's schema.
func TestPlan_PlansATableWithoutASchemaInTheDefaultSchema(t *testing.T) {
	c := testcheck.New(t)
	parent := &protocol.DeclaredTable{Name: "p", Columns: []*protocol.DeclaredColumn{{Name: "id", Type: "bigint", Primary: true}},
		Indexes: []*protocol.DeclaredIndex{{Name: "p_id_idx", Fields: []string{"id"}}}}
	children := &protocol.DeclaredTable{Name: "ch", Columns: []*protocol.DeclaredColumn{
		{Name: "id", Type: "bigint", Primary: true}, {Name: "p_id", Type: "bigint", Nullable: true},
	}, Constraints: []*protocol.DeclaredConstraint{{Name: "ch_p_fk", Type: "FOREIGN KEY", Columns: []string{"p_id"}, ForeignTable: "p", ForeignColumns: []string{"id"}}}}
	r := request([]*protocol.PlanningTable{
		{Subject: unqualified("p"), Action: protocol.PlanningTable_CREATE, Desired: parent},
		{Subject: unqualified("ch"), Action: protocol.PlanningTable_CREATE, Desired: children},
	}, unqualifiedCreation("common/000000", "p"), unqualifiedCreation("common/000001", "ch"))
	steps, _, result := plan(c, r)
	c.Assert(statements(steps), qt.DeepEquals, []string{
		"CREATE TABLE \"p\" (\n  \"id\" bigint PRIMARY KEY NOT NULL\n);",
		`CREATE INDEX "p_id_idx" ON "p" ("id");`,
		"CREATE TABLE \"ch\" (\n  \"id\" bigint PRIMARY KEY NOT NULL,\n  \"p_id\" bigint\n);",
		`ALTER TABLE "ch" ADD CONSTRAINT "ch_p_fk" FOREIGN KEY ("p_id") REFERENCES "p"("id");`,
	})
	var schemas []string
	for _, contribution := range result.GetContributions() {
		for _, step := range contribution.GetSteps() {
			schemas = append(schemas, step.GetOperation().GetValue().GetValue().GetOwner().GetSchema())
		}
	}
	c.Assert(schemas, qt.DeepEquals, []string{"public", "public", "public", "public"})
}

// altered is a kept table t whose held side has columns a..e and the index
// t_old, and whose declaration is desired.
func altered(desired []*protocol.DeclaredColumn, indexes ...*protocol.DeclaredIndex) *protocol.PlanningTable {
	return &protocol.PlanningTable{Subject: subject("t"), Action: protocol.PlanningTable_ALTER,
		Desired: &protocol.DeclaredTable{Name: "t", Columns: append([]*protocol.DeclaredColumn{{Name: "id", Type: "bigint", Primary: true}}, desired...), Indexes: indexes},
		Current: &protocol.ObservedTable{Name: "t", Schema: "public", Columns: []*protocol.ObservedColumn{
			{Name: "id", DataType: "bigint", ColumnType: "bigint", IsNullable: "NO", PrimaryKey: true},
			{Name: "a", DataType: "character varying", ColumnType: "character varying(20)", IsNullable: "YES"},
			{Name: "b", DataType: "bigint", ColumnType: "bigint", IsNullable: "YES", DefaultExpression: new("'0'::bigint")},
			{Name: "c", DataType: "character varying", ColumnType: "character varying", IsNullable: "NO"},
			{Name: "d", DataType: "bigint", ColumnType: "bigint", IsNullable: "YES"},
		}, Constraints: []*protocol.ObservedConstraint{{Name: "PK_t", Type: "PRIMARY KEY", ColumnNames: []string{"id"}}},
			Indexes: []*protocol.ObservedIndex{{Name: "t_old", Columns: []string{"a"}}}}}
}

// A kept table's columns change one statement each: a type Spanner changes
// in place, a default set, changed and dropped, NOT NULL set with the note
// Ptah's built-in Spanner engine wrote and dropped. A column change follows
// the table's index removals and precedes its index additions.
func TestPlan_ChangesColumnsOfAKeptTable(t *testing.T) {
	c := testcheck.New(t)
	table := altered([]*protocol.DeclaredColumn{
		{Name: "a", Type: "varchar(40)"},
		{Name: "b", Type: "bigint", Nullable: true, DefaultSet: true, DefaultValue: "5"},
		{Name: "c", Type: "bytea", Nullable: true, DefaultSet: true, DefaultValue: "x"},
		{Name: "d", Type: "BIGINT", Nullable: true},
	}, &protocol.DeclaredIndex{Name: "t_new", Fields: []string{"a"}})
	r := request([]*protocol.PlanningTable{table},
		common("common/000000", "t", effect(subject("t"), protocol.PlanningEffect_READ), effect(child("t", "index", "t_old"), protocol.PlanningEffect_DROP)),
		common("common/000001", "t", effect(subject("t"), protocol.PlanningEffect_READ), effect(child("t", "index", "t_new"), protocol.PlanningEffect_CREATE)))
	steps, dependencies, result := plan(c, r)
	c.Assert(statements(steps), qt.DeepEquals, []string{
		`DROP INDEX "t_old";`,
		`CREATE INDEX "t_new" ON "t" ("a");`,
		`ALTER TABLE "t" ALTER COLUMN "a" TYPE varchar(40);`,
		`ALTER TABLE "t" ALTER COLUMN "a" SET NOT NULL;`,
		`ALTER TABLE "t" ALTER COLUMN "b" SET DEFAULT 5;`,
		`ALTER TABLE "t" ALTER COLUMN "c" TYPE bytea;`,
		`ALTER TABLE "t" ALTER COLUMN "c" SET DEFAULT 'x';`,
		`ALTER TABLE "t" ALTER COLUMN "c" DROP NOT NULL;`,
	})
	c.Assert(steps[3].Notes, qt.DeepEquals, []string{`SPANNER: SET NOT NULL fails if any row of "t" holds NULL in "a"; the column declares no default to fill it with.`})
	c.Assert(result.GetRewrites(), qt.HasLen, 2)
	for _, change := range steps[2:] {
		c.Assert(dependencies, qt.Contains, [2]string{steps[0].Name, change.Name}, qt.Commentf("%s after the index removal", change.Statement))
		c.Assert(dependencies, qt.Contains, [2]string{change.Name, steps[1].Name}, qt.Commentf("%s before the index addition", change.Statement))
	}
}

// A NOT NULL column without a default is added nullable and then made NOT
// NULL, because Spanner refuses to add it otherwise; a dropped column is a
// statement of its own.
func TestPlan_AddsAndDropsColumns(t *testing.T) {
	c := testcheck.New(t)
	table := altered([]*protocol.DeclaredColumn{
		{Name: "a", Type: "varchar(20)", Nullable: true}, {Name: "b", Type: "bigint", Nullable: true, DefaultSet: true, DefaultValue: "0"},
		{Name: "c", Type: "text"}, {Name: "e", Type: "bigint"}, {Name: "f", Type: "bigint", DefaultSet: true, DefaultValue: "1"},
	})
	r := request([]*protocol.PlanningTable{table},
		&protocol.CommonPlanningStep{Id: &protocol.StepIdentity{Owner: "ptah.run/provider-common", Name: "common/000000"}, Parent: subject("t"),
			AddedColumn: &protocol.PlanningColumn{Name: "e", Type: "bigint"},
			Metadata:    &protocol.PlanningMetadata{Effects: []*protocol.PlanningEffect{effect(subject("t"), protocol.PlanningEffect_READ), effect(child("t", "column", "e"), protocol.PlanningEffect_CREATE)}}},
		&protocol.CommonPlanningStep{Id: &protocol.StepIdentity{Owner: "ptah.run/provider-common", Name: "common/000001"}, Parent: subject("t"),
			AddedColumn: &protocol.PlanningColumn{Name: "f", Type: "bigint"},
			Metadata:    &protocol.PlanningMetadata{Effects: []*protocol.PlanningEffect{effect(subject("t"), protocol.PlanningEffect_READ), effect(child("t", "column", "f"), protocol.PlanningEffect_CREATE)}}},
		common("common/000002", "t", effect(subject("t"), protocol.PlanningEffect_READ), effect(child("t", "column", "d"), protocol.PlanningEffect_DROP)))
	steps, dependencies, _ := plan(c, r)
	c.Assert(statements(steps), qt.DeepEquals, []string{
		`ALTER TABLE "t" ADD COLUMN "e" bigint;`,
		`ALTER TABLE "t" ALTER COLUMN "e" SET NOT NULL;`,
		`ALTER TABLE "t" ADD COLUMN "f" bigint NOT NULL DEFAULT 1;`,
		`ALTER TABLE "t" DROP COLUMN "d";`,
	})
	c.Assert(dependencies, qt.DeepEquals, [][2]string{{steps[0].Name, steps[1].Name}})
}

// A dropped table loses its indexes first: Spanner refuses to drop a table
// that has one.
func TestPlan_DropsATableAfterItsIndexes(t *testing.T) {
	c := testcheck.New(t)
	table := &protocol.PlanningTable{Subject: subject("t"), Action: protocol.PlanningTable_DROP, Current: &protocol.ObservedTable{Name: "t", Schema: "public",
		Columns: []*protocol.ObservedColumn{{Name: "id", ColumnType: "bigint", IsNullable: "NO"}}, Indexes: []*protocol.ObservedIndex{{Name: "t_a"}, {Name: "t_b"}}}}
	r := request([]*protocol.PlanningTable{table}, common("common/000000", "t", effect(subject("t"), protocol.PlanningEffect_DROP)))
	steps, dependencies, _ := plan(c, r)
	c.Assert(statements(steps), qt.DeepEquals, []string{`DROP TABLE "t";`, `DROP INDEX "t_a";`, `DROP INDEX "t_b";`})
	c.Assert(dependencies, qt.DeepEquals, [][2]string{{steps[1].Name, steps[0].Name}, {steps[2].Name, steps[0].Name}})
}

// A table the plan keeps as context gets no step and no receipt.
func TestPlan_LeavesContextTablesAlone(t *testing.T) {
	c := testcheck.New(t)
	r := request([]*protocol.PlanningTable{{Subject: subject("t"), Action: protocol.PlanningTable_CONTEXT, Desired: &protocol.DeclaredTable{Name: "t"}}})
	r.ParentKinds = nil
	steps, _, result := plan(c, r)
	c.Assert(steps, qt.HasLen, 0)
	c.Assert(result.GetParents(), qt.HasLen, 0)
}

// A change Spanner cannot make is a completed refusal naming the table and
// why, before any statement is planned.
func TestPlan_Refusals(t *testing.T) {
	keyChange := altered([]*protocol.DeclaredColumn{{Name: "a", Type: "varchar(20)", Nullable: true}, {Name: "b", Type: "bigint", Nullable: true},
		{Name: "c", Type: "text"}, {Name: "d", Type: "bigint", Nullable: true}})
	keyChange.Desired.PrimaryKey = []string{"id", "b"}
	typeChange := altered([]*protocol.DeclaredColumn{{Name: "a", Type: "varchar(20)", Nullable: true}, {Name: "b", Type: "double precision", Nullable: true},
		{Name: "c", Type: "text"}, {Name: "d", Type: "bigint", Nullable: true}})
	keyType := altered([]*protocol.DeclaredColumn{{Name: "a", Type: "varchar(20)", Nullable: true}, {Name: "b", Type: "bigint", Nullable: true},
		{Name: "c", Type: "text"}, {Name: "d", Type: "bigint", Nullable: true}})
	keyType.Desired.Columns[0].Type = "varchar(10)"
	tests := []struct {
		name, want string
		table      *protocol.PlanningTable
	}{
		{name: "a primary key change", table: keyChange,
			want: `changing the primary key of table "t" from (id) to (id, b): Spanner cannot change a table's primary key; create a new table with the key and copy the rows`},
		{name: "a type Spanner does not change", table: typeChange,
			want: `changing column "b" of table "t" from bigint to double precision: Spanner changes a column's type only between character varying and bytea, or the length of character varying`},
		{name: "a key column's type", table: keyType,
			want: `changing key column "id" of table "t" from bigint to character varying(10): Spanner cannot change a key column`},
		{name: "a rebuild", table: &protocol.PlanningTable{Subject: subject("t"), Action: protocol.PlanningTable_REBUILD, Desired: keyChange.GetDesired(), Current: keyChange.GetCurrent()},
			want: `table "t": Spanner cannot rebuild a table, and the provider plans no rebuild`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			r := request([]*protocol.PlanningTable{test.table})
			result, err := tableplan.Plan(context.Background(), r)
			c.Assert(err, qt.IsNil)
			c.Assert(provider.ValidatePlannedFeaturesReply(backend.Description(), r, result), qt.IsNil)
			c.Assert(result.GetDiagnostics(), qt.HasLen, 1)
			c.Assert(result.GetDiagnostics()[0].GetMessage(), qt.Equals, test.want)
			c.Assert(result.GetDiagnostics()[0].GetParent(), qt.Equals, uint32(0))
			c.Assert(result.GetContributions(), qt.HasLen, 0)
		})
	}
}

// A request the service cannot read is an error, not a refusal.
func TestPlan_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	r := request([]*protocol.PlanningTable{{Subject: subject("t"), Action: protocol.PlanningTable_CREATE, Desired: &protocol.DeclaredTable{Name: "t",
		Columns: []*protocol.DeclaredColumn{{Name: "id", Type: "bigint", Primary: true}}}}})
	result, err := tableplan.Plan(context.Background(), r)
	c.Assert(err, qt.ErrorMatches, `the host planned no creation of table "t"`)
	c.Assert(result, qt.IsNil)
	r.PlanningId = "other"
	result, err = tableplan.Plan(context.Background(), r)
	c.Assert(err, qt.ErrorMatches, `the request names another planning service`)
	c.Assert(result, qt.IsNil)
}

// schemaTablesRequest creates tables t and u in schema app and p in the
// default schema.
func schemaTablesRequest() *protocol.PlanFeaturesRequest {
	inSchema := func(schema, name string) *protocol.ComparisonIdentity {
		identity := subject(name)
		identity.Schema = &protocol.ComparisonPart{Source: schema, Normalized: schema}
		return identity
	}
	declared := func(name string) *protocol.DeclaredTable {
		return &protocol.DeclaredTable{Name: name, Columns: []*protocol.DeclaredColumn{{Name: "id", Type: "bigint", Primary: true}}}
	}
	createStep := func(name string, identity *protocol.ComparisonIdentity) *protocol.CommonPlanningStep {
		return &protocol.CommonPlanningStep{Id: &protocol.StepIdentity{Owner: "ptah.run/provider-common", Name: name}, Parent: identity,
			Metadata: &protocol.PlanningMetadata{Effects: []*protocol.PlanningEffect{effect(identity, protocol.PlanningEffect_CREATE)}}}
	}
	return request([]*protocol.PlanningTable{
		{Subject: inSchema("app", "t"), Action: protocol.PlanningTable_CREATE, Desired: declared("t")},
		{Subject: inSchema("app", "u"), Action: protocol.PlanningTable_CREATE, Desired: declared("u")},
		{Subject: subject("p"), Action: protocol.PlanningTable_CREATE, Desired: declared("p")},
	}, createStep("common/000000", inSchema("app", "t")), createStep("common/000001", inSchema("app", "u")), createStep("common/000002", subject("p")))
}

// For a host that does not carry schemas, a created table in a schema of its
// own follows CREATE SCHEMA IF NOT EXISTS, written once for every table of
// that schema; a table of the default schema needs none.
func TestPlan_CreatesTheSchemaOfATable(t *testing.T) {
	c := testcheck.New(t)
	r := schemaTablesRequest()
	r.TargetFeatures = slices.DeleteFunc(r.TargetFeatures, func(feature string) bool { return feature == provider.CommonNamespacesFeature })
	steps, dependencies, _ := plan(c, r)
	c.Assert(statements(steps), qt.DeepEquals, []string{
		"CREATE TABLE \"app\".\"t\" (\n  \"id\" bigint PRIMARY KEY NOT NULL\n);",
		`CREATE SCHEMA IF NOT EXISTS "app";`,
		"CREATE TABLE \"app\".\"u\" (\n  \"id\" bigint PRIMARY KEY NOT NULL\n);",
		"CREATE TABLE \"p\" (\n  \"id\" bigint PRIMARY KEY NOT NULL\n);",
	})
	c.Assert(dependencies, qt.DeepEquals, [][2]string{{steps[1].Name, steps[0].Name}, {steps[1].Name, steps[2].Name}})
}

// A host that carries schemas (schema.common-namespaces) plans the creation
// of a created table's schema itself, so the service plans the tables alone.
func TestPlan_LeavesTheSchemaToAHostThatCarriesSchemas(t *testing.T) {
	c := testcheck.New(t)
	steps, dependencies, _ := plan(c, schemaTablesRequest())
	c.Assert(statements(steps), qt.DeepEquals, []string{
		"CREATE TABLE \"app\".\"t\" (\n  \"id\" bigint PRIMARY KEY NOT NULL\n);",
		"CREATE TABLE \"app\".\"u\" (\n  \"id\" bigint PRIMARY KEY NOT NULL\n);",
		"CREATE TABLE \"p\" (\n  \"id\" bigint PRIMARY KEY NOT NULL\n);",
	})
	c.Assert(dependencies, qt.HasLen, 0)
}

// A declared WHERE clause becomes NOT NULL tests in byte order of the column
// names, each quoted, for a host that compares index predicates.
func TestPlan_CreatesAFilteredIndex(t *testing.T) {
	c := testcheck.New(t)
	desired := &protocol.DeclaredTable{Name: "t", Columns: []*protocol.DeclaredColumn{
		{Name: "id", Type: "bigint", Primary: true}, {Name: "b", Type: "bigint", Nullable: true}, {Name: "Abc", Type: "bigint", Nullable: true},
	}, Indexes: []*protocol.DeclaredIndex{{Name: "t_f", Fields: []string{"b", "Abc"}, Condition: `b IS NOT NULL AND "Abc" IS NOT NULL`}}}
	r := request([]*protocol.PlanningTable{{Subject: subject("t"), Action: protocol.PlanningTable_CREATE, Desired: desired}},
		common("common/000000", "t", effect(subject("t"), protocol.PlanningEffect_CREATE)))
	steps, _, _ := plan(c, r)
	c.Assert(statements(steps)[1:], qt.DeepEquals, []string{`CREATE INDEX "t_f" ON "t" ("b", "Abc") WHERE "Abc" IS NOT NULL AND "b" IS NOT NULL;`})
}

// A host that does not compare index predicates gets a declared WHERE clause
// refused, because it would rebuild the index on every plan.
func TestPlan_RefusesAFilteredIndexWithoutPredicates(t *testing.T) {
	c := testcheck.New(t)
	desired := &protocol.DeclaredTable{Name: "t", Columns: []*protocol.DeclaredColumn{{Name: "id", Type: "bigint", Primary: true}, {Name: "b", Type: "bigint", Nullable: true}},
		Indexes: []*protocol.DeclaredIndex{{Name: "t_f", Fields: []string{"b"}, Condition: "b IS NOT NULL"}}}
	r := request([]*protocol.PlanningTable{{Subject: subject("t"), Action: protocol.PlanningTable_CREATE, Desired: desired}},
		common("common/000000", "t", effect(subject("t"), protocol.PlanningEffect_CREATE)))
	r.TargetFeatures = slices.DeleteFunc(r.TargetFeatures, func(feature string) bool { return feature == provider.IndexPredicatesFeature })
	result, err := tableplan.Plan(context.Background(), r)
	c.Assert(err, qt.IsNil)
	c.Assert(result.GetDiagnostics(), qt.HasLen, 1)
	c.Assert(result.GetDiagnostics()[0].GetMessage(), qt.Equals,
		`table "t": index "t_f" has a WHERE filter, which needs a host that compares index predicates (schema.index-predicates)`)
}
