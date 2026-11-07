package rowdata_test

import (
	"math"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/testing/protocmp"

	"github.com/stokaro/ptah-spanner-plugin/internal/rowdata"
	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

func null() *protocol.DataValue {
	return &protocol.DataValue{Value: &protocol.DataValue_Null{Null: &protocol.Empty{}}}
}

func boolean(v bool) *protocol.DataValue {
	return &protocol.DataValue{Value: &protocol.DataValue_BoolValue{BoolValue: v}}
}

func integer(v int64) *protocol.DataValue {
	return &protocol.DataValue{Value: &protocol.DataValue_Int64Value{Int64Value: v}}
}

func unsigned(v uint64) *protocol.DataValue {
	return &protocol.DataValue{Value: &protocol.DataValue_Uint64Value{Uint64Value: v}}
}

func double(v float64) *protocol.DataValue {
	return &protocol.DataValue{Value: &protocol.DataValue_DoubleValue{DoubleValue: v}}
}

func text(v string) *protocol.DataValue {
	return &protocol.DataValue{Value: &protocol.DataValue_StringValue{StringValue: v}}
}

func binary(v string) *protocol.DataValue {
	return &protocol.DataValue{Value: &protocol.DataValue_BytesValue{BytesValue: []byte(v)}}
}

func decimal(digits string, scale uint32) *protocol.DataValue {
	return &protocol.DataValue{Value: &protocol.DataValue_DecimalValue{DecimalValue: &protocol.DecimalValue{Digits: digits, Scale: scale}}}
}

func moment(seconds int64, nanos int32) *protocol.DataValue {
	return &protocol.DataValue{Value: &protocol.DataValue_TimestampValue{TimestampValue: &protocol.TimestampValue{Seconds: seconds, Nanos: nanos}}}
}

func date(days int64) *protocol.DataValue {
	return &protocol.DataValue{Value: &protocol.DataValue_DateValue{DateValue: days}}
}

func json(v string) *protocol.DataValue {
	return &protocol.DataValue{Value: &protocol.DataValue_JsonValue{JsonValue: v}}
}

// at is the moment as a timestamp value.
func at(t time.Time) *protocol.DataValue {
	return moment(t.Unix(), int32(t.Nanosecond())) // #nosec G115 -- a nanosecond count is below 1e9
}

// The canonical form of each column type, from the spellings a declaration
// writes and the values a read returns. The stored forms are the emulator's,
// behind PGAdapter 0.56.2: numeric `'12.50'` reads 12.50 and `1e3` 1000,
// jsonb sorts members and writes 2e0 as 2, and a moment reads back rounded to
// the microsecond, a tie to the even one.
func TestCanonical_HappyPath(t *testing.T) {
	tests := []struct {
		name, columnType string
		value, want      *protocol.DataValue
	}{
		{name: "an unset value stays unset", columnType: "bigint", value: &protocol.DataValue{}, want: &protocol.DataValue{}},
		{name: "null stays null", columnType: "jsonb", value: null(), want: null()},
		{name: "a column of no type keeps its value", columnType: "", value: text("12.50"), want: text("12.50")},
		{name: "bigint from an integer", columnType: "bigint", value: integer(-5), want: integer(-5)},
		{name: "bigint from an unsigned integer", columnType: "bigint", value: unsigned(7), want: integer(7)},
		{name: "bigint from a whole double", columnType: "bigint", value: double(3), want: integer(3)},
		{name: "bigint from text", columnType: "bigint", value: text(" 42 "), want: integer(42)},
		{name: "bigint from a decimal of no fraction", columnType: "bigint", value: decimal("1200", 2), want: integer(12)},
		{name: "boolean", columnType: "boolean", value: boolean(true), want: boolean(true)},
		{name: "boolean from text", columnType: "boolean", value: text("T"), want: boolean(true)},
		{name: "boolean from off", columnType: "boolean", value: text("off"), want: boolean(false)},
		{name: "double precision", columnType: "double precision", value: double(1.5), want: double(1.5)},
		{name: "double precision from an integer", columnType: "double precision", value: integer(2), want: double(2)},
		{name: "double precision from text", columnType: "double precision", value: text("-0.25"), want: double(-0.25)},
		{name: "double precision infinity", columnType: "double precision", value: text("Infinity"), want: double(math.Inf(1))},
		{name: "real rounds to float4", columnType: "real", value: double(0.1), want: double(float64(float32(0.1)))},
		{name: "numeric drops the trailing zero", columnType: "numeric", value: text("12.50"), want: decimal("125", 1)},
		{name: "numeric expands an exponent", columnType: "numeric", value: text("1e3"), want: decimal("1000", 0)},
		{name: "numeric zero loses its sign", columnType: "numeric", value: text("-0.0"), want: decimal("0", 0)},
		{name: "numeric small fraction", columnType: "numeric", value: text("0.000100"), want: decimal("1", 4)},
		{name: "numeric without a whole part", columnType: "numeric", value: text(".5"), want: decimal("5", 1)},
		{name: "numeric from a decimal", columnType: "numeric", value: decimal("-1250", 2), want: decimal("-125", 1)},
		{name: "numeric from an integer", columnType: "numeric", value: integer(-7), want: decimal("-7", 0)},
		{name: "numeric from a double", columnType: "numeric", value: double(0.25), want: decimal("25", 2)},
		{name: "varchar counts characters", columnType: "character varying(5)", value: text("ünïcö"), want: text("ünïcö")},
		{name: "varchar from an integer", columnType: "character varying", value: integer(12), want: text("12")},
		{name: "varchar from a boolean", columnType: "character varying", value: boolean(true), want: text("true")},
		{name: "uuid in lower case", columnType: "uuid", value: text("A0EEBC99-9C0B-4EF8-BB6D-6BB9BD380A11"), want: text("a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11")},
		{name: "uuid without hyphens in braces", columnType: "uuid", value: text("{a0eebc999c0b4ef8bb6d6bb9bd380a11}"), want: text("a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11")},
		{name: "bytea from text", columnType: "bytea", value: text("abc"), want: binary("abc")},
		{name: "bytea from hex", columnType: "bytea", value: text(`\x6869`), want: binary("hi")},
		{name: "bytea from spaced hex", columnType: "bytea", value: text(`\x 68 69`), want: binary("hi")},
		{name: "bytea from octal escapes", columnType: "bytea", value: text(`\141\142`), want: binary("ab")},
		{name: "bytea from an escaped backslash", columnType: "bytea", value: text(`\\x`), want: binary(`\x`)},
		{name: "bytea of no byte", columnType: "bytea", value: text(`\x`), want: binary("")},
		{name: "bytea from bytes", columnType: "bytea", value: binary("\x00\xff"), want: binary("\x00\xff")},
		{name: "timestamp from RFC 3339", columnType: "timestamp with time zone", value: text("2026-01-02T03:04:05Z"),
			want: at(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))},
		{name: "timestamp without an offset is UTC", columnType: "timestamp with time zone", value: text("2026-01-02 03:04:05"),
			want: at(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))},
		{name: "timestamp in another zone", columnType: "timestamp with time zone", value: text("2026-01-02T03:04:05.123456789+02:00"),
			want: at(time.Date(2026, 1, 2, 1, 4, 5, 123457000, time.UTC))},
		{name: "timestamp from a date", columnType: "timestamp with time zone", value: text("2026-01-02"),
			want: at(time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))},
		{name: "timestamp tie to the even microsecond below", columnType: "timestamp with time zone", value: moment(1767322800, 123456500),
			want: moment(1767322800, 123456000)},
		{name: "timestamp tie to the even microsecond above", columnType: "timestamp with time zone", value: moment(1767322800, 123457500),
			want: moment(1767322800, 123458000)},
		{name: "date from text", columnType: "date", value: text("1999-12-31"), want: date(10956)},
		{name: "date", columnType: "date", value: date(5), want: date(5)},
		{name: "jsonb sorts members and keeps the last of a name", columnType: "jsonb", value: text(`{"b":1,"a":2,"aa":[1,2,{"z":null}],"a":3}`),
			want: json(`{"a":3,"aa":[1,2,{"z":null}],"b":1}`)},
		{name: "jsonb numbers in their shortest form", columnType: "jsonb", value: json(`[1.50, 2e0, -0.0, 1E-3]`), want: json(`[1.5,2,0,0.001]`)},
		{name: "jsonb keeps characters outside ASCII", columnType: "jsonb", value: text(`{"w": "é<>&A\n"}`), want: json(`{"w":"é<>&A\n"}`)},
		{name: "jsonb from an integer", columnType: "jsonb", value: integer(5), want: json(`5`)},
		{name: "jsonb string", columnType: "jsonb", value: text(`"x"`), want: json(`"x"`)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			got, err := rowdata.Canonical(test.columnType, test.value)
			c.Assert(err, qt.IsNil)
			c.Assert(got, qt.CmpEquals(protocmp.Transform()), test.want)
		})
	}
}

// NaN is a value of a double precision column, and proto.Equal never pairs
// it, so it is asserted apart.
func TestCanonical_NaN(t *testing.T) {
	c := testcheck.New(t)
	got, err := rowdata.Canonical("double precision", text("nan"))
	c.Assert(err, qt.IsNil)
	c.Assert(math.IsNaN(got.GetDoubleValue()), qt.IsTrue)
}

// A value its column cannot hold is refused, naming the type but never the
// value.
func TestCanonical_FailurePath(t *testing.T) {
	tests := []struct {
		name, columnType string
		value            *protocol.DataValue
		wantErr          string
	}{
		{name: "bigint beyond its range", columnType: "bigint", value: unsigned(math.MaxUint64), wantErr: `an integer value is beyond the range of bigint`},
		{name: "bigint from a fraction", columnType: "bigint", value: double(1.5), wantErr: `a floating-point value is not an integer in the range of bigint`},
		{name: "bigint from words", columnType: "bigint", value: text("x"), wantErr: `a text value is not an integer in the range of bigint`},
		{name: "bigint from a boolean", columnType: "bigint", value: boolean(true), wantErr: `a boolean value cannot be written into a bigint column`},
		{name: "boolean from words", columnType: "boolean", value: text("maybe"), wantErr: `a text value is not a boolean`},
		{name: "boolean from an integer", columnType: "boolean", value: integer(1), wantErr: `an integer value cannot be written into a boolean column`},
		{name: "real beyond its range", columnType: "real", value: double(1e300), wantErr: `a floating-point value is beyond the range of real`},
		{name: "double from hexadecimal", columnType: "double precision", value: text("0x1p3"), wantErr: `a text value is not a number`},
		{name: "numeric NaN", columnType: "numeric", value: text("NaN"), wantErr: `a text value is not a decimal number; Ptah writes no numeric NaN or infinity`},
		{name: "numeric infinity", columnType: "numeric", value: double(math.Inf(1)), wantErr: `a floating-point value that is not a finite number has no numeric value Ptah writes`},
		{name: "numeric fraction beyond its bound", columnType: "numeric", value: text("1e-16384"), wantErr: `a decimal value has more than the 16383 digits after the point a numeric holds`},
		{name: "varchar longer than its length", columnType: "character varying(5)", value: text("abcdef"),
			wantErr: `a text value is longer than the 5 characters of a character varying\(5\) column`},
		{name: "varchar with a NUL", columnType: "character varying", value: text("a\x00b"), wantErr: `a text value holds a NUL character, which a character varying column cannot hold`},
		{name: "uuid that is not one", columnType: "uuid", value: text("abc"), wantErr: `a text value is not a uuid`},
		{name: "bytea backslash", columnType: "bytea", value: text(`a\b`), wantErr: `a text value is not bytea input: a backslash starts neither a backslash nor three octal digits`},
		{name: "bytea odd hex", columnType: "bytea", value: text(`\x6`), wantErr: `a text value is not bytea hex digits`},
		{name: "timestamp from words", columnType: "timestamp with time zone", value: text("yesterday"), wantErr: `a text value names no moment; .*`},
		{name: "timestamp before year 1", columnType: "timestamp with time zone", value: text("0000-12-31T23:00:00Z"),
			wantErr: `a moment is outside the years 1 to 9999 a Spanner timestamp holds`},
		{name: "timestamp from an integer", columnType: "timestamp with time zone", value: integer(1), wantErr: `an integer value cannot be written into a timestamp with time zone column`},
		{name: "date from a slashed spelling", columnType: "date", value: text("2026/01/02"), wantErr: `a text value is not a date; write it as 2006-01-02`},
		{name: "date beyond year 9999", columnType: "date", value: date(2932897), wantErr: `a date is outside the years 1 to 9999 a Spanner date holds`},
		{name: "jsonb that does not parse", columnType: "jsonb", value: text("{"), wantErr: `a text value is not JSON`},
		{name: "jsonb with text after the value", columnType: "jsonb", value: text("{} []"), wantErr: `a text value is not JSON`},
		{name: "a type Ptah writes no value of", columnType: "bigint[]", value: integer(1), wantErr: `Ptah writes no value of type bigint\[\]`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			got, err := rowdata.Canonical(test.columnType, test.value)
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(got, qt.IsNil)
		})
	}
}

// ColumnType reads a declared spelling and the one information_schema
// reports alike, and refuses a type no row value carries.
func TestColumnType_HappyPath(t *testing.T) {
	tests := []struct {
		spelling, want string
		kind           protocol.DataKind
	}{
		{spelling: "BIGINT", want: "bigint", kind: protocol.DataKind_DATA_KIND_INT64},
		{spelling: "VARCHAR(100)", want: "character varying(100)", kind: protocol.DataKind_DATA_KIND_STRING},
		{spelling: "character varying", want: "character varying", kind: protocol.DataKind_DATA_KIND_STRING},
		{spelling: "TIMESTAMPTZ", want: "timestamp with time zone", kind: protocol.DataKind_DATA_KIND_TIMESTAMP},
		{spelling: "DOUBLE PRECISION", want: "double precision", kind: protocol.DataKind_DATA_KIND_DOUBLE},
		{spelling: "numeric", want: "numeric", kind: protocol.DataKind_DATA_KIND_DECIMAL},
		{spelling: "BYTEA", want: "bytea", kind: protocol.DataKind_DATA_KIND_BYTES},
		{spelling: "date", want: "date", kind: protocol.DataKind_DATA_KIND_DATE},
		{spelling: "JSONB", want: "jsonb", kind: protocol.DataKind_DATA_KIND_JSON},
		{spelling: "bool", want: "boolean", kind: protocol.DataKind_DATA_KIND_BOOL},
		{spelling: "uuid", want: "uuid", kind: protocol.DataKind_DATA_KIND_STRING},
	}
	for _, test := range tests {
		t.Run(test.spelling, func(t *testing.T) {
			c := testcheck.New(t)
			got, err := rowdata.ColumnType(test.spelling)
			c.Assert(err, qt.IsNil)
			c.Assert(got, qt.Equals, test.want)
			c.Assert(rowdata.Kind(got), qt.Equals, test.kind)
		})
	}
}

func TestColumnType_FailurePath(t *testing.T) {
	tests := []struct {
		spelling, wantErr string
	}{
		{spelling: "bigint[]", wantErr: `Ptah writes no value of type bigint\[\]`},
		{spelling: "ARRAY", wantErr: `type "ARRAY" is not a column type of Spanner's PostgreSQL interface`},
		{spelling: "spanner.commit_timestamp", wantErr: `Ptah writes no value of type spanner.commit_timestamp`},
		{spelling: "timestamp", wantErr: `type "timestamp": Spanner has no timestamp without time zone; use timestamptz`},
	}
	for _, test := range tests {
		t.Run(test.spelling, func(t *testing.T) {
			c := testcheck.New(t)
			got, err := rowdata.ColumnType(test.spelling)
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(got, qt.Equals, "")
		})
	}
}
