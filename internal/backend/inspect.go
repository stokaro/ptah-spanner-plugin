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
	"github.com/stokaro/ptah-spanner-plugin/internal/changestream"
	"github.com/stokaro/ptah-spanner-plugin/internal/commonobject"
	"github.com/stokaro/ptah-spanner-plugin/internal/history"
	"github.com/stokaro/ptah-spanner-plugin/internal/indexfilter"
	"github.com/stokaro/ptah-spanner-plugin/internal/locktable"
	"github.com/stokaro/ptah-spanner-plugin/internal/metadata"
	"github.com/stokaro/ptah-spanner-plugin/internal/ownedtable"
	"github.com/stokaro/ptah-spanner-plugin/internal/rowdeletion"
	"github.com/stokaro/ptah-spanner-plugin/internal/seedledger"
)

// Inspect reads the base tables of the requested schemas, or of the default
// schema when the request names none, with their columns, primary keys,
// secondary indexes and row deletion policies, foreign keys and CHECKs and the
// change streams that name each table where the host negotiated them, and the
// views and sequences of those schemas. The
// snapshot is complete or the call fails: an object the provider does not
// describe yet, or a constraint a host that did not negotiate them would
// miss, fails the read with a reason.
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
	snapshot.NotDescribed = notDescribed(snapshot.GetTargetFeatures())
	tracking, err := streamTracking(ctx, current, snapshot.GetTargetFeatures())
	if err != nil {
		return nil, err
	}
	constraints := slices.Contains(snapshot.GetTargetFeatures(), provider.ObservedConstraintsFeature)
	seeds := slices.Contains(snapshot.GetTargetFeatures(), provider.ReservedSeedsFeature)
	predicates := slices.Contains(snapshot.GetTargetFeatures(), provider.IndexPredicatesFeature)
	for _, table := range tables {
		if held := table.Constraints; !constraints && len(held) > 0 {
			return nil, runtime.ErrorWithReason(codes.FailedPrecondition, fmt.Sprintf(
				"table %q.%q has the %s constraint %q, which needs a host that reads constraints (schema.observed-constraints)",
				table.Schema, table.Name, held[0].Type, held[0].Name))
		}
		if index := filteredIndex(table); !predicates && index != "" {
			return nil, runtime.ErrorWithReason(codes.FailedPrecondition, fmt.Sprintf(
				"index %q of table %q.%q has a WHERE filter, which needs a host that compares index predicates (%s)",
				index, table.Schema, table.Name, provider.IndexPredicatesFeature))
		}
		if purpose := reservedPurpose(table, seeds); purpose != protocol.ReservedObject_PURPOSE_UNSPECIFIED {
			identity := &protocol.Reference{Engine: engineID, Database: current.database, Schema: table.Schema, Kind: "table", Name: table.Name}
			snapshot.ReservedObjects = append(snapshot.ReservedObjects, &protocol.ReservedObject{
				Object: identity, NormalizedObject: proto.CloneOf(identity), Purpose: purpose, ConflictKinds: []string{"table"},
			})
			continue
		}
		observed := observedTable(current.database, table)
		for _, blocker := range tracking.Blockers(table.Schema, table.Name) {
			observed.RemovalBlockers = append(observed.RemovalBlockers, &protocol.RemovalBlocker{Kind: changeStreamKind, Name: blocker.Stream, Columns: blocker.Columns})
		}
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
	if snapshot.CommonObjects, err = commonObjects(ctx, current, schemas, request.GetSchemas()); err != nil {
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

// changeStreamKind is the kind of the removal blockers a read reports, and the
// kind of object it records as not described.
const changeStreamKind = "change_stream"

// notDescribedKinds are the kinds of object the engine holds and the provider
// does not model, which every read records as not described.
func notDescribedKinds() []string {
	return []string{changeStreamKind}
}

// notDescribed is what a read records as not described, for a host that
// carries the records (schema.not-described): every change stream, whatever
// the database holds. A change stream has a lifecycle of its own and the
// provider reads none, so a document made from the read must not let its
// silence read as the absence of every stream. The record is the target's, as
// Ptah's built-in Spanner engine recorded it: listing only the streams a read
// found would say that no other one exists. A host without the feature
// receives none.
func notDescribed(features []string) []*protocol.SchemaCoverageLimit {
	if !slices.Contains(features, provider.NotDescribedFeature) {
		return nil
	}
	return []*protocol.SchemaCoverageLimit{{Kind: changeStreamKind, Reason: "unsupported", Provenance: "derived-from-target"}}
}

// streamTracking reads what the database's change streams name, for a host
// that carries removal blockers: Spanner refuses to drop a table a stream
// names, and a column one names in its column list, so the read reports them
// with each table and the table service refuses such a drop at plan time. A
// host that does not carry them reads none, and the executor refuses the drop
// instead; see changestream.
func streamTracking(ctx context.Context, current *session, features []string) (changestream.Tracking, error) {
	if !slices.Contains(features, provider.RemovalBlockersFeature) {
		return changestream.Tracking{}, nil
	}
	tracking, err := changestream.Read(ctx, current.conn)
	if err != nil {
		return changestream.Tracking{}, callFailed(ctx, "the provider could not read which tables and columns change streams name")
	}
	return tracking, nil
}

// observedTable is a read table as the protocol carries it: each column as
// one complete observation, the primary key in key order, the secondary
// indexes with their WHERE clauses in the spelling Spanner keeps, and the
// foreign keys and CHECKs. A filtered index reaches a host that compares
// index predicates alone; Inspect refuses one for any other host. A CHECK lists no columns, which
// the protocol leaves to the server to know. Names compare exactly, so the
// normalized identity is the identity.
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
			IncludeColumns: append([]string(nil), index.Storing...), Condition: indexfilter.Spelling(index.Filter)}
		for _, key := range index.Keys {
			observed.Columns = append(observed.Columns, key.Column)
			observed.Parts = append(observed.Parts, &protocol.CapturedIndexPart{Column: key.Column, Descending: key.Descending, Nulls: key.Nulls})
		}
		result.Indexes = append(result.Indexes, observed)
	}
	for _, held := range table.Constraints {
		observed := &protocol.ObservedConstraint{Name: held.Name, TableName: table.Name, Schema: table.Schema, Type: held.Type}
		if held.Type == catalog.Check {
			observed.CheckClause = new(held.Clause)
		} else {
			observed.ColumnNames = append([]string(nil), held.Columns...)
			observed.ForeignTable, observed.ForeignSchema = new(held.RefTable), held.RefSchema
			observed.ForeignColumns = append([]string(nil), held.RefColumns...)
			observed.DeleteRule, observed.UpdateRule = new(held.DeleteRule), new(held.UpdateRule)
		}
		result.Constraints = append(result.Constraints, observed)
	}
	return result
}

// filteredIndex names the first index of a table that has a WHERE filter, or
// returns the empty string.
func filteredIndex(table catalog.Table) string {
	for _, index := range table.Indexes {
		if len(index.Filter) > 0 {
			return index.Name
		}
	}
	return ""
}

// reservedPurpose is what the provider keeps in a table a read leaves out of
// the schema: its lock table, or the revision table, migration log or tag
// table of Ptah's migrator under their default names and in exactly their
// layouts. Ptah's built-in Spanner engine left those three names out of every
// read, so a plan never dropped the record of the migrations a database ran.
// For a host that negotiated the reserved seed ledger (seeds), the seed
// ledger in exactly its layout is reserved too, so a plan never drops the
// record of the seed files a database ran; a host that did not reads it as a
// table. Any other table is the user's, and is read as one.
func reservedPurpose(table catalog.Table, seeds bool) protocol.ReservedObject_Purpose {
	columns := make([]ownedtable.Column, len(table.Columns))
	for i, column := range table.Columns {
		columns[i] = ownedtable.Column{Name: column.Name, Type: column.SpannerType, Nullable: column.Nullable}
	}
	switch {
	case lockTable(table):
		return protocol.ReservedObject_LOCK
	case table.Name == history.NativeTable && history.Native(table.Schema, table.Name).Holds(columns, table.PrimaryKey):
		return protocol.ReservedObject_HISTORY
	case table.Name == metadata.LogTable && metadata.Log(table.Schema, table.Name).Holds(columns, table.PrimaryKey):
		return protocol.ReservedObject_LOG
	case table.Name == metadata.TagsTable && metadata.Tags(table.Schema, table.Name).Holds(columns, table.PrimaryKey):
		return protocol.ReservedObject_TAG
	case seeds && table.Schema == defaultSchema && table.Name == seedledger.Table && seedledger.Definition().Holds(columns, table.PrimaryKey):
		return protocol.ReservedObject_SEEDS
	}
	return protocol.ReservedObject_PURPOSE_UNSPECIFIED
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
//
// A host that carries schemas (schema.common-namespaces) also gets the
// schemas of the read's scope, completely: each requested schema the
// database holds, or, for a read that requests none, every schema of the
// database but Spanner's own. The tables of such a read are the default
// schema's, and the schemas are all of them: the host only ever adds a
// schema a declaration names and a read lacks, so listing one whose tables
// it did not read plans nothing in it, and the host's cleanup of a dev
// database reads this list to find the schemas a run created.
func commonObjects(ctx context.Context, current *session, schemas, requested []string) (*protocol.ObservedCommonObjects, error) {
	views, err := catalog.ReadViews(ctx, current.conn, schemas)
	if err == nil {
		var sequences []catalog.Sequence
		if sequences, err = catalog.ReadSequences(ctx, current.conn, schemas); err == nil {
			if slices.Contains(provider.HostFeatures(ctx), provider.CommonObjectsFeature) {
				return carriedCommonObjects(ctx, current, views, sequences, requested)
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

// carriedCommonObjects is the common objects of a read for a host that
// carries them, with the schemas for a host that carries those too.
func carriedCommonObjects(ctx context.Context, current *session, views []catalog.View, sequences []catalog.Sequence,
	requested []string,
) (*protocol.ObservedCommonObjects, error) {
	result := observedCommonObjects(current.database, views, sequences)
	if !slices.Contains(negotiatedFeatures(ctx), provider.CommonNamespacesFeature) {
		return result, nil
	}
	namespaces, err := catalog.ReadSchemas(ctx, current.conn, requested)
	if err != nil {
		return nil, err
	}
	result.Coverage = append(result.Coverage, &protocol.CommonObjectCoverage{Kind: protocol.CommonObjectSupport_SCHEMA,
		Knowledge: &protocol.ModelKnowledge{State: protocol.ModelKnowledge_COMPLETE}})
	for _, name := range namespaces {
		result.Namespaces = append(result.Namespaces, &protocol.ObservedNamespace{
			Identity: &protocol.Reference{Engine: engineID, Database: current.database, Kind: provider.NamespaceKind, Name: name}})
	}
	return result, nil
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
