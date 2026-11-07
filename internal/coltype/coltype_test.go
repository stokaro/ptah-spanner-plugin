package coltype_test

import (
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-spanner-plugin/internal/coltype"
	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// The spellings and what information_schema.columns.spanner_type reported for
// each, measured on the emulator behind PGAdapter 0.56.2.
func TestCanonical_HappyPath(t *testing.T) {
	tests := []struct {
		declared, canonical string
	}{
		{declared: "BIGINT", canonical: "bigint"},
		{declared: "int", canonical: "bigint"},
		{declared: "INTEGER", canonical: "bigint"},
		{declared: "int8", canonical: "bigint"},
		{declared: "bool", canonical: "boolean"},
		{declared: "Boolean", canonical: "boolean"},
		{declared: "bytea", canonical: "bytea"},
		{declared: "date", canonical: "date"},
		{declared: "float", canonical: "double precision"},
		{declared: "float8", canonical: "double precision"},
		{declared: "double   precision", canonical: "double precision"},
		{declared: "real", canonical: "real"},
		{declared: "float4", canonical: "real"},
		{declared: "jsonb", canonical: "jsonb"},
		{declared: "numeric", canonical: "numeric"},
		{declared: "DECIMAL", canonical: "numeric"},
		{declared: "TEXT", canonical: "character varying"},
		{declared: "varchar", canonical: "character varying"},
		{declared: "VARCHAR(255)", canonical: "character varying(255)"},
		{declared: "character varying ( 20 )", canonical: "character varying(20)"},
		{declared: "varchar(2621440)", canonical: "character varying"},
		{declared: "timestamptz", canonical: "timestamp with time zone"},
		{declared: "TIMESTAMP WITH TIME ZONE", canonical: "timestamp with time zone"},
		{declared: "uuid", canonical: "uuid"},
		{declared: "spanner.commit_timestamp", canonical: "spanner.commit_timestamp"},
		{declared: "bigint[]", canonical: "bigint[]"},
		{declared: "text[]", canonical: "character varying[]"},
		{declared: "varchar(10)[]", canonical: "character varying(10)[]"},
	}
	for _, test := range tests {
		t.Run(test.declared, func(t *testing.T) {
			c := testcheck.New(t)
			canonical, err := coltype.Canonical(test.declared)
			c.Assert(err, qt.IsNil)
			c.Assert(canonical, qt.Equals, test.canonical)
		})
	}
}

// The spellings Spanner refuses, each with the reason the provider gives
// before a statement is sent.
func TestCanonical_FailurePath(t *testing.T) {
	tests := []struct {
		declared, wantErr string
	}{
		{declared: "", wantErr: `a column has no type`},
		{declared: "smallint", wantErr: `type "smallint": Spanner stores integers as bigint only`},
		{declared: "int4", wantErr: `type "int4": Spanner stores integers as bigint only`},
		{declared: "serial", wantErr: `type "serial": a serial column is an identity column, which the provider does not describe yet`},
		{declared: "timestamp", wantErr: `type "timestamp": Spanner has no timestamp without time zone; use timestamptz`},
		{declared: "json", wantErr: `type "json": Spanner has no json type; use jsonb`},
		{declared: "interval", wantErr: `type "interval": Spanner refuses a column of type interval`},
		{declared: "char", wantErr: `type "char": Spanner refuses the character type; use varchar`},
		{declared: "xml", wantErr: `type "xml": Spanner has no xml type; use text`},
		{declared: "numeric(10,2)", wantErr: `type "numeric\(10,2\)": Spanner refuses a type modifier on numeric`},
		{declared: "timestamptz(3)", wantErr: `type "timestamptz\(3\)": Spanner refuses a type modifier on timestamptz`},
		{declared: "varchar(0)", wantErr: `type "varchar\(0\)" has a length Spanner refuses`},
		{declared: "varchar(2621441)", wantErr: `type "varchar\(2621441\)" has a length Spanner refuses`},
		{declared: "bigint[][]", wantErr: `type "bigint\[\]\[\]" is an array of arrays, which Spanner refuses`},
		{declared: "money", wantErr: `type "money" is not a column type of Spanner's PostgreSQL interface`},
	}
	for _, test := range tests {
		t.Run(test.declared, func(t *testing.T) {
			c := testcheck.New(t)
			canonical, err := coltype.Canonical(test.declared)
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(canonical, qt.Equals, "")
		})
	}
}

func TestSame(t *testing.T) {
	tests := []struct {
		name, declared, observed string
		want                     bool
	}{
		{name: "text and character varying", declared: "TEXT", observed: "character varying", want: true},
		{name: "a length on both sides", declared: "VARCHAR(20)", observed: "character varying(20)", want: true},
		{name: "a different length", declared: "VARCHAR(20)", observed: "character varying(30)", want: false},
		{name: "a length on one side", declared: "TEXT", observed: "character varying(30)", want: false},
		{name: "integer spellings", declared: "INTEGER", observed: "bigint", want: true},
		{name: "a different type", declared: "BIGINT", observed: "double precision", want: false},
		{name: "a refused declaration", declared: "smallint", observed: "bigint", want: false},
		{name: "an array", declared: "text[]", observed: "character varying[]", want: true},
		{name: "an integer array", declared: "int[]", observed: "bigint[]", want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(coltype.Same(test.declared, test.observed), qt.Equals, test.want)
		})
	}
}

// Measured on the emulator behind PGAdapter 0.56.2 with ALTER COLUMN ...
// TYPE: Spanner changes a column between character varying of any length and
// bytea, arrays of them included, and refuses the rest with `Cannot change
// type of column`.
func TestConvertible(t *testing.T) {
	tests := []struct {
		name, from, to string
		want           bool
	}{
		{name: "a longer varchar", from: "character varying(20)", to: "character varying(30)", want: true},
		{name: "a shorter varchar", from: "character varying(20)", to: "character varying(10)", want: true},
		{name: "text to a bounded varchar", from: "character varying", to: "character varying(100)", want: true},
		{name: "varchar to bytea", from: "character varying(20)", to: "bytea", want: true},
		{name: "bytea to text", from: "bytea", to: "character varying", want: true},
		{name: "an array of text to an array of bytea", from: "character varying[]", to: "bytea[]", want: true},
		{name: "the same type", from: "bigint", to: "bigint", want: true},
		{name: "bigint to double precision", from: "bigint", to: "double precision", want: false},
		{name: "bigint to numeric", from: "bigint", to: "numeric", want: false},
		{name: "jsonb to text", from: "jsonb", to: "character varying", want: false},
		{name: "text to jsonb", from: "character varying", to: "jsonb", want: false},
		{name: "an array to a scalar", from: "character varying[]", to: "character varying", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(coltype.Convertible(test.from, test.to), qt.Equals, test.want)
		})
	}
}

func TestDataType(t *testing.T) {
	tests := []struct{ canonical, want string }{
		{canonical: "bigint", want: "bigint"},
		{canonical: "character varying(20)", want: "character varying"},
		{canonical: "character varying[]", want: "ARRAY"},
		{canonical: "timestamp with time zone", want: "timestamp with time zone"},
	}
	for _, test := range tests {
		t.Run(test.canonical, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(coltype.DataType(test.canonical), qt.Equals, test.want)
		})
	}
}

// The stored spellings of defaults, measured on the emulator: `DEFAULT 0` on a
// bigint is `'0'::bigint`, `DEFAULT 'active'` on text is `'active'::text`. A
// SQL schema file writes a literal quoted and without a cast.
func TestReadDefault(t *testing.T) {
	tests := []struct {
		name   string
		stored *string
		kind   coltype.DefaultKind
		value  string
	}{
		{name: "no default", stored: nil, kind: coltype.Absent, value: ""},
		{name: "a cast integer", stored: new("'0'::bigint"), kind: coltype.Literal, value: "0"},
		{name: "a cast string", stored: new("'active'::text"), kind: coltype.Literal, value: "active"},
		{name: "a doubled quote", stored: new("'it''s'::character varying"), kind: coltype.Literal, value: "it's"},
		{name: "a cast with a length", stored: new("'x'::character varying(20)"), kind: coltype.Literal, value: "x"},
		{name: "a quoted literal without a cast", stored: new("'x'"), kind: coltype.Literal, value: "x"},
		{name: "a boolean", stored: new("TRUE"), kind: coltype.Literal, value: "true"},
		{name: "a bare number", stored: new("1.5"), kind: coltype.Literal, value: "1.5"},
		{name: "not a number", stored: new("NaN"), kind: coltype.Expression, value: "NaN"},
		{name: "a function", stored: new("now()"), kind: coltype.Expression, value: "now()"},
		{name: "a spaced expression", stored: new("  (a + 1) "), kind: coltype.Expression, value: "(a + 1)"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			kind, value := coltype.ReadDefault(test.stored)
			c.Assert(kind, qt.Equals, test.kind)
			c.Assert(value, qt.Equals, test.value)
		})
	}
}

func TestLiteralValue(t *testing.T) {
	tests := []struct{ declared, want string }{
		{declared: "active", want: "active"},
		{declared: "'active'", want: "active"},
		{declared: "'it''s'", want: "it's"},
		{declared: "0", want: "0"},
	}
	for _, test := range tests {
		t.Run(test.declared, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(coltype.LiteralValue(test.declared), qt.Equals, test.want)
		})
	}
}

// Spanner refuses a quoted number on a bigint column (`Expected type INT64;
// found STRING`), so a number on a numeric column and a boolean on a boolean
// column are written bare, and every other value is a quoted string.
func TestLiteralSQL(t *testing.T) {
	tests := []struct {
		name, declaredType, value, want string
	}{
		{name: "a number on bigint", declaredType: "BIGINT", value: "0", want: "0"},
		{name: "a decimal on numeric", declaredType: "numeric", value: "1.25", want: "1.25"},
		{name: "a boolean", declaredType: "bool", value: "TRUE", want: "true"},
		{name: "a string", declaredType: "TEXT", value: "active", want: "'active'"},
		{name: "a quote in a string", declaredType: "varchar(10)", value: "it's", want: "'it''s'"},
		{name: "a literal kept as written", declaredType: "text", value: "'x'", want: "'x'"},
		{name: "a number on text", declaredType: "text", value: "7", want: "'7'"},
		{name: "not a number on bigint", declaredType: "bigint", value: "NaN", want: "'NaN'"},
		{name: "a refused type", declaredType: "money", value: "1", want: "'1'"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(coltype.LiteralSQL(test.declaredType, test.value), qt.Equals, test.want)
		})
	}
}
