package checkexpr_test

import (
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-spanner-plugin/internal/checkexpr"
	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// columns are the column types of the probe table the stored forms were
// measured on, in the spellings a read reports.
var columns = map[string]string{
	"score": "bigint", "price": "double precision", "amt": "numeric", "name": "character varying", "code": "character varying(10)",
	"flag": "boolean", "d": "date", "ts": "timestamp with time zone", "status": "character varying", "f4": "real",
}

// Each declared expression is one constraint with the clause Spanner kept
// for it: measured on the emulator behind PGAdapter 0.56.2 by adding the
// declared CHECK and reading information_schema.check_constraints.
func TestEquivalent_StoredForms(t *testing.T) {
	tests := []struct {
		name, declared, held string
	}{
		{name: "an integer literal", declared: "score >= 0", held: "(score >= '0'::bigint)"},
		{name: "a negative literal", declared: "score >= -5", held: "(score >= '-5'::bigint)"},
		{name: "a fraction against double precision", declared: "price >= 0.5", held: "(price >= (0.5)::double precision)"},
		{name: "an integer against double precision", declared: "price > 0", held: "(price > ('0'::bigint)::double precision)"},
		{name: "an integer against numeric", declared: "amt >= 0", held: "(amt >= ('0'::bigint)::numeric)"},
		{name: "a fraction against numeric", declared: "amt >= 1.25", held: "(amt >= 1.25)"},
		{name: "an empty string", declared: "name <> ''", held: "(name <> ''::text)"},
		{name: "a string against character varying of a length", declared: "code <> 'x'", held: "(code <> 'x'::text)"},
		{name: "a boolean", declared: "flag = true", held: "(flag = true)"},
		{name: "a date", declared: "d > '2020-01-01'", held: "(d > '2020-01-01'::date)"},
		{name: "an IN list", declared: "status IN ('a', 'b')", held: "(status = ANY (ARRAY['a'::text, 'b'::text]))"},
		{name: "BETWEEN", declared: "score BETWEEN 1 AND 10", held: "((score >= '1'::bigint) AND (score <= '10'::bigint))"},
		{name: "AND", declared: "score > 0 AND price > 0", held: "((score > '0'::bigint) AND (price > ('0'::bigint)::double precision))"},
		{name: "OR with IS NOT NULL", declared: "score > 0 OR name IS NOT NULL", held: "((score > '0'::bigint) OR (name IS NOT NULL))"},
		{name: "a function", declared: "length(name) > 2", held: "(length(name) > '2'::bigint)"},
		{name: "an integer IN list", declared: "score IN (1, 2, 3)", held: "(score = ANY (ARRAY['1'::bigint, '2'::bigint, '3'::bigint]))"},
		{name: "NOT", declared: "NOT (score < 0)", held: "(NOT (score < '0'::bigint))"},
		{name: "arithmetic of two types", declared: "score + 1 > price * 2",
			held: "(((score + '1'::bigint))::double precision > (price * ('2'::bigint)::double precision))"},
		{name: "LIKE", declared: "name LIKE 'a%'", held: "(name ~~ 'a%'::text)"},
		{name: "real against an integer", declared: "f4 >= 0", held: "(f4 >= ('0'::bigint)::double precision)"},
		{name: "a large integer", declared: "score <> 3000000000", held: "(score <> '3000000000'::bigint)"},
		{name: "NOT IN of one value", declared: "status NOT IN ('x')", held: "(status <> 'x'::text)"},
		{name: "IS NULL with OR", declared: "name IS NULL OR status = 'a'", held: "((name IS NULL) OR (status = 'a'::text))"},
		{name: "doubled parentheses", declared: "(score >= 0)", held: "(score >= '0'::bigint)"},
		{name: "a column in upper case", declared: "Score >= 0", held: "(score >= '0'::bigint)"},
		{name: "a function of text", declared: "lower(name) = name", held: "(lower(name) = name)"},
		{name: "an explicit cast", declared: "score::float8 > 0", held: "((score)::double precision > ('0'::bigint)::double precision)"},
		{name: "a time with a zone", declared: "ts > '2020-01-01T00:00:00Z'", held: "(ts > '2019-12-31 16:00:00-08'::timestamp with time zone)"},
		{name: "a regular expression", declared: "name ~ '^a'", held: "(name ~ '^a'::text)"},
		{name: "a fraction against an integer column", declared: "score >= 0.5", held: "((score)::numeric >= 0.5)"},
		{name: "an enum column's values", declared: "(score >= 0) AND status IN ('active', 'retired')",
			held: "((score >= '0'::bigint) AND (status = ANY (ARRAY['active'::text, 'retired'::text])))"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(checkexpr.Equivalent(test.declared, test.held, columns), qt.IsTrue)
		})
	}
}

// A declared expression that says something else than the held clause, or
// one either side cannot read, is not the held constraint, so the CHECK is
// replaced.
func TestEquivalent_Differs(t *testing.T) {
	tests := []struct {
		name, declared, held string
	}{
		{name: "another bound", declared: "score >= 1", held: "(score >= '0'::bigint)"},
		{name: "another operator", declared: "score > 0", held: "(score >= '0'::bigint)"},
		{name: "another column", declared: "price >= 0", held: "(score >= '0'::bigint)"},
		{name: "another value in a list", declared: "status IN ('a', 'c')", held: "(status = ANY (ARRAY['a'::text, 'b'::text]))"},
		{name: "a value added to a list", declared: "status IN ('a', 'b', 'c')", held: "(status = ANY (ARRAY['a'::text, 'b'::text]))"},
		{name: "a list in another order", declared: "status IN ('b', 'a')", held: "(status = ANY (ARRAY['a'::text, 'b'::text]))"},
		{name: "AND for OR", declared: "score > 0 OR price > 0", held: "((score > '0'::bigint) AND (price > ('0'::bigint)::double precision))"},
		{name: "an integer division against a numeric one", declared: "score / 2 > 1", held: "(((score)::numeric / (2)::numeric) > ('1'::bigint)::numeric)"},
		{name: "a quoted name in another case", declared: `"Score" >= 0`, held: "(score >= '0'::bigint)"},
		{name: "another string", declared: "name <> 'x'", held: "(name <> ''::text)"},
		{name: "another time", declared: "ts > '2020-01-02T00:00:00Z'", held: "(ts > '2019-12-31 16:00:00-08'::timestamp with time zone)"},
		{name: "a time without a zone", declared: "ts > '2020-01-01 00:00:00'", held: "(ts > '2020-01-01 00:00:00-08'::timestamp with time zone)"},
		{name: "a declaration that does not parse", declared: "score >=", held: "(score >= '0'::bigint)"},
		{name: "a held clause that does not parse", declared: "score >= 0", held: "(score >= '0'::bigint"},
		{name: "a subquery", declared: "score IN (SELECT 1)", held: "(score = ANY (ARRAY['1'::bigint]))"},
		{name: "an escape string", declared: "name <> E'x'", held: "(name <> 'x'::text)"},
		{name: "a comment", declared: "score >= 0 -- note", held: "(score >= '0'::bigint)"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(checkexpr.Equivalent(test.declared, test.held, columns), qt.IsFalse)
		})
	}
}

// A column the table types differently gives the same text another stored
// form, so the types decide.
func TestEquivalent_ReadsColumnTypes(t *testing.T) {
	c := testcheck.New(t)
	c.Assert(checkexpr.Equivalent("score > 0", "(score > ('0'::bigint)::double precision)", map[string]string{"score": "float8"}), qt.IsTrue)
	c.Assert(checkexpr.Equivalent("score > 0", "(score > ('0'::bigint)::double precision)", map[string]string{"score": "bigint"}), qt.IsFalse)
	c.Assert(checkexpr.Equivalent("score > 0", "(score > '0'::bigint)", nil), qt.IsTrue)
}
