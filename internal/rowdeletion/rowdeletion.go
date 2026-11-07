// Package rowdeletion is a Spanner table's row deletion policy, `TTL INTERVAL
// '<interval>' ON <column>`: Spanner deletes a row once the interval has
// passed since the timestamp its column holds. It is the one table setting
// the provider's table lifecycle service owns beside the table itself.
//
// The package holds the policy's models as the host carries them: a desired
// facet a Go, YAML, HCL or SQL source declares, an observed facet a read reports,
// and the change a comparison reports for a table both sides hold. It also
// holds the policy's meaning: the interval is compared by the hours it
// denotes, because the server stores its own spelling of it (see
// IntervalHours), and the catalog's expression is read back into a policy
// (see ParseExpression).
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

// Kind is the model kind of a row deletion policy, desired and observed.
const Kind = tableop.ProviderID + "/row-deletion-policy"

// ChangeKind is the model kind of a change of a kept table's policy.
const ChangeKind = tableop.ProviderID + "/row-deletion-policy-change"

const (
	schemaVersion = 1
	encoding      = "json"
	// displayName is the plural label a report of omitted values uses.
	displayName = "row deletion policies"
)

// Policy is a table's row deletion policy.
type Policy struct {
	// Column is the timestamptz column the interval counts from.
	Column string `json:"column"`
	// Interval is the interval without its quotes, as the declaration spells
	// it or as the server stored it.
	Interval string `json:"interval"`
}

// Change is a change of the policy of a table both sides hold. Before is the
// policy the table holds, nil for none; After is the policy the declaration
// asks for, nil to remove the policy. They are never both nil.
type Change struct {
	Before *Policy `json:"before,omitempty"`
	After  *Policy `json:"after,omitempty"`
}

var (
	desiredModel  = buildModel(protocol.ValueInput_DESIRED)
	observedModel = buildModel(protocol.ValueInput_OBSERVED)
	changeModel   = buildModel(protocol.ValueInput_CHANGE)
)

// Models are the desired, observed and change models of the policy.
func Models() []*protocol.ValueModel {
	return []*protocol.ValueModel{proto.CloneOf(desiredModel), proto.CloneOf(observedModel), proto.CloneOf(changeModel)}
}

// DesiredModel is the desired model of the policy.
func DesiredModel() *protocol.ValueModel { return proto.CloneOf(desiredModel) }

// ObservedModel is the observed model of the policy.
func ObservedModel() *protocol.ValueModel { return proto.CloneOf(observedModel) }

func buildModel(representation protocol.ValueInput_Representation) *protocol.ValueModel {
	kind := Kind
	definition := map[string]any{
		"representation": strings.ToLower(representation.String()), "schema_version": schemaVersion, "encoding": encoding,
		"owner": "the table the policy belongs to: engine spanner, kind table, its schema and its name",
		"fields": map[string]string{
			"column":   "required; the timestamptz column the interval counts from",
			"interval": "required; the interval as Spanner's TTL INTERVAL clause takes it, compared by the hours it denotes at 30 days to a month",
		},
	}
	model := &protocol.ValueModel{Representation: representation, SchemaVersion: schemaVersion, Encoding: encoding}
	switch representation {
	case protocol.ValueInput_DESIRED:
		// A Go table directive, a YAML table entry and a SQL CREATE TABLE
		// state the policy of every table they declare, so a table without
		// one declares none. A SQL document states it in the TTL clause the
		// provider claims (source.sql-table-clauses), and a host that does
		// not negotiate the claim sends the provider no SQL. An HCL table
		// states it only in its platform "spanner" block, which the provider
		// claims at that site alone (see policysource), so HCL is not listed:
		// a table without the block leaves its policy unmanaged.
		model.SourceFormats = []protocol.SourceDocument_Format{protocol.SourceDocument_GO_ANNOTATIONS, protocol.SourceDocument_YAML,
			protocol.SourceDocument_SQL}
		model.DisplayName = displayName
	case protocol.ValueInput_OBSERVED:
		model.DisplayName = displayName
	case protocol.ValueInput_CHANGE:
		kind = ChangeKind
		definition["owner"] = "the table both sides hold"
		definition["fields"] = map[string]string{
			"before": "the policy the table holds, column and interval as the server stored it; absent for none",
			"after":  "the policy the declaration asks for, column and interval as declared; absent to remove the policy",
		}
	default:
		panic("rowdeletion: no model for " + representation.String())
	}
	definition["kind"] = kind
	raw, err := json.Marshal(definition)
	if err != nil {
		panic(err)
	}
	canonical, err := modeljson.Canonical(raw)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(canonical)
	model.Kind, model.DefinitionJson, model.DefinitionSha256, model.CanonicalFormat = kind, canonical, hex.EncodeToString(sum[:]), kind+"/json-v1"
	return model
}

// Facet builds the normalized desired or observed facet that gives the table
// owner names policy. A desired policy is checked as a declaration is.
func Facet(id string, representation protocol.ValueInput_Representation, owner *protocol.Reference, policy Policy) (*protocol.NormalizedValue, error) {
	model := desiredModel
	if representation == protocol.ValueInput_OBSERVED {
		model = observedModel
	}
	payload, err := json.Marshal(policy)
	if err != nil {
		return nil, err
	}
	return Normalize(&protocol.ValueInput{Id: id, Representation: representation, Value: &protocol.Envelope{
		ProviderId: tableop.ProviderID, Kind: Kind, Owner: proto.CloneOf(owner), SchemaVersion: schemaVersion, Encoding: encoding,
		DefinitionSha256: model.GetDefinitionSha256(), Payload: payload,
	}})
}

// CaptureChange builds the normalized change of the policy of the table owner
// names.
func CaptureChange(id string, owner *protocol.Reference, change Change) (*protocol.NormalizedValue, error) {
	payload, err := json.Marshal(change)
	if err != nil {
		return nil, err
	}
	return Normalize(&protocol.ValueInput{Id: id, Representation: protocol.ValueInput_CHANGE, Value: &protocol.Envelope{
		ProviderId: tableop.ProviderID, Kind: ChangeKind, Owner: proto.CloneOf(owner), SchemaVersion: schemaVersion, Encoding: encoding,
		DefinitionSha256: changeModel.GetDefinitionSha256(), Payload: payload,
	}})
}

// Normalize checks a desired or observed policy, or a change of one, and
// returns it in canonical form. A desired policy must be one a declaration can
// ask for (see ValidateDeclared); an observed one needs its column and its
// interval, in whatever spelling the server stored. A change needs an operand
// and operands that differ.
func Normalize(input *protocol.ValueInput) (*protocol.NormalizedValue, error) {
	var model *protocol.ValueModel
	for _, candidate := range []*protocol.ValueModel{desiredModel, observedModel, changeModel} {
		if candidate.GetRepresentation() == input.GetRepresentation() {
			model = candidate
		}
	}
	value := input.GetValue()
	if model == nil || value.GetProviderId() != tableop.ProviderID || value.GetKind() != model.GetKind() || value.GetSchemaVersion() != schemaVersion ||
		value.GetEncoding() != encoding || value.GetDefinitionSha256() != model.GetDefinitionSha256() {
		return nil, fmt.Errorf("a row deletion policy does not match its model")
	}
	owner := value.GetOwner()
	if owner.GetEngine() != tableop.Engine || owner.GetKind() != "table" || owner.GetName() == "" {
		return nil, fmt.Errorf("a row deletion policy belongs to a table")
	}
	var normalized any
	var semantics *protocol.ChangeSemantics
	switch input.GetRepresentation() {
	case protocol.ValueInput_CHANGE:
		change, err := decodeChange(value.GetPayload(), owner.GetName())
		if err != nil {
			return nil, err
		}
		normalized, semantics = change, changeSemantics(change)
	default:
		policy, err := decodePolicy(value.GetPayload())
		if err != nil {
			return nil, err
		}
		if input.GetRepresentation() == protocol.ValueInput_DESIRED {
			if err := ValidateDeclared(fmt.Sprintf("%q", owner.GetName()), policy); err != nil {
				return nil, err
			}
		}
		normalized = policy
	}
	payload, err := json.Marshal(normalized)
	if err != nil {
		return nil, err
	}
	canonical, err := modeljson.Canonical(payload)
	if err != nil {
		return nil, err
	}
	result := proto.CloneOf(value)
	result.Payload = canonical
	return &protocol.NormalizedValue{Id: input.GetId(), Value: result, NormalizedOwner: proto.CloneOf(owner),
		CanonicalFormat: model.GetCanonicalFormat(), CanonicalProjection: bytes.Clone(canonical), ChangeSemantics: semantics}, nil
}

// PolicyOf reads the policy a normalized desired or observed facet carries.
func PolicyOf(value *protocol.NormalizedValue) (Policy, error) {
	if value.GetValue().GetKind() != Kind {
		return Policy{}, fmt.Errorf("the value is not a row deletion policy")
	}
	return decodePolicy(value.GetValue().GetPayload())
}

// ChangeOf reads the change a normalized change value carries.
func ChangeOf(value *protocol.NormalizedValue) (Change, error) {
	if value.GetValue().GetKind() != ChangeKind {
		return Change{}, fmt.Errorf("the value is not a row deletion policy change")
	}
	return decodeChange(value.GetValue().GetPayload(), value.GetValue().GetOwner().GetName())
}

func decodePolicy(payload []byte) (Policy, error) {
	var policy Policy
	if err := strictDecode(payload, &policy); err != nil {
		return Policy{}, err
	}
	if err := validateText(policy); err != nil {
		return Policy{}, fmt.Errorf("a row deletion policy names its column and its interval: %w", err)
	}
	return policy, nil
}

func decodeChange(payload []byte, table string) (Change, error) {
	var change Change
	if err := strictDecode(payload, &change); err != nil {
		return Change{}, err
	}
	switch {
	case change.Before == nil && change.After == nil:
		return Change{}, fmt.Errorf("a row deletion policy change needs an operand")
	case change.Before != nil && validateText(*change.Before) != nil:
		return Change{}, fmt.Errorf("a row deletion policy change holds an incomplete prior policy")
	case change.Before != nil && change.After != nil && Equivalent(*change.Before, *change.After):
		return Change{}, fmt.Errorf("a row deletion policy change of table %q changes nothing", table)
	}
	if change.After != nil {
		if err := ValidateDeclared(fmt.Sprintf("%q", table), *change.After); err != nil {
			return Change{}, err
		}
	}
	return change, nil
}

func strictDecode(payload []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("a row deletion policy is not one the provider reads: %w", err)
	}
	if decoder.More() {
		return fmt.Errorf("a row deletion policy carries data after its value")
	}
	return nil
}

// changeSemantics says what a change does to the rows of its table. Every
// change decides which rows the server deletes, and a later policy cannot
// bring back a row an earlier one deleted. The access of a table does not
// change.
func changeSemantics(change Change) *protocol.ChangeSemantics {
	semantics := &protocol.ChangeSemantics{ReplacesOwner: new(false), RemovesState: new(false), Impact: protocol.ChangeSemantics_BEHAVIORAL,
		Reason: "A row deletion policy decides which rows Spanner deletes; restoring a prior policy cannot recover deleted rows",
		Access: protocol.ChangeSemantics_UNCHANGED, AccessReason: "A row deletion policy grants and revokes nothing"}
	if change.After == nil {
		semantics.Reason = "Removes the row deletion policy; Spanner stops deleting rows, and the rows it deleted stay deleted"
	}
	return semantics
}
