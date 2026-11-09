package rowdata_test

import (
	"math"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"google.golang.org/protobuf/testing/protocmp"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-spanner-plugin/internal/rowdata"
	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// Each literal as the emulator reads it. A timestamp carries its offset,
// because Spanner reads a bare one in America/Los_Angeles, and bytea is hex
// digits, which hold any byte.
func TestLiteral_HappyPath(t *testing.T) {
	tests := []struct {
		name, columnType string
		value            *protocol.DataValue
		want             string
	}{
		{name: "null", columnType: "bigint", value: null(), want: "NULL"},
		{name: "true", columnType: "boolean", value: boolean(true), want: "TRUE"},
		{name: "false", columnType: "boolean", value: boolean(false), want: "FALSE"},
		{name: "bigint", columnType: "bigint", value: integer(-3), want: "-3"},
		{name: "double", columnType: "double precision", value: double(1e6), want: "1e+06"},
		{name: "real", columnType: "real", value: double(float64(float32(0.1))), want: "0.1"},
		{name: "real NaN", columnType: "real", value: double(math.NaN()), want: "'NaN'::float4"},
		{name: "infinity", columnType: "double precision", value: double(math.Inf(1)), want: "'Infinity'::float8"},
		{name: "negative infinity", columnType: "double precision", value: double(math.Inf(-1)), want: "'-Infinity'::float8"},
		{name: "numeric", columnType: "numeric", value: decimal("125", 1), want: "'12.5'"},
		{name: "numeric below one", columnType: "numeric", value: decimal("-5", 3), want: "'-0.005'"},
		{name: "a quote doubled", columnType: "character varying", value: text("it's"), want: "'it''s'"},
		{name: "a backslash kept", columnType: "character varying", value: text(`a\b`), want: `'a\b'`},
		{name: "bytea", columnType: "bytea", value: binary("hi"), want: `'\x6869'::bytea`},
		{name: "bytea of no byte", columnType: "bytea", value: binary(""), want: `'\x'::bytea`},
		{name: "timestamp", columnType: "timestamp with time zone", value: at(time.Date(2026, 6, 30, 23, 59, 59, 123456000, time.UTC)),
			want: "'2026-06-30 23:59:59.123456+00:00'"},
		{name: "timestamp at a whole second", columnType: "timestamp with time zone", value: at(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)),
			want: "'2026-01-02 03:04:05+00:00'"},
		{name: "date", columnType: "date", value: date(10956), want: "'1999-12-31'"},
		{name: "date of year 1", columnType: "date", value: date(-719162), want: "'0001-01-01'"},
		{name: "jsonb", columnType: "jsonb", value: json(`{"a":"it's"}`), want: `'{"a":"it''s"}'`},
		{name: "a column of no type", columnType: "", value: text("x"), want: "'x'"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			got, err := rowdata.Literal(test.columnType, test.value)
			c.Assert(err, qt.IsNil)
			c.Assert(got, qt.Equals, test.want)
		})
	}
}

func write(mode protocol.RowWrite_Mode, schema string, columns, keys []string, types map[string]string, values ...*protocol.DataValue) *protocol.RowWrite {
	return &protocol.RowWrite{ChangeId: "w", Mode: mode, Table: &protocol.Reference{Kind: "table", Schema: schema, Name: "kinds"},
		Columns: columns, KeyColumns: keys, Values: values, ColumnTypes: types}
}

var kindTypes = map[string]string{"id": "bigint", "s": "character varying(100)", "by": "bytea", "t": "timestamp with time zone", "j": "jsonb", "n": "numeric"}

// The statements of each mode, in the shape the built-in engine wrote for a
// row diff: columns sorted, the key last and sorted, a null key with IS NULL.
func TestRenderRowWrites_HappyPath(t *testing.T) {
	tests := []struct {
		name  string
		write *protocol.RowWrite
		want  string
	}{
		{name: "insert", write: write(protocol.RowWrite_INSERT, "", []string{"s", "id", "by"}, []string{"id"}, kindTypes, text("it's"), integer(1), text(`\x6869`)),
			want: `INSERT INTO "kinds" ("by", "id", "s") VALUES ('\x6869'::bytea, 1, 'it''s');`},
		{name: "insert in another schema", write: write(protocol.RowWrite_INSERT, "app", []string{"id"}, []string{"id"}, kindTypes, integer(1)),
			want: `INSERT INTO "app"."kinds" ("id") VALUES (1);`},
		{name: "insert in the default schema named", write: write(protocol.RowWrite_INSERT, "public", []string{"id"}, []string{"id"}, kindTypes, integer(1)),
			want: `INSERT INTO "kinds" ("id") VALUES (1);`},
		{name: "upsert", write: write(protocol.RowWrite_UPSERT, "", []string{"s", "id", "n"}, []string{"id"}, kindTypes, text("x"), integer(2), text("12.50")),
			want: `INSERT INTO "kinds" ("id", "n", "s") VALUES (2, '12.5', 'x') ON CONFLICT ("id") DO UPDATE SET "n" = EXCLUDED."n", "s" = EXCLUDED."s";`},
		{name: "upsert of the key alone", write: write(protocol.RowWrite_UPSERT, "", []string{"id"}, []string{"id"}, kindTypes, integer(2)),
			want: `INSERT INTO "kinds" ("id") VALUES (2) ON CONFLICT ("id") DO NOTHING;`},
		{name: "update", write: write(protocol.RowWrite_UPDATE_BY_KEY, "", []string{"t", "id", "s"}, []string{"id"}, kindTypes,
			text("2026-01-02 03:04:05"), integer(3), null()),
			want: `UPDATE "kinds" SET "s" = NULL, "t" = '2026-01-02 03:04:05+00:00' WHERE "id" = 3;`},
		{name: "delete by a null key", write: write(protocol.RowWrite_DELETE_BY_KEY, "", []string{"s", "id"}, []string{"s", "id"}, kindTypes, null(), integer(4)),
			want: `DELETE FROM "kinds" WHERE "id" = 4 AND "s" IS NULL;`},
		{name: "a column of no type", write: write(protocol.RowWrite_INSERT, "", []string{"id", "x"}, []string{"id"}, kindTypes, integer(5), double(1.5)),
			want: `INSERT INTO "kinds" ("id", "x") VALUES (5, 1.5);`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			got, err := rowdata.RenderRowWrites(&protocol.RenderRowWritesRequest{Engine: "spanner", Writes: []*protocol.RowWrite{test.write}})
			c.Assert(err, qt.IsNil)
			c.Assert(got, qt.CmpEquals(protocmp.Transform()), &protocol.RenderResult{Complete: true,
				Units: []*protocol.Unit{{ChangeId: "w", Body: &protocol.Unit_Sql{Sql: test.want}}}})
		})
	}
}

// A write the provider cannot spell refuses the whole batch and names the
// write and the column, never the value.
func TestRenderRowWrites_Refusal(t *testing.T) {
	tests := []struct {
		name   string
		write  *protocol.RowWrite
		reason protocol.RenderRefusal_Reason
		want   string
	}{
		{name: "a jsonb key", write: write(protocol.RowWrite_DELETE_BY_KEY, "", []string{"j"}, []string{"j"}, kindTypes, json(`{}`)),
			reason: protocol.RenderRefusal_UNSUPPORTED_VALUE, want: `column "j": Spanner compares no jsonb values, so a jsonb column cannot order or address rows`},
		{name: "a value its column cannot hold", write: write(protocol.RowWrite_INSERT, "", []string{"id"}, []string{"id"}, kindTypes, text("one")),
			reason: protocol.RenderRefusal_UNSUPPORTED_VALUE, want: `column "id": a text value is not an integer in the range of bigint`},
		{name: "an array column", write: write(protocol.RowWrite_INSERT, "", []string{"a"}, nil, map[string]string{"a": "bigint[]"}, integer(1)),
			reason: protocol.RenderRefusal_UNSUPPORTED_VALUE, want: `column "a": Ptah writes no value of type bigint\[\]`},
		{name: "an upsert with no key", write: write(protocol.RowWrite_UPSERT, "", []string{"id"}, nil, kindTypes, integer(1)),
			reason: protocol.RenderRefusal_UNSUPPORTED_MODE, want: `an UPSERT names no key to resolve a conflict on`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			got, err := rowdata.RenderRowWrites(&protocol.RenderRowWritesRequest{Engine: "spanner", Writes: []*protocol.RowWrite{
				write(protocol.RowWrite_INSERT, "", []string{"id"}, []string{"id"}, kindTypes, integer(9)), test.write,
			}})
			c.Assert(err, qt.IsNil)
			c.Assert(got.GetUnits(), qt.HasLen, 0)
			c.Assert(got.GetComplete(), qt.IsFalse)
			c.Assert(got.GetRefusal().GetReason(), qt.Equals, test.reason)
			c.Assert(got.GetRefusal().GetMessage(), qt.Matches, test.want)
		})
	}
}

func TestRenderRowWrites_FailurePath(t *testing.T) {
	tests := []struct {
		name    string
		write   *protocol.RowWrite
		wantErr string
	}{
		{name: "an update that sets nothing", write: write(protocol.RowWrite_UPDATE_BY_KEY, "", []string{"id"}, []string{"id"}, kindTypes, integer(1)),
			wantErr: `an UPDATE_BY_KEY write sets no column outside its key`},
		{name: "a table under a path", write: &protocol.RowWrite{ChangeId: "w", Mode: protocol.RowWrite_INSERT,
			Table: &protocol.Reference{Kind: "table", Name: "t", Path: []string{"x"}}, Columns: []string{"id"}, Values: []*protocol.DataValue{integer(1)}},
			wantErr: `a Spanner table is named by its schema and name`},
		{name: "values that do not align", write: &protocol.RowWrite{ChangeId: "w", Mode: protocol.RowWrite_INSERT,
			Table: &protocol.Reference{Kind: "table", Name: "t"}, Columns: []string{"id", "s"}, Values: []*protocol.DataValue{integer(1)}},
			wantErr: `a row write carries 1 values for 2 columns`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			got, err := rowdata.RenderRowWrites(&protocol.RenderRowWritesRequest{Engine: "spanner", Writes: []*protocol.RowWrite{test.write}})
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(got, qt.IsNil)
		})
	}
}

// TypeRows types a column by the live table where it exists and by its
// declaration otherwise, and brings both row sets into one form: a declared
// `12.50` and the 12.5 a read returns are the same value.
func TestTypeRows_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	got, err := rowdata.TypeRows(&protocol.TypeRowsRequest{
		Engine:    "spanner",
		LiveTable: &protocol.LiveTable{Columns: []*protocol.Column{{Name: "id", Type: "bigint"}, {Name: "n", Type: "numeric"}, {Name: "j", Type: "jsonb"}}},
		Columns: []*protocol.RowColumn{
			{Name: "id", DeclaredType: "TEXT"}, {Name: "j", DeclaredType: "JSONB"}, {Name: "n", DeclaredType: "NUMERIC"},
			{Name: "t", DeclaredType: "TIMESTAMPTZ"}, {Name: "x"},
		},
		Declared: []*protocol.Row{{Values: []*protocol.DataValue{integer(1), text(`{"b": 1, "a": 2}`), text("12.50"), text("2026-01-02 03:04:05"), text("kept")}}},
		Live:     []*protocol.Row{{Values: []*protocol.DataValue{integer(1), json(`{"a": 2, "b": 1}`), decimal("125", 1), {}, null()}}},
	})
	c.Assert(err, qt.IsNil)
	c.Assert(got, qt.CmpEquals(protocmp.Transform()), &protocol.TypedRows{
		Columns: []*protocol.DataColumn{
			{Name: "id", Type: "bigint", Kind: protocol.DataKind_DATA_KIND_INT64},
			{Name: "j", Type: "jsonb", Kind: protocol.DataKind_DATA_KIND_JSON},
			{Name: "n", Type: "numeric", Kind: protocol.DataKind_DATA_KIND_DECIMAL},
			{Name: "t", Type: "timestamp with time zone", Kind: protocol.DataKind_DATA_KIND_TIMESTAMP},
			{Name: "x"},
		},
		Declared: []*protocol.Row{{Values: []*protocol.DataValue{integer(1), json(`{"a":2,"b":1}`), decimal("125", 1),
			at(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)), text("kept")}}},
		Live: []*protocol.Row{{Values: []*protocol.DataValue{integer(1), json(`{"a":2,"b":1}`), decimal("125", 1), {}, null()}}},
	})
}

func TestTypeRows_FailurePath(t *testing.T) {
	tests := []struct {
		name    string
		request *protocol.TypeRowsRequest
		wantErr string
	}{
		{name: "a declared value its column cannot hold", request: &protocol.TypeRowsRequest{
			Columns:  []*protocol.RowColumn{{Name: "n", DeclaredType: "numeric"}},
			Declared: []*protocol.Row{{Values: []*protocol.DataValue{text("1")}}, {Values: []*protocol.DataValue{text("NaN")}}},
		}, wantErr: `declared row 2, column "n": a text value is not a decimal number; Ptah writes no numeric NaN or infinity`},
		{name: "a live array column", request: &protocol.TypeRowsRequest{
			LiveTable: &protocol.LiveTable{Columns: []*protocol.Column{{Name: "a", Type: "bigint[]"}}},
			Columns:   []*protocol.RowColumn{{Name: "a"}},
		}, wantErr: `column "a": Ptah writes no value of type bigint\[\]`},
		{name: "a row that does not align", request: &protocol.TypeRowsRequest{
			Columns: []*protocol.RowColumn{{Name: "a", DeclaredType: "bigint"}},
			Live:    []*protocol.Row{{Values: []*protocol.DataValue{integer(1), integer(2)}}},
		}, wantErr: `live row 1 carries 2 values for 1 columns`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			got, err := rowdata.TypeRows(test.request)
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(got, qt.IsNil)
		})
	}
}
