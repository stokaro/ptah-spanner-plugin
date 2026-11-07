package rowdata

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-spanner-plugin/internal/ddl"
)

// maxToken bounds a page token, as the protocol does.
const maxToken = 4096

// Column is a column a page reads: its name and canonical type.
type Column struct {
	Name, Type string
}

// PageQuery is the query of one page of a table in key order.
type PageQuery struct {
	// Schema and Table name the table.
	Schema, Table string
	// Columns are the columns the query selects, in order: the requested ones,
	// then any ordering column the request did not ask for.
	Columns []Column
	// Order are the columns the rows are ordered by: the request's key, then
	// the primary key columns the key leaves out, which make the order total.
	Order []Column
	// After holds the ordering values of the last row of the previous page,
	// or nothing for the first page.
	After []*protocol.DataValue
	// Limit is the number of rows the query fetches.
	Limit int
}

// Text is the statement of the page. Every ordering column orders nulls
// first, and a page after a row holds the rows whose ordering values come
// after that row's: each one is an expanded comparison, because Spanner
// refuses a row comparison (`Unsupported PostgreSQL expression type:
// RowCompareExpr`). Numeric, date, jsonb and uuid columns are read as text,
// which the provider parses itself.
func (q PageQuery) Text() (string, error) {
	selected := make([]string, len(q.Columns))
	for i, column := range q.Columns {
		selected[i] = ddl.Quote(column.Name)
		if readAsText(column.Type) {
			selected[i] += "::text"
		}
	}
	text := "SELECT " + strings.Join(selected, ", ") + " FROM " + ddl.Name(q.Schema, q.Table)
	if len(q.After) > 0 {
		predicate, err := q.after()
		if err != nil {
			return "", err
		}
		text += " WHERE " + predicate
	}
	order := make([]string, len(q.Order))
	for i, column := range q.Order {
		if !Orderable(column.Type) {
			return "", fmt.Errorf("column %q: %w", column.Name, ErrNoOrder)
		}
		order[i] = ddl.Quote(column.Name) + " NULLS FIRST"
	}
	return text + " ORDER BY " + strings.Join(order, ", ") + " LIMIT " + strconv.Itoa(q.Limit), nil
}

// after is the predicate of the rows after the values q.After holds: for each
// position, the earlier columns equal and this one greater, where a null
// value comes before every other.
func (q PageQuery) after() (string, error) {
	if len(q.After) != len(q.Order) {
		return "", errors.New("a page token names another key than the request")
	}
	alternatives := make([]string, len(q.Order))
	for i, column := range q.Order {
		var terms []string
		for j := range i {
			term, err := equalTerm(q.Order[j], q.After[j])
			if err != nil {
				return "", err
			}
			terms = append(terms, term)
		}
		term, err := afterTerm(column, q.After[i])
		if err != nil {
			return "", err
		}
		alternatives[i] = "(" + strings.Join(append(terms, term), " AND ") + ")"
	}
	return strings.Join(alternatives, " OR "), nil
}

// equalTerm is the term that holds for a value of column equal to value.
func equalTerm(column Column, value *protocol.DataValue) (string, error) {
	if _, null := value.GetValue().(*protocol.DataValue_Null); null {
		return ddl.Quote(column.Name) + " IS NULL", nil
	}
	literal, err := Literal(column.Type, value)
	return ddl.Quote(column.Name) + " = " + literal, err
}

// afterTerm is the term that holds for a value of column greater than value
// in the nulls-first order: every value after a null is one that is not null.
func afterTerm(column Column, value *protocol.DataValue) (string, error) {
	if _, null := value.GetValue().(*protocol.DataValue_Null); null {
		return ddl.Quote(column.Name) + " IS NOT NULL", nil
	}
	literal, err := Literal(column.Type, value)
	return ddl.Quote(column.Name) + " > " + literal, err
}

// readAsText reports a column a page selects cast to text.
func readAsText(columnType string) bool {
	switch columnType {
	case "numeric", "date", "jsonb", "uuid":
		return true
	}
	return false
}

// EncodeToken writes the ordering values of the last row of a page as the
// token that resumes the read after it.
func EncodeToken(values []*protocol.DataValue) (string, error) {
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(&protocol.Row{Values: values})
	if err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(encoded)
	if len(token) > maxToken {
		return "", errors.New("the key of a row is too large for a page token")
	}
	return token, nil
}

// DecodeToken reads the ordering values a token holds, which must be one for
// each of count ordering columns.
func DecodeToken(token string, count int) ([]*protocol.DataValue, error) {
	encoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return nil, errors.New("the page token is not one this provider wrote")
	}
	var row protocol.Row
	if err := proto.Unmarshal(encoded, &row); err != nil || len(row.GetValues()) != count {
		return nil, errors.New("the page token is not one this provider wrote for this key")
	}
	for _, value := range row.GetValues() {
		if value.GetValue() == nil {
			return nil, errors.New("the page token is not one this provider wrote for this key")
		}
	}
	return row.GetValues(), nil
}

// Scanned is the value a read returned for a column of the canonical type, as
// the driver decoded it: a bigint as int64, a boolean as bool, a double
// precision as float64, a real as float32, a character varying as string, a
// bytea as []byte, a timestamp with time zone as time.Time, and the columns a
// page reads as text as their text.
func Scanned(columnType string, scanned any) (*protocol.DataValue, error) {
	if scanned == nil {
		return nullValue(), nil
	}
	switch v := scanned.(type) {
	case int64:
		if Kind(columnType) == protocol.DataKind_DATA_KIND_INT64 {
			return int64Value(v), nil
		}
	case bool:
		if columnType == "boolean" {
			return &protocol.DataValue{Value: &protocol.DataValue_BoolValue{BoolValue: v}}, nil
		}
	case float64:
		if columnType == "double precision" {
			return doubleValue(v), nil
		}
	case float32:
		if columnType == "real" {
			return doubleValue(float64(v)), nil
		}
	case []byte:
		if columnType == "bytea" {
			return bytesValue(bytes.Clone(v)), nil
		}
	case time.Time:
		if columnType == "timestamp with time zone" {
			return timestampValue(roundMicrosecond(v.UTC())), nil
		}
	case string:
		return scannedText(columnType, v)
	}
	return nil, fmt.Errorf("a %s column returned a value the provider does not read", columnType)
}

// scannedText reads the text a read returned for a column of the canonical
// type.
func scannedText(columnType, text string) (*protocol.DataValue, error) {
	switch Kind(columnType) {
	case protocol.DataKind_DATA_KIND_STRING:
		return stringValue(text), nil
	case protocol.DataKind_DATA_KIND_DECIMAL:
		decimal, err := parseDecimal(text)
		if err != nil {
			return nil, fmt.Errorf("a numeric column returned a value that is not a decimal number, such as NaN")
		}
		return decimal.value(), nil
	case protocol.DataKind_DATA_KIND_DATE:
		parsed, err := time.Parse(time.DateOnly, text)
		if err != nil {
			return nil, fmt.Errorf("a date column returned a value that is not a date")
		}
		return dateValue(parsed.Unix() / 86400), nil
	case protocol.DataKind_DATA_KIND_JSON:
		canonical, err := CanonicalJSON(text)
		if err != nil {
			return nil, fmt.Errorf("a jsonb column returned a value that is not JSON")
		}
		return jsonValue(canonical), nil
	default:
		return nil, fmt.Errorf("a %s column returned text the provider does not read", columnType)
	}
}

// Limit is the number of rows a page fetches to learn whether rows remain:
// one more than the page holds.
func Limit(maxRows uint32) int {
	return int(min(maxRows, math.MaxInt32-1)) + 1
}
