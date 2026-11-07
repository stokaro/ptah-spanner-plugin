package policysource_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-spanner-plugin/internal/policysource"
	"github.com/stokaro/ptah-spanner-plugin/internal/rowdeletion"
	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

var events = &protocol.Reference{Engine: "spanner", Kind: "table", Schema: "public", Name: "events"}

// document is one source document of format with a binding of the events
// table and a fragment of claim whose mapping holds entries.
func document(format protocol.SourceDocument_Format, entries map[string]*protocol.SourceValue) (*protocol.SourceDocument, *protocol.SourceFragment) {
	fragment := &protocol.SourceFragment{Id: "f", BindingId: "b"}
	switch format {
	case protocol.SourceDocument_GO_ANNOTATIONS:
		fragment.GoDirective = policysource.GoDirectives()[0]
	case protocol.SourceDocument_YAML:
		fragment.YamlKey = policysource.YAMLKeys()[0]
	default:
		fragment.HclBlock = policysource.HCLPlatformBlock()
	}
	mapping := &protocol.SourceMapping{}
	for _, key := range []string{"platform.spanner.row_deletion_column", "platform.spanner.row_deletion_interval", "row_deletion_column",
		"row_deletion_interval", "row_deletion_unit", "Row_Deletion_Column", "row_deletion_columns", "platform.spanner.row_deletion_unit"} {
		if entry, ok := entries[key]; ok {
			mapping.Entries = append(mapping.Entries, &protocol.SourceEntry{Key: key, Value: entry})
		}
	}
	fragment.Content = &protocol.SourceFragment_Value{Value: &protocol.SourceValue{Value: &protocol.SourceValue_Mapping{Mapping: mapping}}}
	return &protocol.SourceDocument{Id: "d", Format: format, Bindings: []*protocol.SourceBinding{{Id: "b", Object: events}},
		Fragments: []*protocol.SourceFragment{fragment}}, fragment
}

func text(value string) *protocol.SourceValue {
	return &protocol.SourceValue{Value: &protocol.SourceValue_Text{Text: value}}
}

// Each format spells the policy as Ptah's built-in Spanner engine read it:
// prefixed attributes of a Go table directive, and the unprefixed keys of a
// YAML group and of HCL overrides.
func TestDecode_HappyPath(t *testing.T) {
	tests := []struct {
		name    string
		format  protocol.SourceDocument_Format
		entries map[string]*protocol.SourceValue
	}{
		{name: "Go", format: protocol.SourceDocument_GO_ANNOTATIONS,
			entries: map[string]*protocol.SourceValue{"platform.spanner.row_deletion_column": text("created_at"), "platform.spanner.row_deletion_interval": text(" 30 days ")}},
		{name: "YAML", format: protocol.SourceDocument_YAML,
			entries: map[string]*protocol.SourceValue{"row_deletion_column": text("created_at"), "row_deletion_interval": text("30 days")}},
		{name: "HCL", format: protocol.SourceDocument_HCL,
			entries: map[string]*protocol.SourceValue{"row_deletion_column": text("created_at"), "row_deletion_interval": text("30 days")}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			source, fragment := document(test.format, test.entries)
			c.Assert(policysource.Owns(fragment), qt.IsTrue)
			table, policy, err := policysource.Decode(source, fragment)
			c.Assert(err, qt.IsNil)
			c.Assert(table.GetName(), qt.Equals, "events")
			c.Assert(policy, qt.Equals, rowdeletion.Policy{Column: "created_at", Interval: "30 days"})
		})
	}
}

// A property the policy does not take, an empty one, and a policy Spanner
// refuses are refused with the table's name. The unit is claimed in order to
// be refused by name: Spanner counts from a timestamp column.
func TestDecode_FailurePath(t *testing.T) {
	tests := []struct {
		name    string
		format  protocol.SourceDocument_Format
		entries map[string]*protocol.SourceValue
		wantErr string
	}{
		{name: "a unit", format: protocol.SourceDocument_GO_ANNOTATIONS,
			entries: map[string]*protocol.SourceValue{"platform.spanner.row_deletion_column": text("c"), "platform.spanner.row_deletion_unit": text("seconds")},
			wantErr: `table "events": row_deletion_unit: a Spanner row deletion policy counts from a timestamptz column and takes no unit`},
		{name: "a miscased property", format: protocol.SourceDocument_YAML, entries: map[string]*protocol.SourceValue{"Row_Deletion_Column": text("c")},
			wantErr: `table "events": unknown row deletion property "Row_Deletion_Column": property names are lower case, as "row_deletion_column"`},
		{name: "an unknown property", format: protocol.SourceDocument_YAML, entries: map[string]*protocol.SourceValue{"row_deletion_columns": text("c")},
			wantErr: `table "events": unknown row deletion property "row_deletion_columns": the policy takes row_deletion_column, row_deletion_interval`},
		{name: "an empty value", format: protocol.SourceDocument_HCL,
			entries: map[string]*protocol.SourceValue{"row_deletion_column": text("c"), "row_deletion_interval": text(" ")},
			wantErr: `table "events": row_deletion_interval is empty; remove it to leave the policy undeclared`},
		{name: "no interval", format: protocol.SourceDocument_HCL, entries: map[string]*protocol.SourceValue{"row_deletion_column": text("c")},
			wantErr: `table "events": a row deletion policy needs row_deletion_interval, the interval after which Spanner deletes a row`},
		{name: "no column", format: protocol.SourceDocument_YAML, entries: map[string]*protocol.SourceValue{"row_deletion_interval": text("30 days")},
			wantErr: `table "events": a row deletion policy needs row_deletion_column, the column its interval counts from`},
		{name: "part of a day", format: protocol.SourceDocument_YAML,
			entries: map[string]*protocol.SourceValue{"row_deletion_column": text("c"), "row_deletion_interval": text("1 hour")},
			wantErr: `the row deletion policy of table "events": the interval is not a whole number of days, which Spanner refuses`},
		{name: "a number", format: protocol.SourceDocument_YAML,
			entries: map[string]*protocol.SourceValue{"row_deletion_column": text("c"), "row_deletion_interval": {Value: &protocol.SourceValue_Number{Number: "30"}}},
			wantErr: `table "events": row_deletion_interval is not text`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			source, fragment := document(test.format, test.entries)
			table, policy, err := policysource.Decode(source, fragment)
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(table, qt.IsNil)
			c.Assert(policy, qt.Equals, rowdeletion.Policy{})
		})
	}
}

// A fragment whose binding names no table of the document is refused.
func TestDecode_RefusesAnUnboundFragment(t *testing.T) {
	c := testcheck.New(t)
	source, fragment := document(protocol.SourceDocument_YAML, map[string]*protocol.SourceValue{"row_deletion_column": text("c")})
	fragment.BindingId = "other"
	table, _, err := policysource.Decode(source, fragment)
	c.Assert(err, qt.ErrorMatches, `a row deletion policy belongs to a table the source declares`)
	c.Assert(table, qt.IsNil)
}

// The written mapping reads back as the same policy in each format.
func TestContent_ReadsBack(t *testing.T) {
	for _, format := range []protocol.SourceDocument_Format{protocol.SourceDocument_GO_ANNOTATIONS, protocol.SourceDocument_YAML, protocol.SourceDocument_HCL} {
		t.Run(format.String(), func(t *testing.T) {
			c := testcheck.New(t)
			source, fragment := document(format, nil)
			fragment.Content = policysource.Content(format, rowdeletion.Policy{Column: "created_at", Interval: "4 WEEKS 2 DAYS"})
			_, policy, err := policysource.Decode(source, fragment)
			c.Assert(err, qt.IsNil)
			c.Assert(policy, qt.Equals, rowdeletion.Policy{Column: "created_at", Interval: "4 WEEKS 2 DAYS"})
		})
	}
}

// The claims are the spelling of Ptah's built-in Spanner engine.
func TestClaims(t *testing.T) {
	c := testcheck.New(t)
	c.Assert(policysource.YAMLKeys()[0].GetKey(), qt.Equals, "platform.spanner")
	c.Assert(policysource.GoDirectives()[0].GetDirective(), qt.Equals, "ptah:schema:table")
	c.Assert(policysource.GoDirectives()[0].GetAttributes(), qt.DeepEquals,
		[]string{"platform.spanner.row_deletion_column", "platform.spanner.row_deletion_interval", "platform.spanner.row_deletion_unit"})
	c.Assert(policysource.HCLPlatformBlock().GetType(), qt.Equals, "spanner")
	c.Assert(policysource.HCLPlatformBlock().GetSite(), qt.Equals, protocol.HCLSourceBlock_TABLE_PLATFORM)
}

// sqlClause is a SQL document whose table-clause fragment carries text, bound
// to the events table.
func sqlClause(text string) (*protocol.SourceDocument, *protocol.SourceFragment) {
	fragment := &protocol.SourceFragment{Id: "f", BindingId: "b", SqlTableClause: policysource.SQLClause(), Selector: policysource.SQLClause().GetSelector(),
		Subject: events, Content: &protocol.SourceFragment_Sql{Sql: text}}
	return &protocol.SourceDocument{Id: "d", Format: protocol.SourceDocument_SQL, Bindings: []*protocol.SourceBinding{{Id: "b", Object: events}},
		Fragments: []*protocol.SourceFragment{fragment}}, fragment
}

// A SQL document's TTL clause, claimed by its opening word, declares the
// policy of the table it belongs to, with the column as written.
func TestDecode_SQLClause_HappyPath(t *testing.T) {
	c := testcheck.New(t)
	document, fragment := sqlClause("TTL INTERVAL '30 days' ON CreatedAt")
	c.Assert(policysource.Owns(fragment), qt.IsTrue)
	table, policy, err := policysource.Decode(document, fragment)
	c.Assert(err, qt.IsNil)
	c.Assert(table.GetName(), qt.Equals, "events")
	c.Assert(policy, qt.Equals, rowdeletion.Policy{Column: "CreatedAt", Interval: "30 days"})
}

// A TTL clause Spanner would not take is refused with the table's name: a
// malformed clause, and an interval Spanner refuses.
func TestDecode_SQLClause_FailurePath(t *testing.T) {
	tests := []struct{ name, clause, wantErr string }{
		{name: "two words after ON", clause: "TTL INTERVAL '30 days' ON a b",
			wantErr: `table "events": the row deletion policy names something other than one column after ON`},
		{name: "an interval Spanner refuses", clause: "TTL INTERVAL 'soon' ON created_at", wantErr: `the row deletion policy of table "events": .*`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			document, fragment := sqlClause(test.clause)
			table, policy, err := policysource.Decode(document, fragment)
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(table, qt.IsNil)
			c.Assert(policy, qt.Equals, rowdeletion.Policy{})
		})
	}
}

// The provider claims the TTL clause of CREATE TABLE for the row deletion
// policy, and a claim of another word is not its own.
func TestSQLClause(t *testing.T) {
	c := testcheck.New(t)
	c.Assert(policysource.SQLClause().GetWords(), qt.DeepEquals, []string{"TTL"})
	c.Assert(policysource.SQLClause().GetModels(), qt.DeepEquals, []string{rowdeletion.Kind})
	other := &protocol.SourceFragment{SqlTableClause: &protocol.SQLTableClause{Words: []string{"PARTITION"}, Selector: "x", Models: []string{rowdeletion.Kind}}}
	c.Assert(policysource.Owns(other), qt.IsFalse)
}
