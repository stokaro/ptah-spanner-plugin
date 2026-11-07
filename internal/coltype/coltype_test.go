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
