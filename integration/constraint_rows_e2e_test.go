//go:build integration

package integration_test

import (
	"path/filepath"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// A changed CHECK or foreign key is replaced by DROP CONSTRAINT and then ADD
// CONSTRAINT. Spanner runs each statement on its own, so when rows break the
// new constraint, the drop would commit and the add be refused, leaving the
// table without the constraint (stokaro/ptah#4204 S93). These tests run the
// host against the emulator and show the provider refuse such a plan before
// its first statement, and apply the replacements the rows keep.

// constraintRefusal is what the provider's reason says after the unit, the
// constraint, the table and the number of rows.
const constraintRefusal = `so no unit ran: Spanner refuses to add a constraint a row breaks; update or delete the rows that break it`

// `ptah schema apply` of a declaration whose changed CHECK or foreign key rows
// already break is refused before any statement runs: the table keeps the
// constraint it held, which still refuses a row it refused before, and the
// foreign key still references the table it referenced.
func TestHostRefusesAConstraintReplacementRowsBreakE2E(t *testing.T) {
	tests := []struct {
		name     string
		before   string
		rows     []string
		after    string
		want     string
		checks   []string
		keys     []string
		violates string
		refused  string
	}{
		{name: "a narrowed CHECK",
			before: "CREATE TABLE t (id bigint PRIMARY KEY, n bigint, CONSTRAINT t_n_check CHECK (n > 0));\n",
			rows:   []string{`INSERT INTO t (id, n) VALUES (1, 3), (2, NULL)`},
			after:  "CREATE TABLE t (id bigint PRIMARY KEY, n bigint, CONSTRAINT t_n_check CHECK (n > 5));\n",
			want:   `unit 2 of 2 adds CHECK "t_n_check" to table "t", which 1 row breaks, ` + constraintRefusal,
			checks: []string{"t_n_check (n > '0'::bigint)"}, violates: `INSERT INTO t (id, n) VALUES (3, 0)`, refused: `.*Check constraint .*t_n_check.* is violated.*`},
		{name: "a foreign key that references another table",
			before: "CREATE TABLE p (id bigint PRIMARY KEY);\nCREATE TABLE q (id bigint PRIMARY KEY);\n" +
				"CREATE TABLE ch (id bigint PRIMARY KEY, r_id bigint, CONSTRAINT ch_r_fk FOREIGN KEY (r_id) REFERENCES q (id));\n",
			rows: []string{`INSERT INTO q (id) VALUES (5)`, `INSERT INTO ch (id, r_id) VALUES (1, 5), (2, NULL)`},
			after: "CREATE TABLE p (id bigint PRIMARY KEY);\nCREATE TABLE q (id bigint PRIMARY KEY);\n" +
				"CREATE TABLE ch (id bigint PRIMARY KEY, r_id bigint, CONSTRAINT ch_r_fk FOREIGN KEY (r_id) REFERENCES p (id));\n",
			want: `unit 2 of 2 adds foreign key "ch_r_fk" to table "ch", which 1 row breaks, ` + constraintRefusal,
			keys: []string{"ch ch_r_fk NO ACTION"}, violates: `INSERT INTO ch (id, r_id) VALUES (3, 6)`, refused: ".*Foreign key `ch_r_fk` constraint violation .* in table `q`.*"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			skipBeforeConstraints(c)
			address := withSearchPath(c, freshDatabase(c))
			output, err := applySQL(c, address, test.before)
			c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
			execSQL(c, address, test.rows...)

			output, err = applySQL(c, address, test.after)
			c.Assert(err, qt.ErrorMatches, "exit status 2", qt.Commentf("%s", output))
			c.Assert(output, qt.Contains, "NOT_APPLIED (0 units confirmed applied): "+test.want)
			c.Assert(heldChecks(c, address), qt.DeepEquals, test.checks)
			c.Assert(heldForeignKeys(c, address), qt.DeepEquals, test.keys)
			_, err = connect(c, address).Exec(c.Context(), test.violates)
			c.Assert(err, qt.ErrorMatches, test.refused)
		})
	}
}

// A replacement the rows keep applies and converges, and so does a CHECK on a
// column the same plan adds with a default: the provider cannot count the rows
// before the column exists, and leaves the CHECK to Spanner, which fills the
// column in every row first.
func TestHostAppliesAConstraintChangeTheRowsKeepE2E(t *testing.T) {
	tests := []struct {
		name   string
		before string
		after  string
		plan   string
		checks []string
	}{
		{name: "a CHECK no row breaks",
			before: "CREATE TABLE t (id bigint PRIMARY KEY, n bigint, CONSTRAINT t_n_check CHECK (n > 0));\n",
			after:  "CREATE TABLE t (id bigint PRIMARY KEY, n bigint, CONSTRAINT t_n_check CHECK (n > 2));\n",
			plan:   `ALTER TABLE "t" DROP CONSTRAINT "t_n_check";` + "\n" + `ALTER TABLE "t" ADD CONSTRAINT "t_n_check" CHECK (n > 2);` + "\n",
			checks: []string{"t_n_check (n > '2'::bigint)"}},
		{name: "a CHECK on a column the plan adds with a default",
			before: "CREATE TABLE t (id bigint PRIMARY KEY, n bigint);\n",
			after:  "CREATE TABLE t (id bigint PRIMARY KEY, n bigint, m bigint DEFAULT 7, CONSTRAINT t_m_check CHECK (m > 0));\n",
			plan:   `ALTER TABLE "t" ADD COLUMN "m" bigint DEFAULT 7;` + "\n" + `ALTER TABLE "t" ADD CONSTRAINT "t_m_check" CHECK (m > 0);` + "\n",
			checks: []string{"t_m_check (m > '0'::bigint)"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			skipBeforeConstraints(c)
			address := withSearchPath(c, freshDatabase(c))
			output, err := applySQL(c, address, test.before)
			c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
			execSQL(c, address, `INSERT INTO t (id, n) VALUES (1, 3), (2, NULL)`)

			output, err = applySQL(c, address, test.after)
			c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
			c.Assert(output, qt.Contains, test.plan)
			c.Assert(heldChecks(c, address), qt.DeepEquals, test.checks)
			output, err = planSQL(c, address, test.after)
			c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
			c.Assert(output, qt.Contains, synced)
		})
	}
}

// Narrowing an enum column's values while a row holds a value the new list
// leaves out is refused before any statement runs, and the column keeps its
// CHECK; narrowing it to a list the rows keep applies. This is the measurement
// that found stokaro/ptah#4204 S93: the table was left without the CHECK.
func TestHostRefusesNarrowingAnEnumARowHoldsE2E(t *testing.T) {
	c := testcheck.New(t)
	skipBeforeEnumColumns(c)
	skipBeforeEnumCheckDecisions(c)
	address := withSearchPath(c, freshDatabase(c))
	held := filepath.Join(writeFiles(c, map[string]string{"schema.yaml": enumValues("a, b, c")}), "schema.yaml")
	output, err := hostCommand(c, "schema", "apply", "--db-url", address, "--schema-file", held, "--auto-approve")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	execSQL(c, address, `INSERT INTO t (id, k) VALUES (1, 'c'), (2, NULL)`)

	narrowed := filepath.Join(writeFiles(c, map[string]string{"schema.yaml": enumValues("a")}), "schema.yaml")
	output, err = hostCommand(c, "schema", "apply", "--db-url", address, "--schema-file", narrowed, "--auto-approve")
	c.Assert(err, qt.ErrorMatches, "exit status 2", qt.Commentf("%s", output))
	c.Assert(output, qt.Contains, `NOT_APPLIED (0 units confirmed applied): unit 2 of 2 adds CHECK "t_k_check" to table "t", which 1 row breaks, `+constraintRefusal)
	c.Assert(heldChecks(c, address), qt.DeepEquals, []string{"t_k_check (k = ANY (ARRAY['a'::text, 'b'::text, 'c'::text]))"})

	kept := filepath.Join(writeFiles(c, map[string]string{"schema.yaml": enumValues("a, c")}), "schema.yaml")
	output, err = hostCommand(c, "schema", "apply", "--db-url", address, "--schema-file", kept, "--auto-approve")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	c.Assert(heldChecks(c, address), qt.DeepEquals, []string{"t_k_check (k = ANY (ARRAY['a'::text, 'c'::text]))"})
}
