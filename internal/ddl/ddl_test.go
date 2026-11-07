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
			// A single key column carries the key in its definition, as
			// Ptah's built-in Spanner engine wrote it.
			name: "a single key", table: "users",
			columns:    []ddl.Column{{Name: "id", Type: "BIGINT", Nullable: true}, {Name: "email", Type: "VARCHAR(255)"}},
			primaryKey: []string{"id"},
			want:       "CREATE TABLE \"users\" (\n  \"id\" BIGINT PRIMARY KEY NOT NULL,\n  \"email\" VARCHAR(255) NOT NULL\n);",
		},
		{
			// A composite key is a clause after the columns, which keep
			// their declared nullability: Spanner makes key columns NOT NULL.
			name: "a composite key in key order", table: "edges",
			columns:    []ddl.Column{{Name: "a", Type: "bigint", Nullable: true}, {Name: "b", Type: "bigint"}, {Name: "note", Type: "text", Nullable: true}},
			primaryKey: []string{"b", "a"},
			want:       "CREATE TABLE \"edges\" (\n  \"a\" bigint,\n  \"b\" bigint NOT NULL,\n  \"note\" text,\n  PRIMARY KEY (\"b\", \"a\")\n);",
		},
		{
			name: "a schema other than public, a default and a check", schema: "app", table: "t",
			columns:    []ddl.Column{{Name: "id", Type: "bigint"}, {Name: "n", Type: "bigint", Default: new("0"), Check: "n >= 0", CheckName: "t_n_check"}},
			primaryKey: []string{"id"},
			want: "CREATE TABLE \"app\".\"t\" (\n  \"id\" bigint PRIMARY KEY NOT NULL,\n" +
				"  \"n\" bigint NOT NULL DEFAULT 0 CONSTRAINT \"t_n_check\" CHECK (n >= 0)\n);",
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
		{name: "a check without a name", table: "t", columns: []ddl.Column{{Name: "a", Type: "bigint", Check: "a > 0"}},
			wantErr: `the CHECK constraint of column "a" has no name`},
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
		{name: "a check", column: ddl.Column{Name: "n", Type: "bigint", Nullable: true, Check: "n > 0", CheckName: "c"},
			wantErr: `column "n" of table "t" carries a CHECK constraint, which is added after the column`},
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
	c.Assert(ddl.DropIndex("public", "t_i"), qt.Equals, `DROP INDEX "t_i";`)
	c.Assert(ddl.DropIndex("app", "t_i"), qt.Equals, `DROP INDEX "app"."t_i";`)
	c.Assert(ddl.DropConstraint("public", "ch", "ch_p_fk"), qt.Equals, `ALTER TABLE "ch" DROP CONSTRAINT "ch_p_fk";`)
	c.Assert(ddl.DropConstraint("app", "ch", "ch_p_fk"), qt.Equals, `ALTER TABLE "app"."ch" DROP CONSTRAINT "ch_p_fk";`)
}

// Spanner takes one action per ALTER TABLE, so each column change is a
// statement of its own.
func TestColumnChanges_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	statement, err := ddl.AlterColumnType("", "t", "a", "varchar(40)")
	c.Assert(err, qt.IsNil)
	c.Assert(statement, qt.Equals, `ALTER TABLE "t" ALTER COLUMN "a" TYPE varchar(40);`)
	statement, err = ddl.SetDefault("app", "t", "b", "'0'::bigint")
	c.Assert(err, qt.IsNil)
	c.Assert(statement, qt.Equals, `ALTER TABLE "app"."t" ALTER COLUMN "b" SET DEFAULT '0'::bigint;`)
	c.Assert(ddl.DropDefault("", "t", "b"), qt.Equals, `ALTER TABLE "t" ALTER COLUMN "b" DROP DEFAULT;`)
	c.Assert(ddl.SetNotNull("", "t", "a"), qt.Equals, `ALTER TABLE "t" ALTER COLUMN "a" SET NOT NULL;`)
	c.Assert(ddl.DropNotNull("", "t", "a"), qt.Equals, `ALTER TABLE "t" ALTER COLUMN "a" DROP NOT NULL;`)
}

func TestColumnChanges_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	statement, err := ddl.AlterColumnType("", "t", "a", "int4")
	c.Assert(err, qt.ErrorMatches, `column "a": type "int4": Spanner stores integers as bigint only`)
	c.Assert(statement, qt.Equals, "")
	statement, err = ddl.SetDefault("", "t", "a", " ")
	c.Assert(err, qt.ErrorMatches, `column "a" has an empty default expression`)
	c.Assert(statement, qt.Equals, "")
}

func TestCreateIndex_HappyPath(t *testing.T) {
	tests := []struct {
		name, schema string
		index        ddl.Index
		want         string
	}{
		{name: "one key", index: ddl.Index{Name: "t_i", Keys: []ddl.IndexKey{{Column: "a"}}}, want: `CREATE INDEX "t_i" ON "t" ("a");`},
		{name: "unique", index: ddl.Index{Name: "t_u", Unique: true, Keys: []ddl.IndexKey{{Column: "a"}}}, want: `CREATE UNIQUE INDEX "t_u" ON "t" ("a");`},
		{name: "direction, NULLS ordering and stored columns", schema: "app",
			index: ddl.Index{Name: "t_i", Keys: []ddl.IndexKey{{Column: "a", Descending: true}, {Column: "b", Nulls: "FIRST"}, {Column: "c", Descending: true, Nulls: "LAST"}},
				Storing: []string{"d", "e"}},
			want: `CREATE INDEX "t_i" ON "app"."t" ("a" DESC, "b" NULLS FIRST, "c" DESC NULLS LAST) INCLUDE ("d", "e");`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			statement, err := ddl.CreateIndex(test.schema, "t", test.index)
			c.Assert(err, qt.IsNil)
			c.Assert(statement, qt.Equals, test.want)
		})
	}
}

func TestCreateIndex_FailurePath(t *testing.T) {
	tests := []struct {
		name    string
		index   ddl.Index
		wantErr string
	}{
		{name: "no name", index: ddl.Index{Keys: []ddl.IndexKey{{Column: "a"}}}, wantErr: `an index of table "t" has no name`},
		{name: "no key", index: ddl.Index{Name: "i"}, wantErr: `index "i" of table "t" has no key`},
		{name: "a key without a column", index: ddl.Index{Name: "i", Keys: []ddl.IndexKey{{}}}, wantErr: `index "i" of table "t" has a key without a column`},
		{name: "an unknown NULLS ordering", index: ddl.Index{Name: "i", Keys: []ddl.IndexKey{{Column: "a", Nulls: "MIDDLE"}}},
			wantErr: `index "i" of table "t" has a key with the NULLS ordering "MIDDLE"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			statement, err := ddl.CreateIndex("", "t", test.index)
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(statement, qt.Equals, "")
		})
	}
}

func TestConstraints_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	statement, err := ddl.AddForeignKey("", "ch", ddl.ForeignKey{Name: "ch_p_fk", Columns: []string{"p_id"}, RefSchema: "public", RefTable: "p", RefColumns: []string{"id"}})
	c.Assert(err, qt.IsNil)
	c.Assert(statement, qt.Equals, `ALTER TABLE "ch" ADD CONSTRAINT "ch_p_fk" FOREIGN KEY ("p_id") REFERENCES "p"("id");`)
	statement, err = ddl.AddForeignKey("app", "ca", ddl.ForeignKey{Name: "f", Columns: []string{"a", "b"}, RefSchema: "app", RefTable: "pa", RefColumns: []string{"x", "y"},
		OnDelete: "CASCADE"})
	c.Assert(err, qt.IsNil)
	c.Assert(statement, qt.Equals, `ALTER TABLE "app"."ca" ADD CONSTRAINT "f" FOREIGN KEY ("a", "b") REFERENCES "app"."pa"("x", "y") ON DELETE CASCADE;`)
	statement, err = ddl.AddCheck("", "k", "k_score_check", " score >= 0 ")
	c.Assert(err, qt.IsNil)
	c.Assert(statement, qt.Equals, `ALTER TABLE "k" ADD CONSTRAINT "k_score_check" CHECK (score >= 0);`)
}

func TestConstraints_FailurePath(t *testing.T) {
	tests := []struct {
		name    string
		key     ddl.ForeignKey
		wantErr string
	}{
		{name: "no name", key: ddl.ForeignKey{Columns: []string{"a"}, RefTable: "p", RefColumns: []string{"id"}},
			wantErr: `a foreign key of table "t" needs a name, its columns and as many referenced columns`},
		{name: "a column count that differs", key: ddl.ForeignKey{Name: "f", Columns: []string{"a", "b"}, RefTable: "p", RefColumns: []string{"id"}},
			wantErr: `a foreign key of table "t" needs a name, its columns and as many referenced columns`},
		{name: "an action Spanner does not have", key: ddl.ForeignKey{Name: "f", Columns: []string{"a"}, RefTable: "p", RefColumns: []string{"id"}, OnDelete: "SET NULL"},
			wantErr: `foreign key "f" of table "t" has the action ON DELETE SET NULL, and Spanner has NO ACTION and CASCADE only`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			statement, err := ddl.AddForeignKey("", "t", test.key)
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(statement, qt.Equals, "")
		})
	}
}

func TestAddCheck_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	statement, err := ddl.AddCheck("", "t", "", "a > 0")
	c.Assert(err, qt.ErrorMatches, `a CHECK constraint of table "t" needs a name and an expression`)
	c.Assert(statement, qt.Equals, "")
}
