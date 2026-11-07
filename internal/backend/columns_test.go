package backend_test

import (
	"context"
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-spanner-plugin/internal/backend"
	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// receipt is the part of an aspect receipt a test compares.
type receipt struct {
	Aspect           protocol.ColumnAspect
	Relation         protocol.ColumnDifference_Relation
	Current, Desired string
	Nullable         *bool
}

func receipts(result *protocol.ComparedColumns) []receipt {
	var out []receipt
	for _, table := range result.GetTables() {
		for _, column := range table.GetColumns() {
			for _, aspect := range column.GetAspects() {
				got := receipt{Aspect: aspect.GetAspect()}
				if difference := aspect.GetDifference(); difference != nil {
					got.Relation, got.Current, got.Desired = difference.GetRelation(), difference.GetCurrent(), difference.GetDesired()
				}
				if nullability := aspect.GetNullability(); nullability != nil {
					got.Nullable = nullability.Nullable
				}
				out = append(out, got)
			}
		}
	}
	return out
}

func comparison(desired *protocol.DeclaredColumn, current *protocol.ObservedColumn, aspects ...protocol.ColumnAspect) *protocol.CompareColumnsRequest {
	return &protocol.CompareColumnsRequest{Engine: "spanner", Tables: []*protocol.ColumnComparisonTable{{
		Desired: &protocol.DeclaredTable{Name: "t", PrimaryKey: []string{"id"}, Columns: []*protocol.DeclaredColumn{
			{Name: "id", Type: "bigint", Nullable: true}, desired,
		}},
		Current: &protocol.ObservedTable{Name: "t", Columns: []*protocol.ObservedColumn{{Name: "id", ColumnType: "bigint", IsNullable: "NO"}, current}},
		Pairs:   []*protocol.ColumnComparisonPair{{DesiredColumn: desired.GetName(), CurrentColumn: current.GetName(), Aspects: aspects}},
	}}}
}

func TestCompareColumns(t *testing.T) {
	tests := []struct {
		name    string
		desired *protocol.DeclaredColumn
		current *protocol.ObservedColumn
		aspect  protocol.ColumnAspect
		want    receipt
	}{
		{name: "text reads back as character varying", desired: &protocol.DeclaredColumn{Name: "c", Type: "TEXT"},
			current: &protocol.ObservedColumn{Name: "c", DataType: "character varying", ColumnType: "character varying"}, aspect: protocol.ColumnAspect_COLUMN_TYPE,
			want: receipt{Aspect: protocol.ColumnAspect_COLUMN_TYPE, Relation: protocol.ColumnDifference_SAME, Current: "character varying", Desired: "TEXT"}},
		{name: "a changed length", desired: &protocol.DeclaredColumn{Name: "c", Type: "VARCHAR(40)"},
			current: &protocol.ObservedColumn{Name: "c", DataType: "character varying", ColumnType: "character varying(20)"}, aspect: protocol.ColumnAspect_COLUMN_TYPE,
			want: receipt{Aspect: protocol.ColumnAspect_COLUMN_TYPE, Relation: protocol.ColumnDifference_CHANGED, Current: "character varying(20)", Desired: "VARCHAR(40)"}},
		{name: "a literal default stored with a cast", desired: &protocol.DeclaredColumn{Name: "c", Type: "bigint", DefaultSet: true, DefaultValue: "0"},
			current: &protocol.ObservedColumn{Name: "c", ColumnType: "bigint", DefaultExpression: new("'0'::bigint")}, aspect: protocol.ColumnAspect_COLUMN_DEFAULT,
			want: receipt{Aspect: protocol.ColumnAspect_COLUMN_DEFAULT, Relation: protocol.ColumnDifference_SAME, Current: "0", Desired: "0"}},
		{name: "a default expression that is a number", desired: &protocol.DeclaredColumn{Name: "c", Type: "bigint", DefaultExpression: "0"},
			current: &protocol.ObservedColumn{Name: "c", ColumnType: "bigint", DefaultExpression: new("'0'::bigint")}, aspect: protocol.ColumnAspect_COLUMN_DEFAULT,
			want: receipt{Aspect: protocol.ColumnAspect_COLUMN_DEFAULT, Relation: protocol.ColumnDifference_SAME, Current: "0", Desired: "0"}},
		{name: "a changed literal", desired: &protocol.DeclaredColumn{Name: "c", Type: "text", DefaultSet: true, DefaultValue: "b"},
			current: &protocol.ObservedColumn{Name: "c", ColumnType: "character varying", DefaultExpression: new("'a'::text")}, aspect: protocol.ColumnAspect_COLUMN_DEFAULT,
			want: receipt{Aspect: protocol.ColumnAspect_COLUMN_DEFAULT, Relation: protocol.ColumnDifference_CHANGED, Current: "a", Desired: "b"}},
		{name: "a default the declaration drops", desired: &protocol.DeclaredColumn{Name: "c", Type: "text"},
			current: &protocol.ObservedColumn{Name: "c", ColumnType: "character varying", DefaultExpression: new("'a'::text")}, aspect: protocol.ColumnAspect_COLUMN_DEFAULT,
			want: receipt{Aspect: protocol.ColumnAspect_COLUMN_DEFAULT, Relation: protocol.ColumnDifference_CHANGED, Current: "a", Desired: ""}},
		{name: "an expression in another case", desired: &protocol.DeclaredColumn{Name: "c", Type: "timestamptz", DefaultExpression: "NOW()"},
			current: &protocol.ObservedColumn{Name: "c", ColumnType: "timestamp with time zone", DefaultExpression: new("now()")}, aspect: protocol.ColumnAspect_COLUMN_DEFAULT,
			want: receipt{Aspect: protocol.ColumnAspect_COLUMN_DEFAULT, Relation: protocol.ColumnDifference_SAME, Current: "now()", Desired: "NOW()"}},
		{name: "a nullable column", desired: &protocol.DeclaredColumn{Name: "c", Type: "text", Nullable: true},
			current: &protocol.ObservedColumn{Name: "c", ColumnType: "character varying", IsNullable: "YES"}, aspect: protocol.ColumnAspect_COLUMN_NULLABILITY,
			want: receipt{Aspect: protocol.ColumnAspect_COLUMN_NULLABILITY, Nullable: new(true)}},
		{name: "a key column declared nullable", desired: &protocol.DeclaredColumn{Name: "c", Type: "text", Nullable: true, Primary: true},
			current: &protocol.ObservedColumn{Name: "c", ColumnType: "character varying", IsNullable: "NO"}, aspect: protocol.ColumnAspect_COLUMN_NULLABILITY,
			want: receipt{Aspect: protocol.ColumnAspect_COLUMN_NULLABILITY, Nullable: new(false)}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			result, err := backend.New().CompareColumns(context.Background(), comparison(test.desired, test.current, test.aspect))
			c.Assert(err, qt.IsNil)
			c.Assert(result.GetComplete(), qt.IsTrue)
			c.Assert(receipts(result), qt.DeepEquals, []receipt{test.want})
		})
	}
}

func TestCompareColumns_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	request := comparison(&protocol.DeclaredColumn{Name: "c", Type: "text"}, &protocol.ObservedColumn{Name: "c"}, protocol.ColumnAspect_COLUMN_TYPE)
	request.Tables[0].Pairs[0].CurrentColumn = "missing"
	result, err := backend.New().CompareColumns(context.Background(), request)
	c.Assert(err, qt.ErrorMatches, `.*a column pair names a column its table does not hold.*`)
	c.Assert(result, qt.IsNil)
}

func TestReadColumnDefaults(t *testing.T) {
	c := testcheck.New(t)
	request := &protocol.ReadColumnDefaultsRequest{Engine: "spanner", Columns: []*protocol.ObservedColumnDefaultInput{
		{Column: &protocol.ObservedColumn{Name: "a"}},
		{Column: &protocol.ObservedColumn{Name: "b", DefaultExpression: new("'0'::bigint")}},
		{Column: &protocol.ObservedColumn{Name: "c", DefaultExpression: new("now()")}},
	}}
	result, err := backend.New().ReadColumnDefaults(context.Background(), request)
	c.Assert(err, qt.IsNil)
	kinds := make([]protocol.ColumnDefaultReceipt_Kind, len(result.GetColumns()))
	values := make([]string, len(result.GetColumns()))
	for i, column := range result.GetColumns() {
		kinds[i], values[i] = column.GetKind(), column.GetValue()
	}
	c.Assert(kinds, qt.DeepEquals, []protocol.ColumnDefaultReceipt_Kind{
		protocol.ColumnDefaultReceipt_ABSENT, protocol.ColumnDefaultReceipt_LITERAL, protocol.ColumnDefaultReceipt_EXPRESSION,
	})
	c.Assert(values, qt.DeepEquals, []string{"", "0", "now()"})
}
