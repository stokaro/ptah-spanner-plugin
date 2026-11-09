package rowdata

import (
	"fmt"

	protocol "ptah.run/provider-sdk/protocol/experimental"
)

// TypeRows answers, for a Spanner table, the type of each column of the
// request and the declared and live rows with every typed value in its
// canonical form; see Canonical.
//
// A column the live table holds takes the type the table has, because that is
// what a statement writes into. A column the table does not have yet takes
// its declared type, the type the schema stage of the same plan creates it
// with. A column neither side types keeps its values. A value its column
// cannot hold is an error naming the row, counted from 1, and the column, but
// never the value. Rows keep their order and the columns each carries.
func TypeRows(request *protocol.TypeRowsRequest) (*protocol.TypedRows, error) {
	live := make(map[string]string, len(request.GetLiveTable().GetColumns()))
	for _, column := range request.GetLiveTable().GetColumns() {
		live[column.GetName()] = column.GetType()
	}
	result := &protocol.TypedRows{Columns: make([]*protocol.DataColumn, len(request.GetColumns()))}
	types := make([]string, len(request.GetColumns()))
	for i, column := range request.GetColumns() {
		spelling, found := live[column.GetName()]
		if !found {
			spelling = column.GetDeclaredType()
		}
		result.Columns[i] = &protocol.DataColumn{Name: column.GetName()}
		if spelling == "" {
			continue
		}
		canonical, err := ColumnType(spelling)
		if err != nil {
			return nil, fmt.Errorf("column %q: %w", column.GetName(), err)
		}
		types[i] = canonical
		result.Columns[i].Type, result.Columns[i].Kind = canonical, Kind(canonical)
	}
	var err error
	if result.Declared, err = canonicalRows(request.GetColumns(), types, request.GetDeclared()); err != nil {
		return nil, fmt.Errorf("declared %w", err)
	}
	if result.Live, err = canonicalRows(request.GetColumns(), types, request.GetLive()); err != nil {
		return nil, fmt.Errorf("live %w", err)
	}
	return result, nil
}

// canonicalRows returns copies of rows with every value of a typed column in
// its canonical form.
func canonicalRows(columns []*protocol.RowColumn, types []string, rows []*protocol.Row) ([]*protocol.Row, error) {
	if rows == nil {
		return nil, nil
	}
	out := make([]*protocol.Row, len(rows))
	for i, row := range rows {
		if len(row.GetValues()) != len(columns) {
			return nil, fmt.Errorf("row %d carries %d values for %d columns", i+1, len(row.GetValues()), len(columns))
		}
		typed := &protocol.Row{Values: make([]*protocol.DataValue, len(columns))}
		for j, value := range row.GetValues() {
			canonical, err := Canonical(types[j], value)
			if err != nil {
				return nil, fmt.Errorf("row %d, column %q: %w", i+1, columns[j].GetName(), err)
			}
			typed.Values[j] = canonical
		}
		out[i] = typed
	}
	return out, nil
}
