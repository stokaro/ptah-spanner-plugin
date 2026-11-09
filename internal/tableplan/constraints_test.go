package tableplan_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// keyed is a kept table ch with columns id and p_id, the held constraints
// held, and the declared columns and constraints of desired.
func keyed(columns []*protocol.DeclaredColumn, constraints []*protocol.DeclaredConstraint, held ...*protocol.ObservedConstraint) *protocol.PlanningTable {
	return &protocol.PlanningTable{Subject: subject("ch"), Action: protocol.PlanningTable_ALTER,
		Desired: &protocol.DeclaredTable{Name: "ch", Columns: append([]*protocol.DeclaredColumn{{Name: "id", Type: "bigint", Primary: true}}, columns...),
			Constraints: constraints},
		Current: &protocol.ObservedTable{Name: "ch", Schema: "public", Columns: []*protocol.ObservedColumn{
			{Name: "id", DataType: "bigint", ColumnType: "bigint", IsNullable: "NO", PrimaryKey: true},
			{Name: "p_id", DataType: "bigint", ColumnType: "bigint", IsNullable: "YES"},
			{Name: "score", DataType: "bigint", ColumnType: "bigint", IsNullable: "NO", DefaultExpression: new("'0'::bigint")},
		}, Constraints: append([]*protocol.ObservedConstraint{{Name: "PK_ch", Type: "PRIMARY KEY", ColumnNames: []string{"id"}}}, held...)}}
}

// heldKey is the foreign key ch_p_fk on p_id that a read reports, with its
// delete rule.
func heldKey(deleteRule string) *protocol.ObservedConstraint {
	return &protocol.ObservedConstraint{Name: "ch_p_fk", TableName: "ch", Schema: "public", Type: "FOREIGN KEY", ColumnNames: []string{"p_id"},
		ForeignTable: new("p"), ForeignSchema: "public", ForeignColumns: []string{"id"}, DeleteRule: new(deleteRule), UpdateRule: new("NO ACTION")}
}

func heldCheck(name, clause string) *protocol.ObservedConstraint {
	return &protocol.ObservedConstraint{Name: name, TableName: "ch", Schema: "public", Type: "CHECK", CheckClause: new(clause)}
}

// keptColumns are ch's columns as declared, p_id with the column's own CHECK
// check, if any.
func keptColumns(check string) []*protocol.DeclaredColumn {
	return []*protocol.DeclaredColumn{{Name: "p_id", Type: "bigint", Nullable: true},
		{Name: "score", Type: "bigint", DefaultSet: true, DefaultValue: "0", Check: check}}
}

func declaredKey(onDelete string) *protocol.DeclaredConstraint {
	return &protocol.DeclaredConstraint{Name: "ch_p_fk", Type: "FOREIGN KEY", Columns: []string{"p_id"}, ForeignTable: "p", ForeignColumns: []string{"id"},
		OnDelete: onDelete}
}

// A kept table's foreign keys and CHECKs are compared with the ones it holds,
// as Ptah's built-in Spanner engine planned them: one declared and not held
// is added, one held and not declared is dropped, and a changed one is
// dropped and added again. A CHECK Spanner keeps in its own form is the
// declared one, which the built-in replaced on every run (stokaro/ptah#4204
// S1), and a drop carries no IF EXISTS, which Spanner refuses (S2).
func TestPlan_ChangesConstraintsOfAKeptTable(t *testing.T) {
	tests := []struct {
		name        string
		table       *protocol.PlanningTable
		want        []string
		wantOrdered [][2]int
	}{
		{name: "a foreign key added", table: keyed(keptColumns(""), []*protocol.DeclaredConstraint{declaredKey("")}),
			want: []string{`ALTER TABLE "ch" ADD CONSTRAINT "ch_p_fk" FOREIGN KEY ("p_id") REFERENCES "p"("id");`}},
		{name: "a foreign key dropped", table: keyed(keptColumns(""), nil, heldKey("NO ACTION")),
			want: []string{`ALTER TABLE "ch" DROP CONSTRAINT "ch_p_fk";`}},
		{name: "a foreign key that changes its rule", table: keyed(keptColumns(""), []*protocol.DeclaredConstraint{declaredKey("CASCADE")}, heldKey("NO ACTION")),
			want: []string{`ALTER TABLE "ch" DROP CONSTRAINT "ch_p_fk";`,
				`ALTER TABLE "ch" ADD CONSTRAINT "ch_p_fk" FOREIGN KEY ("p_id") REFERENCES "p"("id") ON DELETE CASCADE;`},
			wantOrdered: [][2]int{{0, 1}}},
		{name: "a foreign key on the column", table: keyed([]*protocol.DeclaredColumn{{Name: "p_id", Type: "bigint", Nullable: true, ForeignReference: "p(id)",
			OnDelete: "CASCADE"}, {Name: "score", Type: "bigint", DefaultSet: true, DefaultValue: "0"}}, nil),
			want: []string{`ALTER TABLE "ch" ADD CONSTRAINT "fk_ch_p_id" FOREIGN KEY ("p_id") REFERENCES "p"("id") ON DELETE CASCADE;`}},
		{name: "a foreign key kept with its default rule spelled", table: keyed(keptColumns(""), []*protocol.DeclaredConstraint{declaredKey("NO ACTION")},
			heldKey("NO ACTION")), want: make([]string, 0)},
		{name: "a CHECK in Spanner's form kept", table: keyed(keptColumns("score >= 0"), nil, heldCheck("ch_score_check", "(score >= '0'::bigint)")), want: make([]string, 0)},
		{name: "a CHECK added", table: keyed(keptColumns("score >= 0"), nil),
			want: []string{`ALTER TABLE "ch" ADD CONSTRAINT "ch_score_check" CHECK (score >= 0);`}},
		{name: "a CHECK changed", table: keyed(keptColumns("score >= 1"), nil, heldCheck("ch_score_check", "(score >= '0'::bigint)")),
			want:        []string{`ALTER TABLE "ch" DROP CONSTRAINT "ch_score_check";`, `ALTER TABLE "ch" ADD CONSTRAINT "ch_score_check" CHECK (score >= 1);`},
			wantOrdered: [][2]int{{0, 1}}},
		{name: "a table CHECK dropped", table: keyed(keptColumns(""), nil, heldCheck("ch_positive", "(score > '0'::bigint)")),
			want: []string{`ALTER TABLE "ch" DROP CONSTRAINT "ch_positive";`}},
		{name: "a table CHECK in Spanner's form kept", table: keyed(keptColumns(""), []*protocol.DeclaredConstraint{
			{Name: "ch_range", Type: "CHECK", CheckExpression: "score BETWEEN 0 AND 100"}}, heldCheck("ch_range", "((score >= '0'::bigint) AND (score <= '100'::bigint))")), want: make([]string, 0)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			steps, dependencies, result := plan(c, request([]*protocol.PlanningTable{test.table}))
			c.Assert(statements(steps), qt.DeepEquals, test.want)
			var ordered [][2]string
			for _, pair := range test.wantOrdered {
				ordered = append(ordered, [2]string{steps[pair[0]].Name, steps[pair[1]].Name})
			}
			c.Assert(dependencies, qt.DeepEquals, ordered)
			c.Assert(result.GetParents(), qt.HasLen, 1)
			c.Assert(result.GetParents()[0].GetReceipt().GetSteps(), qt.HasLen, len(test.want))
		})
	}
}

// A column added with a CHECK and a foreign key is added bare, and its
// constraints follow it in name order, as the built-in added them: Spanner
// takes no constraint in ADD COLUMN.
func TestPlan_AddsAColumnBeforeItsConstraints(t *testing.T) {
	c := testcheck.New(t)
	table := keyed(append(keptColumns(""), &protocol.DeclaredColumn{Name: "q_id", Type: "bigint", Nullable: true, Check: "q_id > 0", ForeignReference: "q(id)"}), nil)
	r := request([]*protocol.PlanningTable{table},
		&protocol.CommonPlanningStep{Id: &protocol.StepIdentity{Owner: "ptah.run/provider-common", Name: "common/000000"}, Parent: subject("ch"),
			AddedColumn: &protocol.PlanningColumn{Name: "q_id", Type: "bigint"},
			Metadata: &protocol.PlanningMetadata{Effects: []*protocol.PlanningEffect{effect(subject("ch"), protocol.PlanningEffect_READ),
				effect(child("ch", "column", "q_id"), protocol.PlanningEffect_CREATE)}}})
	steps, dependencies, _ := plan(c, r)
	c.Assert(statements(steps), qt.DeepEquals, []string{
		`ALTER TABLE "ch" ADD COLUMN "q_id" bigint;`,
		`ALTER TABLE "ch" ADD CONSTRAINT "ch_q_id_check" CHECK (q_id > 0);`,
		`ALTER TABLE "ch" ADD CONSTRAINT "fk_ch_q_id" FOREIGN KEY ("q_id") REFERENCES "q"("id");`,
	})
	c.Assert(dependencies, qt.DeepEquals, [][2]string{{steps[0].Name, steps[1].Name}, {steps[0].Name, steps[2].Name}})
}

// A constraint is dropped before the column it names: Spanner refuses to drop
// a column a constraint depends on.
func TestPlan_DropsConstraintsBeforeTheirColumns(t *testing.T) {
	c := testcheck.New(t)
	table := keyed([]*protocol.DeclaredColumn{{Name: "score", Type: "bigint", DefaultSet: true, DefaultValue: "0"}}, nil, heldKey("NO ACTION"))
	r := request([]*protocol.PlanningTable{table},
		common("common/000000", "ch", effect(subject("ch"), protocol.PlanningEffect_READ), effect(child("ch", "column", "p_id"), protocol.PlanningEffect_DROP)))
	steps, dependencies, _ := plan(c, r)
	c.Assert(statements(steps), qt.DeepEquals, []string{`ALTER TABLE "ch" DROP COLUMN "p_id";`, `ALTER TABLE "ch" DROP CONSTRAINT "ch_p_fk";`})
	c.Assert(dependencies, qt.DeepEquals, [][2]string{{steps[1].Name, steps[0].Name}})
}

// dropped is a removed table whose read reports its foreign keys.
func dropped(name string, keys ...*protocol.ObservedConstraint) *protocol.PlanningTable {
	return &protocol.PlanningTable{Subject: subject(name), Action: protocol.PlanningTable_DROP, Current: &protocol.ObservedTable{Name: name, Schema: "public",
		Columns:     []*protocol.ObservedColumn{{Name: "id", ColumnType: "bigint", IsNullable: "NO"}, {Name: "ref", ColumnType: "bigint", IsNullable: "YES"}},
		Constraints: append(keys, &protocol.ObservedConstraint{Name: name + "_check", TableName: name, Schema: "public", Type: "CHECK", CheckClause: new("(id > '0'::bigint)")})}}
}

func foreignKey(table, name, references string) *protocol.ObservedConstraint {
	return &protocol.ObservedConstraint{Name: name, TableName: table, Schema: "public", Type: "FOREIGN KEY", ColumnNames: []string{"ref"},
		ForeignTable: new(references), ForeignSchema: "public", ForeignColumns: []string{"id"}}
}

// A removed table's foreign keys are dropped before the table and before the
// table each references, since Spanner refuses to drop a referenced table;
// two tables that reference each other lose both keys first. Ptah's built-in
// Spanner engine dropped them so, and a down file of a schema with a foreign
// key needs it (stokaro/ptah#4204 S41). A CHECK leaves with its table.
func TestPlan_DropsForeignKeysBeforeTheirTables(t *testing.T) {
	c := testcheck.New(t)
	r := request([]*protocol.PlanningTable{
		dropped("ch", foreignKey("ch", "ch_p_fk", "p")), dropped("p"),
		dropped("x", foreignKey("x", "x_y_fk", "y")), dropped("y", foreignKey("y", "y_x_fk", "x")),
	}, common("common/000000", "ch", effect(subject("ch"), protocol.PlanningEffect_DROP)), common("common/000001", "p", effect(subject("p"), protocol.PlanningEffect_DROP)),
		common("common/000002", "x", effect(subject("x"), protocol.PlanningEffect_DROP)), common("common/000003", "y", effect(subject("y"), protocol.PlanningEffect_DROP)))
	steps, dependencies, _ := plan(c, r)
	c.Assert(statements(steps), qt.DeepEquals, []string{
		`DROP TABLE "ch";`, `ALTER TABLE "ch" DROP CONSTRAINT "ch_p_fk";`, `DROP TABLE "p";`,
		`DROP TABLE "x";`, `ALTER TABLE "x" DROP CONSTRAINT "x_y_fk";`, `DROP TABLE "y";`, `ALTER TABLE "y" DROP CONSTRAINT "y_x_fk";`,
	})
	c.Assert(dependencies, qt.DeepEquals, [][2]string{
		{steps[1].Name, steps[0].Name}, {steps[1].Name, steps[2].Name},
		{steps[4].Name, steps[3].Name}, {steps[4].Name, steps[5].Name},
		{steps[6].Name, steps[5].Name}, {steps[6].Name, steps[3].Name},
	})
}

// A kept table that stops referencing a removed table drops its key before
// the removal, and one that starts referencing a created table adds its key
// after the creation.
func TestPlan_OrdersKeptKeysAroundOtherTables(t *testing.T) {
	c := testcheck.New(t)
	created := &protocol.PlanningTable{Subject: subject("q"), Action: protocol.PlanningTable_CREATE, Desired: &protocol.DeclaredTable{Name: "q",
		Columns: []*protocol.DeclaredColumn{{Name: "id", Type: "bigint", Primary: true}}}}
	kept := keyed(keptColumns(""), []*protocol.DeclaredConstraint{{Name: "ch_q_fk", Type: "FOREIGN KEY", Columns: []string{"p_id"}, ForeignTable: "q",
		ForeignColumns: []string{"id"}}}, heldKey("NO ACTION"))
	r := request([]*protocol.PlanningTable{created, kept, dropped("p")},
		common("common/000000", "q", effect(subject("q"), protocol.PlanningEffect_CREATE)), common("common/000001", "p", effect(subject("p"), protocol.PlanningEffect_DROP)))
	steps, dependencies, _ := plan(c, r)
	c.Assert(statements(steps), qt.DeepEquals, []string{
		"CREATE TABLE \"q\" (\n  \"id\" bigint PRIMARY KEY NOT NULL\n);",
		`ALTER TABLE "ch" DROP CONSTRAINT "ch_p_fk";`,
		`ALTER TABLE "ch" ADD CONSTRAINT "ch_q_fk" FOREIGN KEY ("p_id") REFERENCES "q"("id");`,
		`DROP TABLE "p";`,
	})
	c.Assert(dependencies, qt.DeepEquals, [][2]string{{steps[0].Name, steps[2].Name}, {steps[1].Name, steps[3].Name}})
}

// A declared foreign key Spanner cannot hold is refused by name on a kept
// table as on a created one.
func TestPlan_RefusesAForeignKeySpannerCannotHold(t *testing.T) {
	c := testcheck.New(t)
	r := request([]*protocol.PlanningTable{keyed(keptColumns(""), []*protocol.DeclaredConstraint{declaredKey("SET NULL")})})
	steps, _, result := plan(c, r)
	c.Assert(steps, qt.HasLen, 0)
	c.Assert(result.GetDiagnostics(), qt.HasLen, 1)
	c.Assert(result.GetDiagnostics()[0].GetMessage(), qt.Equals, `foreign key "ch_p_fk" of table "ch" has ON DELETE SET NULL; Spanner has NO ACTION and CASCADE only`)
}

// A host that lowers an enum column for Spanner hands its value list as the
// column's own CHECK too; the list is written once, as the built-in wrote it.
func TestPlan_WritesAnEnumListOnce(t *testing.T) {
	c := testcheck.New(t)
	desired := &protocol.DeclaredTable{Name: "authors", Columns: []*protocol.DeclaredColumn{
		{Name: "id", Type: "bigint", Primary: true},
		{Name: "kind", Type: "TEXT", DefaultSet: true, DefaultValue: "a", EnumValues: []string{"a", "b"}, Check: "kind IN ('a', 'b')"},
		{Name: "mood", Type: "TEXT", EnumValues: []string{"x"}, Check: "mood <> ''"},
	}}
	r := request([]*protocol.PlanningTable{{Subject: subject("authors"), Action: protocol.PlanningTable_CREATE, Desired: desired}},
		common("common/000000", "authors", effect(subject("authors"), protocol.PlanningEffect_CREATE)))
	steps, _, _ := plan(c, r)
	c.Assert(statements(steps), qt.DeepEquals, []string{"CREATE TABLE \"authors\" (\n  \"id\" bigint PRIMARY KEY NOT NULL,\n" +
		"  \"kind\" TEXT NOT NULL DEFAULT 'a' CONSTRAINT \"authors_kind_check\" CHECK (kind IN ('a', 'b')),\n" +
		"  \"mood\" TEXT NOT NULL CONSTRAINT \"authors_mood_check\" CHECK ((mood <> '') AND mood IN ('x'))\n);"})
}

// A removed table's foreign key is dropped after every table the plan
// creates, so a creation Spanner refuses stops the plan before it drops
// anything, as the built-in ordered it.
func TestPlan_CreatesTablesBeforeDroppingKeys(t *testing.T) {
	c := testcheck.New(t)
	created := &protocol.PlanningTable{Subject: subject("u"), Action: protocol.PlanningTable_CREATE, Desired: &protocol.DeclaredTable{Name: "u",
		Columns: []*protocol.DeclaredColumn{{Name: "id", Type: "bigint", Primary: true}}}}
	r := request([]*protocol.PlanningTable{created, dropped("ch", foreignKey("ch", "ch_p_fk", "p")), dropped("p")},
		common("common/000000", "u", effect(subject("u"), protocol.PlanningEffect_CREATE)),
		common("common/000001", "ch", effect(subject("ch"), protocol.PlanningEffect_DROP)), common("common/000002", "p", effect(subject("p"), protocol.PlanningEffect_DROP)))
	steps, dependencies, _ := plan(c, r)
	c.Assert(statements(steps), qt.DeepEquals, []string{
		"CREATE TABLE \"u\" (\n  \"id\" bigint PRIMARY KEY NOT NULL\n);", `DROP TABLE "ch";`, `ALTER TABLE "ch" DROP CONSTRAINT "ch_p_fk";`, `DROP TABLE "p";`,
	})
	c.Assert(dependencies, qt.DeepEquals, [][2]string{{steps[0].Name, steps[2].Name}, {steps[2].Name, steps[1].Name}, {steps[2].Name, steps[3].Name}})
}
