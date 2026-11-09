package rowdata

import (
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	protocol "ptah.run/provider-sdk/protocol/experimental"
)

// timestampLayout writes a moment as the literal a timestamp with time zone
// column reads: the offset is always written, because Spanner reads a bare
// moment in America/Los_Angeles, and a canonical moment holds no digit past
// the microsecond.
const timestampLayout = "2006-01-02 15:04:05.999999-07:00"

// Literal writes a value as the SQL literal a column of columnType reads,
// after Canonical: NULL, TRUE or FALSE, digits for a bigint, the shortest
// decimal of a double, and a single-quoted literal for everything else, the
// single quote doubled. Spanner's PostgreSQL interface reads strings as
// standard SQL does, so a backslash stays itself. A bytea value is written in
// hex digits, `'\x6869'::bytea`, which holds any byte: a text literal could
// not carry one that is not UTF-8. A column of no type is written from the
// case of its value.
func Literal(columnType string, value *protocol.DataValue) (string, error) {
	switch v := value.GetValue().(type) {
	case *protocol.DataValue_Null:
		return "NULL", nil
	case *protocol.DataValue_BoolValue:
		if v.BoolValue {
			return "TRUE", nil
		}
		return "FALSE", nil
	case *protocol.DataValue_Int64Value:
		return strconv.FormatInt(v.Int64Value, 10), nil
	case *protocol.DataValue_Uint64Value:
		return strconv.FormatUint(v.Uint64Value, 10), nil
	case *protocol.DataValue_DoubleValue:
		return doubleLiteral(columnType, v.DoubleValue), nil
	case *protocol.DataValue_DecimalValue:
		return quote(decimalText(v.DecimalValue.GetDigits(), int64(v.DecimalValue.GetScale()))), nil
	case *protocol.DataValue_StringValue:
		if strings.IndexByte(v.StringValue, 0) >= 0 {
			return "", fmt.Errorf("a text value holds a NUL character, which no Spanner literal holds")
		}
		return quote(v.StringValue), nil
	case *protocol.DataValue_BytesValue:
		return `'\x` + hex.EncodeToString(v.BytesValue) + `'::bytea`, nil
	case *protocol.DataValue_TimestampValue:
		moment := time.Unix(v.TimestampValue.GetSeconds(), int64(v.TimestampValue.GetNanos())).UTC()
		return quote(moment.Format(timestampLayout)), nil
	case *protocol.DataValue_DateValue:
		return quote(time.Unix(v.DateValue*86400, 0).UTC().Format(time.DateOnly)), nil
	case *protocol.DataValue_JsonValue:
		return quote(v.JsonValue), nil
	case nil:
		return "", fmt.Errorf("a row write names a column it carries no value for")
	default:
		return "", fmt.Errorf("a %s value has no literal Ptah writes", caseName(value))
	}
}

// doubleLiteral writes a double as its shortest decimal, and NaN and the
// infinities as the quoted words the float input reads, cast to the column's
// type.
func doubleLiteral(columnType string, number float64) string {
	cast, bits := "float8", 64
	if columnType == "real" {
		cast, bits = "float4", 32
	}
	switch {
	case math.IsNaN(number):
		return "'NaN'::" + cast
	case math.IsInf(number, 1):
		return "'Infinity'::" + cast
	case math.IsInf(number, -1):
		return "'-Infinity'::" + cast
	default:
		return strconv.FormatFloat(number, 'g', -1, bits)
	}
}

// quote writes text as a single-quoted literal with the single quote doubled.
func quote(text string) string {
	return "'" + strings.ReplaceAll(text, "'", "''") + "'"
}
