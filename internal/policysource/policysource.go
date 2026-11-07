// Package policysource reads and writes a Spanner table's row deletion policy
// in the sources that declare one: a Go table directive, a YAML table entry
// and an HCL table, in the spelling Ptah's built-in Spanner engine read.
//
//	//ptah:schema:table name="events" platform.spanner.row_deletion_column="created_at" platform.spanner.row_deletion_interval="30 days"
//
//	tables:
//	  events:
//	    platform:
//	      spanner: {row_deletion_column: created_at, row_deletion_interval: 30 days}
//
//	table "events" {
//	  platform "spanner" {
//	    override "row_deletion_column" { value = "created_at" }
//	    override "row_deletion_interval" { value = "30 days" }
//	  }
//	}
//
// The host lexes each source and sends the provider the values of the keys,
// attributes and overrides the provider claims; this package owns the claims
// and the meaning. It claims row_deletion_unit as well, in every format, so
// that the name is refused with a reason: Spanner's clause reads a timestamp
// column and has no unit. A policy needs its column and its interval, and an
// interval the server refuses is refused here, before a plan is made.
package policysource

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-spanner-plugin/internal/rowdeletion"
)

// The property names of a policy, as a YAML group and an HCL override spell
// them. A Go attribute adds GoPrefix.
const (
	ColumnProperty   = "row_deletion_column"
	IntervalProperty = "row_deletion_interval"
	UnitProperty     = "row_deletion_unit"
)

// GoPrefix starts the name of a Go attribute that sets a platform property of
// the spanner engine, as the host spells one.
const GoPrefix = "platform.spanner."

// YAMLKey is the table key that claims the spanner group of a YAML table's
// platform key. The host sends the group's mapping with its keys unprefixed
// and keeps no copy of it, so every key the group holds is read or refused
// here.
const YAMLKey = "platform.spanner"

// TableDirective is the host's directive that declares a table, whose
// platform attributes the provider claims.
const TableDirective = "ptah:schema:table"

// properties are the names the provider claims, in the order a refusal
// lists them.
func properties() []string { return []string{ColumnProperty, IntervalProperty, UnitProperty} }

// YAMLKeys returns the provider's YAML claim: the spanner platform group of a
// table entry.
func YAMLKeys() []*protocol.YAMLSourceKey {
	return []*protocol.YAMLSourceKey{{Scope: protocol.YAMLSourceKey_TABLE, Key: YAMLKey, Selector: YAMLKey, Models: []string{rowdeletion.Kind}}}
}

// GoDirectives returns the provider's Go claim: the platform attributes of
// the host's table directive.
func GoDirectives() []*protocol.GoSourceDirective {
	attributes := make([]string, 0, len(properties()))
	for _, name := range properties() {
		attributes = append(attributes, GoPrefix+name)
	}
	return []*protocol.GoSourceDirective{{Scope: protocol.GoSourceDirective_TABLE, Directive: TableDirective, Selector: TableDirective,
		Attributes: attributes, Models: []string{rowdeletion.Kind}}}
}

// HCLPlatformBlock returns the provider's HCL claim: the overrides of a
// table's platform "spanner" block. A table without the block states nothing
// about its policy, which it keeps.
func HCLPlatformBlock() *protocol.HCLSourceBlock {
	attributes := make([]*protocol.HCLSourceAttribute, 0, len(properties()))
	for _, name := range properties() {
		attributes = append(attributes, &protocol.HCLSourceAttribute{Name: name, Kind: protocol.HCLSourceAttribute_STRING})
	}
	return &protocol.HCLSourceBlock{Site: protocol.HCLSourceBlock_TABLE_PLATFORM, Type: "spanner", Selector: YAMLKey, Attributes: attributes,
		Models: []string{rowdeletion.Kind}}
}

// Owns reports whether a fragment carries one of the provider's claims.
func Owns(fragment *protocol.SourceFragment) bool {
	switch {
	case fragment.GetYamlKey() != nil:
		return fragment.GetYamlKey().GetKey() == YAMLKey
	case fragment.GetGoDirective() != nil:
		return fragment.GetGoDirective().GetDirective() == TableDirective
	case fragment.GetHclBlock() != nil:
		return fragment.GetHclBlock().GetSite() == protocol.HCLSourceBlock_TABLE_PLATFORM
	}
	return false
}

// Decode reads the policy one claimed fragment declares, with the table it
// belongs to: the binding the fragment names. A property the provider does not
// read, an empty value, a policy without its column or its interval, and an
// interval Spanner refuses are refused with the table's name.
func Decode(document *protocol.SourceDocument, fragment *protocol.SourceFragment) (*protocol.Reference, rowdeletion.Policy, error) {
	var table *protocol.Reference
	for _, binding := range document.GetBindings() {
		if binding.GetId() == fragment.GetBindingId() {
			table = proto.CloneOf(binding.GetObject())
		}
	}
	if table == nil || table.GetKind() != "table" {
		return nil, rowdeletion.Policy{}, fmt.Errorf("a row deletion policy belongs to a table the source declares")
	}
	label := strconv.Quote(table.GetName())
	values, err := propertyValues(fragment)
	if err != nil {
		return nil, rowdeletion.Policy{}, fmt.Errorf("table %s: %w", label, err)
	}
	policy, err := policyOf(values)
	if err != nil {
		return nil, rowdeletion.Policy{}, fmt.Errorf("table %s: %w", label, err)
	}
	if err := rowdeletion.ValidateDeclared(label, policy); err != nil {
		return nil, rowdeletion.Policy{}, err
	}
	return table, policy, nil
}

// propertyValues reads the fragment's mapping into property names and their
// text, without the Go prefix.
func propertyValues(fragment *protocol.SourceFragment) (map[string]string, error) {
	mapping := fragment.GetValue().GetMapping()
	if mapping == nil {
		return nil, fmt.Errorf("the spanner platform properties are not a mapping")
	}
	values := make(map[string]string, len(mapping.GetEntries()))
	for _, entry := range mapping.GetEntries() {
		name := entry.GetKey()
		if fragment.GetGoDirective() != nil {
			name = strings.TrimPrefix(name, GoPrefix)
		}
		text, ok := entry.GetValue().GetValue().(*protocol.SourceValue_Text)
		if !ok {
			return nil, fmt.Errorf("%s is not text", name)
		}
		values[name] = text.Text
	}
	return values, nil
}

// policyOf reads a policy out of its properties. A name outside the two the
// policy takes is refused, naming the lower-case spelling when that is one of
// them, as Ptah's built-in Spanner engine refused it.
func policyOf(values map[string]string) (rowdeletion.Policy, error) {
	managed := []string{ColumnProperty, IntervalProperty}
	for _, name := range slices.Sorted(maps.Keys(values)) {
		switch lower := strings.ToLower(name); {
		case slices.Contains(managed, name):
		case name == UnitProperty || lower == UnitProperty:
			return rowdeletion.Policy{}, fmt.Errorf("%s: a Spanner row deletion policy counts from a timestamptz column and takes no unit", name)
		case slices.Contains(managed, lower):
			return rowdeletion.Policy{}, fmt.Errorf("unknown row deletion property %q: property names are lower case, as %q", name, lower)
		default:
			return rowdeletion.Policy{}, fmt.Errorf("unknown row deletion property %q: the policy takes %s", name, strings.Join(managed, ", "))
		}
	}
	for _, name := range slices.Sorted(maps.Keys(values)) {
		if strings.TrimSpace(values[name]) == "" {
			return rowdeletion.Policy{}, fmt.Errorf("%s is empty; remove it to leave the policy undeclared", name)
		}
	}
	policy := rowdeletion.Policy{Column: strings.TrimSpace(values[ColumnProperty]), Interval: strings.TrimSpace(values[IntervalProperty])}
	switch {
	case policy.Column == "":
		return rowdeletion.Policy{}, fmt.Errorf("a row deletion policy needs %s, the column its interval counts from", ColumnProperty)
	case policy.Interval == "":
		return rowdeletion.Policy{}, fmt.Errorf("a row deletion policy needs %s, the interval after which Spanner deletes a row", IntervalProperty)
	}
	return policy, nil
}

// Content is the mapping that declares policy in format: the two properties,
// each prefixed for a Go directive.
func Content(format protocol.SourceDocument_Format, policy rowdeletion.Policy) *protocol.SourceFragment_Value {
	prefix := ""
	if format == protocol.SourceDocument_GO_ANNOTATIONS {
		prefix = GoPrefix
	}
	text := func(value string) *protocol.SourceValue {
		return &protocol.SourceValue{Value: &protocol.SourceValue_Text{Text: value}}
	}
	return &protocol.SourceFragment_Value{Value: &protocol.SourceValue{Value: &protocol.SourceValue_Mapping{Mapping: &protocol.SourceMapping{Entries: []*protocol.SourceEntry{
		{Key: prefix + ColumnProperty, Value: text(policy.Column)},
		{Key: prefix + IntervalProperty, Value: text(policy.Interval)},
	}}}}}
}
