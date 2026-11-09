//go:build integration

package integration_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	"google.golang.org/protobuf/testing/protocmp"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	providerruntime "ptah.run/provider-sdk/runtime"

	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

func int64Value(v int64) *protocol.DataValue {
	return &protocol.DataValue{Value: &protocol.DataValue_Int64Value{Int64Value: v}}
}

func boolValue(v bool) *protocol.DataValue {
	return &protocol.DataValue{Value: &protocol.DataValue_BoolValue{BoolValue: v}}
}

// xSeed is a seed file that writes one row of x.
var xSeed = &protocol.Seed{Path: "001_x.all.sql", Filename: "001_x.all.sql", Env: "dev",
	Checksum: "748f62889eb2193003cd3e6c7f046a5354dbace99cf3dd3281d3c0fead74537a", Text: "INSERT INTO x (id) VALUES (8);"}

// statement runs one statement on a session and returns its result. The SDK
// client reports an outcome other than SUCCEEDED as an error beside the
// result, so the tests compare the result's outcome.
func statement(c *qt.C, client *providerruntime.Client, request *protocol.RunStatementRequest) *protocol.StatementResult {
	c.Helper()
	result, _ := client.RunStatement(c.Context(), request)
	return result
}

// A read-only query runs in a read-only transaction and returns its columns
// with their types and its rows; a migration's `-- +ptah check` assertion runs
// this way. A statement that changes rows is refused before it runs.
func TestStatementsSubprocess_ReadOnly(t *testing.T) {
	c := testcheck.New(t)
	address := freshDatabase(c)
	execSQL(c, address, "CREATE TABLE t (id bigint NOT NULL, PRIMARY KEY (id))", "INSERT INTO t (id) VALUES (1)")
	client := startProvider(c)
	session := openSession(c, client, address)

	result := statement(c, client, &protocol.RunStatementRequest{SessionId: session.GetId(), MaxRows: 100, RequestId: "r1", Access: protocol.RunStatementRequest_READ_ONLY,
		Sql: "SELECT count(*) = 1 AS one, count(*) AS n FROM t"})
	c.Assert(result.GetOutcome(), qt.Equals, protocol.StatementResult_SUCCEEDED)
	c.Assert(result.GetColumns(), qt.CmpEquals(protocmp.Transform()), []*protocol.DataColumn{
		{Name: "one", Type: "boolean", Kind: protocol.DataKind_DATA_KIND_BOOL},
		{Name: "n", Type: "bigint", Kind: protocol.DataKind_DATA_KIND_INT64},
	})
	c.Assert(result.GetRows(), qt.CmpEquals(protocmp.Transform()), []*protocol.Row{row(boolValue(true), int64Value(1))})
	c.Assert(result.RowsAffected, qt.IsNil)

	result = statement(c, client, &protocol.RunStatementRequest{SessionId: session.GetId(), MaxRows: 100, RequestId: "r2", Access: protocol.RunStatementRequest_READ_ONLY,
		Sql: "DELETE FROM t WHERE true"})
	c.Assert(result.GetOutcome(), qt.Equals, protocol.StatementResult_REFUSED)
	c.Assert(result.GetRefusal(), qt.Equals, "the statement is not a query that only reads rows")
	c.Assert(queryRows(c, address, "SELECT count(*) FROM t"), qt.DeepEquals, []string{"1"})
}

// A statement runs as written with its arguments bound in placeholder order,
// reports the rows it changed, and returns every value as the row services
// read it: a numeric in its shortest decimal, a moment in UTC, a date as days,
// and an array, which no common value holds, as its text under a native type.
// A statement Spanner refuses carries the server's message and SQLSTATE.
func TestStatementsSubprocess_ReadWrite(t *testing.T) {
	c := testcheck.New(t)
	address := freshDatabase(c)
	execSQL(c, address, "CREATE TABLE v (id bigint NOT NULL, s varchar(10), n numeric, d date, ts timestamptz, b bytea, j jsonb, f float8, a bigint[], PRIMARY KEY (id))")
	client := startProvider(c)
	session := openSession(c, client, address)

	result := statement(c, client, &protocol.RunStatementRequest{SessionId: session.GetId(), Access: protocol.RunStatementRequest_READ_WRITE, MaxRows: 100, RequestId: "w1",
		Sql: "INSERT INTO v (id, s, n, d, ts, b, j, f, a) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, ARRAY[1, 2])",
		Arguments: []*protocol.DataValue{int64Value(1), textValue("it's"), decimalValue("1250", 2),
			{Value: &protocol.DataValue_DateValue{DateValue: 20455}},
			{Value: &protocol.DataValue_TimestampValue{TimestampValue: &protocol.TimestampValue{Seconds: 1767323045}}},
			{Value: &protocol.DataValue_BytesValue{BytesValue: []byte("hi")}},
			{Value: &protocol.DataValue_JsonValue{JsonValue: `{"b": 1, "a": 2}`}},
			{Value: &protocol.DataValue_DoubleValue{DoubleValue: 1.5}}}})
	c.Assert(result.GetOutcome(), qt.Equals, protocol.StatementResult_SUCCEEDED)
	c.Assert(result.GetRowsAffected(), qt.Equals, uint64(1))
	c.Assert(result.GetColumns(), qt.HasLen, 0)

	result = statement(c, client, &protocol.RunStatementRequest{SessionId: session.GetId(), Access: protocol.RunStatementRequest_READ_WRITE, MaxRows: 100, RequestId: "w2", Sql: "SELECT s, n, d, ts, b, j, f, a FROM v"})
	c.Assert(result.GetOutcome(), qt.Equals, protocol.StatementResult_SUCCEEDED)
	c.Assert(result.GetRows(), qt.CmpEquals(protocmp.Transform()), []*protocol.Row{row(textValue("it's"), decimalValue("125", 1),
		&protocol.DataValue{Value: &protocol.DataValue_DateValue{DateValue: 20455}},
		&protocol.DataValue{Value: &protocol.DataValue_TimestampValue{TimestampValue: &protocol.TimestampValue{Seconds: 1767323045}}},
		&protocol.DataValue{Value: &protocol.DataValue_BytesValue{BytesValue: []byte("hi")}},
		&protocol.DataValue{Value: &protocol.DataValue_JsonValue{JsonValue: `{"a":2,"b":1}`}},
		&protocol.DataValue{Value: &protocol.DataValue_DoubleValue{DoubleValue: 1.5}},
		&protocol.DataValue{Value: &protocol.DataValue_NativeValue{NativeValue: &protocol.NativeValue{Type: "spanner-postgresql-oid-1016", Payload: []byte("{1,2}")}}},
	)})

	result = statement(c, client, &protocol.RunStatementRequest{SessionId: session.GetId(), Access: protocol.RunStatementRequest_READ_WRITE, MaxRows: 100, RequestId: "w3", Sql: "INSERT INTO v (id) VALUES (1)"})
	c.Assert(result.GetOutcome(), qt.Equals, protocol.StatementResult_FAILED)
	c.Assert(result.GetErrorCode(), qt.Equals, "23505")
	c.Assert(result.GetError(), qt.Matches, `.*previously existing row.*`)
}

// A result larger than a page stays open: each page holds at most the rows
// asked for, the last one releases the result, and closing a released result
// succeeds.
func TestStatementsSubprocess_Pages(t *testing.T) {
	c := testcheck.New(t)
	address := freshDatabase(c)
	execSQL(c, address, "CREATE TABLE p (id bigint NOT NULL, PRIMARY KEY (id))", "INSERT INTO p (id) VALUES (1), (2), (3), (4), (5)")
	client := startProvider(c)
	session := openSession(c, client, address)
	result := statement(c, client, &protocol.RunStatementRequest{SessionId: session.GetId(), Access: protocol.RunStatementRequest_READ_WRITE, RequestId: "p1", MaxRows: 2, Sql: "SELECT id FROM p ORDER BY id"})
	c.Assert(result.GetOutcome(), qt.Equals, protocol.StatementResult_SUCCEEDED)
	c.Assert(result.GetRows(), qt.CmpEquals(protocmp.Transform()), []*protocol.Row{row(int64Value(1)), row(int64Value(2))})
	c.Assert(result.GetNextPageToken(), qt.Not(qt.Equals), "")

	page, err := client.ReadStatementRows(c.Context(), &protocol.ReadStatementRowsRequest{SessionId: session.GetId(), PageToken: result.GetNextPageToken(), MaxRows: 2, ColumnCount: 1})
	c.Assert(err, qt.IsNil)
	c.Assert(page.GetRows(), qt.CmpEquals(protocmp.Transform()), []*protocol.Row{row(int64Value(3)), row(int64Value(4))})
	last, err := client.ReadStatementRows(c.Context(), &protocol.ReadStatementRowsRequest{SessionId: session.GetId(), PageToken: page.GetNextPageToken(), MaxRows: 2, ColumnCount: 1})
	c.Assert(err, qt.IsNil)
	c.Assert(last.GetRows(), qt.CmpEquals(protocmp.Transform()), []*protocol.Row{row(int64Value(5))})
	c.Assert(last.GetNextPageToken(), qt.Equals, "")
	_, err = client.CloseStatement(c.Context(), &protocol.CloseStatementRequest{SessionId: session.GetId(), PageToken: page.GetNextPageToken()})
	c.Assert(err, qt.IsNil)
}

// A statement and a row read that name a transaction run in it and see what
// it wrote, which the session's own reads outside it do not see until it
// commits. A seed waits for the open transaction to end: it runs in a
// transaction of its own and cannot join the session's.
func TestStatementsSubprocess_InATransaction(t *testing.T) {
	c := testcheck.New(t)
	address := freshDatabase(c)
	execSQL(c, address, "CREATE TABLE x (id bigint NOT NULL, PRIMARY KEY (id))")
	client := startProvider(c)
	session := openSession(c, client, address)
	_, err := client.BeginTransaction(c.Context(), &protocol.BeginTransactionRequest{SessionId: session.GetId(), TransactionId: "t1", Isolation: protocol.TransactionIsolation_SERIALIZABLE})
	c.Assert(err, qt.IsNil)

	result := statement(c, client, &protocol.RunStatementRequest{SessionId: session.GetId(), Access: protocol.RunStatementRequest_READ_WRITE, MaxRows: 100, RequestId: "i1", TransactionId: "t1", Sql: "INSERT INTO x (id) VALUES (7)"})
	c.Assert(result.GetOutcome(), qt.Equals, protocol.StatementResult_SUCCEEDED)
	page, err := client.ReadRows(c.Context(), &protocol.ReadRowsRequest{SessionId: session.GetId(), TransactionId: "t1", Table: &protocol.Reference{Kind: "table", Name: "x"},
		Columns: []string{"id"}, KeyColumns: []string{"id"}, MaxRows: 10})
	c.Assert(err, qt.IsNil)
	c.Assert(page.GetRows(), qt.CmpEquals(protocmp.Transform()), []*protocol.Row{row(int64Value(7))})
	outside := statement(c, client, &protocol.RunStatementRequest{SessionId: session.GetId(), MaxRows: 100, RequestId: "i2", Access: protocol.RunStatementRequest_READ_ONLY, Sql: "SELECT count(*) FROM x"})
	c.Assert(outside.GetRows(), qt.CmpEquals(protocmp.Transform()), []*protocol.Row{row(int64Value(0))})

	seed, _ := client.ApplySeed(c.Context(), &protocol.ApplySeedRequest{SessionId: session.GetId(), RequestId: "s1",
		Seed: xSeed})
	c.Assert(seed.GetOutcome(), qt.Equals, protocol.SeedResult_NOT_APPLIED)
	c.Assert(seed.GetReason(), qt.Equals, "the session has an open transaction; finish it before running a seed; nothing ran")

	finished, err := client.FinishTransaction(c.Context(), &protocol.FinishTransactionRequest{SessionId: session.GetId(), TransactionId: "t1", Action: protocol.FinishTransactionRequest_COMMIT})
	c.Assert(err, qt.IsNil)
	c.Assert(finished.GetOutcome(), qt.Equals, protocol.TransactionResult_COMMITTED)
	seed, err = client.ApplySeed(c.Context(), &protocol.ApplySeedRequest{SessionId: session.GetId(), RequestId: "s2",
		Seed: xSeed})
	c.Assert(err, qt.IsNil)
	c.Assert(seed.GetOutcome(), qt.Equals, protocol.SeedResult_APPLIED)
	c.Assert(queryRows(c, address, "SELECT id FROM x ORDER BY id"), qt.DeepEquals, []string{"7", "8"})
}
