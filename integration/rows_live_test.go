//go:build integration

package integration_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"google.golang.org/protobuf/testing/protocmp"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

func nullValue() *protocol.DataValue {
	return &protocol.DataValue{Value: &protocol.DataValue_Null{Null: &protocol.Empty{}}}
}

func textValue(v string) *protocol.DataValue {
	return &protocol.DataValue{Value: &protocol.DataValue_StringValue{StringValue: v}}
}

func decimalValue(digits string, scale uint32) *protocol.DataValue {
	return &protocol.DataValue{Value: &protocol.DataValue_DecimalValue{DecimalValue: &protocol.DecimalValue{Digits: digits, Scale: scale}}}
}

func row(values ...*protocol.DataValue) *protocol.Row { return &protocol.Row{Values: values} }

// readAll reads every page of a table, two rows a page, and returns the rows
// of every page in order and the number of pages.
func readAll(c *qt.C, request *protocol.ReadRowsRequest) ([]*protocol.Row, int) {
	c.Helper()
	client := startProvider(c)
	request.SessionId = openSession(c, client, request.GetSessionId()).GetId()
	var rows []*protocol.Row
	pages := 0
	for request.GetPageToken() != "" || pages == 0 {
		page, err := client.ReadRows(c.Context(), request)
		c.Assert(err, qt.IsNil)
		c.Assert(page.GetExists(), qt.IsTrue)
		rows = append(rows, page.GetRows()...)
		pages++
		request.PageToken = page.GetNextPageToken()
	}
	return rows, pages
}

// A read pages through a table in the order of the key it names, nulls
// first, with the primary key breaking ties the key leaves, so every row
// arrives once although the key is not unique. Numeric values arrive in their
// shortest decimal form and moments in UTC.
func TestReadRowsPagesInKeyOrderLive(t *testing.T) {
	c := testcheck.New(t)
	address := freshDatabase(c)
	execSQL(c, address, "CREATE TABLE k (id bigint NOT NULL, code varchar(10), n numeric, t timestamptz, PRIMARY KEY (id))",
		"INSERT INTO k (id, code, n, t) VALUES (1, 'b', '12.50', '2026-01-02T03:04:05Z'), (2, NULL, '1e3', NULL), (3, 'a', NULL, NULL), "+
			"(4, NULL, '-0.0', NULL), (5, 'b', '0.000100', NULL)")
	rows, pages := readAll(c, &protocol.ReadRowsRequest{SessionId: address, Table: &protocol.Reference{Kind: "table", Name: "k"},
		Columns: []string{"code", "n", "t"}, KeyColumns: []string{"code"}, MaxRows: 2})
	c.Assert(pages, qt.Equals, 3)
	c.Assert(rows, qt.CmpEquals(protocmp.Transform()), []*protocol.Row{
		row(nullValue(), decimalValue("1000", 0), nullValue()),
		row(nullValue(), decimalValue("0", 0), nullValue()),
		row(textValue("a"), nullValue(), nullValue()),
		row(textValue("b"), decimalValue("125", 1), &protocol.DataValue{Value: &protocol.DataValue_TimestampValue{TimestampValue: &protocol.TimestampValue{Seconds: 1767323045}}}),
		row(textValue("b"), decimalValue("1", 4), nullValue()),
	})
}

// A table the database does not hold is a complete page that says so, and a
// column the table does not hold refuses the read.
func TestReadRowsOfAMissingTableOrColumnLive(t *testing.T) {
	c := testcheck.New(t)
	address := freshDatabase(c)
	execSQL(c, address, "CREATE TABLE k (id bigint NOT NULL, PRIMARY KEY (id))")
	client := startProvider(c)
	session := openSession(c, client, address)
	page, err := client.ReadRows(c.Context(), &protocol.ReadRowsRequest{SessionId: session.GetId(), Table: &protocol.Reference{Kind: "table", Name: "absent"},
		Columns: []string{"id"}, KeyColumns: []string{"id"}, MaxRows: 10})
	c.Assert(err, qt.IsNil)
	c.Assert(page.GetExists(), qt.IsFalse)
	c.Assert(page.GetComplete(), qt.IsTrue)
	_, err = client.ReadRows(c.Context(), &protocol.ReadRowsRequest{SessionId: session.GetId(), Table: &protocol.Reference{Kind: "table", Name: "k"},
		Columns: []string{"id", "nope"}, KeyColumns: []string{"id"}, MaxRows: 10})
	c.Assert(err, qt.ErrorMatches, `.*\(FailedPrecondition\): the table holds no column "nope"`)
}
