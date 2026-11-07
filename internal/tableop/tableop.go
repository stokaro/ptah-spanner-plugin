// Package tableop is the native operation the provider plans for a table it
// owns: one schema statement of Spanner's PostgreSQL interface, kept as a
// structured value so the host can bind, digest and replay it, and written as
// SQL only when the host renders the plan.
//
// An operation names its table in its envelope's owner and carries one action
// with the operands that action needs. Normalize checks it and says what it
// does to the data; Statement writes its SQL. Both refuse an operation another
// release or another provider could have written differently, so a saved plan
// never runs something this provider would not have planned.
package tableop

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/stokaro/ptah/provider-sdk/modeljson"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-spanner-plugin/internal/ddl"
)

// ProviderID is the provider that owns the operation model.
const ProviderID = "github.com/stokaro/ptah-spanner-plugin"

// Kind is the model kind of a table operation.
const Kind = ProviderID + "/table-operation"

// Engine is the engine an operation's owner names.
const Engine = "spanner"

const (
	schemaVersion   = 1
	encoding        = "json"
	canonicalFormat = Kind + "/json-v1"
)

// Action is what an operation does to its table.
type Action string

// The actions, one per schema statement.
const (
	CreateTable     Action = "create-table"
	DropTable       Action = "drop-table"
	AddColumn       Action = "add-column"
	DropColumn      Action = "drop-column"
	AlterColumnType Action = "alter-column-type"
	SetDefault      Action = "set-default"
	DropDefault     Action = "drop-default"
	SetNotNull      Action = "set-not-null"
	DropNotNull     Action = "drop-not-null"
	CreateIndex     Action = "create-index"
	DropIndex       Action = "drop-index"
	AddForeignKey   Action = "add-foreign-key"
	AddCheck        Action = "add-check"
	// AddRowDeletion gives a table without a row deletion policy one,
	// AlterRowDeletion replaces a table's policy, and DropRowDeletion
	// removes it. Spanner refuses each verb in another's place.
	AddRowDeletion   Action = "add-row-deletion"
	AlterRowDeletion Action = "alter-row-deletion"
	DropRowDeletion  Action = "drop-row-deletion"
	// CreateSchema creates the schema a created table names, unless it
	// exists.
	CreateSchema Action = "create-schema"
)

// Operation is one action on the table its envelope names. Each action reads
// the fields its comment names and refuses any other.
type Operation struct {
	Action Action `json:"action"`
	// Columns and PrimaryKey are the table a create-table makes, the key in
	// key order.
	Columns    []Column `json:"columns,omitempty"`
	PrimaryKey []string `json:"primaryKey,omitempty"`
	// Column is the column an add-column adds.
	Column *Column `json:"column,omitempty"`
	// Name is the column a drop-column, alter-column-type, set-default,
	// drop-default, set-not-null or drop-not-null names, the index a
	// drop-index drops, or the constraint an add-check adds.
	Name string `json:"name,omitempty"`
	// Type is the type an alter-column-type gives the column, as declared.
	Type string `json:"type,omitempty"`
	// Default is the SQL that follows DEFAULT in a set-default.
	Default string `json:"default,omitempty"`
	// Expression is the condition of an add-check.
	Expression string `json:"expression,omitempty"`
	// Index is the index a create-index makes.
	Index *Index `json:"index,omitempty"`
	// ForeignKey is the constraint an add-foreign-key adds.
	ForeignKey *ForeignKey `json:"foreignKey,omitempty"`
	// RowDeletion is the row deletion policy a create-table gives its table,
	// or the one an add-row-deletion or alter-row-deletion sets.
	RowDeletion *RowDeletion `json:"rowDeletion,omitempty"`
}

// RowDeletion is a table's row deletion policy: Spanner deletes a row once
// the interval has passed since the timestamp the column holds. The interval
// is written as declared.
type RowDeletion struct {
	Column   string `json:"column"`
	Interval string `json:"interval"`
}

// Column is a column of a create-table or an add-column.
type Column struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Nullable bool   `json:"nullable"`
	// Default is the SQL that follows DEFAULT, or nil.
	Default *string `json:"default,omitempty"`
	// Check and CheckName are the CHECK constraint written in the column's
	// definition of a create-table.
	Check     string `json:"check,omitempty"`
	CheckName string `json:"checkName,omitempty"`
}

// Index is the index of a create-index.
type Index struct {
	Name    string     `json:"name"`
	Unique  bool       `json:"unique"`
	Keys    []IndexKey `json:"keys"`
	Storing []string   `json:"storing,omitempty"`
}

// IndexKey is one key part of an index.
type IndexKey struct {
	Column     string `json:"column"`
	Descending bool   `json:"descending"`
	// Nulls is FIRST, LAST, or empty for the default of the key's direction.
	Nulls string `json:"nulls,omitempty"`
}

// ForeignKey is the constraint of an add-foreign-key.
type ForeignKey struct {
	Name       string   `json:"name"`
	Columns    []string `json:"columns"`
	RefSchema  string   `json:"refSchema"`
	RefTable   string   `json:"refTable"`
	RefColumns []string `json:"refColumns"`
	// OnDelete is CASCADE, or empty for NO ACTION.
	OnDelete string `json:"onDelete,omitempty"`
}

var model = buildModel()

// Model is the operation model the provider declares.
func Model() *protocol.ValueModel { return proto.CloneOf(model) }

func buildModel() *protocol.ValueModel {
	raw, err := json.Marshal(map[string]any{
		"kind": Kind, "representation": "operation", "schema_version": schemaVersion, "encoding": encoding,
		"owner": "the table the statement changes: engine spanner, kind table, its schema and its name; no database, catalog or path",
		"actions": []Action{CreateTable, DropTable, AddColumn, DropColumn, AlterColumnType, SetDefault, DropDefault, SetNotNull, DropNotNull,
			CreateIndex, DropIndex, AddForeignKey, AddCheck, AddRowDeletion, AlterRowDeletion, DropRowDeletion, CreateSchema},
		"fields": map[string]string{
			"action":     "required; one schema statement of Spanner's PostgreSQL interface",
			"columns":    "create-table: every column in order, each with name, type as declared, nullable, an optional default as SQL and an optional named check",
			"primaryKey": "create-table: the key columns in key order",
			"column":     "add-column: one column, nullable or with a default and without a check",
			"name": "the column an alter, drop-column or drop-default names, the index a drop-index drops, the constraint an add-check adds, " +
				"the schema a create-schema creates",
			"type":       "alter-column-type: the type as declared; Spanner changes a type only between character varying and bytea",
			"default":    "set-default: the SQL that follows DEFAULT",
			"expression": "add-check: the condition",
			"index":      "create-index: name, unique, ordered keys with direction and NULLS ordering, and the stored columns",
			"foreignKey": "add-foreign-key: name, columns, the referenced schema, table and columns, and ON DELETE CASCADE or none",
			"rowDeletion": "create-table, add-row-deletion and alter-row-deletion: the row deletion policy's column and its interval as declared; " +
				"drop-row-deletion takes none",
		},
		"execution": "one statement outside a transaction; the statement commits as it runs",
	})
	if err != nil {
		panic(err)
	}
	canonical, err := modeljson.Canonical(raw)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(canonical)
	return &protocol.ValueModel{Kind: Kind, Representation: protocol.ValueInput_OPERATION, SchemaVersion: schemaVersion, Encoding: encoding,
		DefinitionJson: canonical, DefinitionSha256: hex.EncodeToString(sum[:]), CanonicalFormat: canonicalFormat}
}

// Owner is the reference an operation on the table schema.name names.
func Owner(schema, name string) *protocol.Reference {
	return &protocol.Reference{Engine: Engine, Kind: "table", Schema: schema, Name: name}
}

// Capture builds the normalized operation id that applies operation to the
// table owner names.
func Capture(id string, owner *protocol.Reference, operation Operation) (*protocol.NormalizedValue, error) {
	payload, err := json.Marshal(operation)
	if err != nil {
		return nil, err
	}
	return Normalize(&protocol.ValueInput{Id: id, Representation: protocol.ValueInput_OPERATION, Value: &protocol.Envelope{
		ProviderId: ProviderID, Kind: Kind, Owner: proto.CloneOf(owner), SchemaVersion: schemaVersion, Encoding: encoding,
		DefinitionSha256: model.GetDefinitionSha256(), Payload: payload,
	}})
}

// Normalize checks an operation value and returns it in canonical form, with
// what it does to the data. It refuses an envelope of another model, an owner
// that is not a table, and an action whose operands are missing, extra or
// unwritable.
func Normalize(input *protocol.ValueInput) (*protocol.NormalizedValue, error) {
	value := input.GetValue()
	if input.GetRepresentation() != protocol.ValueInput_OPERATION || value.GetProviderId() != ProviderID || value.GetKind() != Kind ||
		value.GetSchemaVersion() != schemaVersion || value.GetEncoding() != encoding || value.GetDefinitionSha256() != model.GetDefinitionSha256() {
		return nil, fmt.Errorf("a Spanner table operation does not match its model")
	}
	owner := value.GetOwner()
	if owner.GetEngine() != Engine || owner.GetKind() != "table" || owner.GetName() == "" || owner.GetSchema() == "" ||
		owner.GetDatabase() != "" || owner.GetCatalog() != "" || len(owner.GetPath()) != 0 || len(owner.GetOverload()) != 0 {
		return nil, fmt.Errorf("a Spanner table operation names a table by its schema and name and nothing else")
	}
	operation, err := Decode(value.GetPayload())
	if err != nil {
		return nil, err
	}
	statement, err := operation.statement(owner.GetSchema(), owner.GetName())
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(operation)
	if err != nil {
		return nil, err
	}
	canonical, err := modeljson.Canonical(payload)
	if err != nil {
		return nil, err
	}
	normalized := proto.CloneOf(value)
	normalized.Payload = canonical
	impact, reason := operation.impact()
	return &protocol.NormalizedValue{Id: input.GetId(), Value: normalized, NormalizedOwner: proto.CloneOf(owner),
		CanonicalFormat: canonicalFormat, CanonicalProjection: []byte(statement),
		OperationSemantics: &protocol.OperationSemantics{Impact: impact, Reason: reason,
			Access: protocol.ChangeSemantics_UNCHANGED, AccessReason: "A table statement grants and revokes nothing"}}, nil
}

// Decode reads an operation payload, refusing a field the model does not
// have.
func Decode(payload []byte) (Operation, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var operation Operation
	if err := decoder.Decode(&operation); err != nil {
		return Operation{}, fmt.Errorf("a Spanner table operation is not one the provider writes: %w", err)
	}
	if decoder.More() {
		return Operation{}, fmt.Errorf("a Spanner table operation carries data after its value")
	}
	return operation, nil
}

// Statement writes the SQL of a normalized operation value. It normalizes the
// value again and refuses one whose stored form differs, so a value changed
// after planning never runs.
func Statement(captured *protocol.NormalizedValue) (string, error) {
	normalized, err := Normalize(&protocol.ValueInput{Id: captured.GetId(), Representation: protocol.ValueInput_OPERATION, Value: captured.GetValue()})
	if err != nil {
		return "", err
	}
	if !proto.Equal(normalized, captured) {
		return "", fmt.Errorf("a Spanner table operation changed after it was planned")
	}
	return string(normalized.GetCanonicalProjection()), nil
}

// fields lists the operands an action reads.
var fields = map[Action][]string{
	CreateTable:      {"columns", "primaryKey", "rowDeletion"},
	DropTable:        {},
	AddColumn:        {"column"},
	DropColumn:       {"name"},
	AlterColumnType:  {"name", "type"},
	SetDefault:       {"name", "default"},
	DropDefault:      {"name"},
	SetNotNull:       {"name"},
	DropNotNull:      {"name"},
	CreateIndex:      {"index"},
	DropIndex:        {"name"},
	AddForeignKey:    {"foreignKey"},
	AddCheck:         {"name", "expression"},
	AddRowDeletion:   {"rowDeletion"},
	AlterRowDeletion: {"rowDeletion"},
	DropRowDeletion:  {},
	CreateSchema:     {"name"},
}

// present lists the operands an operation sets.
func (o Operation) present() []string {
	var names []string
	for name, set := range map[string]bool{
		"columns": len(o.Columns) != 0, "primaryKey": len(o.PrimaryKey) != 0, "column": o.Column != nil, "name": o.Name != "", "type": o.Type != "",
		"default": o.Default != "", "expression": o.Expression != "", "index": o.Index != nil, "foreignKey": o.ForeignKey != nil,
		"rowDeletion": o.RowDeletion != nil,
	} {
		if set {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

// statement checks the operands of o and writes its SQL for the table
// schema.table.
func (o Operation) statement(schema, table string) (string, error) {
	allowed, known := fields[o.Action]
	if !known {
		return "", fmt.Errorf("a Spanner table operation has the unknown action %q", o.Action)
	}
	for _, name := range o.present() {
		if !slices.Contains(allowed, name) {
			return "", fmt.Errorf("a %s operation does not take %s", o.Action, name)
		}
	}
	switch o.Action {
	case CreateTable:
		columns := make([]ddl.Column, len(o.Columns))
		for i, column := range o.Columns {
			columns[i] = column.ddl()
		}
		return ddl.CreateTable(schema, table, columns, o.PrimaryKey, o.RowDeletion.ddl())
	case DropTable:
		return ddl.DropTable(schema, table), nil
	case AddRowDeletion, AlterRowDeletion:
		return o.rowDeletionStatement(schema, table)
	case DropRowDeletion:
		return ddl.DropTTL(schema, table), nil
	case AddColumn:
		if o.Column == nil {
			return "", fmt.Errorf("an add-column operation names no column")
		}
		return ddl.AddColumn(schema, table, o.Column.ddl())
	case CreateIndex:
		if o.Index == nil {
			return "", fmt.Errorf("a create-index operation names no index")
		}
		return ddl.CreateIndex(schema, table, o.Index.ddl())
	case AddForeignKey:
		if o.ForeignKey == nil {
			return "", fmt.Errorf("an add-foreign-key operation names no constraint")
		}
		return ddl.AddForeignKey(schema, table, o.ForeignKey.ddl())
	}
	return o.namedStatement(schema, table)
}

// rowDeletionStatement writes the ADD TTL or ALTER TTL of an
// add-row-deletion or alter-row-deletion.
func (o Operation) rowDeletionStatement(schema, table string) (string, error) {
	if o.RowDeletion == nil {
		return "", fmt.Errorf("a %s operation names no row deletion policy", o.Action)
	}
	if o.Action == AddRowDeletion {
		return ddl.AddTTL(schema, table, *o.RowDeletion.ddl())
	}
	return ddl.AlterTTL(schema, table, *o.RowDeletion.ddl())
}

// namedStatement writes the SQL of an action that names one column, index or
// constraint of the table schema.table.
func (o Operation) namedStatement(schema, table string) (string, error) {
	if o.Name == "" {
		return "", fmt.Errorf("a %s operation names nothing", o.Action)
	}
	switch o.Action {
	case DropColumn:
		return ddl.DropColumn(schema, table, o.Name), nil
	case AlterColumnType:
		return ddl.AlterColumnType(schema, table, o.Name, o.Type)
	case SetDefault:
		return ddl.SetDefault(schema, table, o.Name, o.Default)
	case DropDefault:
		return ddl.DropDefault(schema, table, o.Name), nil
	case SetNotNull:
		return ddl.SetNotNull(schema, table, o.Name), nil
	case DropNotNull:
		return ddl.DropNotNull(schema, table, o.Name), nil
	case DropIndex:
		return ddl.DropIndex(schema, o.Name), nil
	case CreateSchema:
		return ddl.CreateSchema(o.Name), nil
	default:
		return ddl.AddCheck(schema, table, o.Name, o.Expression)
	}
}

// impact says what o does to the rows of its table, and why.
func (o Operation) impact() (protocol.ChangeSemantics_Impact, string) {
	switch o.Action {
	case DropTable:
		return protocol.ChangeSemantics_DESTRUCTIVE, "Drops a table and its rows"
	case DropColumn:
		return protocol.ChangeSemantics_DESTRUCTIVE, "Drops a column and its values"
	case AlterColumnType:
		return protocol.ChangeSemantics_BEHAVIORAL, "Changes the type of a column; Spanner converts every value and refuses a value the new type cannot hold"
	case SetNotNull:
		return protocol.ChangeSemantics_BEHAVIORAL, "Makes a column NOT NULL; Spanner refuses it while a row holds NULL there"
	case AddCheck:
		return protocol.ChangeSemantics_BEHAVIORAL, "Adds a CHECK constraint; Spanner refuses it while a row breaks it"
	case AddForeignKey:
		return protocol.ChangeSemantics_BEHAVIORAL, "Adds a foreign key; Spanner refuses it while a row breaks it"
	case CreateIndex:
		if o.Index != nil && o.Index.Unique {
			return protocol.ChangeSemantics_BEHAVIORAL, "Creates a unique index; Spanner refuses it while two rows share a key"
		}
		return protocol.ChangeSemantics_ADDITIVE, "Creates an index"
	case DropIndex:
		return protocol.ChangeSemantics_ADDITIVE, "Drops an index; no row changes"
	case CreateTable:
		return protocol.ChangeSemantics_ADDITIVE, "Creates a table"
	case CreateSchema:
		return protocol.ChangeSemantics_ADDITIVE, "Creates a schema"
	case AddRowDeletion, AlterRowDeletion:
		return protocol.ChangeSemantics_BEHAVIORAL, "Sets a row deletion policy; Spanner deletes the rows older than its interval"
	case DropRowDeletion:
		return protocol.ChangeSemantics_ADDITIVE, "Removes a row deletion policy; Spanner stops deleting rows"
	case AddColumn:
		return protocol.ChangeSemantics_ADDITIVE, "Adds a column"
	default:
		return protocol.ChangeSemantics_ADDITIVE, "Changes a column's default or makes it nullable; no row changes"
	}
}

func (c Column) ddl() ddl.Column {
	return ddl.Column{Name: c.Name, Type: c.Type, Nullable: c.Nullable, Default: c.Default, Check: c.Check, CheckName: c.CheckName}
}

// ddl is the policy as ddl writes it, nil for none.
func (r *RowDeletion) ddl() *ddl.TTL {
	if r == nil {
		return nil
	}
	return &ddl.TTL{Column: r.Column, Interval: r.Interval}
}

func (i Index) ddl() ddl.Index {
	keys := make([]ddl.IndexKey, len(i.Keys))
	for n, key := range i.Keys {
		keys[n] = ddl.IndexKey{Column: key.Column, Descending: key.Descending, Nulls: key.Nulls}
	}
	return ddl.Index{Name: i.Name, Unique: i.Unique, Keys: keys, Storing: i.Storing}
}

func (k ForeignKey) ddl() ddl.ForeignKey {
	return ddl.ForeignKey{Name: k.Name, Columns: k.Columns, RefSchema: k.RefSchema, RefTable: k.RefTable, RefColumns: k.RefColumns,
		OnDelete: strings.ToUpper(k.OnDelete)}
}
