package changestream_test

import (
	"slices"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-spanner-plugin/internal/changestream"
	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// Drops finds every DROP TABLE and DROP COLUMN in the spellings Spanner takes,
// the provider's own and hand-written ones, and names the table as Spanner
// resolves it: an unquoted name folded to lower case, a name without a schema
// in the default one.
func TestDrops_FindsDrops(t *testing.T) {
	tests := []struct {
		name       string
		statements []string
		want       []changestream.Drop
	}{
		{name: "the provider's DROP TABLE", statements: []string{`DROP TABLE "t";`}, want: []changestream.Drop{{Schema: "public", Table: "t"}}},
		{name: "a table in another schema", statements: []string{`DROP TABLE "app"."t2";`}, want: []changestream.Drop{{Schema: "app", Table: "t2"}}},
		{name: "an unquoted name", statements: []string{`drop table App.T`}, want: []changestream.Drop{{Schema: "app", Table: "t"}}},
		{name: "a quoted name keeps its case", statements: []string{`DROP TABLE "Mixed"`}, want: []changestream.Drop{{Schema: "public", Table: "Mixed"}}},
		{name: "IF EXISTS and RESTRICT", statements: []string{`DROP TABLE IF EXISTS t RESTRICT`}, want: []changestream.Drop{{Schema: "public", Table: "t"}}},
		{name: "comments around the words", statements: []string{"-- drop it\nDROP /* now */ TABLE t;\n"}, want: []changestream.Drop{{Schema: "public", Table: "t"}}},
		{name: "the provider's DROP COLUMN", statements: []string{`ALTER TABLE "t" DROP COLUMN "a";`},
			want: []changestream.Drop{{Schema: "public", Table: "t", Column: "a"}}},
		{name: "DROP without COLUMN", statements: []string{`ALTER TABLE t DROP A`}, want: []changestream.Drop{{Schema: "public", Table: "t", Column: "a"}}},
		{name: "ALTER TABLE IF EXISTS in another schema", statements: []string{`ALTER TABLE IF EXISTS app.t2 DROP COLUMN c RESTRICT`},
			want: []changestream.Drop{{Schema: "app", Table: "t2", Column: "c"}}},
		{name: "a quoted column named like a keyword", statements: []string{`ALTER TABLE t DROP "ttl"`},
			want: []changestream.Drop{{Schema: "public", Table: "t", Column: "ttl"}}},
		{name: "the unit of each drop", statements: []string{`CREATE TABLE other (id bigint PRIMARY KEY)`, `ALTER TABLE t DROP COLUMN a`, `DROP TABLE "t"`},
			want: []changestream.Drop{{Unit: 1, Schema: "public", Table: "t", Column: "a"}, {Unit: 2, Schema: "public", Table: "t"}}},
		{name: "several statements in one unit", statements: []string{`DROP TABLE a; ALTER TABLE b DROP COLUMN c;`},
			want: []changestream.Drop{{Schema: "public", Table: "a"}, {Schema: "public", Table: "b", Column: "c"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(changestream.Drops(test.statements), qt.DeepEquals, test.want)
		})
	}
}

// A statement of another shape drops nothing: other objects, other ALTER
// TABLE actions, and shapes Spanner refuses, which it refuses when they run.
func TestDrops_IgnoresOtherStatements(t *testing.T) {
	tests := []struct {
		name      string
		statement string
	}{
		{name: "DROP CONSTRAINT", statement: `ALTER TABLE t DROP CONSTRAINT t_fk`},
		{name: "DROP TTL", statement: `ALTER TABLE t DROP TTL`},
		{name: "DROP SYNONYM", statement: `ALTER TABLE t DROP SYNONYM s`},
		{name: "ADD COLUMN", statement: `ALTER TABLE t ADD COLUMN a bigint`},
		{name: "DROP INDEX", statement: `DROP INDEX t_a`},
		{name: "DROP VIEW", statement: `DROP VIEW v`},
		{name: "two tables in one DROP", statement: `DROP TABLE t, u`},
		{name: "CASCADE", statement: `DROP TABLE t CASCADE`},
		{name: "a column without a name", statement: `ALTER TABLE t DROP COLUMN`},
		{name: "a string for a name", statement: `DROP TABLE 't'`},
		{name: "a row change", statement: `DELETE FROM t WHERE id = 1`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(changestream.Drops([]string{test.statement}), qt.IsNil)
		})
	}
}

// After a statement that may change what a change stream names, the catalog
// read before the batch no longer says what the streams name, so Drops stops
// there and leaves the drops after it to Spanner. The drops before it stay.
func TestDrops_StopsWhereStreamsMayChange(t *testing.T) {
	tests := []struct {
		name       string
		statements []string
		want       []changestream.Drop
	}{
		{name: "DROP CHANGE STREAM", statements: []string{`DROP CHANGE STREAM cs`, `DROP TABLE t`}},
		{name: "ALTER CHANGE STREAM", statements: []string{`DROP TABLE a`, `ALTER CHANGE STREAM cs SET FOR ALL`, `ALTER TABLE t DROP COLUMN c`},
			want: []changestream.Drop{{Schema: "public", Table: "a"}}},
		{name: "CREATE CHANGE STREAM", statements: []string{`CREATE CHANGE STREAM cs FOR t`, `DROP TABLE t`}},
		{name: "a CHANGE STREAM statement in the same unit", statements: []string{`drop change stream cs; drop table t;`}},
		{name: "a table renamed to the dropped name", statements: []string{`ALTER TABLE u RENAME TO t`, `DROP TABLE t`}},
		{name: "a text the lexer cannot read", statements: []string{`SELECT 'open`, `DROP TABLE t`}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(changestream.Drops(test.statements), qt.DeepEquals, test.want)
		})
	}
}

// tracking is what the streams of the emulator measurement name: cs1 FOR t,
// cs3 FOR t(a, b), cs4 FOR app.t2(c). A stream FOR ALL has no rows.
var tracking = changestream.Tracking{
	Tables: []changestream.Tracked{
		{Stream: "cs1", Schema: "public", Table: "t"}, {Stream: "cs3", Schema: "public", Table: "t"}, {Stream: "cs4", Schema: "app", Table: "t2"},
	},
	Columns: []changestream.Tracked{
		{Stream: "cs3", Schema: "public", Table: "t", Column: "a"}, {Stream: "cs3", Schema: "public", Table: "t", Column: "b"},
		{Stream: "cs4", Schema: "app", Table: "t2", Column: "c"},
	},
}

// A drop a stream names is refused with the unit, the table or column, every
// stream that names it, and what to do. A table is named by a stream with or
// without a column list; a column only by a stream that lists it.
func TestRefusal_NamesTheDropAndItsStreams(t *testing.T) {
	tests := []struct {
		name  string
		drops []changestream.Drop
		total int
		want  string
	}{
		{name: "a table two streams name", drops: []changestream.Drop{{Unit: 1, Schema: "public", Table: "t"}}, total: 2,
			want: `unit 2 of 2 drops table "t", which change streams "cs1" and "cs3" name, so no unit ran: Spanner refuses to drop a table a change stream names; ` +
				`drop the change stream, or alter it to stop tracking the table, before dropping the table`},
		{name: "a table in another schema", drops: []changestream.Drop{{Schema: "app", Table: "t2"}}, total: 1,
			want: `unit 1 of 1 drops table "app"."t2", which change stream "cs4" names, so no unit ran: Spanner refuses to drop a table a change stream names; ` +
				`drop the change stream, or alter it to stop tracking the table, before dropping the table`},
		{name: "a column a stream lists", drops: []changestream.Drop{{Schema: "public", Table: "t", Column: "a"}}, total: 3,
			want: `unit 1 of 3 drops column "a" of table "t", which change stream "cs3" names, so no unit ran: Spanner refuses to drop a column a change stream names; ` +
				`alter the change stream to stop tracking the column, or drop it, before dropping the column`},
		{name: "the first drop a stream names", drops: []changestream.Drop{{Schema: "public", Table: "other"}, {Unit: 1, Schema: "app", Table: "t2", Column: "c"}}, total: 2,
			want: `unit 2 of 2 drops column "c" of table "app"."t2", which change stream "cs4" names, so no unit ran: Spanner refuses to drop a column a change stream names; ` +
				`alter the change stream to stop tracking the column, or drop it, before dropping the column`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(changestream.Refusal(test.drops, tracking, test.total), qt.Equals, test.want)
		})
	}
}

// Spanner drops what no stream names explicitly: a column of a table a stream
// tracks whole (FOR t), a table only a stream FOR ALL tracks, a table in
// another schema with the same name.
func TestRefusal_LeavesUnnamedDrops(t *testing.T) {
	tests := []struct {
		name  string
		drops []changestream.Drop
	}{
		{name: "a column of a table a stream tracks whole", drops: []changestream.Drop{{Schema: "public", Table: "t", Column: "id"}}},
		{name: "a table no stream names", drops: []changestream.Drop{{Schema: "public", Table: "other"}}},
		{name: "the name in another schema", drops: []changestream.Drop{{Schema: "app", Table: "t"}}},
		{name: "a column of another table", drops: []changestream.Drop{{Schema: "public", Table: "t2", Column: "c"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(changestream.Refusal(test.drops, tracking, 1), qt.Equals, "")
		})
	}
}

// A host shows a reason of one line of at most 512 bytes. Long names give way
// to the number of streams, and then to a reason without names.
func TestRefusal_FitsAHostReason(t *testing.T) {
	long := strings.Repeat("s", 200)
	streams := changestream.Tracking{Tables: []changestream.Tracked{
		{Stream: long + "1", Schema: "public", Table: "t"}, {Stream: long + "2", Schema: "public", Table: "t"},
		{Stream: "cs", Schema: "public", Table: strings.Repeat("t", 500)},
	}}
	tests := []struct {
		name string
		drop changestream.Drop
		want string
	}{
		{name: "streams with long names", drop: changestream.Drop{Schema: "public", Table: "t"},
			want: `unit 1 of 1 drops table "t", which 2 change streams name, so no unit ran: Spanner refuses to drop a table a change stream names; ` +
				`drop the change stream, or alter it to stop tracking the table, before dropping the table`},
		{name: "a table with a long name", drop: changestream.Drop{Schema: "public", Table: strings.Repeat("t", 500)},
			want: `unit 1 of 1 drops a table, which a change stream names, so no unit ran: Spanner refuses to drop a table a change stream names; ` +
				`drop the change stream, or alter it to stop tracking the table, before dropping the table`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(changestream.Refusal([]changestream.Drop{test.drop}, streams, 1), qt.Equals, test.want)
		})
	}
}

// The streams that name a table are its removal blockers, each once and in
// name order, with the columns each lists for that table; a stream that names
// the table whole lists none. Another table's rows, and one of the name in
// another schema, are left out.
func TestBlockers_NamesTheStreamsOfATable(t *testing.T) {
	named := changestream.Tracking{
		Tables: slices.Concat(tracking.Tables, []changestream.Tracked{{Stream: "cs0", Schema: "public", Table: "t"}}),
		Columns: slices.Concat(tracking.Columns, []changestream.Tracked{{Stream: "cs3", Schema: "public", Table: "other", Column: "a"},
			{Stream: "cs3", Schema: "public", Table: "t", Column: "a"}}),
	}
	tests := []struct {
		name          string
		schema, table string
		want          []changestream.Blocker
	}{
		{name: "a table three streams name", schema: "public", table: "t",
			want: []changestream.Blocker{{Stream: "cs0"}, {Stream: "cs1"}, {Stream: "cs3", Columns: []string{"a", "b"}}}},
		{name: "a table in another schema", schema: "app", table: "t2", want: []changestream.Blocker{{Stream: "cs4", Columns: []string{"c"}}}},
		{name: "a table no stream names", schema: "app", table: "t"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(named.Blockers(test.schema, test.table), qt.DeepEquals, test.want)
		})
	}
}
