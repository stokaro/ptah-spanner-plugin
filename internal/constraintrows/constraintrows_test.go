package constraintrows_test

import (
	"errors"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/stokaro/ptah-spanner-plugin/internal/constraintrows"
	"github.com/stokaro/ptah-spanner-plugin/internal/sqltext"
	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// first is a statement that comes before the addition a test reads and
// changes no row: an addition the first statement makes is not counted.
const first = `CREATE INDEX i ON u (a)`

// table names a table of the default schema as Additions resolves it.
func table(name string) sqltext.TableName {
	return sqltext.TableName{Schema: "public", Table: name}
}

// Additions finds the CHECKs and foreign keys the statements add after their
// first statement, in the spellings Spanner takes, the provider's own and
// hand-written ones. It names the table as Spanner resolves it and keeps the
// CHECK expression as written.
func TestAdditions_FindsAdditions(t *testing.T) {
	tests := []struct {
		name       string
		statements []string
		want       []constraintrows.Addition
	}{
		{name: "the provider's CHECK replacement",
			statements: []string{`ALTER TABLE "items" DROP CONSTRAINT "items_kind_check";`, `ALTER TABLE "items" ADD CONSTRAINT "items_kind_check" CHECK (kind IN ('a'));`},
			want:       []constraintrows.Addition{{Unit: 1, Table: table("items"), Name: "items_kind_check", Kind: constraintrows.Check, Expression: "kind IN ('a')"}}},
		{name: "the provider's foreign key",
			statements: []string{first, `ALTER TABLE "ch" ADD CONSTRAINT "ch_p_fk" FOREIGN KEY ("p_id") REFERENCES "p"("id") ON DELETE CASCADE;`},
			want: []constraintrows.Addition{{Unit: 1, Table: table("ch"), Name: "ch_p_fk",
				Kind: constraintrows.ForeignKey, Columns: []string{"p_id"}, Referenced: table("p"), ReferencedColumns: []string{"id"}}}},
		{name: "a composite foreign key in another schema, unquoted, with both actions",
			statements: []string{first, `alter table App.CA add constraint CA_fk foreign key (X, y) references app.pa (a, B) on update no action on delete no action`},
			want: []constraintrows.Addition{{Unit: 1, Table: sqltext.TableName{Schema: "app", Table: "ca"}, Name: "ca_fk",
				Kind: constraintrows.ForeignKey, Columns: []string{"x", "y"}, Referenced: sqltext.TableName{Schema: "app", Table: "pa"}, ReferencedColumns: []string{"a", "b"}}}},
		{name: "a foreign key without a name",
			statements: []string{first, `ALTER TABLE ch ADD FOREIGN KEY (p_id) REFERENCES p (id)`},
			want: []constraintrows.Addition{{Unit: 1, Table: table("ch"),
				Kind: constraintrows.ForeignKey, Columns: []string{"p_id"}, Referenced: table("p"), ReferencedColumns: []string{"id"}}}},
		{name: "a CHECK without a name, after IF EXISTS",
			statements: []string{first, `ALTER TABLE IF EXISTS t ADD CHECK (n > 0)`},
			want:       []constraintrows.Addition{{Unit: 1, Table: table("t"), Kind: constraintrows.Check, Expression: "n > 0"}}},
		{name: "parentheses, a comment and a string with a parenthesis in the expression",
			statements: []string{first, "ALTER TABLE t ADD CONSTRAINT k CHECK ( (n > 0) AND /* ) */ s <> ')'\n);"},
			want:       []constraintrows.Addition{{Unit: 1, Table: table("t"), Name: "k", Kind: constraintrows.Check, Expression: "(n > 0) AND /* ) */ s <> ')'"}}},
		{name: "a drop and an addition in one unit",
			statements: []string{`ALTER TABLE t DROP CONSTRAINT k; ALTER TABLE t ADD CONSTRAINT k CHECK (n > 0);`},
			want:       []constraintrows.Addition{{Table: table("t"), Name: "k", Kind: constraintrows.Check, Expression: "n > 0"}}},
		{name: "a column of another table the batch adds",
			statements: []string{`ALTER TABLE u ADD COLUMN c bigint`, `ALTER TABLE t ADD CONSTRAINT k CHECK (c > 0)`},
			want:       []constraintrows.Addition{{Unit: 1, Table: table("t"), Name: "k", Kind: constraintrows.Check, Expression: "c > 0"}}},
		{name: "a column of the table the CHECK does not read",
			statements: []string{`ALTER TABLE public.t ADD COLUMN c bigint DEFAULT 7`, `ALTER TABLE t ADD CONSTRAINT k CHECK (n > 0)`},
			want:       []constraintrows.Addition{{Unit: 1, Table: table("t"), Name: "k", Kind: constraintrows.Check, Expression: "n > 0"}}},
		{name: "a column of the table the foreign key does not read",
			statements: []string{`ALTER TABLE ch ADD COLUMN c bigint`, `ALTER TABLE p ALTER COLUMN name SET DEFAULT ('x')`, `ALTER TABLE ch ADD CONSTRAINT fk FOREIGN KEY (p_id) REFERENCES p (id)`},
			want: []constraintrows.Addition{{Unit: 2, Table: table("ch"), Name: "fk",
				Kind: constraintrows.ForeignKey, Columns: []string{"p_id"}, Referenced: table("p"), ReferencedColumns: []string{"id"}}}},
		{name: "objects other than tables before the addition",
			statements: []string{`CREATE VIEW v AS SELECT 1`, `CREATE SEQUENCE s`, `DROP INDEX i`, `CREATE CHANGE STREAM cs FOR t`, `ALTER TABLE t ADD TTL INTERVAL '1 day' ON at`,
				`ALTER TABLE t ADD CONSTRAINT k CHECK (n > 0)`},
			want: []constraintrows.Addition{{Unit: 5, Table: table("t"), Name: "k", Kind: constraintrows.Check, Expression: "n > 0"}}},
		{name: "a table the batch creates is not the table of the addition",
			statements: []string{`CREATE TABLE other (id bigint PRIMARY KEY)`, `ALTER TABLE t ADD CONSTRAINT k CHECK (n > 0)`},
			want:       []constraintrows.Addition{{Unit: 1, Table: table("t"), Name: "k", Kind: constraintrows.Check, Expression: "n > 0"}}},
		{name: "every addition after the first statement",
			statements: []string{`ALTER TABLE t ADD CONSTRAINT k1 CHECK (n > 0)`, `ALTER TABLE t ADD CONSTRAINT k2 CHECK (n < 9)`, `ALTER TABLE u ADD CONSTRAINT k3 CHECK (m > 0)`},
			want: []constraintrows.Addition{{Unit: 1, Table: table("t"), Name: "k2", Kind: constraintrows.Check, Expression: "n < 9"},
				{Unit: 2, Table: table("u"), Name: "k3", Kind: constraintrows.Check, Expression: "m > 0"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(constraintrows.Additions(test.statements), qt.DeepEquals, test.want)
		})
	}
}

// An addition of another shape is left to Spanner: one the first statement
// makes, a clause Spanner may not enforce the constraint under, a foreign key
// without the referenced columns, other constraint kinds, and text that does
// not read as a constraint.
func TestAdditions_LeavesOtherShapes(t *testing.T) {
	tests := []struct {
		name      string
		statement string
	}{
		{name: "a clause after the CHECK", statement: `ALTER TABLE t ADD CONSTRAINT k CHECK (n > 0) NOT VALID`},
		{name: "an empty CHECK", statement: `ALTER TABLE t ADD CONSTRAINT k CHECK ()`},
		{name: "a CHECK that does not close", statement: `ALTER TABLE t ADD CONSTRAINT k CHECK ((n > 0)`},
		{name: "a constraint without a name after CONSTRAINT", statement: `ALTER TABLE t ADD CONSTRAINT CHECK`},
		{name: "a foreign key without the referenced columns", statement: `ALTER TABLE ch ADD CONSTRAINT fk FOREIGN KEY (p_id) REFERENCES p`},
		{name: "a foreign key NOT ENFORCED", statement: `ALTER TABLE ch ADD CONSTRAINT fk FOREIGN KEY (p_id) REFERENCES p (id) NOT ENFORCED`},
		{name: "a foreign key with a MATCH type", statement: `ALTER TABLE ch ADD CONSTRAINT fk FOREIGN KEY (p_id) REFERENCES p (id) MATCH FULL`},
		{name: "a foreign key with an action twice", statement: `ALTER TABLE ch ADD CONSTRAINT fk FOREIGN KEY (p_id) REFERENCES p (id) ON DELETE CASCADE ON DELETE NO ACTION`},
		{name: "a foreign key with fewer referenced columns", statement: `ALTER TABLE ch ADD CONSTRAINT fk FOREIGN KEY (a, b) REFERENCES p (id)`},
		{name: "a foreign key with a string for a column", statement: `ALTER TABLE ch ADD CONSTRAINT fk FOREIGN KEY ('a') REFERENCES p (id)`},
		{name: "a UNIQUE constraint", statement: `ALTER TABLE t ADD CONSTRAINT u UNIQUE (n)`},
		{name: "a primary key", statement: `ALTER TABLE t ADD PRIMARY KEY (id)`},
		{name: "a column", statement: `ALTER TABLE t ADD COLUMN c bigint CHECK (c > 0)`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(constraintrows.Additions([]string{first, test.statement}), qt.IsNil)
		})
	}
}

// The first statement's addition is not counted: when Spanner refuses it,
// nothing of the batch has run.
func TestAdditions_LeavesTheFirstStatement(t *testing.T) {
	c := testcheck.New(t)
	c.Assert(constraintrows.Additions([]string{`ALTER TABLE t ADD CONSTRAINT k CHECK (n > 0)`, `CREATE INDEX i ON u (a)`}), qt.IsNil)
}

// A statement before an addition that may change the rows, or the columns
// the constraint reads, leaves the addition to Spanner: a count before the
// batch would not say what Spanner checks. A statement that may change any
// table's rows ends the reading; a change of one table, or of one column,
// leaves only the additions that read it. The additions before the statement
// stay.
func TestAdditions_StepsAsideWhereRowsMayChange(t *testing.T) {
	check := `ALTER TABLE t ADD CONSTRAINT k CHECK (n > 0)`
	key := `ALTER TABLE ch ADD CONSTRAINT fk FOREIGN KEY (p_id) REFERENCES p (id)`
	tests := []struct {
		name       string
		statements []string
		want       []constraintrows.Addition
	}{
		{name: "a row change", statements: []string{first, `UPDATE t SET n = 1`, check}},
		{name: "a row change in the unit of the addition", statements: []string{first, `DELETE FROM t WHERE n < 0; ` + check}},
		{name: "a text the lexer cannot read", statements: []string{first, `SELECT 'open`, check}},
		{name: "a statement the provider does not know", statements: []string{first, `ALTER DATABASE db SET spanner.default_leader = 'x'`, check}},
		{name: "a table renamed", statements: []string{first, `ALTER TABLE u RENAME TO t`, check}},
		{name: "an ALTER TABLE action the provider does not know", statements: []string{first, `ALTER TABLE ONLY t ADD COLUMN n bigint`, check}},
		{name: "the table created", statements: []string{`CREATE TABLE IF NOT EXISTS t (id bigint PRIMARY KEY, n bigint)`, check}},
		{name: "the table dropped", statements: []string{`DROP TABLE IF EXISTS "t"`, check}},
		{name: "a column of the table added without COLUMN", statements: []string{`ALTER TABLE t ADD n bigint`, check}},
		{name: "a column the CHECK reads added", statements: []string{`ALTER TABLE t ADD COLUMN IF NOT EXISTS n bigint DEFAULT 7`, check}},
		{name: "a column the CHECK reads changed", statements: []string{`ALTER TABLE "t" ALTER COLUMN "n" SET NOT NULL`, check}},
		{name: "a column the CHECK reads dropped", statements: []string{`ALTER TABLE t DROP COLUMN IF EXISTS n`, check}},
		{name: "the referenced table created", statements: []string{`CREATE TABLE p (id bigint PRIMARY KEY)`, key}},
		{name: "a column of the key changed", statements: []string{`ALTER TABLE ch ALTER COLUMN p_id TYPE bigint`, key}},
		{name: "a referenced column changed", statements: []string{`ALTER TABLE p ALTER COLUMN id SET NOT NULL`, key}},
		{name: "the additions before a row change stay", statements: []string{first, check, `DELETE FROM t WHERE n < 0`, key},
			want: []constraintrows.Addition{{Unit: 1, Table: table("t"), Name: "k", Kind: constraintrows.Check, Expression: "n > 0"}}},
		{name: "a change of one table leaves another's additions", statements: []string{`CREATE TABLE t (id bigint PRIMARY KEY, n bigint)`, check, key},
			want: []constraintrows.Addition{{Unit: 2, Table: table("ch"), Name: "fk",
				Kind: constraintrows.ForeignKey, Columns: []string{"p_id"}, Referenced: table("p"), ReferencedColumns: []string{"id"}}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(constraintrows.Additions(test.statements), qt.DeepEquals, test.want)
		})
	}
}

// The count of a CHECK counts the rows where the expression is false, which
// NOT leaves out where it is null, as a CHECK takes a row there. The count of
// a foreign key counts the rows whose key columns all hold a value that no
// referenced row holds, with every name quoted.
func TestAddition_Query(t *testing.T) {
	tests := []struct {
		name     string
		addition constraintrows.Addition
		want     string
	}{
		{name: "a CHECK", addition: constraintrows.Addition{Table: table("items"), Name: "k", Kind: constraintrows.Check, Expression: "kind IN ('a')"},
			want: `SELECT COUNT(*) FROM "items" WHERE NOT (kind IN ('a'))`},
		{name: "a CHECK of a table in another schema", addition: constraintrows.Addition{Table: sqltext.TableName{Schema: "app", Table: `Mixed"Name`}, Kind: constraintrows.Check, Expression: "n > 0"},
			want: `SELECT COUNT(*) FROM "app"."Mixed""Name" WHERE NOT (n > 0)`},
		{name: "a foreign key", addition: constraintrows.Addition{Table: table("ch"), Name: "fk",
			Kind: constraintrows.ForeignKey, Columns: []string{"p_id"}, Referenced: table("p"), ReferencedColumns: []string{"id"}},
			want: `SELECT COUNT(*) FROM "ch" AS child WHERE child."p_id" IS NOT NULL AND NOT EXISTS (SELECT 1 FROM "p" AS parent WHERE parent."id" = child."p_id")`},
		{name: "a composite foreign key across schemas", addition: constraintrows.Addition{Table: sqltext.TableName{Schema: "app", Table: "ca"},
			Kind: constraintrows.ForeignKey, Columns: []string{"x", "y"}, Referenced: table("pa"), ReferencedColumns: []string{"a", "b"}},
			want: `SELECT COUNT(*) FROM "app"."ca" AS child WHERE child."x" IS NOT NULL AND child."y" IS NOT NULL AND ` +
				`NOT EXISTS (SELECT 1 FROM "pa" AS parent WHERE parent."a" = child."x" AND parent."b" = child."y")`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(test.addition.Query(), qt.Equals, test.want)
		})
	}
}

// A refusal names the unit, the constraint, its table and how many rows break
// it, and what to do.
func TestRefusal_NamesTheConstraintAndTheRows(t *testing.T) {
	tests := []struct {
		name     string
		addition constraintrows.Addition
		rows     int64
		total    int
		want     string
	}{
		{name: "a CHECK one row breaks", addition: constraintrows.Addition{Unit: 1, Table: table("items"), Name: "items_kind_check", Kind: constraintrows.Check, Expression: "kind IN ('a')"}, rows: 1, total: 2,
			want: `unit 2 of 2 adds CHECK "items_kind_check" to table "items", which 1 row breaks, so no unit ran: ` +
				`Spanner refuses to add a constraint a row breaks; update or delete the rows that break it before adding the constraint`},
		{name: "a CHECK without a name in another schema", addition: constraintrows.Addition{Unit: 2, Table: sqltext.TableName{Schema: "app", Table: "t"}, Kind: constraintrows.Check, Expression: "n > 0"}, rows: 12, total: 3,
			want: `unit 3 of 3 adds a CHECK without a name to table "app"."t", which 12 rows break, so no unit ran: ` +
				`Spanner refuses to add a constraint a row breaks; update or delete the rows that break it before adding the constraint`},
		{name: "a foreign key", addition: constraintrows.Addition{Unit: 1, Table: table("ch"), Name: "ch_p_fk", Kind: constraintrows.ForeignKey}, rows: 2, total: 2,
			want: `unit 2 of 2 adds foreign key "ch_p_fk" to table "ch", which 2 rows break, so no unit ran: ` +
				`Spanner refuses to add a constraint a row breaks; update or delete the rows that break it, or add the rows they reference, before adding the constraint`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(constraintrows.Refusal(test.addition, test.rows, test.total), qt.Equals, test.want)
		})
	}
}

// A host shows a reason of one line of at most 512 bytes. Long names give way
// to the kind of the constraint.
func TestRefusal_FitsAHostReason(t *testing.T) {
	c := testcheck.New(t)
	addition := constraintrows.Addition{Unit: 1, Table: sqltext.TableName{Schema: strings.Repeat("s", 128), Table: strings.Repeat("t", 128)},
		Name: strings.Repeat("k", 128), Kind: constraintrows.Check, Expression: "n > 0"}
	c.Assert(constraintrows.Refusal(addition, 1, 2), qt.Equals, `unit 2 of 2 adds a CHECK, which 1 row breaks, so no unit ran: `+
		`Spanner refuses to add a constraint a row breaks; update or delete the rows that break it before adding the constraint`)
}

// A count that fails refuses the batch with the server's SQLSTATE, never its
// message, which may quote a value.
func TestUncounted_NamesTheFailure(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "a refusal of the server", err: &pgconn.PgError{Code: "P0001", Message: "division by zero: 1 / 0"},
			want: `the provider could not count the rows that break CHECK "k" of table "t", which unit 2 of 2 adds, so no unit ran: Spanner refused the count with SQLSTATE P0001`},
		{name: "another failure", err: errors.New("connection reset"),
			want: `the provider could not count the rows that break CHECK "k" of table "t", which unit 2 of 2 adds, so no unit ran: the count failed`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			addition := constraintrows.Addition{Unit: 1, Table: table("t"), Name: "k", Kind: constraintrows.Check, Expression: "1 / n > 0"}
			c.Assert(constraintrows.Uncounted(addition, test.err, 2), qt.Equals, test.want)
		})
	}
}

// Additions finds the unique indexes, NOT NULLs and character limits the
// statements add after their first statement, in the provider's spellings and
// hand-written ones. NOT NULL on a column a statement before it adds without
// a default is counted, since every row holds a null there.
func TestAdditions_FindsRulesOnRows(t *testing.T) {
	tests := []struct {
		name       string
		statements []string
		want       []constraintrows.Addition
	}{
		{name: "the provider's unique index replacement", statements: []string{`DROP INDEX "t_a";`, `CREATE UNIQUE INDEX "t_a" ON "t" ("a");`},
			want: []constraintrows.Addition{{Unit: 1, Kind: constraintrows.UniqueIndex, Table: table("t"), Name: "t_a", Columns: []string{"a"}}}},
		{name: "a unique index with directions, INCLUDE and WHERE in another schema",
			statements: []string{first, `create unique index T_AB on App.T (A desc nulls first, "B" ASC) include (c) where "A" is not null and c is not null`},
			want: []constraintrows.Addition{{Unit: 1, Kind: constraintrows.UniqueIndex, Table: sqltext.TableName{Schema: "app", Table: "t"}, Name: "t_ab",
				Columns: []string{"a", "B"}, Expression: `"A" is not null and c is not null`}}},
		{name: "the provider's NOT NULL", statements: []string{first, `ALTER TABLE "t" ALTER COLUMN "b" SET NOT NULL;`},
			want: []constraintrows.Addition{{Unit: 1, Kind: constraintrows.NotNull, Table: table("t"), Columns: []string{"b"}}}},
		{name: "two NOT NULLs of one table", statements: []string{`ALTER TABLE t ALTER COLUMN a SET NOT NULL`, `ALTER TABLE t ALTER COLUMN b SET NOT NULL`},
			want: []constraintrows.Addition{{Unit: 1, Kind: constraintrows.NotNull, Table: table("t"), Columns: []string{"b"}}}},
		{name: "the provider's NOT NULL column without a default", statements: []string{`ALTER TABLE "t" ADD COLUMN "c" bigint;`, `ALTER TABLE "t" ALTER COLUMN "c" SET NOT NULL;`},
			want: []constraintrows.Addition{{Unit: 1, Kind: constraintrows.NotNull, Table: table("t"), Columns: []string{"c"}, Unfilled: true}}},
		{name: "the provider's shorter varchar", statements: []string{`ALTER TABLE "t" ALTER COLUMN "s1" TYPE varchar(10);`, `ALTER TABLE "t" ALTER COLUMN "s2" TYPE varchar(3);`},
			want: []constraintrows.Addition{{Unit: 1, Kind: constraintrows.CharacterLimit, Table: table("t"), Columns: []string{"s2"}, Limit: 3}}},
		{name: "SET DATA TYPE character varying without a length", statements: []string{first, `ALTER TABLE t ALTER COLUMN x SET DATA TYPE character varying`},
			want: []constraintrows.Addition{{Unit: 1, Kind: constraintrows.CharacterLimit, Table: table("t"), Columns: []string{"x"}, Limit: 2621440}}},
		{name: "an array", statements: []string{first, `ALTER TABLE t ALTER COLUMN xs TYPE varchar (3)[]`},
			want: []constraintrows.Addition{{Unit: 1, Kind: constraintrows.CharacterLimit, Table: table("t"), Columns: []string{"xs"}, Limit: 3, Array: true}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(constraintrows.Additions(test.statements), qt.DeepEquals, test.want)
		})
	}
}

// A unique index or a column change of another shape asks nothing of the
// rows the provider counts, or is left to Spanner.
func TestAdditions_LeavesOtherIndexesAndColumnChanges(t *testing.T) {
	tests := []struct {
		name      string
		statement string
	}{
		{name: "a unique index IF NOT EXISTS", statement: `CREATE UNIQUE INDEX IF NOT EXISTS i ON t (a)`},
		{name: "a unique index interleaved in a parent", statement: `CREATE UNIQUE INDEX i ON t (a) INTERLEAVE IN p`},
		{name: "a unique index on an expression", statement: `CREATE UNIQUE INDEX i ON t (lower(a))`},
		{name: "a unique index NULLS NOT DISTINCT", statement: `CREATE UNIQUE INDEX i ON t (a) NULLS NOT DISTINCT`},
		{name: "a unique index with a qualified name", statement: `CREATE UNIQUE INDEX app.i ON t (a)`},
		{name: "an index that is not unique", statement: `CREATE INDEX i ON t (a)`},
		{name: "a change to bytea", statement: `ALTER TABLE t ALTER COLUMN s TYPE bytea`},
		{name: "a type change with USING", statement: `ALTER TABLE t ALTER COLUMN s TYPE varchar(3) USING s`},
		{name: "DROP NOT NULL", statement: `ALTER TABLE t ALTER COLUMN s DROP NOT NULL`},
		{name: "SET NOT NULL without COLUMN", statement: `ALTER TABLE t ALTER s SET NOT NULL`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(constraintrows.Additions([]string{first, test.statement}), qt.IsNil)
		})
	}
}

// A statement before a unique index, a NOT NULL or a character limit that
// changes a column the rule reads leaves the rule to Spanner, and so does a
// column added with a default, which Spanner fills in every row.
func TestAdditions_StepsAsideForChangedColumns(t *testing.T) {
	tests := []struct {
		name       string
		statements []string
	}{
		{name: "a key column of the unique index changed", statements: []string{`ALTER TABLE t ALTER COLUMN a TYPE varchar(5)`, `CREATE UNIQUE INDEX i ON t (a)`}},
		{name: "a column the unique index's filter reads added", statements: []string{`ALTER TABLE t ADD COLUMN c bigint`, `CREATE UNIQUE INDEX i ON t (a) WHERE c IS NOT NULL`}},
		{name: "the table of the unique index created", statements: []string{`CREATE TABLE t (id bigint PRIMARY KEY, a bigint)`, `CREATE UNIQUE INDEX i ON t (a)`}},
		{name: "a column added with a default made NOT NULL", statements: []string{`ALTER TABLE t ADD COLUMN c bigint DEFAULT 1`, `ALTER TABLE t ALTER COLUMN c SET NOT NULL`}},
		{name: "an added column changed before NOT NULL", statements: []string{`ALTER TABLE t ADD COLUMN c bigint`, `ALTER TABLE t ALTER COLUMN c SET DEFAULT (1)`,
			`ALTER TABLE t ALTER COLUMN c SET NOT NULL`}},
		{name: "a column added without a default in a table created first", statements: []string{`CREATE TABLE t (id bigint PRIMARY KEY)`, `ALTER TABLE t ADD COLUMN c bigint`,
			`ALTER TABLE t ALTER COLUMN c SET NOT NULL`}},
		{name: "a column changed before its limit", statements: []string{`ALTER TABLE t ALTER COLUMN s SET DEFAULT ('x')`, `ALTER TABLE t ALTER COLUMN s TYPE varchar(3)`}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(constraintrows.Additions(test.statements), qt.IsNil)
		})
	}
}

// The count of a unique index counts the rows whose key another row holds,
// among the rows with no null key column and within the index's filter. NOT
// NULL counts the nulls, or every row for a column the batch adds without a
// default. A character limit counts the values longer than the limit, read as
// text, one row once however many of its elements are.
func TestAddition_QueryOfRulesOnRows(t *testing.T) {
	tests := []struct {
		name     string
		addition constraintrows.Addition
		want     string
	}{
		{name: "a unique index", addition: constraintrows.Addition{Kind: constraintrows.UniqueIndex, Table: table("t"), Name: "t_a", Columns: []string{"a"}},
			want: `SELECT CAST(COALESCE(SUM(shared), 0) AS bigint) FROM (SELECT COUNT(*) AS shared FROM "t" WHERE "a" IS NOT NULL GROUP BY "a" HAVING COUNT(*) > 1) AS duplicates`},
		{name: "a composite unique index with a filter", addition: constraintrows.Addition{Kind: constraintrows.UniqueIndex, Table: sqltext.TableName{Schema: "app", Table: "t"},
			Columns: []string{"a", "B"}, Expression: "c IS NOT NULL"},
			want: `SELECT CAST(COALESCE(SUM(shared), 0) AS bigint) FROM (SELECT COUNT(*) AS shared FROM "app"."t" WHERE "a" IS NOT NULL AND "B" IS NOT NULL AND (c IS NOT NULL) ` +
				`GROUP BY "a", "B" HAVING COUNT(*) > 1) AS duplicates`},
		{name: "NOT NULL", addition: constraintrows.Addition{Kind: constraintrows.NotNull, Table: table("t"), Columns: []string{"b"}},
			want: `SELECT COUNT(*) FROM "t" WHERE "b" IS NULL`},
		{name: "NOT NULL on a column added without a default", addition: constraintrows.Addition{Kind: constraintrows.NotNull, Table: table("t"), Columns: []string{"c"}, Unfilled: true},
			want: `SELECT COUNT(*) FROM "t"`},
		{name: "a character limit", addition: constraintrows.Addition{Kind: constraintrows.CharacterLimit, Table: table("t"), Columns: []string{"s"}, Limit: 3},
			want: `SELECT COUNT(*) FROM "t" WHERE length(CAST("s" AS varchar)) > 3`},
		{name: "a character limit of an array", addition: constraintrows.Addition{Kind: constraintrows.CharacterLimit, Table: table("t"), Columns: []string{"xs"}, Limit: 3, Array: true},
			want: `SELECT COUNT(*) FROM "t" WHERE EXISTS (SELECT 1 FROM UNNEST("xs") AS element WHERE length(CAST(element AS varchar)) > 3)`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(test.addition.Query(), qt.Equals, test.want)
		})
	}
}

// A refusal of a unique index, a NOT NULL or a character limit names the
// unit, the index or the column, the table and how many rows break it, and
// what to do.
func TestRefusal_NamesTheRuleOnRows(t *testing.T) {
	tests := []struct {
		name     string
		addition constraintrows.Addition
		rows     int64
		want     string
	}{
		{name: "a unique index", addition: constraintrows.Addition{Unit: 1, Kind: constraintrows.UniqueIndex, Table: table("t"), Name: "t_a", Columns: []string{"a"}}, rows: 2,
			want: `unit 2 of 2 creates unique index "t_a" on table "t", which 2 rows break, so no unit ran: ` +
				`Spanner refuses a unique index over rows that share a key; update or delete the rows that share a key before creating the index`},
		{name: "NOT NULL", addition: constraintrows.Addition{Unit: 1, Kind: constraintrows.NotNull, Table: table("t"), Columns: []string{"b"}}, rows: 1,
			want: `unit 2 of 2 makes column "b" of table "t" NOT NULL, which 1 row breaks, so no unit ran: ` +
				`Spanner refuses NOT NULL over a null; set a value in the rows that hold a null before making the column NOT NULL`},
		{name: "a character limit", addition: constraintrows.Addition{Unit: 1, Kind: constraintrows.CharacterLimit, Table: sqltext.TableName{Schema: "app", Table: "t"},
			Columns: []string{"s2"}, Limit: 3}, rows: 4,
			want: `unit 2 of 2 limits column "s2" of table "app"."t" to 3 characters, which 4 rows break, so no unit ran: ` +
				`Spanner refuses a limit a value exceeds; shorten the values that exceed it before limiting the column`},
		{name: "a character limit of an array", addition: constraintrows.Addition{Unit: 1, Kind: constraintrows.CharacterLimit, Table: table("t"),
			Columns: []string{"xs"}, Limit: 3, Array: true}, rows: 1,
			want: `unit 2 of 2 limits the elements of column "xs" of table "t" to 3 characters, which 1 row breaks, so no unit ran: ` +
				`Spanner refuses a limit a value exceeds; shorten the values that exceed it before limiting the column`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(constraintrows.Refusal(test.addition, test.rows, 2), qt.Equals, test.want)
		})
	}
}

// A count of a rule on rows that fails names the rule and the SQLSTATE, and
// gives way to the rule's kind when the names do not fit a host's reason.
func TestUncounted_NamesTheRuleOnRows(t *testing.T) {
	long := strings.Repeat("c", 400)
	tests := []struct {
		name     string
		addition constraintrows.Addition
		want     string
	}{
		{name: "a character limit of a bytea column", addition: constraintrows.Addition{Unit: 1, Kind: constraintrows.CharacterLimit, Table: table("t"), Columns: []string{"x"}, Limit: 5},
			want: `the provider could not count the rows that break the limit of 5 characters on column "x" of table "t", which unit 2 of 2 sets, so no unit ran: ` +
				`Spanner refused the count with SQLSTATE P0001`},
		{name: "NOT NULL on a column with a long name", addition: constraintrows.Addition{Unit: 1, Kind: constraintrows.NotNull, Table: table("t"), Columns: []string{long}},
			want: `the provider could not count the rows that break NOT NULL on a column, which unit 2 of 2 sets, so no unit ran: Spanner refused the count with SQLSTATE P0001`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(constraintrows.Uncounted(test.addition, &pgconn.PgError{Code: "P0001"}, 2), qt.Equals, test.want)
		})
	}
}
