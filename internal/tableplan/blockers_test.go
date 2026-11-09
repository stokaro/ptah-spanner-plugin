package tableplan_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// heldTrackedTable is table t as a read reports it, with the change streams
// that name it as its removal blockers.
func heldTrackedTable(blockers ...*protocol.RemovalBlocker) *protocol.ObservedTable {
	return &protocol.ObservedTable{Name: "t", Schema: "public", Columns: []*protocol.ObservedColumn{
		{Name: "id", DataType: "bigint", ColumnType: "bigint", IsNullable: "NO", PrimaryKey: true},
		{Name: "a", DataType: "bigint", ColumnType: "bigint", IsNullable: "YES"},
		{Name: "b", DataType: "bigint", ColumnType: "bigint", IsNullable: "YES"},
	}, Constraints: []*protocol.ObservedConstraint{{Name: "PK_t", Type: "PRIMARY KEY", ColumnNames: []string{"id"}}}, RemovalBlockers: blockers}
}

// trackedDrop is a request that drops table t, held with blockers.
func trackedDrop(blockers ...*protocol.RemovalBlocker) *protocol.PlanFeaturesRequest {
	return request([]*protocol.PlanningTable{{Subject: subject("t"), Action: protocol.PlanningTable_DROP, Current: heldTrackedTable(blockers...)}},
		common("common/000000", "t", effect(subject("t"), protocol.PlanningEffect_DROP)))
}

// trackedColumnDrop is a request that keeps table t, held with blockers,
// without its column a.
func trackedColumnDrop(blockers ...*protocol.RemovalBlocker) *protocol.PlanFeaturesRequest {
	return request([]*protocol.PlanningTable{{Subject: subject("t"), Action: protocol.PlanningTable_ALTER,
		Desired: &protocol.DeclaredTable{Name: "t", Columns: []*protocol.DeclaredColumn{{Name: "id", Type: "bigint", Primary: true}, {Name: "b", Type: "bigint", Nullable: true}}},
		Current: heldTrackedTable(blockers...)}},
		common("common/000000", "t", effect(subject("t"), protocol.PlanningEffect_READ), effect(child("t", "column", "a"), protocol.PlanningEffect_DROP)))
}

// stream is a change stream that names table t, and the columns it names.
func stream(name string, columns ...string) *protocol.RemovalBlocker {
	return &protocol.RemovalBlocker{Kind: "change_stream", Name: name, Columns: columns}
}

// A plan that drops a table a change stream names, with or without a column
// list, or a column one names in its column list, is refused before a
// statement exists, naming the table or column, every stream that names it,
// and what to do.
func TestPlan_RefusesDroppingWhatAChangeStreamNames(t *testing.T) {
	tests := []struct {
		name    string
		request *protocol.PlanFeaturesRequest
		want    string
	}{
		{name: "a table a stream names whole", request: trackedDrop(stream("cs1")),
			want: `dropping table "t": change stream "cs1" names it, and Spanner refuses to drop a table a change stream names; ` +
				`drop the change stream, or alter it to stop tracking the table, before dropping the table`},
		{name: "a table two streams name", request: trackedDrop(stream("cs3", "a"), stream("cs1")),
			want: `dropping table "t": change streams "cs1" and "cs3" name it, and Spanner refuses to drop a table a change stream names; ` +
				`drop the change stream, or alter it to stop tracking the table, before dropping the table`},
		{name: "a column a stream lists", request: trackedColumnDrop(stream("cs1"), stream("cs3", "a", "b")),
			want: `dropping column "a" of table "t": change stream "cs3" names it, and Spanner refuses to drop a column a change stream names; ` +
				`alter the change stream to stop tracking the column, or drop it, before dropping the column`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(refusal(c, test.request), qt.Equals, test.want)
		})
	}
}

// Spanner drops what no change stream names explicitly: a column of a table a
// stream tracks whole, a column no stream lists, and a table no stream names.
func TestPlan_DropsWhatNoChangeStreamNames(t *testing.T) {
	tests := []struct {
		name    string
		request *protocol.PlanFeaturesRequest
		want    []string
	}{
		{name: "a column of a table a stream tracks whole", request: trackedColumnDrop(stream("cs1")), want: []string{`ALTER TABLE "t" DROP COLUMN "a";`}},
		{name: "a column no stream lists", request: trackedColumnDrop(stream("cs4", "b")), want: []string{`ALTER TABLE "t" DROP COLUMN "a";`}},
		{name: "a table no stream names", request: trackedDrop(), want: []string{`DROP TABLE "t";`}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			steps, _, _ := plan(c, test.request)
			c.Assert(statements(steps), qt.DeepEquals, test.want)
		})
	}
}
