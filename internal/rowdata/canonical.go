package rowdata

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"
	protocol "ptah.run/provider-sdk/protocol/experimental"
)

// Canonical is value in the canonical form of a column of the canonical type
// columnType: the form a declared value and the value a read returns both
// take, so that equal values compare equal. A value the row does not carry
// stays unset, null stays null, and a column of no type keeps its value. A
// value the column cannot hold is an error that names the type and never the
// value.
func Canonical(columnType string, value *protocol.DataValue) (*protocol.DataValue, error) {
	switch value.GetValue().(type) {
	case nil:
		return &protocol.DataValue{}, nil
	case *protocol.DataValue_Null:
		return nullValue(), nil
	}
	if columnType == "" {
		return proto.CloneOf(value), nil
	}
	switch Kind(columnType) {
	case protocol.DataKind_DATA_KIND_INT64:
		return canonicalInteger(value)
	case protocol.DataKind_DATA_KIND_BOOL:
		return canonicalBool(value)
	case protocol.DataKind_DATA_KIND_DOUBLE:
		return canonicalDouble(columnType, value)
	case protocol.DataKind_DATA_KIND_DECIMAL:
		return canonicalNumeric(value)
	case protocol.DataKind_DATA_KIND_STRING:
		return canonicalText(columnType, value)
	case protocol.DataKind_DATA_KIND_BYTES:
		return canonicalBytes(value)
	case protocol.DataKind_DATA_KIND_TIMESTAMP:
		return canonicalTimestamp(value)
	case protocol.DataKind_DATA_KIND_DATE:
		return canonicalDate(value)
	case protocol.DataKind_DATA_KIND_JSON:
		return canonicalJSONValue(value)
	default:
		return nil, fmt.Errorf("Ptah writes no value of type %s", columnType)
	}
}

func nullValue() *protocol.DataValue {
	return &protocol.DataValue{Value: &protocol.DataValue_Null{Null: &protocol.Empty{}}}
}

func int64Value(value int64) *protocol.DataValue {
	return &protocol.DataValue{Value: &protocol.DataValue_Int64Value{Int64Value: value}}
}

func doubleValue(value float64) *protocol.DataValue {
	return &protocol.DataValue{Value: &protocol.DataValue_DoubleValue{DoubleValue: value}}
}

func stringValue(value string) *protocol.DataValue {
	return &protocol.DataValue{Value: &protocol.DataValue_StringValue{StringValue: value}}
}

func bytesValue(value []byte) *protocol.DataValue {
	return &protocol.DataValue{Value: &protocol.DataValue_BytesValue{BytesValue: value}}
}

func dateValue(days int64) *protocol.DataValue {
	return &protocol.DataValue{Value: &protocol.DataValue_DateValue{DateValue: days}}
}

func jsonValue(text string) *protocol.DataValue {
	return &protocol.DataValue{Value: &protocol.DataValue_JsonValue{JsonValue: text}}
}

// cannotHold is the refusal of a value of one case in a column of a type.
func cannotHold(value *protocol.DataValue, columnType string) error {
	name := caseName(value)
	article := "a"
	if strings.ContainsRune("aeiou", rune(name[0])) {
		article = "an"
	}
	return fmt.Errorf("%s %s value cannot be written into a %s column", article, name, columnType)
}

// caseName names the case a value takes, for a refusal that names no value.
func caseName(value *protocol.DataValue) string {
	switch value.GetValue().(type) {
	case *protocol.DataValue_BoolValue:
		return "boolean"
	case *protocol.DataValue_Int64Value, *protocol.DataValue_Uint64Value:
		return "integer"
	case *protocol.DataValue_DoubleValue:
		return "floating-point"
	case *protocol.DataValue_StringValue:
		return "text"
	case *protocol.DataValue_BytesValue:
		return "binary"
	case *protocol.DataValue_DecimalValue:
		return "decimal"
	case *protocol.DataValue_TimestampValue:
		return "timestamp"
	case *protocol.DataValue_DateValue:
		return "date"
	case *protocol.DataValue_IntervalValue:
		return "interval"
	case *protocol.DataValue_JsonValue:
		return "JSON"
	default:
		return "provider-defined"
	}
}

// canonicalInteger is a bigint: an integer in range, a double that holds an
// integer, decimal digits, or a decimal of no fraction.
func canonicalInteger(value *protocol.DataValue) (*protocol.DataValue, error) {
	switch v := value.GetValue().(type) {
	case *protocol.DataValue_Int64Value:
		return int64Value(v.Int64Value), nil
	case *protocol.DataValue_Uint64Value:
		if v.Uint64Value > math.MaxInt64 {
			return nil, fmt.Errorf("an integer value is beyond the range of bigint")
		}
		return int64Value(int64(v.Uint64Value)), nil
	case *protocol.DataValue_DoubleValue:
		if v.DoubleValue != math.Trunc(v.DoubleValue) || v.DoubleValue < math.MinInt64 || v.DoubleValue >= math.MaxInt64 {
			return nil, fmt.Errorf("a floating-point value is not an integer in the range of bigint")
		}
		return int64Value(int64(v.DoubleValue)), nil
	case *protocol.DataValue_StringValue:
		parsed, err := strconv.ParseInt(strings.TrimSpace(v.StringValue), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("a text value is not an integer in the range of bigint")
		}
		return int64Value(parsed), nil
	case *protocol.DataValue_DecimalValue:
		decimal, err := normalizedDecimal(v.DecimalValue.GetDigits(), int64(v.DecimalValue.GetScale()))
		if err != nil {
			return nil, err
		}
		if decimal.scale != 0 {
			return nil, fmt.Errorf("a decimal value is not an integer")
		}
		parsed, err := strconv.ParseInt(decimal.digits, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("a decimal value is beyond the range of bigint")
		}
		return int64Value(parsed), nil
	default:
		return nil, cannotHold(value, "bigint")
	}
}

// booleanWords are the spellings of a boolean the PostgreSQL interface reads,
// in any case.
var booleanWords = map[string]bool{
	"true": true, "t": true, "yes": true, "y": true, "on": true, "1": true,
	"false": false, "f": false, "no": false, "n": false, "off": false, "0": false,
}

func canonicalBool(value *protocol.DataValue) (*protocol.DataValue, error) {
	switch v := value.GetValue().(type) {
	case *protocol.DataValue_BoolValue:
		return &protocol.DataValue{Value: &protocol.DataValue_BoolValue{BoolValue: v.BoolValue}}, nil
	case *protocol.DataValue_StringValue:
		parsed, found := booleanWords[strings.ToLower(strings.TrimSpace(v.StringValue))]
		if !found {
			return nil, fmt.Errorf("a text value is not a boolean")
		}
		return &protocol.DataValue{Value: &protocol.DataValue_BoolValue{BoolValue: parsed}}, nil
	default:
		return nil, cannotHold(value, "boolean")
	}
}

// canonicalDouble is a double precision value, or a real one rounded to the
// float4 Spanner stores. NaN and both infinities are values of either type:
// the emulator stores 'NaN'::float8 and 'Infinity' and reads them back.
func canonicalDouble(columnType string, value *protocol.DataValue) (*protocol.DataValue, error) {
	var number float64
	switch v := value.GetValue().(type) {
	case *protocol.DataValue_DoubleValue:
		number = v.DoubleValue
	case *protocol.DataValue_Int64Value:
		number = float64(v.Int64Value)
	case *protocol.DataValue_Uint64Value:
		number = float64(v.Uint64Value)
	case *protocol.DataValue_StringValue:
		parsed, err := parseFloatText(v.StringValue)
		if err != nil {
			return nil, err
		}
		number = parsed
	case *protocol.DataValue_DecimalValue:
		parsed, err := strconv.ParseFloat(decimalText(v.DecimalValue.GetDigits(), int64(v.DecimalValue.GetScale())), 64)
		if err != nil {
			return nil, fmt.Errorf("a decimal value is beyond the range of %s", columnType)
		}
		number = parsed
	default:
		return nil, cannotHold(value, columnType)
	}
	if columnType == "real" {
		if !math.IsInf(number, 0) && !math.IsNaN(number) && math.Abs(number) > math.MaxFloat32 {
			return nil, fmt.Errorf("a floating-point value is beyond the range of real")
		}
		number = float64(float32(number))
	}
	return doubleValue(number), nil
}

// floatWords are the spellings of the special values the PostgreSQL
// interface reads, in any case.
var floatWords = map[string]float64{
	"nan": math.NaN(), "infinity": math.Inf(1), "+infinity": math.Inf(1), "-infinity": math.Inf(-1),
	"inf": math.Inf(1), "+inf": math.Inf(1), "-inf": math.Inf(-1),
}

// parseFloatText reads a decimal number or a special value. It refuses the
// hexadecimal and underscore forms strconv reads and SQL does not.
func parseFloatText(text string) (float64, error) {
	trimmed := strings.TrimSpace(text)
	if special, found := floatWords[strings.ToLower(trimmed)]; found {
		return special, nil
	}
	if strings.ContainsAny(trimmed, "xX_pP") {
		return 0, fmt.Errorf("a text value is not a number")
	}
	parsed, err := strconv.ParseFloat(trimmed, 64)
	if err != nil {
		return 0, fmt.Errorf("a text value is not a number in the range of double precision")
	}
	return parsed, nil
}

// canonicalNumeric is a numeric value in its shortest decimal form: Spanner
// compares 12.50 and 12.5 as one value and keeps the scale it was given
// (`'12.50'` reads back as 12.50, `1e3` as 1000, `'-0.0'` as 0.0), so the
// canonical form drops trailing zeros of the fraction and the sign of zero.
func canonicalNumeric(value *protocol.DataValue) (*protocol.DataValue, error) {
	var decimal decimalParts
	var err error
	switch v := value.GetValue().(type) {
	case *protocol.DataValue_DecimalValue:
		decimal, err = normalizedDecimal(v.DecimalValue.GetDigits(), int64(v.DecimalValue.GetScale()))
	case *protocol.DataValue_Int64Value:
		decimal, err = parseDecimal(strconv.FormatInt(v.Int64Value, 10))
	case *protocol.DataValue_Uint64Value:
		decimal, err = parseDecimal(strconv.FormatUint(v.Uint64Value, 10))
	case *protocol.DataValue_DoubleValue:
		if math.IsNaN(v.DoubleValue) || math.IsInf(v.DoubleValue, 0) {
			return nil, fmt.Errorf("a floating-point value that is not a finite number has no numeric value Ptah writes")
		}
		decimal, err = parseDecimal(strconv.FormatFloat(v.DoubleValue, 'f', -1, 64))
	case *protocol.DataValue_StringValue:
		decimal, err = parseDecimal(strings.TrimSpace(v.StringValue))
	default:
		return nil, cannotHold(value, "numeric")
	}
	if err != nil {
		return nil, err
	}
	return decimal.value(), nil
}

// canonicalText is a character varying or uuid value. A number or a boolean
// becomes the text that spells it. A value longer than a varchar's length is
// refused: Spanner counts the length in characters (`'ünïcö'` fits a
// varchar(5), `'abcdef'` does not), and a uuid is written in the lower-case
// hyphenated form a read returns.
func canonicalText(columnType string, value *protocol.DataValue) (*protocol.DataValue, error) {
	var text string
	switch v := value.GetValue().(type) {
	case *protocol.DataValue_StringValue:
		text = v.StringValue
	case *protocol.DataValue_Int64Value:
		text = strconv.FormatInt(v.Int64Value, 10)
	case *protocol.DataValue_Uint64Value:
		text = strconv.FormatUint(v.Uint64Value, 10)
	case *protocol.DataValue_DoubleValue:
		text = strconv.FormatFloat(v.DoubleValue, 'g', -1, 64)
	case *protocol.DataValue_BoolValue:
		text = strconv.FormatBool(v.BoolValue)
	case *protocol.DataValue_DecimalValue:
		decimal, err := normalizedDecimal(v.DecimalValue.GetDigits(), int64(v.DecimalValue.GetScale()))
		if err != nil {
			return nil, err
		}
		text = decimal.text()
	default:
		return nil, cannotHold(value, columnType)
	}
	if strings.IndexByte(text, 0) >= 0 {
		return nil, fmt.Errorf("a text value holds a NUL character, which a %s column cannot hold", columnType)
	}
	if columnType == "uuid" {
		return canonicalUUID(text)
	}
	if length, bounded := varcharLength(columnType); bounded && utf8.RuneCountInString(text) > length {
		return nil, fmt.Errorf("a text value is longer than the %d characters of a %s column", length, columnType)
	}
	return stringValue(text), nil
}

// varcharLength is the length of a character varying(n).
func varcharLength(columnType string) (int, bool) {
	inner, found := strings.CutPrefix(columnType, "character varying(")
	if !found {
		return 0, false
	}
	length, err := strconv.Atoi(strings.TrimSuffix(inner, ")"))
	return length, err == nil
}

// canonicalUUID reads the spellings the uuid input takes, with or without
// hyphens and braces, in any case, and writes the lower-case hyphenated one.
func canonicalUUID(text string) (*protocol.DataValue, error) {
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "{") && strings.HasSuffix(trimmed, "}") {
		trimmed = trimmed[1 : len(trimmed)-1]
	}
	digits := strings.ReplaceAll(trimmed, "-", "")
	decoded, err := hex.DecodeString(digits)
	if err != nil || len(decoded) != 16 {
		return nil, fmt.Errorf("a text value is not a uuid")
	}
	spelled := hex.EncodeToString(decoded)
	return stringValue(spelled[0:8] + "-" + spelled[8:12] + "-" + spelled[12:16] + "-" + spelled[16:20] + "-" + spelled[20:]), nil
}

// canonicalBytes is a bytea value. Text is read as the bytea input reads it,
// which is what Spanner stores for a text literal written into the column:
// `'\x6869'` and `'\x 68 69'` are the bytes hi, `'\141\142'` the bytes ab,
// `'\\x'` a backslash and an x, and `'abc'` the bytes of the text. A
// backslash that starts none of those forms is refused, as the server refuses
// `'a\b'` with `invalid input syntax for type bytea`.
func canonicalBytes(value *protocol.DataValue) (*protocol.DataValue, error) {
	switch v := value.GetValue().(type) {
	case *protocol.DataValue_BytesValue:
		return bytesValue(bytes.Clone(v.BytesValue)), nil
	case *protocol.DataValue_StringValue:
		decoded, err := byteaInput(v.StringValue)
		if err != nil {
			return nil, err
		}
		return bytesValue(decoded), nil
	default:
		return nil, cannotHold(value, "bytea")
	}
}

// byteaInput reads text in the bytea input's hex or escape format.
func byteaInput(text string) ([]byte, error) {
	if hexDigits, found := strings.CutPrefix(text, `\x`); found {
		decoded, err := hex.DecodeString(strings.Join(strings.Fields(hexDigits), ""))
		if err != nil {
			return nil, fmt.Errorf("a text value is not bytea hex digits")
		}
		return decoded, nil
	}
	decoded := make([]byte, 0, len(text))
	for i := 0; i < len(text); i++ {
		if text[i] != '\\' {
			decoded = append(decoded, text[i])
			continue
		}
		switch {
		case i+1 < len(text) && text[i+1] == '\\':
			decoded = append(decoded, '\\')
			i++
		case i+3 < len(text) && text[i+1] >= '0' && text[i+1] <= '3' && octal(text[i+2]) && octal(text[i+3]):
			decoded = append(decoded, (text[i+1]-'0')<<6|(text[i+2]-'0')<<3|(text[i+3]-'0'))
			i += 3
		default:
			return nil, fmt.Errorf("a text value is not bytea input: a backslash starts neither a backslash nor three octal digits")
		}
	}
	return decoded, nil
}

func octal(character byte) bool { return character >= '0' && character <= '7' }

// The range of a Spanner timestamp and date: the years 1 to 9999 in UTC.
var (
	earliest = time.Date(1, time.January, 1, 0, 0, 0, 0, time.UTC)
	latest   = time.Date(9999, time.December, 31, 23, 59, 59, 999999000, time.UTC)
)

// canonicalTimestamp is a timestamp with time zone in UTC, at the microsecond
// a read returns. Spanner reads a stored moment back rounded to the
// microsecond, a tie to the even one: `.123456789` reads back as .123457,
// `.1234565` as .123456 and `.1234575` as .123458. Text names a moment in one
// of the spellings a declaration writes; text without an offset is UTC, as the
// host reads it, and the literal written for it carries the offset, because
// Spanner would read a bare one in America/Los_Angeles.
func canonicalTimestamp(value *protocol.DataValue) (*protocol.DataValue, error) {
	var moment time.Time
	switch v := value.GetValue().(type) {
	case *protocol.DataValue_TimestampValue:
		moment = time.Unix(v.TimestampValue.GetSeconds(), int64(v.TimestampValue.GetNanos())).UTC()
	case *protocol.DataValue_StringValue:
		parsed, ok := parseMoment(v.StringValue)
		if !ok {
			return nil, fmt.Errorf("a text value names no moment; write it as RFC 3339, 2006-01-02 15:04:05 or 2006-01-02")
		}
		moment = parsed
	case *protocol.DataValue_DateValue:
		moment = time.Unix(v.DateValue*86400, 0).UTC()
	default:
		return nil, cannotHold(value, "timestamp with time zone")
	}
	moment = roundMicrosecond(moment)
	if moment.Before(earliest) || moment.After(latest) {
		return nil, fmt.Errorf("a moment is outside the years 1 to 9999 a Spanner timestamp holds")
	}
	return timestampValue(moment), nil
}

func timestampValue(moment time.Time) *protocol.DataValue {
	return &protocol.DataValue{Value: &protocol.DataValue_TimestampValue{TimestampValue: &protocol.TimestampValue{
		Seconds: moment.Unix(), Nanos: int32(moment.Nanosecond())}}} // #nosec G115 -- a nanosecond count is below 1e9
}

// roundMicrosecond rounds a moment to the microsecond, a tie to the even
// microsecond.
func roundMicrosecond(moment time.Time) time.Time {
	nanos := moment.Nanosecond()
	micro, rest := nanos/1000, nanos%1000
	if rest > 500 || rest == 500 && micro%2 == 1 {
		micro++
	}
	return moment.Truncate(time.Second).Add(time.Duration(micro) * time.Microsecond)
}

// momentLayouts are the spellings a declaration writes a moment in, the ones
// the host's row diff reads. A layout without a zone reads as UTC.
var momentLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02T15:04:05.999999999",
	"2006-01-02 15:04:05.999999999Z07:00",
	"2006-01-02 15:04:05.999999999-07",
	"2006-01-02 15:04:05.999999999",
	time.DateOnly,
}

func parseMoment(text string) (time.Time, bool) {
	trimmed := strings.TrimSpace(text)
	for _, layout := range momentLayouts {
		if parsed, err := time.Parse(layout, trimmed); err == nil {
			return parsed.UTC(), true
		}
	}
	return time.Time{}, false
}

// canonicalDate is a date as days since 1970-01-01. Text is written as
// 2006-01-02.
func canonicalDate(value *protocol.DataValue) (*protocol.DataValue, error) {
	var days int64
	switch v := value.GetValue().(type) {
	case *protocol.DataValue_DateValue:
		days = v.DateValue
	case *protocol.DataValue_StringValue:
		parsed, err := time.Parse(time.DateOnly, strings.TrimSpace(v.StringValue))
		if err != nil {
			return nil, fmt.Errorf("a text value is not a date; write it as 2006-01-02")
		}
		days = parsed.Unix() / 86400
	default:
		return nil, cannotHold(value, "date")
	}
	if days < earliest.Unix()/86400 || days > latest.Unix()/86400 {
		return nil, fmt.Errorf("a date is outside the years 1 to 9999 a Spanner date holds")
	}
	return dateValue(days), nil
}

// canonicalJSONValue is a jsonb value in canonical text; see CanonicalJSON.
// A number or a boolean is the JSON scalar that spells it.
func canonicalJSONValue(value *protocol.DataValue) (*protocol.DataValue, error) {
	var text string
	switch v := value.GetValue().(type) {
	case *protocol.DataValue_JsonValue:
		text = v.JsonValue
	case *protocol.DataValue_StringValue:
		text = v.StringValue
	case *protocol.DataValue_Int64Value:
		text = strconv.FormatInt(v.Int64Value, 10)
	case *protocol.DataValue_Uint64Value:
		text = strconv.FormatUint(v.Uint64Value, 10)
	case *protocol.DataValue_DoubleValue:
		if math.IsNaN(v.DoubleValue) || math.IsInf(v.DoubleValue, 0) {
			return nil, fmt.Errorf("a floating-point value that is not a finite number has no JSON form")
		}
		text = strconv.FormatFloat(v.DoubleValue, 'g', -1, 64)
	case *protocol.DataValue_BoolValue:
		text = strconv.FormatBool(v.BoolValue)
	default:
		return nil, cannotHold(value, "jsonb")
	}
	canonical, err := CanonicalJSON(text)
	if err != nil {
		return nil, err
	}
	return jsonValue(canonical), nil
}
