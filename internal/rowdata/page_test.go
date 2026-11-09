package rowdata_test

import (
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"google.golang.org/protobuf/testing/protocmp"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-spanner-plugin/internal/rowdata"
	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

var (
	codeColumn = rowdata.Column{Name: "code", Type: "character varying(10)"}
	idColumn   = rowdata.Column{Name: "id", Type: "bigint"}
	nColumn    = rowdata.Column{Name: "n", Type: "numeric"}
)

// A page orders nulls first and resumes after the last row's ordering values
// with expanded comparisons, which Spanner accepts where it refuses a row
// comparison. Numeric, date, jsonb and uuid columns are read as text.
func TestPageQuery_HappyPath(t *testing.T) {
	tests := []struct {
		name  string
		query rowdata.PageQuery
		want  string
	}{
		{name: "the first page", query: rowdata.PageQuery{Table: "k", Columns: []rowdata.Column{codeColumn, nColumn, idColumn},
			Order: []rowdata.Column{codeColumn, idColumn}, Limit: 3},
			want: `SELECT "code", "n"::text, "id" FROM "k" ORDER BY "code" NULLS FIRST, "id" NULLS FIRST LIMIT 3`},
		{name: "after a row", query: rowdata.PageQuery{Schema: "app", Table: "k", Columns: []rowdata.Column{codeColumn, idColumn},
			Order: []rowdata.Column{codeColumn, idColumn}, After: []*protocol.DataValue{text("it's"), integer(7)}, Limit: 3},
			want: `SELECT "code", "id" FROM "app"."k" WHERE ("code" > 'it''s') OR ("code" = 'it''s' AND "id" > 7) ORDER BY "code" NULLS FIRST, "id" NULLS FIRST LIMIT 3`},
		{name: "after a row with a null key", query: rowdata.PageQuery{Table: "k", Columns: []rowdata.Column{codeColumn, idColumn},
			Order: []rowdata.Column{codeColumn, idColumn}, After: []*protocol.DataValue{null(), integer(7)}, Limit: 2},
			want: `SELECT "code", "id" FROM "k" WHERE ("code" IS NOT NULL) OR ("code" IS NULL AND "id" > 7) ORDER BY "code" NULLS FIRST, "id" NULLS FIRST LIMIT 2`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			got, err := test.query.Text()
			c.Assert(err, qt.IsNil)
			c.Assert(got, qt.Equals, test.want)
		})
	}
}

func TestPageQuery_FailurePath(t *testing.T) {
	tests := []struct {
		name    string
		query   rowdata.PageQuery
		wantErr string
	}{
		{name: "a jsonb key", query: rowdata.PageQuery{Table: "k", Columns: []rowdata.Column{{Name: "j", Type: "jsonb"}},
			Order: []rowdata.Column{{Name: "j", Type: "jsonb"}}, Limit: 2},
			wantErr: `column "j": Spanner compares no jsonb values, so a jsonb column cannot order or address rows`},
		{name: "a token of another key", query: rowdata.PageQuery{Table: "k", Columns: []rowdata.Column{idColumn},
			Order: []rowdata.Column{idColumn}, After: []*protocol.DataValue{integer(1), integer(2)}, Limit: 2},
			wantErr: `a page token names another key than the request`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			got, err := test.query.Text()
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(got, qt.Equals, "")
		})
	}
}

// A token carries the ordering values of the last row of a page and reads
// back as them, null included.
func TestToken_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	values := []*protocol.DataValue{text("a"), null(), integer(3)}
	token, err := rowdata.EncodeToken(values)
	c.Assert(err, qt.IsNil)
	got, err := rowdata.DecodeToken(token, 3)
	c.Assert(err, qt.IsNil)
	c.Assert(got, qt.CmpEquals(protocmp.Transform()), values)
}

func TestToken_FailurePath(t *testing.T) {
	c := testcheck.New(t)
	token, err := rowdata.EncodeToken([]*protocol.DataValue{integer(3)})
	c.Assert(err, qt.IsNil)
	tests := []struct {
		name, token string
		count       int
		wantErr     string
	}{
		{name: "not base64", token: "%%%", count: 1, wantErr: `the page token is not one this provider wrote`},
		{name: "another key length", token: token, count: 2, wantErr: `the page token is not one this provider wrote for this key`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			got, err := rowdata.DecodeToken(test.token, test.count)
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(got, qt.IsNil)
		})
	}
}

// Scanned reads what the driver decodes for each column type through
// PGAdapter's simple protocol, and the text of the columns a page casts.
func TestScanned_HappyPath(t *testing.T) {
	stored := time.Date(2026, 1, 2, 4, 4, 5, 123456000, time.FixedZone("CET", 3600))
	tests := []struct {
		name, columnType string
		scanned          any
		want             *protocol.DataValue
	}{
		{name: "null", columnType: "bigint", scanned: nil, want: null()},
		{name: "bigint", columnType: "bigint", scanned: int64(5), want: integer(5)},
		{name: "boolean", columnType: "boolean", scanned: true, want: boolean(true)},
		{name: "double", columnType: "double precision", scanned: 0.5, want: double(0.5)},
		{name: "real", columnType: "real", scanned: float32(0.5), want: double(0.5)},
		{name: "varchar", columnType: "character varying(5)", scanned: "ünïcö", want: text("ünïcö")},
		{name: "uuid", columnType: "uuid", scanned: "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11", want: text("a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11")},
		{name: "bytea", columnType: "bytea", scanned: []byte("hi"), want: binary("hi")},
		{name: "timestamp in UTC", columnType: "timestamp with time zone", scanned: stored, want: at(stored.UTC())},
		{name: "numeric text", columnType: "numeric", scanned: "12.50", want: decimal("125", 1)},
		{name: "date text", columnType: "date", scanned: "0001-01-01", want: date(-719162)},
		{name: "jsonb text", columnType: "jsonb", scanned: `{"b": 1, "a": 2}`, want: json(`{"a":2,"b":1}`)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			got, err := rowdata.Scanned(test.columnType, test.scanned)
			c.Assert(err, qt.IsNil)
			c.Assert(got, qt.CmpEquals(protocmp.Transform()), test.want)
		})
	}
}

func TestScanned_FailurePath(t *testing.T) {
	tests := []struct {
		name, columnType string
		scanned          any
		wantErr          string
	}{
		{name: "a numeric NaN", columnType: "numeric", scanned: "NaN", wantErr: `a numeric column returned a value that is not a decimal number, such as NaN`},
		{name: "a value of another type", columnType: "bigint", scanned: "5", wantErr: `a bigint column returned text the provider does not read`},
		{name: "a type the provider does not read", columnType: "boolean", scanned: int64(1), wantErr: `a boolean column returned a value the provider does not read`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			got, err := rowdata.Scanned(test.columnType, test.scanned)
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(got, qt.IsNil)
		})
	}
}
