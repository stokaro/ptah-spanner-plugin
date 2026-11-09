package rowdeletion_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-spanner-plugin/internal/rowdeletion"
	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

var table = &protocol.Reference{Engine: "spanner", Kind: "table", Schema: "public", Name: "posts"}

func value(representation protocol.ValueInput_Representation, payload string) *protocol.ValueInput {
	kind, digest := rowdeletion.Kind, ""
	if representation == protocol.ValueInput_CHANGE {
		kind = rowdeletion.ChangeKind
	}
	for _, model := range rowdeletion.Models() {
		if model.GetRepresentation() == representation {
			digest = model.GetDefinitionSha256()
		}
	}
	return &protocol.ValueInput{Id: "p", Representation: representation, Value: &protocol.Envelope{
		ProviderId: "github.com/stokaro/ptah-spanner-plugin", Kind: kind, SchemaVersion: 1, Encoding: "json", DefinitionSha256: digest,
		Owner: table, Payload: []byte(payload),
	}}
}

// A desired policy is one a declaration can ask for; an observed one keeps
// whatever spelling the server stored.
func TestNormalize_HappyPath(t *testing.T) {
	tests := []struct {
		name           string
		representation protocol.ValueInput_Representation
		payload, want  string
	}{
		{name: "desired", representation: protocol.ValueInput_DESIRED, payload: `{ "interval": "30 days", "column": "created_at" }`,
			want: `{"column":"created_at","interval":"30 days"}`},
		{name: "observed", representation: protocol.ValueInput_OBSERVED, payload: `{"column":"created_at","interval":"4 WEEKS 2 DAYS"}`,
			want: `{"column":"created_at","interval":"4 WEEKS 2 DAYS"}`},
		{name: "an observed spelling the provider cannot read", representation: protocol.ValueInput_OBSERVED, payload: `{"column":"c","interval":"P1D"}`,
			want: `{"column":"c","interval":"P1D"}`},
		{name: "a change", representation: protocol.ValueInput_CHANGE,
			payload: `{"after":{"column":"seen_at","interval":"60 days"},"before":{"column":"created_at","interval":"4 WEEKS 2 DAYS"}}`,
			want:    `{"after":{"column":"seen_at","interval":"60 days"},"before":{"column":"created_at","interval":"4 WEEKS 2 DAYS"}}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			normalized, err := rowdeletion.Normalize(value(test.representation, test.payload))
			c.Assert(err, qt.IsNil)
			c.Assert(string(normalized.GetCanonicalProjection()), qt.Equals, test.want)
		})
	}
}

// Every change decides which rows Spanner deletes, and says so.
func TestNormalize_ChangeSemantics(t *testing.T) {
	tests := []struct{ name, payload, reason string }{
		{name: "a new policy", payload: `{"after":{"column":"c","interval":"30 days"}}`,
			reason: "A row deletion policy decides which rows Spanner deletes; restoring a prior policy cannot recover deleted rows"},
		{name: "a removal", payload: `{"before":{"column":"c","interval":"30 days"}}`,
			reason: "Removes the row deletion policy; Spanner stops deleting rows, and the rows it deleted stay deleted"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			normalized, err := rowdeletion.Normalize(value(protocol.ValueInput_CHANGE, test.payload))
			c.Assert(err, qt.IsNil)
			c.Assert(normalized.GetChangeSemantics().GetImpact(), qt.Equals, protocol.ChangeSemantics_BEHAVIORAL)
			c.Assert(normalized.GetChangeSemantics().GetReason(), qt.Equals, test.reason)
			c.Assert(normalized.GetChangeSemantics().GetAccess(), qt.Equals, protocol.ChangeSemantics_UNCHANGED)
		})
	}
}

func TestNormalize_FailurePath(t *testing.T) {
	tests := []struct {
		name           string
		representation protocol.ValueInput_Representation
		payload        string
		wantErr        string
	}{
		{name: "an operation", representation: protocol.ValueInput_OPERATION, payload: `{"column":"c","interval":"1 day"}`,
			wantErr: `a row deletion policy does not match its model`},
		{name: "no interval", representation: protocol.ValueInput_DESIRED, payload: `{"column":"c"}`,
			wantErr: `a row deletion policy names its column and its interval: the policy names no interval`},
		{name: "an unknown field", representation: protocol.ValueInput_OBSERVED, payload: `{"column":"c","interval":"1 day","x":1}`,
			wantErr: `a row deletion policy is not one the provider reads: .*`},
		{name: "a declared interval Spanner refuses", representation: protocol.ValueInput_DESIRED, payload: `{"column":"c","interval":"12 hours"}`,
			wantErr: `the row deletion policy of table "posts": the interval is not a whole number of days, which Spanner refuses`},
		{name: "a change without operands", representation: protocol.ValueInput_CHANGE, payload: `{}`, wantErr: `a row deletion policy change needs an operand`},
		{name: "a change that changes nothing", representation: protocol.ValueInput_CHANGE,
			payload: `{"before":{"column":"c","interval":"4 WEEKS 2 DAYS"},"after":{"column":"c","interval":"30 days"}}`,
			wantErr: `a row deletion policy change of table "posts" changes nothing`},
		{name: "a change to a policy Spanner refuses", representation: protocol.ValueInput_CHANGE, payload: `{"after":{"column":"c","interval":"P1D"}}`,
			wantErr: `the row deletion policy of table "posts": the interval is not a number of months, weeks, days or hours, such as 30 days`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			normalized, err := rowdeletion.Normalize(value(test.representation, test.payload))
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(normalized, qt.IsNil)
		})
	}
}

// A desired policy lists the Go, YAML and SQL sources, which state the policy
// of every table they declare; HCL states it in a platform block only, which
// the provider claims at that site.
func TestModels_SourceFormats(t *testing.T) {
	c := testcheck.New(t)
	c.Assert(rowdeletion.DesiredModel().GetSourceFormats(), qt.DeepEquals,
		[]protocol.SourceDocument_Format{protocol.SourceDocument_GO_ANNOTATIONS, protocol.SourceDocument_YAML, protocol.SourceDocument_SQL})
	c.Assert(rowdeletion.ObservedModel().GetSourceFormats(), qt.HasLen, 0)
	c.Assert(rowdeletion.ObservedModel().GetDisplayName(), qt.Equals, "row deletion policies")
}
