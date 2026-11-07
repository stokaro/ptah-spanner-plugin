// Package coltype knows the column types of Spanner's PostgreSQL interface:
// which spellings a CREATE TABLE accepts and the spelling information_schema
// reports for each.
//
// Every fact here was measured on the Cloud Spanner emulator behind PGAdapter
// 0.56.2, by creating a table with the spelling and reading
// information_schema.columns.spanner_type back.
package coltype

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// unboundedVarchar is the length Spanner treats as no length at all: a column
// declared varchar(2621440) reads back as `character varying`.
const unboundedVarchar = 2621440

// scalars maps each accepted scalar spelling to the one information_schema
// reports. `int`, `integer` and `int8` are all stored as bigint; `float` is
// double precision and `float4` is real; `text` and `varchar` are a character
// varying without a length.
var scalars = map[string]string{
	"bigint":                   "bigint",
	"int8":                     "bigint",
	"int":                      "bigint",
	"integer":                  "bigint",
	"boolean":                  "boolean",
	"bool":                     "boolean",
	"bytea":                    "bytea",
	"date":                     "date",
	"double precision":         "double precision",
	"float8":                   "double precision",
	"float":                    "double precision",
	"real":                     "real",
	"float4":                   "real",
	"jsonb":                    "jsonb",
	"numeric":                  "numeric",
	"decimal":                  "numeric",
	"text":                     "character varying",
	"varchar":                  "character varying",
	"character varying":        "character varying",
	"timestamptz":              "timestamp with time zone",
	"timestamp with time zone": "timestamp with time zone",
	"uuid":                     "uuid",
	"spanner.commit_timestamp": "spanner.commit_timestamp",
}

// refusals names the spellings Spanner refuses with the reason it gives, so a
// declaration fails before a statement is sent.
var refusals = map[string]string{
	"smallint":                    "Spanner stores integers as bigint only",
	"int2":                        "Spanner stores integers as bigint only",
	"int4":                        "Spanner stores integers as bigint only",
	"serial":                      "a serial column is an identity column, which the provider does not describe yet",
	"bigserial":                   "a serial column is an identity column, which the provider does not describe yet",
	"timestamp":                   "Spanner has no timestamp without time zone; use timestamptz",
	"timestamp without time zone": "Spanner has no timestamp without time zone; use timestamptz",
	"json":                        "Spanner has no json type; use jsonb",
	"interval":                    "Spanner refuses a column of type interval",
	"char":                        "Spanner refuses the character type; use varchar",
	"character":                   "Spanner refuses the character type; use varchar",
	"bpchar":                      "Spanner refuses the character type; use varchar",
	"oid":                         "Spanner refuses the oid type",
	"xml":                         "Spanner has no xml type; use text",
}

var (
	spaces    = regexp.MustCompile(`\s+`)
	varcharN  = regexp.MustCompile(`^(?:varchar|character varying)\s*\(\s*(\d+)\s*\)$`)
	modifiers = regexp.MustCompile(`^([a-z][a-z0-9_. ]*?)\s*\(.*\)$`)
)

// Canonical returns the spelling information_schema reports for a declared
// type, or an error naming why Spanner refuses it. Case and spacing do not
// matter, and an array type keeps its element's canonical spelling with `[]`.
func Canonical(declared string) (string, error) {
	spelling := spaces.ReplaceAllString(strings.ToLower(strings.TrimSpace(declared)), " ")
	if spelling == "" {
		return "", fmt.Errorf("a column has no type")
	}
	if element, found := strings.CutSuffix(spelling, "[]"); found {
		canonical, err := Canonical(element)
		if err != nil {
			return "", err
		}
		if strings.HasSuffix(canonical, "[]") {
			return "", fmt.Errorf("type %q is an array of arrays, which Spanner refuses", declared)
		}
		return canonical + "[]", nil
	}
	if match := varcharN.FindStringSubmatch(spelling); match != nil {
		length, err := strconv.ParseInt(match[1], 10, 64)
		if err != nil || length < 1 || length > unboundedVarchar {
			return "", fmt.Errorf("type %q has a length Spanner refuses", declared)
		}
		if length == unboundedVarchar {
			return "character varying", nil
		}
		return fmt.Sprintf("character varying(%d)", length), nil
	}
	if canonical, found := scalars[spelling]; found {
		return canonical, nil
	}
	if reason, found := refusals[spelling]; found {
		return "", fmt.Errorf("type %q: %s", declared, reason)
	}
	if match := modifiers.FindStringSubmatch(spelling); match != nil {
		if _, found := scalars[match[1]]; found {
			return "", fmt.Errorf("type %q: Spanner refuses a type modifier on %s", declared, match[1])
		}
	}
	return "", fmt.Errorf("type %q is not a column type of Spanner's PostgreSQL interface", declared)
}
