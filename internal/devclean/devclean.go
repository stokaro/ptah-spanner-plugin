// Package devclean decides what a cleanup of a Spanner dev database removes,
// in which order, with which statements, and what stops it. It reads a
// snapshot of the database the backend took, a Catalog, and runs no
// statement itself.
//
// The snapshot has two halves. information_schema names the tables, views,
// indexes, foreign keys, change streams and schemas with their relations,
// and the database's DDL, as PGAdapter's SHOW DATABASE DDL prints it, names
// every object the database holds. The DDL is the inventory: the emulator's
// information_schema.sequences answers no rows while a sequence exists, so
// the sequences and the locality groups come from the DDL alone, and a
// statement of a kind the
// provider has no drop for is an object that stops the cleanup rather than a
// leftover the next run trips over.
package devclean

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/stokaro/ptah-spanner-plugin/internal/ddl"
	"github.com/stokaro/ptah-spanner-plugin/internal/sqltext"
)

// The kinds of the objects a cleanup lists, as a dev object reference names
// them.
const (
	KindChangeStream = "change stream"
	KindView         = "view"
	KindForeignKey   = "foreign key"
	KindSearchIndex  = "search index"
	KindIndex        = "index"
	KindTable        = "table"
	KindSequence     = "sequence"
	KindLocality     = "locality group"
	KindSchema       = "schema"
)

// Catalog is what the backend read from one database.
type Catalog struct {
	// Tables are the base tables and views of information_schema.tables.
	Tables []Table
	// Indexes are the rows of information_schema.indexes.
	Indexes []Index
	// ForeignKeys are the foreign keys of information_schema.table_constraints,
	// each with the table it references.
	ForeignKeys []ForeignKey
	// ChangeStreams are the rows of information_schema.change_streams.
	ChangeStreams []Object
	// Schemas are the names of information_schema.schemata.
	Schemas []string
	// DDL holds the statements of SHOW DATABASE DDL, one per row.
	DDL []string
	// Locks reports that public.ptah_locks is the provider's lock table, in
	// its exact layout, which a cleanup leaves alone.
	Locks bool
}

// Table is one row of information_schema.tables.
type Table struct {
	Schema, Name string
	// View reports a view rather than a base table.
	View bool
	// Parent is the table this one is interleaved in, in the same schema.
	Parent string
}

// Index is one row of information_schema.indexes.
type Index struct {
	Schema, Table, Name string
	// Type is the index_type column: INDEX, SEARCH or PRIMARY_KEY.
	Type string
	// Managed reports an index Spanner keeps for a foreign key itself.
	Managed bool
}

// ForeignKey is one foreign key and the table it references.
type ForeignKey struct {
	Schema, Table, Name string
	// RefSchema and RefTable name the referenced table.
	RefSchema, RefTable string
}

// Object is one object a cleanup removes or stops at.
type Object struct {
	Kind, Schema, Name string
	// Table is the table an index or a foreign key belongs to.
	Table string
	// Refusal says why the cleanup cannot remove the object. An object with a
	// refusal stops the whole cleanup before it removes anything.
	Refusal string
}

// ErrInconsistent reports a snapshot whose two halves disagree, such as a
// table the DDL creates that information_schema does not list. The backend
// read them one after the other, and a database that changed between the two
// reads has no list a cleanup can trust.
var ErrInconsistent = errors.New("the database changed while the provider read it")

// lockTable is the provider's lock table; see internal/locktable.
const lockTable = "ptah_locks"

// Objects lists what a cleanup of the schemas in scope removes or stops at,
// in the order the cleanup removes them, or every schema when scope is
// empty. A whole-database cleanup also removes every schema but public, which
// Spanner keeps; a cleanup of named schemas empties them and keeps them.
func Objects(catalog Catalog, scope []string) ([]Object, error) {
	inScope := func(schema string) bool { return len(scope) == 0 || slices.Contains(scope, schema) }
	declared, err := declaredObjects(catalog.DDL)
	if err != nil {
		return nil, err
	}
	if err := consistent(catalog, declared); err != nil {
		return nil, err
	}
	keys := slices.Clone(catalog.ForeignKeys)
	slices.SortFunc(keys, func(a, b ForeignKey) int {
		return cmp.Or(cmp.Compare(a.Schema, b.Schema), cmp.Compare(a.Table, b.Table), cmp.Compare(a.Name, b.Name))
	})
	candidates := slices.Concat(refusedOnce(declared), streamObjects(catalog.ChangeStreams), viewObjects(catalog.Tables), keyObjects(keys),
		indexObjects(catalog.Indexes), tableObjects(catalog.Tables, keys, inScope), declaredOf(declared, KindSequence), declaredOf(declared, KindLocality))
	if len(scope) == 0 {
		candidates = append(candidates, schemaObjects(catalog.Schemas)...)
	}
	var objects []Object
	for _, object := range candidates {
		ownLocks := catalog.Locks && object.Kind == KindTable && object.Schema == ddl.DefaultSchema && object.Name == lockTable
		if inScope(object.Schema) && !ownLocks {
			objects = append(objects, object)
		}
	}
	return objects, nil
}

// refusedOnce returns each refused object of the DDL once: two privileges
// granted to one role are one object.
func refusedOnce(declared []Object) []Object {
	seen := make(map[Object]bool)
	var refused []Object
	for _, object := range declared {
		if object.Refusal != "" && !seen[object] {
			seen[object] = true
			refused = append(refused, object)
		}
	}
	return refused
}

// streamObjects are the change streams, by schema and name.
func streamObjects(streams []Object) []Object {
	var objects []Object
	for _, stream := range sorted(streams) {
		objects = append(objects, Object{Kind: KindChangeStream, Schema: stream.Schema, Name: stream.Name})
	}
	return objects
}

// viewObjects are the views, by schema and name.
func viewObjects(tables []Table) []Object {
	var objects []Object
	for _, table := range sortedTables(tables) {
		if table.View {
			objects = append(objects, Object{Kind: KindView, Schema: table.Schema, Name: table.Name})
		}
	}
	return objects
}

// keyObjects are the foreign keys, in the order of keys.
func keyObjects(keys []ForeignKey) []Object {
	var objects []Object
	for _, key := range keys {
		objects = append(objects, Object{Kind: KindForeignKey, Schema: key.Schema, Name: key.Name, Table: key.Table})
	}
	return objects
}

// indexObjects are the search indexes and then the other secondary indexes,
// each by schema and name. A primary key and an index Spanner keeps for a
// foreign key go with their table.
func indexObjects(indexes []Index) []Object {
	ordered := slices.Clone(indexes)
	slices.SortFunc(ordered, func(a, b Index) int { return cmp.Or(cmp.Compare(a.Schema, b.Schema), cmp.Compare(a.Name, b.Name)) })
	var objects []Object
	for _, kind := range []string{"SEARCH", "INDEX"} {
		for _, index := range ordered {
			if index.Type != kind || index.Managed {
				continue
			}
			object := Object{Kind: KindIndex, Schema: index.Schema, Name: index.Name, Table: index.Table}
			if kind == "SEARCH" {
				object.Kind = KindSearchIndex
			}
			objects = append(objects, object)
		}
	}
	return objects
}

// tableObjects are the base tables in the order Spanner drops them. A table
// of the scope that a foreign key outside the scope references carries a
// refusal.
func tableObjects(tables []Table, keys []ForeignKey, inScope func(string) bool) []Object {
	var base []Table
	for _, table := range tables {
		if !table.View {
			base = append(base, table)
		}
	}
	var objects []Object
	for _, table := range interleaveOrder(base) {
		object := Object{Kind: KindTable, Schema: table.Schema, Name: table.Name}
		for _, key := range keys {
			if inScope(table.Schema) && key.RefSchema == table.Schema && key.RefTable == table.Name && !inScope(key.Schema) {
				object.Refusal = "a foreign key of a table outside the cleanup's schemas references it"
			}
		}
		objects = append(objects, object)
	}
	return objects
}

// declaredOf are the objects of kind the DDL creates, in its order.
func declaredOf(declared []Object, kind string) []Object {
	var objects []Object
	for _, object := range declared {
		if object.Kind == kind {
			objects = append(objects, object)
		}
	}
	return objects
}

// schemaObjects are the user's schemas but public, by name.
func schemaObjects(schemas []string) []Object {
	var objects []Object
	for _, schema := range slices.Sorted(slices.Values(schemas)) {
		if userSchema(schema) && schema != ddl.DefaultSchema {
			objects = append(objects, Object{Kind: KindSchema, Schema: schema, Name: schema})
		}
	}
	return objects
}

// Blocking returns the objects that stop a cleanup.
func Blocking(objects []Object) []Object {
	var blocking []Object
	for _, object := range objects {
		if object.Refusal != "" {
			blocking = append(blocking, object)
		}
	}
	return blocking
}

// Statement is the statement that removes object. A view, a sequence and an
// index are named in their schema, a foreign key on its table, and Spanner's
// PostgreSQL interface writes none of them with CASCADE, which it refuses.
func Statement(object Object) (string, error) {
	switch object.Kind {
	case KindChangeStream:
		return "DROP CHANGE STREAM " + ddl.Name(object.Schema, object.Name), nil
	case KindView:
		return "DROP VIEW " + ddl.Name(object.Schema, object.Name), nil
	case KindForeignKey:
		return "ALTER TABLE " + ddl.Name(object.Schema, object.Table) + " DROP CONSTRAINT " + ddl.Quote(object.Name), nil
	case KindSearchIndex:
		return "DROP SEARCH INDEX " + ddl.Name(object.Schema, object.Name), nil
	case KindIndex:
		return "DROP INDEX " + ddl.Name(object.Schema, object.Name), nil
	case KindTable:
		return "DROP TABLE " + ddl.Name(object.Schema, object.Name), nil
	case KindSequence:
		return "DROP SEQUENCE " + ddl.Name(object.Schema, object.Name), nil
	case KindLocality:
		return "DROP LOCALITY GROUP " + ddl.Quote(object.Name), nil
	case KindSchema:
		return "DROP SCHEMA " + ddl.Quote(object.Name), nil
	}
	return "", fmt.Errorf("the provider has no statement that removes a %s", object.Kind)
}

// userSchema reports a schema the user can hold objects in: not one the
// server keeps for its catalogs.
func userSchema(schema string) bool {
	return !slices.Contains([]string{"information_schema", "pg_catalog", "spanner_sys"}, schema)
}

// consistent checks that the DDL creates exactly the tables, views, indexes,
// change streams and schemas information_schema lists, so neither half of the
// snapshot leaves out an object the other holds.
func consistent(catalog Catalog, declared []Object) error {
	type key struct{ kind, schema, name string }
	listed := make(map[key]bool)
	for _, table := range catalog.Tables {
		kind := KindTable
		if table.View {
			kind = KindView
		}
		listed[key{kind, table.Schema, table.Name}] = true
	}
	for _, index := range catalog.Indexes {
		if index.Type != "PRIMARY_KEY" && !index.Managed {
			// The emulator prints a search index's DDL as CREATE INDEX, so the
			// two index kinds are compared as one.
			listed[key{KindIndex, index.Schema, index.Name}] = true
		}
	}
	for _, stream := range catalog.ChangeStreams {
		listed[key{KindChangeStream, stream.Schema, stream.Name}] = true
	}
	for _, schema := range catalog.Schemas {
		if userSchema(schema) && schema != ddl.DefaultSchema {
			listed[key{KindSchema, schema, schema}] = true
		}
	}
	created := make(map[key]bool)
	for _, object := range declared {
		kind := object.Kind
		if kind == KindSearchIndex {
			kind = KindIndex
		}
		if kind == KindSequence || kind == KindLocality || object.Refusal != "" {
			continue
		}
		created[key{kind, object.Schema, object.Name}] = true
	}
	for found := range listed {
		if !created[found] {
			return fmt.Errorf("%w: information_schema lists a %s the DDL does not create", ErrInconsistent, found.kind)
		}
	}
	for found := range created {
		if !listed[found] {
			return fmt.Errorf("%w: the DDL creates a %s information_schema does not list", ErrInconsistent, found.kind)
		}
	}
	return nil
}

// declaredObjects reads the object each DDL statement creates. A statement
// that alters a table creates nothing of its own. A statement of a kind the
// provider has no drop for is an object with a refusal.
func declaredObjects(statements []string) ([]Object, error) {
	var objects []Object
	for _, statement := range statements {
		source, words, err := sqltext.Words(statement)
		if err != nil {
			return nil, fmt.Errorf("the provider cannot read the database's DDL: %w", err)
		}
		if len(words) == 0 {
			continue
		}
		object, err := declaredObject(source, words)
		if err != nil {
			return nil, err
		}
		if object.Kind != "" {
			objects = append(objects, object)
		}
	}
	return objects, nil
}

// declaredObject reads the object one DDL statement creates. An ALTER
// statement creates none: it states an option of the database or of an
// object another statement creates.
func declaredObject(source, words []string) (Object, error) {
	if words[0] == "ALTER" {
		return Object{}, nil
	}
	if words[0] != "CREATE" {
		return refusedObject(source, words), nil
	}
	head := 1
	for head < len(words) && slices.Contains([]string{"OR", "REPLACE", "UNIQUE"}, words[head]) {
		head++
	}
	kinds := []struct {
		words []string
		kind  string
	}{
		{[]string{"TABLE"}, KindTable},
		{[]string{"INDEX"}, KindIndex},
		{[]string{"SEARCH", "INDEX"}, KindSearchIndex},
		{[]string{"VIEW"}, KindView},
		{[]string{"SEQUENCE"}, KindSequence},
		{[]string{"LOCALITY", "GROUP"}, KindLocality},
		{[]string{"CHANGE", "STREAM"}, KindChangeStream},
		{[]string{"SCHEMA"}, KindSchema},
	}
	for _, candidate := range kinds {
		end := head + len(candidate.words)
		if end > len(words) || !slices.Equal(words[head:end], candidate.words) {
			continue
		}
		at := end
		if at+2 < len(words) && words[at] == "IF" && words[at+1] == "NOT" && words[at+2] == "EXISTS" {
			at += 3
		}
		schema, name, ok := qualifiedName(source, at)
		if !ok {
			return Object{}, fmt.Errorf("the provider cannot read the name a CREATE %s statement of the database's DDL gives", strings.Join(candidate.words, " "))
		}
		switch candidate.kind {
		case KindSchema:
			schema = name
		case KindLocality:
			// A locality group belongs to the database, not to a schema.
			schema = ""
		}
		return Object{Kind: candidate.kind, Schema: schema, Name: name}, nil
	}
	return refusedObject(source, words), nil
}

// twoWordKinds are the kinds of object Spanner names in two words, which a
// statement the provider has no drop for may create.
var twoWordKinds = []string{"PROPERTY GRAPH", "VECTOR INDEX"}

// refusedObject is the object a statement of a kind the provider has no
// drop for creates. A CREATE statement names its kind in its second word, or
// two words, and the object after it, such as CREATE ROLE; another statement
// names its kind in its first word, and a GRANT or a REVOKE names the role it
// grants to last.
func refusedObject(source, words []string) Object {
	kind, at := strings.ToLower(words[0]), 1
	switch {
	case words[0] == "CREATE" && len(words) > 2 && slices.Contains(twoWordKinds, words[1]+" "+words[2]):
		kind, at = strings.ToLower(words[1]+" "+words[2]), 3
	case words[0] == "CREATE" && len(words) > 1:
		kind, at = strings.ToLower(words[1]), 2
	case words[0] == "GRANT" || words[0] == "REVOKE":
		at = len(source) - 1
	}
	name := kind
	if at < len(source) && isName(source[at]) {
		name = sqltext.Unquote(source[at])
	}
	return Object{Kind: kind, Schema: ddl.DefaultSchema, Name: name,
		Refusal: "the provider has no statement that removes this kind of object from a dev database"}
}

// qualifiedName reads a name at source[at], qualified by a schema or not. An
// unqualified name is in the default schema.
func qualifiedName(source []string, at int) (schema, name string, ok bool) {
	if at >= len(source) || !isName(source[at]) {
		return "", "", false
	}
	if at+2 < len(source) && source[at+1] == "." && isName(source[at+2]) {
		return sqltext.Unquote(source[at]), sqltext.Unquote(source[at+2]), true
	}
	return ddl.DefaultSchema, sqltext.Unquote(source[at]), true
}

// isName reports a token that spells a name: a word or a quoted name.
func isName(token string) bool {
	if token == "" {
		return false
	}
	first := token[0]
	return first == '"' || first == '_' || first >= 'a' && first <= 'z' || first >= 'A' && first <= 'Z'
}

// sorted orders objects by schema and name.
func sorted(objects []Object) []Object {
	result := slices.Clone(objects)
	slices.SortFunc(result, func(a, b Object) int { return cmp.Or(cmp.Compare(a.Schema, b.Schema), cmp.Compare(a.Name, b.Name)) })
	return result
}

// sortedTables orders tables by schema and name.
func sortedTables(tables []Table) []Table {
	result := slices.Clone(tables)
	slices.SortFunc(result, func(a, b Table) int { return cmp.Or(cmp.Compare(a.Schema, b.Schema), cmp.Compare(a.Name, b.Name)) })
	return result
}

// interleaveOrder orders base tables so that an interleaved table comes
// before the table it is interleaved in, which Spanner refuses to drop while
// a child exists: deepest first, then by schema and name.
func interleaveOrder(tables []Table) []Table {
	parents := make(map[[2]string]string, len(tables))
	for _, table := range tables {
		parents[[2]string{table.Schema, table.Name}] = table.Parent
	}
	depth := func(table Table) int {
		level := 0
		for parent := table.Parent; parent != "" && level <= len(tables); level++ {
			parent = parents[[2]string{table.Schema, parent}]
		}
		return level
	}
	result := sortedTables(tables)
	slices.SortStableFunc(result, func(a, b Table) int { return cmp.Compare(depth(b), depth(a)) })
	return result
}
