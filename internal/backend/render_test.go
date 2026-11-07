package backend_test

import (
	"context"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/stokaro/ptah-spanner-plugin/internal/backend"
	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

func table(name string) *protocol.Reference {
	return &protocol.Reference{Engine: "spanner", Database: "app", Schema: "public", Kind: "table", Name: name}
}

func TestRender_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	request := &protocol.RenderRequest{Changes: []*protocol.Change{
		{Id: "1", Object: table("users"), Operation: &protocol.Change_CreateTable{CreateTable: &protocol.Table{Columns: []*protocol.Column{
			{Name: "id", Type: "BIGINT", PrimaryKey: true}, {Name: "email", Type: "VARCHAR(255)", Nullable: true},
		}}}},
		{Id: "2", Object: table("users"), Operation: &protocol.Change_AddColumn{AddColumn: &protocol.Column{Name: "note", Type: "TEXT", Nullable: true}}},
		{Id: "3", Object: table("users"), Operation: &protocol.Change_DropColumn{DropColumn: "email"}},
		{Id: "4", Object: table("old"), Operation: &protocol.Change_DropTable{DropTable: &protocol.Empty{}}},
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
		"CREATE TABLE \"users\" (\n  \"id\" BIGINT NOT NULL,\n  \"email\" VARCHAR(255),\n  PRIMARY KEY (\"id\")\n);",
		`ALTER TABLE "users" ADD COLUMN "note" TEXT;`,
		`ALTER TABLE "users" DROP COLUMN "email";`,
		`DROP TABLE "old";`,
	})
}

// A change the provider cannot write refuses the whole batch, so the host
// never receives part of a plan.
func TestRender_FailurePath(t *testing.T) {
	tests := []struct {
		name    string
		change  *protocol.Change
		wantErr string
	}{
		{name: "a native operation", change: &protocol.Change{Id: "x", Object: table("t"), Operation: &protocol.Change_Native{Native: &protocol.NativeOperation{}}},
			wantErr: `.*the provider creates and drops tables and adds and drops columns; other changes come with later releases.*`},
		{name: "a primary key column added", change: &protocol.Change{Id: "x", Object: table("t"),
			Operation: &protocol.Change_AddColumn{AddColumn: &protocol.Column{Name: "k", Type: "bigint", PrimaryKey: true}}},
			wantErr: `.*Spanner cannot add a primary key column to an existing table.*`},
		{name: "a NOT NULL column added without a default", change: &protocol.Change{Id: "x", Object: table("t"),
			Operation: &protocol.Change_AddColumn{AddColumn: &protocol.Column{Name: "k", Type: "bigint"}}},
			wantErr: `.*NOT NULL without a default, which Spanner refuses to add to an existing table.*`},
		{name: "a type Spanner refuses", change: &protocol.Change{Id: "x", Object: table("t"),
			Operation: &protocol.Change_CreateTable{CreateTable: &protocol.Table{Columns: []*protocol.Column{{Name: "k", Type: "int4", PrimaryKey: true}}}}},
			wantErr: `.*Spanner stores integers as bigint only.*`},
		{name: "a table with engine settings", change: &protocol.Change{Id: "x", Object: table("t"),
			Operation: &protocol.Change_CreateTable{CreateTable: &protocol.Table{Columns: []*protocol.Column{{Name: "k", Type: "bigint"}}, Facets: []*protocol.Envelope{{}}}}},
			wantErr: `.*the provider creates a table from its columns alone.*`},
		{name: "a reference with a path", change: &protocol.Change{Id: "x", Object: &protocol.Reference{Name: "t", Path: []string{"a"}},
			Operation: &protocol.Change_DropTable{DropTable: &protocol.Empty{}}},
			wantErr: `.*a Spanner table has a schema and a name and nothing else.*`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			ok := &protocol.Change{Id: "ok", Object: table("other"), Operation: &protocol.Change_DropTable{DropTable: &protocol.Empty{}}}
			result, err := backend.New().Render(context.Background(), &protocol.RenderRequest{Changes: []*protocol.Change{ok, test.change}})
			c.Assert(status.Code(err), qt.Equals, codes.InvalidArgument)
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(result, qt.IsNil)
		})
	}
}
