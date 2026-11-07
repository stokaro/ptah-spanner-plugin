package backend

import (
	"context"
	"errors"
	"fmt"
	"slices"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"github.com/stokaro/ptah/provider-sdk/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-spanner-plugin/internal/catalog"
	"github.com/stokaro/ptah-spanner-plugin/internal/commonobject"
	"github.com/stokaro/ptah-spanner-plugin/internal/locktable"
	"github.com/stokaro/ptah-spanner-plugin/internal/rowdeletion"
)

// Inspect reads the base tables of the requested schemas, or of the default
// schema when the request names none, with their columns, primary keys,
// secondary indexes and row deletion policies, and the views and sequences of
// those schemas. The snapshot is complete or
// the call fails: an object the provider does not describe yet fails the read
// with a reason.
func (p *Provider) Inspect(ctx context.Context, request *protocol.InspectRequest) (*protocol.Schema, error) {
	current, err := p.lookup(request.GetSessionId())
	if err != nil {
		return nil, err
	}
	schemas := request.GetSchemas()
	if len(schemas) == 0 {
		schemas = []string{defaultSchema}
	}
	if slices.Contains(schemas, "") {
		return nil, runtime.ErrorWithReason(codes.InvalidArgument, "a schema selector is empty")
	}
	current.mu.Lock()
	defer current.mu.Unlock()
	tables, err := catalog.Read(ctx, current.conn, schemas)
	if err != nil {
		if unsupported, found := errors.AsType[*catalog.Unsupported](err); found {
			return nil, runtime.ErrorWithReason(codes.FailedPrecondition, unsupported.Reason)
		}
		return nil, callFailed(ctx, "the provider could not read the Spanner catalog")
	}
	snapshot := &protocol.Schema{
		Complete:       true,
		Engine:         engineID,
		TargetFeatures: negotiatedFeatures(ctx),
		Coverage:       &protocol.ModelCoverage{Representation: protocol.ValueInput_OBSERVED},
	}
	snapshot.Coverage.Kinds = []*protocol.ModelKindCoverage{policyCoverage(current.database, request.GetSchemas())}
	for _, table := range tables {
		if lockTable(table) {
			identity := &protocol.Reference{Engine: engineID, Database: current.database, Schema: table.Schema, Kind: "table", Name: table.Name}
			snapshot.ReservedObjects = append(snapshot.ReservedObjects, &protocol.ReservedObject{
				Object: identity, NormalizedObject: proto.CloneOf(identity), Purpose: protocol.ReservedObject_LOCK, ConflictKinds: []string{"table"},
			})
			continue
		}
		observed := observedTable(current.database, table)
		if table.RowDeletion != nil {
			facet, err := rowdeletion.Facet("policy."+table.Schema+"."+table.Name, protocol.ValueInput_OBSERVED, proto.CloneOf(observed.GetIdentity()), *table.RowDeletion)
			if err != nil {
				return nil, runtime.ErrorWithReason(codes.FailedPrecondition, fmt.Sprintf("table %s: %v", qualifiedName(table), err))
			}
			observed.Facets = append(observed.Facets, facet.GetValue())
			snapshot.Coverage.Subjects = append(snapshot.Coverage.Subjects, &protocol.ModelSubjectCoverage{Kind: rowdeletion.Kind,
				Subject: proto.CloneOf(observed.GetIdentity()), NormalizedSubject: proto.CloneOf(observed.GetNormalizedIdentity()),
				Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_COMPLETE}})
		}
		snapshot.Tables = append(snapshot.Tables, observed)
	}
	if snapshot.CommonObjects, err = commonObjects(ctx, current, schemas); err != nil {
		return nil, err
	}
	// A read narrowed to schemas says which namespace each selector names.
	for _, selector := range request.GetSchemas() {
		snapshot.ScopeBindings = append(snapshot.ScopeBindings, &protocol.InspectionScopeBinding{
			Selector: selector,
			Namespaces: []*protocol.ModelNamespace{{
				Database: &protocol.NamespaceComponent{Source: current.database, Normalized: current.database},
				Catalog:  &protocol.NamespaceComponent{},
				Schema:   &protocol.NamespaceComponent{Source: selector, Normalized: selector},
			}},
		})
	}
	return snapshot, nil
}

// observedTable is a read table as the protocol carries it: each column as
// one complete observation, the primary key in key order, and the secondary
// indexes. Names compare exactly, so the normalized identity is the identity.
func observedTable(database string, table catalog.Table) *protocol.Table {
	identity := &protocol.Reference{Engine: engineID, Database: database, Schema: table.Schema, Kind: "table", Name: table.Name}
	result := &protocol.Table{Identity: identity, NormalizedIdentity: proto.CloneOf(identity)}
	keys := make(map[string]bool, len(table.PrimaryKey))
	for _, key := range table.PrimaryKey {
		keys[key] = true
	}
	for _, column := range table.Columns {
		nullable := "YES"
		if !column.Nullable {
			nullable = "NO"
		}
		observed := &protocol.ObservedColumn{
			Name: column.Name, DataType: column.DataType, FormattedType: column.SpannerType, ColumnType: column.SpannerType,
			IsNullable: nullable, DefaultExpression: column.Default, CharacterMaxLength: column.MaxLength,
			OrdinalPosition: column.Ordinal, PrimaryKey: keys[column.Name],
		}
		result.Columns = append(result.Columns, &protocol.Column{Name: column.Name, Observed: observed})
	}
	if len(table.PrimaryKey) > 0 {
		result.PrimaryKey = &protocol.ObservedConstraint{
			Name: table.PrimaryKeyName, TableName: table.Name, Schema: table.Schema, Type: "PRIMARY KEY",
			ColumnNames: append([]string(nil), table.PrimaryKey...),
		}
	}
	for _, index := range table.Indexes {
		observed := &protocol.ObservedIndex{Name: index.Name, TableName: table.Name, Schema: table.Schema, Unique: index.Unique,
			IncludeColumns: append([]string(nil), index.Storing...)}
		for _, key := range index.Keys {
			observed.Columns = append(observed.Columns, key.Column)
			observed.Parts = append(observed.Parts, &protocol.CapturedIndexPart{Column: key.Column, Descending: key.Descending, Nulls: key.Nulls})
		}
		result.Indexes = append(result.Indexes, observed)
	}
	return result
}

// lockTable reports the provider's own lock table: the table locktable makes,
// in the default schema, with exactly its columns and key. A table of that
// name with another layout is the user's, and is read as one.
func lockTable(table catalog.Table) bool {
	if table.Schema != defaultSchema || table.Name != locktable.Table || len(table.Columns) != len(locktable.Layout) || len(table.Indexes) != 0 ||
		!slices.Equal(table.PrimaryKey, []string{locktable.Layout[0].Name}) {
		return false
	}
	for i, column := range table.Columns {
		if column.Name != locktable.Layout[i].Name || column.SpannerType != locktable.Layout[i].Type || column.Nullable || column.Default != nil {
			return false
		}
	}
	return true
}

// policyCoverage says what the read knows of row deletion policies: every
// table's, so a table without a policy holds none. A read narrowed to schemas
// knows them inside those schemas alone.
func policyCoverage(database string, schemas []string) *protocol.ModelKindCoverage {
	coverage := &protocol.ModelKindCoverage{Model: rowdeletion.ObservedModel(), Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_COMPLETE}}
	if len(schemas) == 0 {
		return coverage
	}
	coverage.Knowledge = &protocol.ModelKnowledge{State: protocol.ModelKnowledge_UNINSPECTED, Reason: "The read covers the selected schemas only"}
	for _, schema := range slices.Compact(slices.Sorted(slices.Values(schemas))) {
		coverage.Regions = append(coverage.Regions, &protocol.ModelNamespaceKnowledge{
			Scope: &protocol.ModelNamespace{
				Database: &protocol.NamespaceComponent{Source: database, Normalized: database},
				Catalog:  &protocol.NamespaceComponent{},
				Schema:   &protocol.NamespaceComponent{Source: schema, Normalized: schema},
			},
			Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_COMPLETE},
		})
	}
	return coverage
}

// qualifiedName names a read table for a reason.
func qualifiedName(table catalog.Table) string {
	return fmt.Sprintf("%q.%q", table.Schema, table.Name)
}

// commonObjects reads the views and sequences of the schemas the read covers,
// each kind completely: a view or a sequence the read does not report does
// not exist in those schemas. A host that does not carry common objects gets
// none, and a read that holds one is refused for it rather than left
// without it: the host would plan as if the object did not exist.
func commonObjects(ctx context.Context, current *session, schemas []string) (*protocol.ObservedCommonObjects, error) {
	views, err := catalog.ReadViews(ctx, current.conn, schemas)
	if err == nil {
		var sequences []catalog.Sequence
		if sequences, err = catalog.ReadSequences(ctx, current.conn, schemas); err == nil {
			if slices.Contains(provider.HostFeatures(ctx), provider.CommonObjectsFeature) {
				return observedCommonObjects(current.database, views, sequences), nil
			}
			err = uncarried(views, sequences)
		}
	}
	if err == nil {
		return nil, nil
	}
	if unsupported, found := errors.AsType[*catalog.Unsupported](err); found {
		return nil, runtime.ErrorWithReason(codes.FailedPrecondition, unsupported.Reason)
	}
	return nil, callFailed(ctx, "the provider could not read the Spanner catalog")
}

func observedCommonObjects(database string, views []catalog.View, sequences []catalog.Sequence) *protocol.ObservedCommonObjects {
	result := &protocol.ObservedCommonObjects{Coverage: []*protocol.CommonObjectCoverage{
		{Kind: protocol.CommonObjectSupport_VIEW, Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_COMPLETE}},
		{Kind: protocol.CommonObjectSupport_SEQUENCE, Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_COMPLETE}},
	}}
	for _, view := range views {
		result.Views = append(result.Views, &protocol.ObservedView{Body: view.Definition,
			Identity: &protocol.Reference{Engine: engineID, Database: database, Schema: view.Schema, Kind: "view", Name: view.Name}})
	}
	for _, sequence := range sequences {
		result.Sequences = append(result.Sequences, &protocol.ObservedSequence{DataType: commonobject.SequenceType, Start: sequence.CounterStart,
			Identity: &protocol.Reference{Engine: engineID, Database: database, Schema: sequence.Schema, Kind: "sequence", Name: sequence.Name}})
	}
	return result
}

// uncarried refuses a read that holds a view or a sequence for a host that
// does not carry common objects.
func uncarried(views []catalog.View, sequences []catalog.Sequence) error {
	const reason = "%s %q.%q needs a host that carries views and sequences (%s)"
	switch {
	case len(views) > 0:
		return &catalog.Unsupported{Reason: fmt.Sprintf(reason, "view", views[0].Schema, views[0].Name, provider.CommonObjectsFeature)}
	case len(sequences) > 0:
		return &catalog.Unsupported{Reason: fmt.Sprintf(reason, "sequence", sequences[0].Schema, sequences[0].Name, provider.CommonObjectsFeature)}
	}
	return nil
}
