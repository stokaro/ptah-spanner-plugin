package commonobject_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-spanner-plugin/internal/commonobject"
	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// A query declared in another spelling than the one Spanner stores reads as
// the stored one: measured on the emulator behind PGAdapter 0.56.2,
// `select "id" from t` reads back as `SELECT id FROM t`.
func TestViewQuery(t *testing.T) {
	tests := []struct{ query, want string }{
		{query: "SELECT id, a FROM t", want: "SELECT id, a FROM t"},
		{query: "select   \"id\"  from   t;\n", want: "SELECT id FROM t"},
		{query: "SELECT id -- the key\n, a /* and a */ FROM t", want: "SELECT id, a FROM t"},
		{query: `SELECT "Mixed", "a b", "select" FROM "T"`, want: `SELECT "Mixed", "a b", "select" FROM "T"`},
		{query: "select count(*) as n from t where a in ( 1, 2 )", want: "SELECT count(*) AS n FROM t WHERE a IN (1, 2)"},
		{query: "SELECT t.id, 'it''s' FROM public.t", want: "SELECT t.id, 'it''s' FROM public.t"},
		{query: "SELECT id::text FROM t", want: "SELECT id::text FROM t"},
	}
	for _, test := range tests {
		t.Run(test.query, func(t *testing.T) {
			c := testcheck.New(t)
			c.Assert(commonobject.ViewQuery(test.query), qt.Equals, test.want)
		})
	}
}

// A prepared declaration keeps each object's identity and gives its query and
// type in Spanner's spelling.
func TestPrepare(t *testing.T) {
	c := testcheck.New(t)
	prepared := commonobject.Prepare(&protocol.DeclaredCommonObjects{
		Views:     []*protocol.SQLView{{Schema: "public", Name: "v", Body: "select \"id\" from t"}},
		Sequences: []*protocol.SQLSequence{{Schema: "public", Name: "s", AsType: "INT8", Start: new(int64(10))}, {Name: "u"}},
	})
	c.Assert(prepared.GetViews()[0].GetBody(), qt.Equals, "SELECT id FROM t")
	c.Assert(prepared.GetViews()[0].GetName(), qt.Equals, "v")
	c.Assert(prepared.GetSequences()[0].GetAsType(), qt.Equals, "bigint")
	c.Assert(prepared.GetSequences()[0].GetStart(), qt.Equals, int64(10))
	c.Assert(prepared.GetSequences()[1].GetAsType(), qt.Equals, "")
}

// What Spanner cannot hold is refused by name before a statement is planned.
func TestRefusals(t *testing.T) {
	c := testcheck.New(t)
	c.Assert(commonobject.Refusals(&protocol.DeclaredCommonObjects{
		Views: []*protocol.SQLView{
			{Name: "ok", Body: "SELECT 1"}, {Name: "m", Body: "SELECT 1", Materialized: true}, {Name: "w", Body: "SELECT 1", WithCheck: true},
			{Name: "o", Body: "SELECT 1", Attributes: []string{"security_barrier"}}, {Name: "c", Body: "SELECT 1", Comment: "x"},
		},
		Sequences: []*protocol.SQLSequence{
			{Name: "ok", Start: new(int64(5))}, {Name: "i", Increment: new(int64(2))}, {Name: "a", AsType: "integer"}, {Name: "o", OwnedBy: "t.id"},
		},
	}), qt.DeepEquals, []commonobject.Refusal{
		{Kind: "view", Object: `"m"`, Reason: `view "m": Spanner has no materialized view`},
		{Kind: "view", Object: `"w"`, Reason: `WITH CHECK OPTION on view "w": Spanner's views cannot be written through`},
		{Kind: "view", Object: `"o"`, Reason: `WITH (security_barrier) on view "o": a Spanner view keeps no option a read would report back`},
		{Kind: "view", Object: `"c"`, Reason: `the comment on view "c": Spanner's PostgreSQL interface stores no comment`},
		{Kind: "sequence", Object: `"i"`,
			Reason: `sequence "i" declares INCREMENT, MINVALUE, MAXVALUE, CACHE or CYCLE: a Spanner sequence is bit-reversed and takes START COUNTER WITH alone`},
		{Kind: "sequence", Object: `"a"`, Reason: `sequence "a" is declared AS integer: a Spanner sequence is a bigint`},
		{Kind: "sequence", Object: `"o"`, Reason: `sequence "o" is OWNED BY a column: Spanner keeps no owner of a sequence`},
	})
}

// Each change is the statement Ptah's built-in Spanner engine wrote.
func TestRender_HappyPath(t *testing.T) {
	tests := []struct {
		name   string
		change *protocol.CommonObjectChange
		want   string
	}{
		{name: "a view", change: &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_CreateView{CreateView: &protocol.ViewCreation{
			View: &protocol.SQLView{Schema: "public", Name: "v", Body: "SELECT id FROM t;"}}}}, want: "CREATE VIEW \"v\" AS\nSELECT id FROM t;"},
		{name: "a replaced view", change: &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_CreateView{CreateView: &protocol.ViewCreation{
			View: &protocol.SQLView{Schema: "app", Name: "v", Body: "SELECT 1"}, Replace: true}}}, want: "CREATE OR REPLACE VIEW \"app\".\"v\" AS\nSELECT 1;"},
		{name: "a dropped view", change: &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_DropView{DropView: &protocol.Reference{Kind: "view", Name: "v"}}},
			want: `DROP VIEW "v";`},
		{name: "a sequence", change: &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_CreateSequence{CreateSequence: &protocol.SQLSequence{Name: "s"}}},
			want: `CREATE SEQUENCE "s";`},
		{name: "a sequence with a counter start", change: &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_CreateSequence{
			CreateSequence: &protocol.SQLSequence{Name: "s", AsType: "bigint", Start: new(int64(1000))}}}, want: `CREATE SEQUENCE "s" START COUNTER WITH 1000;`},
		{name: "a restarted counter", change: &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_AlterSequence{AlterSequence: &protocol.SequenceAlteration{
			Sequence: &protocol.Reference{Kind: "sequence", Name: "s"}, Desired: &protocol.SQLSequence{Name: "s", Start: new(int64(5000))}, Changed: []string{"start"}}}},
			want: `ALTER SEQUENCE "s" RESTART COUNTER WITH 5000;`},
		{name: "a dropped sequence", change: &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_DropSequence{
			DropSequence: &protocol.Reference{Kind: "sequence", Schema: "app", Name: "s"}}}, want: `DROP SEQUENCE "app"."s";`},
		{name: "a schema, as Ptah's built-in Spanner engine created one", change: &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_CreateSchema{
			CreateSchema: &protocol.NamespaceCreation{Name: "app"}}}, want: `CREATE SCHEMA IF NOT EXISTS "app";`},
		{name: "a dropped schema, without CASCADE", change: &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_DropSchema{
			DropSchema: &protocol.Reference{Kind: "schema", Name: "app"}}}, want: `DROP SCHEMA "app";`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			statement, err := commonobject.Render(test.change)
			c.Assert(err, qt.IsNil)
			c.Assert(statement, qt.Equals, test.want)
		})
	}
}

func TestRender_FailurePath(t *testing.T) {
	tests := []struct {
		name    string
		change  *protocol.CommonObjectChange
		wantErr string
	}{
		{name: "a materialized view", change: &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_DropView{
			DropView: &protocol.Reference{Kind: "materialized_view", Name: "m"}}}, wantErr: `DROP of materialized_view "m": Spanner has no materialized view`},
		{name: "an increment change", change: &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_AlterSequence{AlterSequence: &protocol.SequenceAlteration{
			Sequence: &protocol.Reference{Kind: "sequence", Name: "s"}, Desired: &protocol.SQLSequence{Name: "s"}, Changed: []string{"increment"}}}},
			wantErr: `changing the increment of sequence "s": a Spanner sequence takes START COUNTER WITH alone`},
		{name: "a role", change: &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_DropRole{DropRole: &protocol.Reference{Kind: "role", Name: "r"}}},
			wantErr: `the provider writes schemas, views and sequences, and Spanner has no role`},
		{name: "a schema with a comment", change: &protocol.CommonObjectChange{Operation: &protocol.CommonObjectChange_CreateSchema{
			CreateSchema: &protocol.NamespaceCreation{Name: "app", Comment: "x"}}}, wantErr: `the comment on schema "app": Spanner's PostgreSQL interface stores no comment`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			statement, err := commonobject.Render(test.change)
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(statement, qt.Equals, "")
		})
	}
}
