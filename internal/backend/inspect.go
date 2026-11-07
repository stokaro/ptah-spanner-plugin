package backend

import (
	"context"
	"errors"
	"slices"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-spanner-plugin/internal/catalog"
	"github.com/stokaro/ptah-spanner-plugin/internal/history"
	"github.com/stokaro/ptah-spanner-plugin/internal/locktable"
	"github.com/stokaro/ptah-spanner-plugin/internal/metadata"
	"github.com/stokaro/ptah-spanner-plugin/internal/ownedtable"
)

// Inspect reads the base tables of the requested schemas, or of the default
// schema when the request names none, with their columns, primary keys and
// secondary indexes. The snapshot is complete or the call fails: an object
// the provider does not describe yet fails the read with a reason.
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
	for _, table := range tables {
		if purpose := reservedPurpose(table); purpose != protocol.ReservedObject_PURPOSE_UNSPECIFIED {
			identity := &protocol.Reference{Engine: engineID, Database: current.database, Schema: table.Schema, Kind: "table", Name: table.Name}
			snapshot.ReservedObjects = append(snapshot.ReservedObjects, &protocol.ReservedObject{
				Object: identity, NormalizedObject: proto.CloneOf(identity), Purpose: purpose, ConflictKinds: []string{"table"},
			})
			continue
		}
		snapshot.Tables = append(snapshot.Tables, observedTable(current.database, table))
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

// reservedPurpose is what the provider keeps in a table a read leaves out of
// the schema: its lock table, or the revision table, migration log or tag
// table of Ptah's migrator under their default names and in exactly their
// layouts. Ptah's built-in Spanner engine left those three names out of every
// read, so a plan never dropped the record of the migrations a database ran.
// Any other table is the user's, and is read as one.
func reservedPurpose(table catalog.Table) protocol.ReservedObject_Purpose {
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
