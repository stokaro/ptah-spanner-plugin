// Package rowdeletion is the model of a Spanner table's row deletion policy,
// `TTL INTERVAL '<interval>' ON <column>`: the setting the provider's table
// lifecycle service owns beside the table itself.
//
// The provider declares the model so its planning service can own the
// lifecycle of its tables: the host assigns a table to the service that owns
// the table's settings. No source reads the policy and no read reports it
// yet, so no value of the model reaches the provider; a table that holds a
// policy is refused by the read.
package rowdeletion

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/stokaro/ptah/provider-sdk/modeljson"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-spanner-plugin/internal/tableop"
)

// Kind is the model kind of a row deletion policy.
const Kind = tableop.ProviderID + "/row-deletion-policy"

const (
	schemaVersion = 1
	encoding      = "json"
)

// Policy is a table's row deletion policy: Spanner deletes a row once the
// interval has passed since the time its column holds.
type Policy struct {
	Column   string `json:"column"`
	Interval string `json:"interval"`
}

var models = buildModels()

// Models are the desired and observed models of the policy.
func Models() []*protocol.ValueModel {
	return []*protocol.ValueModel{proto.CloneOf(models[0]), proto.CloneOf(models[1])}
}

func buildModels() [2]*protocol.ValueModel {
	var result [2]*protocol.ValueModel
	for i, representation := range []protocol.ValueInput_Representation{protocol.ValueInput_DESIRED, protocol.ValueInput_OBSERVED} {
		raw, err := json.Marshal(map[string]any{
			"kind": Kind, "representation": strings.ToLower(representation.String()), "schema_version": schemaVersion, "encoding": encoding,
			"owner": "the table the policy belongs to: engine spanner, kind table, its schema and its name",
			"fields": map[string]string{
				"column":   "required; the timestamptz column the interval counts from",
				"interval": "required; the interval as Spanner's TTL INTERVAL clause takes it",
			},
		})
		if err != nil {
			panic(err)
		}
		canonical, err := modeljson.Canonical(raw)
		if err != nil {
			panic(err)
		}
		sum := sha256.Sum256(canonical)
		result[i] = &protocol.ValueModel{Kind: Kind, Representation: representation, SchemaVersion: schemaVersion, Encoding: encoding,
			DefinitionJson: canonical, DefinitionSha256: hex.EncodeToString(sum[:]), CanonicalFormat: Kind + "/json-v1"}
	}
	return result
}

// Normalize checks a desired or observed policy and returns it in canonical
// form.
func Normalize(input *protocol.ValueInput) (*protocol.NormalizedValue, error) {
	var definition *protocol.ValueModel
	for _, candidate := range models {
		if candidate.GetRepresentation() == input.GetRepresentation() {
			definition = candidate
		}
	}
	value := input.GetValue()
	if definition == nil || value.GetProviderId() != tableop.ProviderID || value.GetKind() != Kind || value.GetSchemaVersion() != schemaVersion ||
		value.GetEncoding() != encoding || value.GetDefinitionSha256() != definition.GetDefinitionSha256() {
		return nil, fmt.Errorf("a row deletion policy does not match its model")
	}
	owner := value.GetOwner()
	if owner.GetEngine() != tableop.Engine || owner.GetKind() != "table" || owner.GetName() == "" {
		return nil, fmt.Errorf("a row deletion policy belongs to a table")
	}
	decoder := json.NewDecoder(bytes.NewReader(value.GetPayload()))
	decoder.DisallowUnknownFields()
	var policy Policy
	if err := decoder.Decode(&policy); err != nil {
		return nil, fmt.Errorf("a row deletion policy is not one the provider reads: %w", err)
	}
	if policy.Column == "" || strings.TrimSpace(policy.Interval) == "" {
		return nil, fmt.Errorf("a row deletion policy names its column and its interval")
	}
	payload, err := json.Marshal(policy)
	if err != nil {
		return nil, err
	}
	canonical, err := modeljson.Canonical(payload)
	if err != nil {
		return nil, err
	}
	normalized := proto.CloneOf(value)
	normalized.Payload = canonical
	return &protocol.NormalizedValue{Id: input.GetId(), Value: normalized, NormalizedOwner: proto.CloneOf(owner),
		CanonicalFormat: definition.GetCanonicalFormat(), CanonicalProjection: bytes.Clone(canonical)}, nil
}
