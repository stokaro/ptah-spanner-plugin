package backend

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-spanner-plugin/internal/rowdata"
)

// pageBytes bounds the rows of one page, so the page and its envelope stay
// within the transport's message limit.
const pageBytes = 3 << 20

// TypeRows types the declared and live rows of one table offline; see
// rowdata.TypeRows. A value its column cannot hold refuses the request.
func (*Provider) TypeRows(ctx context.Context, request *protocol.TypeRowsRequest) (*protocol.TypedRows, error) {
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	result, err := rowdata.TypeRows(request)
	if err != nil {
		return nil, runtime.ErrorWithReason(codes.InvalidArgument, err.Error())
	}
	return result, nil
}

// RenderRowWrites renders typed row writes offline; see
// rowdata.RenderRowWrites. Every unit is a statement, so the TEXT form a
// migration file asks for is the same text.
func (*Provider) RenderRowWrites(ctx context.Context, request *protocol.RenderRowWritesRequest) (*protocol.RenderResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	result, err := rowdata.RenderRowWrites(request)
	if err != nil {
		return nil, runtime.ErrorWithReason(codes.InvalidArgument, err.Error())
	}
	return result, nil
}

// ReadRows reads one page of a table in key order. A table that does not
// exist is a complete page that says so; a table that does not hold a
// requested column, or holds one of a type the provider reads no value of, is
// an error. The provider offers no transaction, so a read names none.
func (p *Provider) ReadRows(ctx context.Context, request *protocol.ReadRowsRequest) (*protocol.RowPage, error) {
	table := request.GetTable()
	if err := rowdata.ValidateTable(table); err != nil {
		return nil, runtime.ErrorWithReason(codes.InvalidArgument, err.Error())
	}
	if request.GetTransactionId() != "" {
		return nil, runtime.ErrorWithReason(codes.InvalidArgument, "the provider offers no transactions, so a row read names none")
	}
	current, err := p.lookup(request.GetSessionId())
	if err != nil {
		return nil, err
	}
	if table.GetDatabase() != "" && table.GetDatabase() != current.database {
		return nil, runtime.ErrorWithReason(codes.InvalidArgument, "the table belongs to another database than the session's")
	}
	schema := table.GetSchema()
	if schema == "" {
		schema = defaultSchema
	}
	current.mu.Lock()
	defer current.mu.Unlock()
	read, err := tableColumns(ctx, current.conn, schema, table.GetName())
	if err != nil {
		return nil, callFailed(ctx, "the provider could not read the table's columns from the Spanner catalog")
	}
	if read == nil {
		return &protocol.RowPage{Table: proto.CloneOf(table), Exists: false, Complete: true}, nil
	}
	query, columns, err := pageQuery(request, schema, read)
	if err != nil {
		return nil, err
	}
	text, err := query.Text()
	if err != nil {
		return nil, runtime.ErrorWithReason(codes.FailedPrecondition, err.Error())
	}
	result := &protocol.RowPage{Table: proto.CloneOf(table), Exists: true, Columns: columns}
	last, more, err := readPage(ctx, current.conn, text, query, request, result)
	if err != nil {
		if driverFailed := (*driverError)(nil); errors.As(err, &driverFailed) {
			return nil, callFailed(ctx, "the row read did not complete")
		}
		return nil, runtime.ErrorWithReason(codes.FailedPrecondition, err.Error())
	}
	if !more {
		result.Complete = true
		return result, nil
	}
	if result.NextPageToken, err = rowdata.EncodeToken(last); err != nil {
		return nil, runtime.ErrorWithReason(codes.FailedPrecondition, err.Error())
	}
	return result, nil
}

// readTable is a base table as a page reads it: the canonical type of each
// column, and the primary key columns in key order. A column of a type the
// provider reads no value of keeps its catalog spelling, which a page refuses
// when it reads the column.
type readTable struct {
	types      map[string]string
	primaryKey []string
}

// tableColumns reads the base table schema.name, or nil when it does not
// exist.
func tableColumns(ctx context.Context, conn *pgx.Conn, schema, name string) (*readTable, error) {
	rows, err := conn.Query(ctx, `SELECT c.column_name, c.spanner_type FROM information_schema.columns c
JOIN information_schema.tables t ON t.table_schema = c.table_schema AND t.table_name = c.table_name
WHERE c.table_schema = $1 AND c.table_name = $2 AND t.table_type = 'BASE TABLE'
ORDER BY c.ordinal_position`, schema, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	table := &readTable{types: make(map[string]string)}
	for rows.Next() {
		var column, spelling string
		if err := rows.Scan(&column, &spelling); err != nil {
			return nil, err
		}
		table.types[column] = spelling
		if canonical, err := rowdata.ColumnType(spelling); err == nil {
			table.types[column] = canonical
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(table.types) == 0 {
		return nil, nil
	}
	keys, err := conn.Query(ctx, `SELECT column_name FROM information_schema.index_columns
WHERE table_schema = $1 AND table_name = $2 AND index_name = 'PRIMARY_KEY' AND ordinal_position IS NOT NULL
ORDER BY ordinal_position`, schema, name)
	if err != nil {
		return nil, err
	}
	if table.primaryKey, err = pgx.CollectRows(keys, pgx.RowTo[string]); err != nil {
		return nil, err
	}
	return table, nil
}

// pageQuery is the query of the page a request asks for and the requested
// columns with their types. The rows are ordered by the request's key, then
// by the primary key columns the key leaves out, so the order is total even
// when the key is not unique.
func pageQuery(request *protocol.ReadRowsRequest, schema string, read *readTable) (rowdata.PageQuery, []*protocol.DataColumn, error) {
	types := read.types
	query := rowdata.PageQuery{Schema: schema, Table: request.GetTable().GetName(), Limit: rowdata.Limit(request.GetMaxRows())}
	columns := make([]*protocol.DataColumn, len(request.GetColumns()))
	for i, name := range request.GetColumns() {
		columnType, found := types[name]
		if !found {
			return rowdata.PageQuery{}, nil, runtime.ErrorWithReason(codes.FailedPrecondition, fmt.Sprintf("the table holds no column %q", name))
		}
		kind := rowdata.Kind(columnType)
		if kind == protocol.DataKind_DATA_KIND_UNSPECIFIED {
			return rowdata.PageQuery{}, nil, runtime.ErrorWithReason(codes.FailedPrecondition, fmt.Sprintf("column %q: Ptah reads no value of type %s", name, columnType))
		}
		columns[i] = &protocol.DataColumn{Name: name, Type: columnType, Kind: kind}
		query.Columns = append(query.Columns, rowdata.Column{Name: name, Type: columnType})
	}
	order := slices.Clone(request.GetKeyColumns())
	for _, key := range read.primaryKey {
		if !slices.Contains(order, key) {
			order = append(order, key)
		}
	}
	for _, name := range order {
		column := rowdata.Column{Name: name, Type: types[name]}
		query.Order = append(query.Order, column)
		if !slices.Contains(request.GetColumns(), name) {
			query.Columns = append(query.Columns, column)
		}
	}
	if request.GetPageToken() != "" {
		after, err := rowdata.DecodeToken(request.GetPageToken(), len(order))
		if err != nil {
			return rowdata.PageQuery{}, nil, runtime.ErrorWithReason(codes.InvalidArgument, err.Error())
		}
		query.After = after
	}
	return query, columns, nil
}

// readPage reads the rows of one page into result. It stops at the request's
// row bound and at the page's byte bound, and reports whether rows remain and
// the ordering values of the last row it kept.
func readPage(ctx context.Context, conn *pgx.Conn, text string, query rowdata.PageQuery, request *protocol.ReadRowsRequest,
	result *protocol.RowPage) (last []*protocol.DataValue, more bool, err error) {
	rows, err := conn.Query(ctx, text)
	if err != nil {
		return nil, false, &driverError{err}
	}
	defer rows.Close()
	size := 0
	for rows.Next() {
		if len(result.Rows) == int(request.GetMaxRows()) {
			return last, true, nil
		}
		scanned, err := rows.Values()
		if err != nil {
			return nil, false, &driverError{err}
		}
		values := make(map[string]*protocol.DataValue, len(query.Columns))
		for i, column := range query.Columns {
			value, err := rowdata.Scanned(column.Type, scanned[i])
			if err != nil {
				return nil, false, fmt.Errorf("column %q: %w", column.Name, err)
			}
			values[column.Name] = value
		}
		row := &protocol.Row{Values: make([]*protocol.DataValue, len(request.GetColumns()))}
		for i, name := range request.GetColumns() {
			row.Values[i] = values[name]
		}
		size += proto.Size(row)
		if size > pageBytes {
			if len(result.Rows) == 0 {
				return nil, false, errors.New("one row of the table is larger than a page holds")
			}
			return last, true, nil
		}
		result.Rows = append(result.Rows, row)
		last = make([]*protocol.DataValue, len(query.Order))
		for i, column := range query.Order {
			last[i] = values[column.Name]
		}
	}
	if err := rows.Err(); err != nil {
		return nil, false, &driverError{err}
	}
	return last, false, nil
}

// driverError is a failure the driver reported. Its text can quote a value,
// so it is not repeated to the host.
type driverError struct{ err error }

func (e *driverError) Error() string { return "the PostgreSQL driver failed" }
func (e *driverError) Unwrap() error { return e.err }
