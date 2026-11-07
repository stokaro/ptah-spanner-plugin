package commonobject_test

import (
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-spanner-plugin/internal/commonobject"
	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// viewTables are the declared tables t and u the view shapes read.
func viewTables() map[string][]commonobject.Column {
	return map[string][]commonobject.Column{
		"t": {{Name: "id", Type: "bigint"}, {Name: "name", Type: "varchar(20)"}, {Name: "price", Type: "double precision"},
			{Name: "flag", Type: "boolean"}, {Name: "ts", Type: "timestamptz"}},
		"u": {{Name: "id", Type: "bigint"}, {Name: "t_id", Type: "bigint"}, {Name: "label", Type: "text"}},
	}
}

// The declared query and the query Spanner stores for it are one query: the
// stored forms measured on the emulator behind PGAdapter 0.56.2, with the
// casts, parentheses, rewritten predicates, qualifiers and aliases Spanner
// writes, and the columns it names for a `*`.
func TestSameViewQuery_HappyPath(t *testing.T) {
	for _, test := range []struct {
		name, declared, held string
	}{
		{"a plain select", "SELECT id, name FROM t", "SELECT id, name FROM t"},
		{"spelling", `select "id", name from t;`, "SELECT id, name FROM t"},
		{"a cast literal", "SELECT id FROM t WHERE id > 0", "SELECT id FROM t WHERE (id > '0'::bigint)"},
		{"a double precision literal", "SELECT id FROM t WHERE price > 1.5", "SELECT id FROM t WHERE (price > (1.5)::double precision)"},
		{"IN", "SELECT id FROM t WHERE name IN ('a', 'b')", "SELECT id FROM t WHERE (name = ANY (ARRAY['a'::text, 'b'::text]))"},
		{"BETWEEN", "SELECT id FROM t WHERE id BETWEEN 1 AND 10", "SELECT id FROM t WHERE ((id >= '1'::bigint) AND (id <= '10'::bigint))"},
		{"a join", "SELECT t.id, u.label FROM t JOIN u ON u.t_id = t.id", "SELECT t.id, u.label FROM (t JOIN u ON ((u.t_id = t.id)))"},
		{"LIMIT", "SELECT id FROM t ORDER BY id LIMIT 10", "SELECT id FROM t ORDER BY id LIMIT '10'::bigint"},
		{"an alias", "SELECT x.id FROM t AS x", "SELECT id FROM t x"},
		{"a star", "SELECT * FROM t", "SELECT id, name, price, flag, ts FROM t"},
		{"UNION ALL", "SELECT id FROM t UNION ALL SELECT id FROM u", "SELECT t.id FROM t UNION ALL SELECT u.id FROM u"},
		{"a quoted alias", `SELECT id AS "Id" FROM t`, `SELECT id AS "Id" FROM t`},
		{"NOT and a function", "SELECT id FROM t WHERE NOT flag AND ts > now()", "SELECT id FROM t WHERE ((NOT flag) AND (ts > now()))"},
		{"DISTINCT", "SELECT DISTINCT name FROM t", "SELECT DISTINCT name FROM t"},
		{"a descending order", "SELECT id FROM t ORDER BY id DESC", "SELECT id FROM t ORDER BY id DESC"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)

			c.Assert(commonobject.SameViewQuery(test.declared, test.held, viewTables()), qt.IsTrue)
		})
	}
}

// Two queries that say different things are not one query, and a query the
// comparison cannot read is never one with another: a subquery, a CASE, an
// aggregate call, GROUP BY, a table the declaration does not declare, and a
// star over a join.
func TestSameViewQuery_FailurePath(t *testing.T) {
	for _, test := range []struct {
		name, declared, held string
	}{
		{"another literal", "SELECT id FROM t WHERE id > 1", "SELECT id FROM t WHERE (id > '0'::bigint)"},
		{"another column", "SELECT id, label FROM u", "SELECT id, t_id FROM u"},
		{"another table", "SELECT id FROM t", "SELECT id FROM u"},
		{"another limit", "SELECT id FROM t ORDER BY id LIMIT 5", "SELECT id FROM t ORDER BY id LIMIT '10'::bigint"},
		{"another direction", "SELECT id FROM t ORDER BY id", "SELECT id FROM t ORDER BY id DESC"},
		{"another alias", "SELECT id AS a FROM t", "SELECT id AS b FROM t"},
		{"UNION and UNION ALL", "SELECT id FROM t UNION SELECT id FROM u", "SELECT t.id FROM t UNION ALL SELECT u.id FROM u"},
		{"a star the table no longer matches", "SELECT * FROM t", "SELECT id, name FROM t"},
		{"a subquery", "SELECT id FROM t WHERE id IN (SELECT t_id FROM u)", "SELECT id FROM t WHERE (id IN (SELECT u.t_id FROM u))"},
		{"a CASE", "SELECT CASE WHEN id > 5 THEN 'big' ELSE 'small' END AS size FROM t",
			"SELECT CASE WHEN (id > '5'::bigint) THEN 'big'::text ELSE 'small'::text END AS size FROM t"},
		{"an aggregate", "SELECT count(*) AS n FROM t", "SELECT count(*) AS n FROM t"},
		{"GROUP BY", "SELECT name FROM t GROUP BY name", "SELECT name FROM t GROUP BY name"},
		{"an undeclared table", "SELECT id FROM v", "SELECT id FROM v"},
		{"a star over a join", "SELECT * FROM t JOIN u ON u.t_id = t.id", "SELECT * FROM t JOIN u ON u.t_id = t.id"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)

			c.Assert(commonobject.SameViewQuery(test.declared, test.held, viewTables()), qt.IsFalse)
		})
	}
}
