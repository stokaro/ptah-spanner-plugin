package backend

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"
	"ptah.run/provider-sdk/runtime"

	"github.com/stokaro/ptah-spanner-plugin/internal/migrationunits"
	"github.com/stokaro/ptah-spanner-plugin/internal/rowdata"
)

// Bounds of the operator statements a session runs.
const (
	// resultLimit bounds the results a session keeps open past their first
	// page.
	resultLimit = 16
	// maxResultBytes bounds the rows one result holds in memory. A larger
	// result is not read in part.
	maxResultBytes = 64 << 20
	// statementPageBytes bounds one page below the transport's message limit.
	statementPageBytes = 3 << 20
)

// openResult is the rest of a result past the pages already handed out. The
// provider reads a result whole before it answers, so no open result holds the
// session's connection.
type openResult struct {
	rows    []*protocol.Row
	columns int
	// transaction is the ID of the transaction the statement ran in, whose
	// end releases the result.
	transaction string
}

// RunStatement runs one statement the operator wrote, such as a migration's
// `-- +ptah check` assertion, and returns what it read.
//
// A READ_ONLY statement runs only when it is one query that changes no row
// (see migrationunits.ReadOnly), and then in a read-only transaction, where
// Spanner refuses a write. Spanner's PostgreSQL interface reaches one database
// per connection, so a query reads only objects of the session's database.
// Any other statement runs as written: in the request's transaction, or on
// its own, where a schema statement runs outside a transaction and a row
// write commits as it runs.
func (p *Provider) RunStatement(ctx context.Context, request *protocol.RunStatementRequest) (*protocol.StatementResult, error) {
	readOnly := request.GetAccess() == protocol.RunStatementRequest_READ_ONLY
	refuse := func(reason string) (*protocol.StatementResult, error) {
		return &protocol.StatementResult{RequestId: request.GetRequestId(), Outcome: protocol.StatementResult_REFUSED, Refusal: reason}, nil
	}
	if readOnly {
		if ok, reason := migrationunits.ReadOnly(request.GetSql()); !ok {
			return refuse(reason)
		}
	}
	arguments, reason := statementArguments(request.GetArguments())
	if reason != "" {
		return refuse(reason)
	}
	current, err := p.lookup(request.GetSessionId())
	if err != nil {
		return nil, err
	}
	current.mu.Lock()
	defer current.mu.Unlock()
	target, err := current.statementTarget(ctx, request)
	if err != nil {
		return nil, err
	}
	defer target.end()
	q, record := target.q, target.record
	if !readOnly && !current.guard(ctx) {
		return refuse("the session lost a lock it held, and runs nothing more")
	}
	read, err := runQuery(ctx, q, request.GetSql(), arguments)
	if failed, found := errors.AsType[*pgconn.PgError](err); found {
		if record != nil {
			record.failed = true
		}
		return &protocol.StatementResult{RequestId: request.GetRequestId(), Outcome: protocol.StatementResult_FAILED,
			Error: serverMessage(failed.Message), ErrorCode: failed.Code}, nil
	}
	if err != nil {
		if record != nil {
			record.failed = true
		}
		if readOnly {
			return nil, statementFailure(ctx, err)
		}
		// The statement may have run: nobody knows whether it applied.
		return &protocol.StatementResult{RequestId: request.GetRequestId(), Outcome: protocol.StatementResult_UNKNOWN}, nil
	}
	result := &protocol.StatementResult{RequestId: request.GetRequestId(), Outcome: protocol.StatementResult_SUCCEEDED, Columns: read.columns}
	if read.tag.Insert() || read.tag.Update() || read.tag.Delete() {
		affected := uint64(max(read.tag.RowsAffected(), 0)) // #nosec G115 -- a row count is not negative, and max keeps it so
		result.RowsAffected = &affected
	}
	first, rest := statementPage(read.rows, request.GetMaxRows())
	result.Rows = first
	if len(rest) > 0 {
		token, err := current.keepResult(rest, len(read.columns), request.GetTransactionId())
		if err != nil {
			if readOnly {
				return nil, err
			}
			return &protocol.StatementResult{RequestId: request.GetRequestId(), Outcome: protocol.StatementResult_UNKNOWN}, nil
		}
		result.NextPageToken = token
	}
	return result, nil
}

// statementTarget is where a statement runs: the querier, the caller's
// transaction it runs in, if any, and end, which finishes what the target
// opened.
type statementTarget struct {
	q      querier
	record *transaction
	end    func()
}

// statementTarget opens where a statement runs: a read-only transaction of
// its own for a READ_ONLY statement, the transaction the request names, or
// the session's connection. A read-only transaction changed nothing, so its
// end rolls it back. The caller holds mu.
func (s *session) statementTarget(ctx context.Context, request *protocol.RunStatementRequest) (statementTarget, error) {
	switch {
	case request.GetAccess() == protocol.RunStatementRequest_READ_ONLY:
		tx, err := s.conn.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
		if err != nil {
			return statementTarget{}, callFailed(ctx, "the provider could not begin a read-only transaction")
		}
		return statementTarget{q: tx, end: func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }}, nil
	case request.GetTransactionId() != "":
		record, err := s.openTransaction(request.GetTransactionId())
		if err != nil {
			return statementTarget{}, err
		}
		return statementTarget{q: record.tx, record: record, end: func() {}}, nil
	}
	return statementTarget{q: s.conn, end: func() {}}, nil
}

// querier runs a statement and returns its rows. *pgx.Conn and pgx.Tx
// implement it.
type querier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

// queryResult is a statement's whole result: its columns with their types,
// every row, and the command tag that says what it changed.
type queryResult struct {
	columns []*protocol.DataColumn
	rows    []*protocol.Row
	tag     pgconn.CommandTag
}

// ReadStatementRows hands out the next page of a result RunStatement kept
// open. The last page releases the result.
func (p *Provider) ReadStatementRows(_ context.Context, request *protocol.ReadStatementRowsRequest) (*protocol.StatementRows, error) {
	current, err := p.lookup(request.GetSessionId())
	if err != nil {
		return nil, err
	}
	current.mu.Lock()
	defer current.mu.Unlock()
	open := current.results[request.GetPageToken()]
	switch {
	case open == nil:
		return nil, runtime.ErrorWithReason(codes.FailedPrecondition, "the result is released or was never opened")
	case int(request.GetColumnCount()) != open.columns:
		return nil, runtime.ErrorWithReason(codes.InvalidArgument, "the result has another number of columns")
	}
	delete(current.results, request.GetPageToken())
	page, rest := statementPage(open.rows, request.GetMaxRows())
	result := &protocol.StatementRows{Rows: page}
	if len(rest) > 0 {
		result.NextPageToken = rand.Text()
		current.results[result.NextPageToken] = &openResult{rows: rest, columns: open.columns, transaction: open.transaction}
	}
	return result, nil
}

// CloseStatement releases a result before its last page. A result already
// released closes again without error.
func (p *Provider) CloseStatement(_ context.Context, request *protocol.CloseStatementRequest) (*protocol.Empty, error) {
	current, err := p.lookup(request.GetSessionId())
	if err != nil {
		return nil, err
	}
	current.mu.Lock()
	defer current.mu.Unlock()
	delete(current.results, request.GetPageToken())
	return &protocol.Empty{}, nil
}

// keepResult keeps the rows of a result past its first page under a new
// token. The caller holds mu.
func (s *session) keepResult(rows []*protocol.Row, columns int, transaction string) (string, error) {
	if len(s.results) >= resultLimit {
		return "", runtime.ErrorWithReason(codes.ResourceExhausted, "the session keeps the most results it holds open; read or close one")
	}
	if s.results == nil {
		s.results = make(map[string]*openResult)
	}
	token := rand.Text()
	s.results[token] = &openResult{rows: rows, columns: columns, transaction: transaction}
	return token, nil
}

// closeResults releases the results read in a transaction that ended. The
// caller holds mu.
func (s *session) closeResults(transaction string) {
	for token, open := range s.results {
		if open.transaction == transaction {
			delete(s.results, token)
		}
	}
}

// statementPage splits rows into a page of at most maxRows rows within the
// page bound, and the rest. A page holds at least one row.
func statementPage(rows []*protocol.Row, maxRows uint32) (page, rest []*protocol.Row) {
	size := 0
	for i, row := range rows {
		size += proto.Size(row)
		if i == int(maxRows) || i > 0 && size > statementPageBytes {
			return rows[:i], rows[i:]
		}
	}
	return rows, nil
}

// runQuery runs a statement and reads its whole result: the columns with
// their types and every row, within maxResultBytes.
func runQuery(ctx context.Context, q querier, sql string, arguments []any) (queryResult, error) {
	rows, err := q.Query(ctx, sql, arguments...)
	if err != nil {
		return queryResult{}, err
	}
	defer rows.Close()
	fields := rows.FieldDescriptions()
	columns := make([]*protocol.DataColumn, len(fields))
	for i, field := range fields {
		columns[i] = resultColumn(field)
	}
	var result []*protocol.Row
	size := 0
	for rows.Next() {
		decoded, err := rows.Values()
		if err != nil {
			return queryResult{}, err
		}
		raw := rows.RawValues()
		row := &protocol.Row{Values: make([]*protocol.DataValue, len(fields))}
		for i, column := range columns {
			if row.Values[i], err = resultValue(column, decoded[i], raw[i]); err != nil {
				return queryResult{}, err
			}
		}
		if size += proto.Size(row); size > maxResultBytes {
			return queryResult{}, errors.New("the result is larger than the provider reads")
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return queryResult{}, err
	}
	return queryResult{columns: columns, rows: result, tag: rows.CommandTag()}, nil
}

// resultTypes are the types a result column of a PostgreSQL type OID takes,
// in the spelling information_schema gives a column of that type. A smaller
// integer reads as bigint, the only integer type Spanner stores.
var resultTypes = map[uint32]string{
	16: "boolean", 20: "bigint", 21: "bigint", 23: "bigint", 700: "real", 701: "double precision", 1700: "numeric",
	25: "character varying", 1043: "character varying", 17: "bytea", 1184: "timestamp with time zone", 1082: "date",
	3802: "jsonb", 2950: "uuid",
}

// textTypes are the result types read from the text the server sent rather
// than the value the driver decoded, as rowdata.Scanned reads them.
var textTypes = map[string]bool{"numeric": true, "date": true, "jsonb": true, "uuid": true}

// nativePrefix names the native type of a result column of a type no common
// value holds, such as an array, with its type OID after it.
const nativePrefix = "spanner-postgresql-oid-"

// resultColumn is a result column as the protocol carries it: a type and the
// case its values take, or a native type for a type no common value holds.
func resultColumn(field pgconn.FieldDescription) *protocol.DataColumn {
	if spelling, found := resultTypes[field.DataTypeOID]; found {
		return &protocol.DataColumn{Name: field.Name, Type: spelling, Kind: rowdata.Kind(spelling)}
	}
	native := fmt.Sprintf("%s%d", nativePrefix, field.DataTypeOID)
	return &protocol.DataColumn{Name: field.Name, Type: native, Kind: protocol.DataKind_DATA_KIND_NATIVE, NativeType: native}
}

// resultValue is one value of a result column, from the value the driver
// decoded or the text the server sent.
func resultValue(column *protocol.DataColumn, decoded any, raw []byte) (*protocol.DataValue, error) {
	switch {
	case decoded == nil:
		return &protocol.DataValue{Value: &protocol.DataValue_Null{Null: &protocol.Empty{}}}, nil
	case column.GetKind() == protocol.DataKind_DATA_KIND_NATIVE:
		return &protocol.DataValue{Value: &protocol.DataValue_NativeValue{NativeValue: &protocol.NativeValue{Type: column.GetNativeType(), Payload: append([]byte(nil), raw...)}}}, nil
	case textTypes[column.GetType()]:
		return rowdata.Scanned(column.GetType(), string(raw))
	}
	switch number := decoded.(type) {
	case int16:
		decoded = int64(number)
	case int32:
		decoded = int64(number)
	}
	return rowdata.Scanned(column.GetType(), decoded)
}

// statementArguments are the arguments of a statement as the driver binds
// them, in the order the statement's placeholders take them, or the reason
// one cannot be bound.
func statementArguments(values []*protocol.DataValue) ([]any, string) {
	arguments := make([]any, len(values))
	for i, value := range values {
		switch v := value.GetValue().(type) {
		case *protocol.DataValue_Null:
			arguments[i] = nil
		case *protocol.DataValue_BoolValue:
			arguments[i] = v.BoolValue
		case *protocol.DataValue_Int64Value:
			arguments[i] = v.Int64Value
		case *protocol.DataValue_Uint64Value:
			if v.Uint64Value > math.MaxInt64 {
				return nil, fmt.Sprintf("argument %d is larger than a Spanner bigint holds", i+1)
			}
			arguments[i] = int64(v.Uint64Value)
		case *protocol.DataValue_DoubleValue:
			arguments[i] = v.DoubleValue
		case *protocol.DataValue_StringValue:
			arguments[i] = v.StringValue
		case *protocol.DataValue_BytesValue:
			arguments[i] = v.BytesValue
		case *protocol.DataValue_DecimalValue:
			arguments[i] = decimalArgument(v.DecimalValue)
		case *protocol.DataValue_TimestampValue:
			arguments[i] = time.Unix(v.TimestampValue.GetSeconds(), int64(v.TimestampValue.GetNanos())).UTC()
		case *protocol.DataValue_DateValue:
			arguments[i] = time.Unix(v.DateValue*86400, 0).UTC().Format(time.DateOnly)
		case *protocol.DataValue_JsonValue:
			arguments[i] = v.JsonValue
		default:
			return nil, fmt.Sprintf("argument %d is a value the provider binds no parameter of, such as an interval", i+1)
		}
	}
	return arguments, ""
}

// decimalArgument is a decimal as the text a numeric parameter reads.
func decimalArgument(value *protocol.DecimalValue) string {
	digits, negative := strings.CutPrefix(value.GetDigits(), "-")
	scale := int(value.GetScale())
	if len(digits) <= scale {
		digits = strings.Repeat("0", scale-len(digits)+1) + digits
	}
	text := digits
	if scale > 0 {
		text = digits[:len(digits)-scale] + "." + digits[len(digits)-scale:]
	}
	if negative {
		text = "-" + text
	}
	return text
}

// serverMessage is the server's message for a failed statement, as the
// contract carries it: valid text within provider.MaxStatementErrorBytes.
func serverMessage(message string) string {
	message = strings.ToValidUTF8(strings.TrimSpace(message), "?")
	if message == "" {
		message = "Spanner refused the statement"
	}
	for len(message) > provider.MaxStatementErrorBytes {
		message = strings.ToValidUTF8(message[:provider.MaxStatementErrorBytes], "")
	}
	return message
}

// statementFailure reports a read-only statement whose result could not be
// read. It changed nothing, so the failure is an error rather than an
// unknown outcome.
func statementFailure(ctx context.Context, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return callFailed(ctx, "the query was canceled")
	}
	return runtime.ErrorWithReason(codes.FailedPrecondition, "the provider could not read the query's result")
}
