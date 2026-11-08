//go:build integration

package integration_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// Spanner checks a constraint added to a table against every row, and refuses
// it when a row breaks it, but only when the statement runs, after the
// statements before it have committed. The tests here run the provider's
// executor against the emulator and show the check it makes before the first
// unit, and where it leaves the decision to Spanner.

// checkedRows is the table the CHECK tests start from: a CHECK every row
// keeps, a row the narrowed CHECK breaks, and a null, which a CHECK takes.
var checkedRows = []string{
	`CREATE TABLE items (id bigint PRIMARY KEY, kind text, CONSTRAINT items_kind_check CHECK (kind IN ('a', 'b', 'c')))`,
	`INSERT INTO items (id, kind) VALUES (1, 'c'), (2, NULL)`,
}

// keyedRows are the tables the foreign key tests start from: two rows of ch
// reference no row of p, and one holds a null key.
var keyedRows = []string{
	`CREATE TABLE p (id bigint PRIMARY KEY)`, `CREATE TABLE ch (id bigint PRIMARY KEY, p_id bigint)`,
	`INSERT INTO ch (id, p_id) VALUES (1, 99), (2, NULL), (3, 98)`,
}

// heldCheck is the CHECK checkedRows creates, as Spanner keeps it.
const heldCheck = "items_kind_check (kind = ANY (ARRAY['a'::text, 'b'::text, 'c'::text]))"

// A request that adds, after its first unit, a CHECK or a foreign key rows of
// the table break is refused before its first unit runs, with a reason that
// names the unit, the constraint, the table and how many rows break it.
// Nothing of the request applies: a replaced CHECK stays, and a table the
// request creates first is not created. A constraint the first unit adds is
// left to Spanner, whose refusal applies nothing either.
func TestExecutionSubprocess_RefusesConstraintsRowsBreak(t *testing.T) {
	tests := []struct {
		name    string
		setup   []string
		units   []string
		wantErr string
		tables  []string
		checks  []string
	}{
		{name: "a narrowed CHECK", setup: checkedRows,
			units: []string{`ALTER TABLE "items" DROP CONSTRAINT "items_kind_check"`, `ALTER TABLE "items" ADD CONSTRAINT "items_kind_check" CHECK (kind IN ('a'))`},
			wantErr: `provider execution: NOT_APPLIED \(0 units confirmed applied\): unit 2 of 2 adds CHECK "items_kind_check" to table "items", which 1 row breaks, ` +
				`so no unit ran: Spanner refuses to add a constraint a row breaks; update or delete the rows that break it before adding the constraint`,
			tables: []string{"items"}, checks: []string{heldCheck}},
		{name: "a foreign key after a created table", setup: keyedRows,
			units: []string{`CREATE TABLE other (id bigint PRIMARY KEY)`, `ALTER TABLE "ch" ADD CONSTRAINT "ch_p_fk" FOREIGN KEY ("p_id") REFERENCES "p"("id")`},
			wantErr: `provider execution: NOT_APPLIED \(0 units confirmed applied\): unit 2 of 2 adds foreign key "ch_p_fk" to table "ch", which 2 rows break, ` +
				`so no unit ran: Spanner refuses to add a constraint a row breaks; update or delete the rows that break it, or add the rows they reference, before adding the constraint`,
			tables: []string{"ch", "p"}},
		{name: "a CHECK the first unit adds", setup: checkedRows,
			units:   []string{`ALTER TABLE items ADD CONSTRAINT items_a CHECK (kind = 'a')`, `CREATE TABLE other (id bigint PRIMARY KEY)`},
			wantErr: `provider execution: NOT_APPLIED \(0 units confirmed applied\): Spanner refused unit 1 of 2 with SQLSTATE P0001`,
			tables:  []string{"items"}, checks: []string{heldCheck}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			address := freshDatabase(c)
			execSQL(c, address, test.setup...)
			client := startProvider(c)
			session := openSession(c, client, address)
			execution, err := client.Execute(c.Context(), &protocol.ExecuteRequest{SessionId: session.GetId(), RequestId: "e1", Units: sqlUnits(test.units...)})
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(execution.GetOutcome(), qt.Equals, protocol.Execution_NOT_APPLIED)
			c.Assert(execution.GetAppliedCount(), qt.Equals, uint32(0))
			c.Assert(tables(c, address), qt.DeepEquals, test.tables)
			c.Assert(heldChecks(c, address), qt.DeepEquals, test.checks)
		})
	}
}

// The executor counts the rows before the request runs, so it leaves an
// addition to Spanner when a unit before it may change the rows or the
// columns the constraint reads. Each request here applies: a count before the
// request would have refused the first, which an UPDATE makes true, and could
// not have read the column the second adds, which Spanner fills with its
// default. A replacement no row breaks applies too.
func TestExecutionSubprocess_RunsConstraintsTheRowsKeep(t *testing.T) {
	tests := []struct {
		name   string
		units  []string
		checks []string
	}{
		{name: "rows changed before the replacement",
			units: []string{`UPDATE items SET kind = 'a' WHERE id = 1`, `ALTER TABLE items DROP CONSTRAINT items_kind_check`,
				`ALTER TABLE items ADD CONSTRAINT items_kind_check CHECK (kind IN ('a'))`},
			checks: []string{"items_kind_check (kind = 'a'::text)"}},
		{name: "a column the CHECK reads added with a default",
			units:  []string{`ALTER TABLE items ADD COLUMN n bigint DEFAULT 7`, `ALTER TABLE items ADD CONSTRAINT items_n_check CHECK (n > 0)`},
			checks: []string{heldCheck, "items_n_check (n > '0'::bigint)"}},
		{name: "a replacement no row breaks",
			units:  []string{`ALTER TABLE items DROP CONSTRAINT items_kind_check`, `ALTER TABLE items ADD CONSTRAINT items_kind_check CHECK (kind IN ('a', 'c'))`},
			checks: []string{"items_kind_check (kind = ANY (ARRAY['a'::text, 'c'::text]))"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			address := freshDatabase(c)
			execSQL(c, address, checkedRows...)
			client := startProvider(c)
			session := openSession(c, client, address)
			execution, err := client.Execute(c.Context(), &protocol.ExecuteRequest{SessionId: session.GetId(), RequestId: "e1", Units: sqlUnits(test.units...)})
			c.Assert(err, qt.IsNil)
			c.Assert(execution.GetOutcome(), qt.Equals, protocol.Execution_APPLIED)
			c.Assert(execution.GetAppliedCount(), qt.Equals, uint32(len(test.units))) // #nosec G115 -- a test request holds three units at most
			c.Assert(heldChecks(c, address), qt.DeepEquals, test.checks)
		})
	}
}
