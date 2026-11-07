package tableop_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-spanner-plugin/internal/tableop"
	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// Each action writes one statement of Spanner's PostgreSQL interface, and
// says what it does to the rows of its table.
func TestCapture_HappyPath(t *testing.T) {
	tests := []struct {
		name      string
		schema    string
		operation tableop.Operation
		statement string
		impact    protocol.ChangeSemantics_Impact
	}{
		{name: "create a table", schema: "public", operation: tableop.Operation{Action: tableop.CreateTable, PrimaryKey: []string{"id"},
			Columns: []tableop.Column{{Name: "id", Type: "bigint"}, {Name: "n", Type: "bigint", Default: new("0"), Check: "n >= 0", CheckName: "t_n_check"}}},
			statement: "CREATE TABLE \"t\" (\n  \"id\" bigint PRIMARY KEY NOT NULL,\n  \"n\" bigint NOT NULL DEFAULT 0 CONSTRAINT \"t_n_check\" CHECK (n >= 0)\n);",
			impact:    protocol.ChangeSemantics_ADDITIVE},
		{name: "drop a table", schema: "app", operation: tableop.Operation{Action: tableop.DropTable}, statement: `DROP TABLE "app"."t";`,
			impact: protocol.ChangeSemantics_DESTRUCTIVE},
		{name: "add a column", schema: "public", operation: tableop.Operation{Action: tableop.AddColumn, Column: &tableop.Column{Name: "c", Type: "text", Nullable: true}},
			statement: `ALTER TABLE "t" ADD COLUMN "c" text;`, impact: protocol.ChangeSemantics_ADDITIVE},
		{name: "drop a column", schema: "public", operation: tableop.Operation{Action: tableop.DropColumn, Name: "c"}, statement: `ALTER TABLE "t" DROP COLUMN "c";`,
			impact: protocol.ChangeSemantics_DESTRUCTIVE},
		{name: "change a type", schema: "public", operation: tableop.Operation{Action: tableop.AlterColumnType, Name: "c", Type: "varchar(40)"},
			statement: `ALTER TABLE "t" ALTER COLUMN "c" TYPE varchar(40);`, impact: protocol.ChangeSemantics_BEHAVIORAL},
		{name: "set a default", schema: "public", operation: tableop.Operation{Action: tableop.SetDefault, Name: "c", Default: "5"},
			statement: `ALTER TABLE "t" ALTER COLUMN "c" SET DEFAULT 5;`, impact: protocol.ChangeSemantics_ADDITIVE},
		{name: "drop a default", schema: "public", operation: tableop.Operation{Action: tableop.DropDefault, Name: "c"},
			statement: `ALTER TABLE "t" ALTER COLUMN "c" DROP DEFAULT;`, impact: protocol.ChangeSemantics_ADDITIVE},
		{name: "set NOT NULL", schema: "public", operation: tableop.Operation{Action: tableop.SetNotNull, Name: "c"},
			statement: `ALTER TABLE "t" ALTER COLUMN "c" SET NOT NULL;`, impact: protocol.ChangeSemantics_BEHAVIORAL},
		{name: "drop NOT NULL", schema: "public", operation: tableop.Operation{Action: tableop.DropNotNull, Name: "c"},
			statement: `ALTER TABLE "t" ALTER COLUMN "c" DROP NOT NULL;`, impact: protocol.ChangeSemantics_ADDITIVE},
		{name: "create an index", schema: "public", operation: tableop.Operation{Action: tableop.CreateIndex, Index: &tableop.Index{Name: "t_i",
			Keys: []tableop.IndexKey{{Column: "a", Descending: true}, {Column: "b", Nulls: "FIRST"}}, Storing: []string{"c"}}},
			statement: `CREATE INDEX "t_i" ON "t" ("a" DESC, "b" NULLS FIRST) INCLUDE ("c");`, impact: protocol.ChangeSemantics_ADDITIVE},
		{name: "create a unique index", schema: "public", operation: tableop.Operation{Action: tableop.CreateIndex, Index: &tableop.Index{Name: "t_u", Unique: true,
			Keys: []tableop.IndexKey{{Column: "a"}}}}, statement: `CREATE UNIQUE INDEX "t_u" ON "t" ("a");`, impact: protocol.ChangeSemantics_BEHAVIORAL},
		{name: "drop an index", schema: "app", operation: tableop.Operation{Action: tableop.DropIndex, Name: "t_i"}, statement: `DROP INDEX "app"."t_i";`,
			impact: protocol.ChangeSemantics_ADDITIVE},
		{name: "add a foreign key", schema: "public", operation: tableop.Operation{Action: tableop.AddForeignKey, ForeignKey: &tableop.ForeignKey{Name: "f",
			Columns: []string{"p_id"}, RefSchema: "public", RefTable: "p", RefColumns: []string{"id"}, OnDelete: "cascade"}},
			statement: `ALTER TABLE "t" ADD CONSTRAINT "f" FOREIGN KEY ("p_id") REFERENCES "p"("id") ON DELETE CASCADE;`, impact: protocol.ChangeSemantics_BEHAVIORAL},
		{name: "add a check", schema: "public", operation: tableop.Operation{Action: tableop.AddCheck, Name: "k", Expression: "a > 0"},
			statement: `ALTER TABLE "t" ADD CONSTRAINT "k" CHECK (a > 0);`, impact: protocol.ChangeSemantics_BEHAVIORAL},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			value, err := tableop.Capture("step", tableop.Owner(test.schema, "t"), test.operation)
			c.Assert(err, qt.IsNil)
			c.Assert(value.GetOperationSemantics().GetImpact(), qt.Equals, test.impact)
			c.Assert(value.GetOperationSemantics().GetAccess(), qt.Equals, protocol.ChangeSemantics_UNCHANGED)
			statement, err := tableop.Statement(value)
			c.Assert(err, qt.IsNil)
			c.Assert(statement, qt.Equals, test.statement)
		})
	}
}

// An operation that misses its operands, carries another action's operands,
// or names something other than a table is refused before it is planned.
func TestCapture_FailurePath(t *testing.T) {
	tests := []struct {
		name      string
		owner     *protocol.Reference
		operation tableop.Operation
		wantErr   string
	}{
		{name: "an unknown action", owner: tableop.Owner("public", "t"), operation: tableop.Operation{Action: "truncate"},
			wantErr: `a Spanner table operation has the unknown action "truncate"`},
		{name: "an operand of another action", owner: tableop.Owner("public", "t"), operation: tableop.Operation{Action: tableop.DropTable, Name: "x"},
			wantErr: `a drop-table operation does not take name`},
		{name: "a missing name", owner: tableop.Owner("public", "t"), operation: tableop.Operation{Action: tableop.DropColumn},
			wantErr: `a drop-column operation names nothing`},
		{name: "a missing index", owner: tableop.Owner("public", "t"), operation: tableop.Operation{Action: tableop.CreateIndex},
			wantErr: `a create-index operation names no index`},
		{name: "a NOT NULL column without a default", owner: tableop.Owner("public", "t"),
			operation: tableop.Operation{Action: tableop.AddColumn, Column: &tableop.Column{Name: "c", Type: "bigint"}},
			wantErr:   `column "c" of table "t" is NOT NULL without a default, which Spanner refuses to add to an existing table`},
		{name: "an owner without a schema", owner: &protocol.Reference{Engine: "spanner", Kind: "table", Name: "t"},
			operation: tableop.Operation{Action: tableop.DropTable}, wantErr: `a Spanner table operation names a table by its schema and name and nothing else`},
		{name: "an owner that is a view", owner: &protocol.Reference{Engine: "spanner", Kind: "view", Schema: "public", Name: "t"},
			operation: tableop.Operation{Action: tableop.DropTable}, wantErr: `a Spanner table operation names a table by its schema and name and nothing else`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			value, err := tableop.Capture("step", test.owner, test.operation)
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(value, qt.IsNil)
		})
	}
}

// A stored operation another model wrote, or one changed after planning,
// never runs.
func TestStatement_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	value, err := tableop.Capture("step", tableop.Owner("public", "t"), tableop.Operation{Action: tableop.DropTable})
	c.Assert(err, qt.IsNil)

	changed := proto.CloneOf(value)
	changed.CanonicalProjection = []byte(`DROP TABLE "other";`)
	statement, err := tableop.Statement(changed)
	c.Assert(err, qt.ErrorMatches, `a Spanner table operation changed after it was planned`)
	c.Assert(statement, qt.Equals, "")

	foreign := proto.CloneOf(value)
	foreign.Value.Kind = "github.com/stokaro/ptah-spanner-plugin/other"
	statement, err = tableop.Statement(foreign)
	c.Assert(err, qt.ErrorMatches, `a Spanner table operation does not match its model`)
	c.Assert(statement, qt.Equals, "")

	unknown := proto.CloneOf(value)
	unknown.Value.Payload = []byte(`{"action":"drop-table","extra":1}`)
	statement, err = tableop.Statement(unknown)
	c.Assert(err, qt.ErrorMatches, `a Spanner table operation is not one the provider writes: .*unknown field "extra"`)
	c.Assert(statement, qt.Equals, "")
}

func TestModel(t *testing.T) {
	c := testcheck.New(t)
	model := tableop.Model()
	c.Assert(model.GetKind(), qt.Equals, "github.com/stokaro/ptah-spanner-plugin/table-operation")
	c.Assert(model.GetRepresentation(), qt.Equals, protocol.ValueInput_OPERATION)
	c.Assert(model.GetDefinitionSha256(), qt.HasLen, 64)
}
