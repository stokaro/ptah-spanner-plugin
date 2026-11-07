package rowdeletion_test

import (
	"testing"

	qt "github.com/frankban/quicktest"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-spanner-plugin/internal/rowdeletion"
	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

func policy(representation protocol.ValueInput_Representation, payload string) *protocol.ValueInput {
	var digest string
	for _, model := range rowdeletion.Models() {
		if model.GetRepresentation() == representation {
			digest = model.GetDefinitionSha256()
		}
	}
	return &protocol.ValueInput{Id: "p", Representation: representation, Value: &protocol.Envelope{
		ProviderId: "github.com/stokaro/ptah-spanner-plugin", Kind: rowdeletion.Kind, SchemaVersion: 1, Encoding: "json", DefinitionSha256: digest,
		Owner: &protocol.Reference{Engine: "spanner", Kind: "table", Schema: "public", Name: "posts"}, Payload: []byte(payload),
	}}
}

func TestNormalize_HappyPath(t *testing.T) {
	for _, representation := range []protocol.ValueInput_Representation{protocol.ValueInput_DESIRED, protocol.ValueInput_OBSERVED} {
		t.Run(representation.String(), func(t *testing.T) {
			c := testcheck.New(t)
			value, err := rowdeletion.Normalize(policy(representation, `{ "interval": "30 days", "column": "created_at" }`))
			c.Assert(err, qt.IsNil)
			c.Assert(string(value.GetCanonicalProjection()), qt.Equals, `{"column":"created_at","interval":"30 days"}`)
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
		{name: "no interval", representation: protocol.ValueInput_DESIRED, payload: `{"column":"c"}`, wantErr: `a row deletion policy names its column and its interval`},
		{name: "an unknown field", representation: protocol.ValueInput_OBSERVED, payload: `{"column":"c","interval":"1 day","x":1}`,
			wantErr: `a row deletion policy is not one the provider reads: .*`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			value, err := rowdeletion.Normalize(policy(test.representation, test.payload))
			c.Assert(err, qt.ErrorMatches, test.wantErr)
			c.Assert(value, qt.IsNil)
		})
	}
}
