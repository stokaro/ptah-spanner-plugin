// Package coltype knows the column types of Spanner's PostgreSQL interface:
// which spellings a CREATE TABLE accepts, the spelling information_schema
// reports for each, which changes of type ALTER COLUMN makes in place, and how
// a default is written and read back.
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

// Same reports whether a declared type and the type a read reports name one
// column type. A declared spelling Spanner refuses is never the same.
func Same(declared, observed string) bool {
	canonical, err := Canonical(declared)
	if err != nil {
		return false
	}
	return canonical == strings.ToLower(strings.TrimSpace(observed))
}

// narrowIntegers are the spellings of PostgreSQL's 32-bit integer that
// Spanner accepts. It stores each as bigint: measured on the emulator behind
// PGAdapter 0.56.2, an integer column reads back as bigint and takes
// 9999999999. Spanner keeps one integer type, 64 bits wide.
var narrowIntegers = map[string]bool{"integer": true, "int": true}

// Widened reports what Spanner leaves out of a declared type it accepts, as
// one line naming the declared spelling and what is lost, or false when it
// keeps the whole type. The one such type is PostgreSQL's 32-bit integer,
// alone or as an array's element: Spanner stores bigint, so the 32-bit range
// is not enforced. A spelling Spanner refuses is not widened.
func Widened(declared string) (string, bool) {
	spelling := spaces.ReplaceAllString(strings.ToLower(strings.TrimSpace(declared)), " ")
	element, _ := strings.CutSuffix(spelling, "[]")
	if !narrowIntegers[element] {
		return "", false
	}
	return spelling + ": the 32-bit range; Spanner stores bigint", true
}

// DataType is information_schema.columns.data_type for a canonical type: the
// type without its length, and ARRAY for an array.
func DataType(canonical string) string {
	if strings.HasSuffix(canonical, "[]") {
		return "ARRAY"
	}
	if strings.HasPrefix(canonical, "character varying(") {
		return "character varying"
	}
	return canonical
}

// Convertible reports whether ALTER COLUMN ... TYPE changes a column from one
// canonical type to another in place. Measured on the emulator behind PGAdapter
// 0.56.2, Spanner changes a column between character varying of any length and
// bytea, and an array of one to an array of the other, and refuses every other
// change: bigint to double precision answers `Cannot change type of column`.
// A column that keeps its type converts trivially.
func Convertible(from, to string) bool {
	if from == to {
		return true
	}
	fromElement, fromArray := strings.CutSuffix(from, "[]")
	toElement, toArray := strings.CutSuffix(to, "[]")
	return fromArray == toArray && textOrBytes(fromElement) && textOrBytes(toElement)
}

func textOrBytes(canonical string) bool {
	return canonical == "bytea" || canonical == "character varying" || strings.HasPrefix(canonical, "character varying(")
}

// DefaultKind says how a stored default reads.
type DefaultKind int

const (
	// Absent is a column without a default.
	Absent DefaultKind = iota
	// Literal is a constant value: Spanner stores `DEFAULT 0` as
	// `'0'::bigint` and `DEFAULT 'x'` as `'x'::text`.
	Literal
	// Expression is anything else, such as `now()`.
	Expression
)

// quotedLiteral is a single-quoted literal with an optional cast, the form
// Spanner stores a literal default in and the form a SQL schema file writes
// one in.
var quotedLiteral = regexp.MustCompile(`^'((?:[^']|'')*)'(?:::[a-z][a-z0-9_. ]*(?:\(\d+\))?(?:\[\])?)?$`)

// ReadDefault reads a default as the server stores it, or as an expression
// is written: a literal and its value, or an expression and its text.
func ReadDefault(stored *string) (DefaultKind, string) {
	if stored == nil {
		return Absent, ""
	}
	text := strings.TrimSpace(*stored)
	if match := quotedLiteral.FindStringSubmatch(text); match != nil {
		return Literal, strings.ReplaceAll(match[1], "''", "'")
	}
	switch strings.ToLower(text) {
	case "true", "false":
		return Literal, strings.ToLower(text)
	}
	if plainNumber(text) {
		return Literal, text
	}
	return Expression, text
}

// LiteralValue is the value a declared literal default stands for. A SQL
// schema file keeps the literal as written, `'active'`, and a struct tag keeps
// the bare value, `active`; both stand for the value active.
func LiteralValue(declared string) string {
	if match := quotedLiteral.FindStringSubmatch(strings.TrimSpace(declared)); match != nil {
		return strings.ReplaceAll(match[1], "''", "'")
	}
	return declared
}

// LiteralSQL writes a declared literal default for a column of declaredType in
// the form Spanner accepts after DEFAULT. A number on a numeric column and a
// boolean on a boolean column are written bare: Spanner refuses `DEFAULT '7'`
// on a bigint column with `Expected type INT64; found STRING`. Any other value
// is a quoted string, and a value already written as a literal is kept as
// written.
func LiteralSQL(declaredType, declared string) string {
	trimmed := strings.TrimSpace(declared)
	if quotedLiteral.MatchString(trimmed) {
		return trimmed
	}
	canonical, err := Canonical(declaredType)
	if err == nil {
		switch canonical {
		case "boolean":
			if lowered := strings.ToLower(trimmed); lowered == "true" || lowered == "false" {
				return lowered
			}
		case "bigint", "numeric", "double precision", "real":
			if plainNumber(trimmed) {
				return trimmed
			}
		}
	}
	return "'" + strings.ReplaceAll(declared, "'", "''") + "'"
}

// plainNumber reports a decimal number written the way SQL writes one. It
// refuses NaN, Inf and hexadecimal forms, which strconv.ParseFloat accepts and
// a bare SQL default does not.
func plainNumber(value string) bool {
	digits := strings.TrimLeft(value, "+-")
	if digits == "" || len(value)-len(digits) > 1 {
		return false
	}
	mantissa, exponent, hasExponent := strings.Cut(strings.ToLower(digits), "e")
	if hasExponent && !allDigits(strings.TrimLeft(exponent, "+-")) {
		return false
	}
	whole, fraction, hasPoint := strings.Cut(mantissa, ".")
	switch {
	case !hasPoint:
		return allDigits(whole)
	case whole == "":
		return allDigits(fraction)
	default:
		return allDigits(whole) && (fraction == "" || allDigits(fraction))
	}
}

func allDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
