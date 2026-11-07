package backend_test

import (
	"context"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-spanner-plugin/internal/backend"
	"github.com/stokaro/ptah-spanner-plugin/internal/tableop"
	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// native is the change that runs operation on table, as the host renders a
// planned step: a standalone statement, or an ALTER TABLE naming its table
// as the parent, which the host names without its default schema.
func native(c *qt.C, id, table string, role protocol.NativeOperation_Role, operation tableop.Operation) *protocol.Change {
	c.Helper()
	value, err := tableop.Capture(id, tableop.Owner("public", table), operation)
	c.Assert(err, qt.IsNil)
	change := &protocol.Change{Id: id, Object: &protocol.Reference{Engine: "spanner", Kind: "table", Name: table},
		Operation: &protocol.Change_Native{Native: &protocol.NativeOperation{Value: value, Role: role}}}
	if role == protocol.NativeOperation_ALTER_TABLE {
		change.GetNative().Parent = &protocol.Reference{Engine: "spanner", Kind: "table", Name: table}
	}
	return change
}

func TestRender_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	request := &protocol.RenderRequest{Engine: "spanner", Changes: []*protocol.Change{
		native(c, "1", "users", protocol.NativeOperation_STATEMENT, tableop.Operation{Action: tableop.CreateTable, PrimaryKey: []string{"id"},
			Columns: []tableop.Column{{Name: "id", Type: "BIGINT"}, {Name: "email", Type: "VARCHAR(255)", Nullable: true}}}),
		native(c, "2", "users", protocol.NativeOperation_ALTER_TABLE, tableop.Operation{Action: tableop.AddColumn, Column: &tableop.Column{Name: "note", Type: "TEXT", Nullable: true}}),
		native(c, "3", "users", protocol.NativeOperation_STATEMENT, tableop.Operation{Action: tableop.CreateIndex, Index: &tableop.Index{Name: "users_email",
			Keys: []tableop.IndexKey{{Column: "email"}}}}),
		native(c, "4", "old", protocol.NativeOperation_STATEMENT, tableop.Operation{Action: tableop.DropTable}),
	}}
	result, err := backend.New().Render(context.Background(), request)
	c.Assert(err, qt.IsNil)
	c.Assert(result.GetComplete(), qt.IsTrue)
	statements := make([]string, len(result.GetUnits()))
	changes := make([]string, len(result.GetUnits()))
	for i, unit := range result.GetUnits() {
		statements[i], changes[i] = unit.GetSql(), unit.GetChangeId()
	}
	c.Assert(changes, qt.DeepEquals, []string{"1", "2", "3", "4"})
	c.Assert(statements, qt.DeepEquals, []string{
		"CREATE TABLE \"users\" (\n  \"id\" BIGINT PRIMARY KEY NOT NULL,\n  \"email\" VARCHAR(255)\n);",
		`ALTER TABLE "users" ADD COLUMN "note" TEXT;`,
		`CREATE INDEX "users_email" ON "users" ("email");`,
		`DROP TABLE "old";`,
	})
}

// A change the provider cannot write refuses the whole batch, so the host
// never receives part of a plan.
func TestRender_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	changed := native(c, "x", "t", protocol.NativeOperation_STATEMENT, tableop.Operation{Action: tableop.DropTable})
	changed.GetNative().GetValue().CanonicalProjection = []byte(`DROP TABLE "other";`)
	foreignParent := native(c, "x", "t", protocol.NativeOperation_ALTER_TABLE, tableop.Operation{Action: tableop.DropColumn, Name: "c"})
	foreignParent.GetNative().Parent = &protocol.Reference{Engine: "spanner", Kind: "table", Name: "u"}
	standaloneParent := native(c, "x", "t", protocol.NativeOperation_STATEMENT, tableop.Operation{Action: tableop.DropTable})
	standaloneParent.GetNative().Parent = &protocol.Reference{Engine: "spanner", Kind: "table", Name: "t"}
	tests := []struct {
		name    string
		change  *protocol.Change
		wantErr string
	}{
		{name: "a common table change", change: &protocol.Change{Id: "x", Operation: &protocol.Change_DropTable{DropTable: &protocol.Empty{}}},
			wantErr: `.*the provider renders the table operations it plans and nothing else.*`},
		{name: "an operation changed after planning", change: changed, wantErr: `.*a Spanner table operation changed after it was planned.*`},
		{name: "an ALTER TABLE of another table", change: foreignParent, wantErr: `.*a table operation names its own table as its parent.*`},
		{name: "a statement with a parent", change: standaloneParent, wantErr: `.*a standalone table operation has no parent.*`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			ok := native(c, "ok", "other", protocol.NativeOperation_STATEMENT, tableop.Operation{Action: tableop.DropTable})
			result, err := backend.New().Render(context.Background(), &protocol.RenderRequest{Changes: []*protocol.Change{ok, proto.CloneOf(test.change)}})
			c.Assert(status.Code(err), qt.Equals, codes.InvalidArgument)
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(result, qt.IsNil)
		})
	}
}
