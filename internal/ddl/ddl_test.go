package ddl_test

import (
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-spanner-plugin/internal/ddl"
	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

func TestCreateTable_HappyPath(t *testing.T) {
	tests := []struct {
		name, schema, table string
		columns             []ddl.Column
		primaryKey          []string
		want                string
	}{
		{
			name: "a single key", table: "users",
			columns:    []ddl.Column{{Name: "id", Type: "BIGINT", Nullable: true}, {Name: "email", Type: "VARCHAR(255)"}},
			primaryKey: []string{"id"},
			want:       "CREATE TABLE \"users\" (\n  \"id\" BIGINT NOT NULL,\n  \"email\" VARCHAR(255) NOT NULL,\n  PRIMARY KEY (\"id\")\n);",
		},
		{
			name: "a composite key in key order", table: "edges",
			columns:    []ddl.Column{{Name: "a", Type: "bigint"}, {Name: "b", Type: "bigint"}, {Name: "note", Type: "text", Nullable: true}},
			primaryKey: []string{"b", "a"},
			want:       "CREATE TABLE \"edges\" (\n  \"a\" bigint NOT NULL,\n  \"b\" bigint NOT NULL,\n  \"note\" text,\n  PRIMARY KEY (\"b\", \"a\")\n);",
		},
		{
			name: "a schema other than public and a default", schema: "app", table: "t",
			columns:    []ddl.Column{{Name: "id", Type: "bigint"}, {Name: "n", Type: "bigint", Default: new("0")}},
			primaryKey: []string{"id"},
			want:       "CREATE TABLE \"app\".\"t\" (\n  \"id\" bigint NOT NULL,\n  \"n\" bigint NOT NULL DEFAULT 0,\n  PRIMARY KEY (\"id\")\n);",
		},
		{
			name: "the public schema stays unqualified and a quote is doubled", schema: "public", table: `odd"name`,
			columns: []ddl.Column{{Name: `c"1`, Type: "text", Nullable: true}},
			want:    "CREATE TABLE \"odd\"\"name\" (\n  \"c\"\"1\" text\n);",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			statement, err := ddl.CreateTable(test.schema, test.table, test.columns, test.primaryKey)
			c.Assert(err, qt.IsNil)
			c.Assert(statement, qt.Equals, test.want)
		})
	}
}

func TestCreateTable_FailurePath(t *testing.T) {
	tests := []struct {
		name, table string
		columns     []ddl.Column
		primaryKey  []string
		wantErr     string
	}{
		{name: "no name", columns: []ddl.Column{{Name: "id", Type: "bigint"}}, wantErr: `a table has no name`},
		{name: "no column", table: "t", wantErr: `table "t" has no column`},
		{name: "a column twice", table: "t", columns: []ddl.Column{{Name: "a", Type: "bigint"}, {Name: "a", Type: "bigint"}}, wantErr: `table "t" declares column "a" twice`},
		{name: "a key column twice", table: "t", columns: []ddl.Column{{Name: "a", Type: "bigint"}}, primaryKey: []string{"a", "a"},
			wantErr: `table "t" names primary key column "a" twice`},
		{name: "an undeclared key column", table: "t", columns: []ddl.Column{{Name: "a", Type: "bigint"}}, primaryKey: []string{"b"},
			wantErr: `table "t" names primary key column "b", which it does not declare`},
		{name: "a type Spanner refuses", table: "t", columns: []ddl.Column{{Name: "a", Type: "smallint"}},
			wantErr: `column "a": type "smallint": Spanner stores integers as bigint only`},
		{name: "a column without a name", table: "t", columns: []ddl.Column{{Type: "bigint"}}, wantErr: `a column has no name`},
		{name: "an empty default", table: "t", columns: []ddl.Column{{Name: "a", Type: "bigint", Default: new(" ")}},
			wantErr: `column "a" has an empty default expression`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			statement, err := ddl.CreateTable("", test.table, test.columns, test.primaryKey)
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(statement, qt.Equals, "")
		})
	}
}

func TestAddColumn_HappyPath(t *testing.T) {
	tests := []struct {
		name   string
		column ddl.Column
		want   string
	}{
		{name: "a nullable column", column: ddl.Column{Name: "note", Type: "text", Nullable: true}, want: `ALTER TABLE "t" ADD COLUMN "note" text;`},
		{name: "a NOT NULL column with a default", column: ddl.Column{Name: "n", Type: "bigint", Default: new("0")},
			want: `ALTER TABLE "t" ADD COLUMN "n" bigint NOT NULL DEFAULT 0;`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			statement, err := ddl.AddColumn("public", "t", test.column)
			c.Assert(err, qt.IsNil)
			c.Assert(statement, qt.Equals, test.want)
		})
	}
}

// Spanner refuses ADD COLUMN for a NOT NULL column without a default, even on
// an empty table, measured by Ptah's built-in Spanner engine on the emulator.
func TestAddColumn_FailurePath(t *testing.T) {
	tests := []struct {
		name    string
		column  ddl.Column
		wantErr string
	}{
		{name: "NOT NULL without a default", column: ddl.Column{Name: "n", Type: "bigint"},
			wantErr: `column "n" of table "t" is NOT NULL without a default, which Spanner refuses to add to an existing table`},
		{name: "a type Spanner refuses", column: ddl.Column{Name: "n", Type: "json", Nullable: true},
			wantErr: `column "n": type "json": Spanner has no json type; use jsonb`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			statement, err := ddl.AddColumn("", "t", test.column)
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(statement, qt.Equals, "")
		})
	}
}

func TestDropStatements(t *testing.T) {
	c := testcheck.New(t)
	c.Assert(ddl.DropTable("", "t"), qt.Equals, `DROP TABLE "t";`)
	c.Assert(ddl.DropTable("app", "t"), qt.Equals, `DROP TABLE "app"."t";`)
	c.Assert(ddl.DropColumn("public", "t", "c"), qt.Equals, `ALTER TABLE "t" DROP COLUMN "c";`)
}
