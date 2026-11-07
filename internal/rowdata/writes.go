package rowdata

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-spanner-plugin/internal/ddl"
)

// RenderRowWrites renders each write as one statement, in input order, with
// every value written as a literal of its column's type, the type TypeRows
// gave it. The statements have the shape Ptah's built-in Spanner engine wrote
// for a row diff: the columns of an INSERT sorted by name, the SET of an
// UPDATE holding the columns outside the key sorted by name, and a key
// predicate that names each key column in sorted order and addresses a null
// key with IS NULL. An UPSERT is an INSERT with ON CONFLICT on the key that
// updates the other columns from EXCLUDED, which Spanner accepts when the key
// is the table's primary key.
//
// A value its column cannot hold, and a jsonb key column, which Spanner
// cannot compare, refuse the whole batch with UNSUPPORTED_VALUE, naming the
// write and the column but never the value. The table is named as the schema
// renderer names it: unqualified in the default schema.
func RenderRowWrites(request *protocol.RenderRowWritesRequest) (*protocol.RenderResult, error) {
	units := make([]*protocol.Unit, 0, len(request.GetWrites()))
	for _, write := range request.GetWrites() {
		if err := ValidateTable(write.GetTable()); err != nil {
			return nil, err
		}
		statement, err := renderWrite(write)
		if refused, ok := errors.AsType[*writeRefusal](err); ok {
			return &protocol.RenderResult{Refusal: &protocol.RenderRefusal{
				ChangeId: write.GetChangeId(), Reason: refused.reason, Message: refused.message,
			}}, nil
		}
		if err != nil {
			return nil, err
		}
		units = append(units, &protocol.Unit{ChangeId: write.GetChangeId(), Body: &protocol.Unit_Sql{Sql: statement}})
	}
	return &protocol.RenderResult{Complete: true, Units: units}, nil
}

// ValidateTable refuses a table reference a statement cannot name: one in a
// catalog, under a path, with an overload, of another engine, or with no name.
func ValidateTable(table *protocol.Reference) error {
	if table.GetName() == "" || table.GetCatalog() != "" || len(table.GetPath()) != 0 || len(table.GetOverload()) != 0 ||
		(table.GetEngine() != "" && table.GetEngine() != "spanner") {
		return errors.New("a Spanner table is named by its schema and name")
	}
	return nil
}

// writeRefusal is a write the batch stops at, with a reason that names no
// value.
type writeRefusal struct {
	reason  protocol.RenderRefusal_Reason
	message string
}

func (r *writeRefusal) Error() string { return r.message }

func refuseValue(format string, arguments ...any) error {
	return &writeRefusal{reason: protocol.RenderRefusal_UNSUPPORTED_VALUE, message: fmt.Sprintf(format, arguments...)}
}

func renderWrite(write *protocol.RowWrite) (string, error) {
	if len(write.GetValues()) != len(write.GetColumns()) {
		return "", fmt.Errorf("a row write carries %d values for %d columns", len(write.GetValues()), len(write.GetColumns()))
	}
	literals := make(map[string]string, len(write.GetColumns()))
	types := make(map[string]string, len(write.GetColumns()))
	for i, column := range write.GetColumns() {
		columnType, literal, err := columnLiteral(write.GetColumnTypes()[column], write.GetValues()[i])
		if err != nil {
			return "", refuseValue("column %q: %v", column, err)
		}
		literals[column], types[column] = literal, columnType
	}
	table := ddl.Name(write.GetTable().GetSchema(), write.GetTable().GetName())
	columns := slices.Sorted(slices.Values(write.GetColumns()))
	keys := slices.Sorted(slices.Values(write.GetKeyColumns()))
	var others []string
	for _, column := range columns {
		if !slices.Contains(keys, column) {
			others = append(others, column)
		}
	}
	switch write.GetMode() {
	case protocol.RowWrite_INSERT:
		return insert(table, columns, literals), nil
	case protocol.RowWrite_UPSERT:
		if len(keys) == 0 {
			return "", &writeRefusal{reason: protocol.RenderRefusal_UNSUPPORTED_MODE, message: "an UPSERT names no key to resolve a conflict on"}
		}
		action := "DO NOTHING"
		if len(others) > 0 {
			assignments := make([]string, len(others))
			for i, column := range others {
				assignments[i] = ddl.Quote(column) + " = EXCLUDED." + ddl.Quote(column)
			}
			action = "DO UPDATE SET " + strings.Join(assignments, ", ")
		}
		return strings.TrimSuffix(insert(table, columns, literals), ";") + " ON CONFLICT (" + quoteAll(keys) + ") " + action + ";", nil
	case protocol.RowWrite_UPDATE_BY_KEY:
		if len(others) == 0 {
			return "", errors.New("an UPDATE_BY_KEY write sets no column outside its key")
		}
		assignments := make([]string, len(others))
		for i, column := range others {
			assignments[i] = ddl.Quote(column) + " = " + literals[column]
		}
		where, err := keyPredicate(keys, types, literals)
		if err != nil {
			return "", err
		}
		return "UPDATE " + table + " SET " + strings.Join(assignments, ", ") + " WHERE " + where + ";", nil
	case protocol.RowWrite_DELETE_BY_KEY:
		where, err := keyPredicate(keys, types, literals)
		if err != nil {
			return "", err
		}
		return "DELETE FROM " + table + " WHERE " + where + ";", nil
	default:
		return "", &writeRefusal{reason: protocol.RenderRefusal_UNSUPPORTED_MODE, message: fmt.Sprintf("Ptah writes no row in mode %s", write.GetMode())}
	}
}

// columnLiteral is the canonical type of a column, empty for a column of no
// type, and the literal of its value.
func columnLiteral(spelling string, value *protocol.DataValue) (columnType, literal string, err error) {
	if spelling != "" {
		if columnType, err = ColumnType(spelling); err != nil {
			return "", "", err
		}
	}
	canonical, err := Canonical(columnType, value)
	if err != nil {
		return "", "", err
	}
	if literal, err = Literal(columnType, canonical); err != nil {
		return "", "", err
	}
	return columnType, literal, nil
}

func insert(table string, columns []string, literals map[string]string) string {
	values := make([]string, len(columns))
	for i, column := range columns {
		values[i] = literals[column]
	}
	return "INSERT INTO " + table + " (" + quoteAll(columns) + ") VALUES (" + strings.Join(values, ", ") + ");"
}

// keyPredicate is `key = literal` for each key column in sorted order, joined
// with AND. A key column holding null is addressed with IS NULL: written as
// `= NULL` the predicate holds for no row, and the statement changes nothing.
func keyPredicate(keys []string, types, literals map[string]string) (string, error) {
	if len(keys) == 0 {
		return "", errors.New("a row write that addresses a row names no key")
	}
	parts := make([]string, len(keys))
	for i, key := range keys {
		literal, found := literals[key]
		switch {
		case !found:
			return "", fmt.Errorf("a row write names key column %q among no column it carries", key)
		case types[key] == "jsonb":
			return "", refuseValue("column %q: %v", key, ErrNoOrder)
		case literal == "NULL":
			parts[i] = ddl.Quote(key) + " IS NULL"
		default:
			parts[i] = ddl.Quote(key) + " = " + literal
		}
	}
	return strings.Join(parts, " AND "), nil
}

func quoteAll(names []string) string {
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = ddl.Quote(name)
	}
	return strings.Join(quoted, ", ")
}
