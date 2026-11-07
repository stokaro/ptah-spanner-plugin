// Package catalog reads the tables of a Spanner database through
// information_schema, as PGAdapter answers it, into plain values.
//
// The PostgreSQL interface emulates pg_catalog only in part (pg_index and
// pg_depend do not exist), so every query here reads information_schema,
// including the views Spanner adds there: spanner_type, spanner_is_managed and
// row_deletion_policy_expression.
//
// A read describes what it returns completely or fails with an *Unsupported
// error that says what it found and does not describe. Ptah plans to create
// what a read leaves out, so leaving an object out silently would plan a
// change the database refuses or, worse, accepts. The foreign keys and CHECKs
// a table holds are read whole; a caller that cannot pass them on refuses
// them itself.
package catalog

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/stokaro/ptah-spanner-plugin/internal/ddl"
)

// Queryer runs a catalog query. *pgx.Conn implements it.
type Queryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Table is one base table with what the provider describes of it.
type Table struct {
	Schema  string
	Name    string
	Columns []Column
	// PrimaryKeyName is the name of the primary key constraint, and
	// PrimaryKey its columns in key order.
	PrimaryKeyName string
	PrimaryKey     []string
	// Indexes are the secondary indexes a user created, in name order. The
	// indexes Spanner creates for itself, such as the one backing a foreign
	// key, are left out.
	Indexes []Index
	// Constraints are the foreign keys and CHECKs the table holds, in name
	// order. The CHECKs Spanner keeps for its NOT NULL columns are left out:
	// the columns' nullability says them.
	Constraints []Constraint
}

// The constraint types a read reports.
const (
	ForeignKey = "FOREIGN KEY"
	Check      = "CHECK"
)

// Constraint is a foreign key or a CHECK a table holds.
type Constraint struct {
	Name string
	// Type is ForeignKey or Check.
	Type string
	// Columns are a foreign key's columns in key order.
	Columns []string
	// RefSchema and RefTable name the table a foreign key references, and
	// RefColumns its columns, each paired with the column of Columns at the
	// same position.
	RefSchema, RefTable string
	RefColumns          []string
	// DeleteRule and UpdateRule are a foreign key's rules in the spelling of
	// information_schema, such as NO ACTION or CASCADE.
	DeleteRule, UpdateRule string
	// Clause is a CHECK's expression as the server keeps it, such as
	// `(score >= '0'::bigint)` for a declared `score >= 0`.
	Clause string
}

// Column is one column of a table.
type Column struct {
	Name    string
	Ordinal int64
	// DataType is information_schema.columns.data_type, and SpannerType the
	// full spelling Spanner keeps, with a length or an array element type.
	DataType    string
	SpannerType string
	Nullable    bool
	// Default is the default expression as the server stores it, or nil.
	Default *string
	// MaxLength is the declared length of a character varying column.
	MaxLength *int64
}

// Index is one secondary index.
type Index struct {
	Name   string
	Unique bool
	Keys   []IndexKey
	// Storing are the columns the index stores beside its keys, in the order
	// the catalog lists them.
	Storing []string
}

// IndexKey is one key part of an index.
type IndexKey struct {
	Column     string
	Descending bool
	// Nulls is FIRST or LAST where the key's NULLS ordering differs from the
	// default for its direction, and empty where it is the default; see
	// ddl.IndexKey.StatedNulls.
	Nulls string
}

// Unsupported reports an object a read found and does not describe. Reason
// names the object and why; it quotes schema names, never data.
type Unsupported struct {
	Reason string
}

func (e *Unsupported) Error() string { return e.Reason }

// Read reads every base table of the named schemas, in schema then name order.
// A schema that holds no table, or does not exist, contributes nothing.
func Read(ctx context.Context, q Queryer, schemas []string) ([]Table, error) {
	var tables []Table
	for _, schema := range slices.Sorted(slices.Values(schemas)) {
		if err := refuseUndescribed(ctx, q, schema); err != nil {
			return nil, err
		}
		found, err := baseTables(ctx, q, schema)
		if err != nil {
			return nil, err
		}
		for i := range found {
			if err := readTable(ctx, q, &found[i]); err != nil {
				return nil, err
			}
		}
		if err := readConstraints(ctx, q, schema, found); err != nil {
			return nil, err
		}
		tables = append(tables, found...)
	}
	return tables, nil
}

// baseTables lists the base tables of a schema. A table interleaved in a
// parent, or carrying a row deletion policy, is refused: the provider does
// not describe either yet.
func baseTables(ctx context.Context, q Queryer, schema string) ([]Table, error) {
	rows, err := q.Query(ctx, `SELECT table_schema, table_name, parent_table_name, row_deletion_policy_expression
FROM information_schema.tables
WHERE table_type = 'BASE TABLE' AND table_schema = $1
ORDER BY table_name`, schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tables []Table
	for rows.Next() {
		var schema, name string
		var parent, policy *string
		if err := rows.Scan(&schema, &name, &parent, &policy); err != nil {
			return nil, err
		}
		if parent != nil && *parent != "" {
			return nil, &Unsupported{Reason: fmt.Sprintf("table %s is interleaved in a parent table, which the provider does not describe yet", qualified(schema, name))}
		}
		if policy != nil && *policy != "" {
			return nil, &Unsupported{Reason: fmt.Sprintf("table %s has a row deletion policy, which the provider does not describe yet", qualified(schema, name))}
		}
		tables = append(tables, Table{Schema: schema, Name: name})
	}
	return tables, rows.Err()
}

// refuseUndescribed fails the read when a schema holds an object the
// provider does not describe yet: a view.
func refuseUndescribed(ctx context.Context, q Queryer, schema string) error {
	view, err := firstName(ctx, q, `SELECT table_schema, table_name FROM information_schema.tables
WHERE table_type = 'VIEW' AND table_schema = $1 ORDER BY table_name LIMIT 1`, schema)
	if err != nil {
		return err
	}
	if view != "" {
		return &Unsupported{Reason: fmt.Sprintf("view %s is not described by the provider yet", view)}
	}
	return nil
}

// readConstraints reads the foreign keys and CHECKs of a schema's tables and
// gives each table its own.
func readConstraints(ctx context.Context, q Queryer, schema string, tables []Table) error {
	byTable := make(map[string]*Table, len(tables))
	for i := range tables {
		byTable[tables[i].Name] = &tables[i]
	}
	keys, err := readForeignKeys(ctx, q, schema)
	if err != nil {
		return err
	}
	checks, err := readChecks(ctx, q, schema)
	if err != nil {
		return err
	}
	for _, held := range slices.Concat(keys, checks) {
		if table := byTable[held.table]; table != nil {
			table.Constraints = append(table.Constraints, held.Constraint)
		}
	}
	for _, table := range tables {
		slices.SortFunc(table.Constraints, func(a, b Constraint) int { return strings.Compare(a.Name, b.Name) })
	}
	return nil
}

// heldConstraint is a constraint of the table it names.
type heldConstraint struct {
	Constraint
	table string
}

// readForeignKeys reads the foreign keys of a schema's tables. Each key column
// is paired with the column of the referenced key at the position
// position_in_unique_constraint names, as the SQL standard defines it: the
// referenced key is the primary key or the unique index Spanner builds for the
// referenced columns, which referential_constraints names.
//
// The emulator behind PGAdapter 0.56.2 answers that position in the order the
// foreign key lists its columns, so a composite key that lists the referenced
// columns in another order than the referenced key reads back paired in the
// key's order (stokaro/ptah#4204 S78). Ptah's built-in Spanner engine read the
// same pairing.
func readForeignKeys(ctx context.Context, q Queryer, schema string) ([]heldConstraint, error) {
	rows, err := q.Query(ctx, `SELECT tc.table_name, tc.constraint_name, kcu.column_name, rkcu.table_schema, rkcu.table_name, rkcu.column_name,
  rc.delete_rule, rc.update_rule, rc.spanner_state
FROM information_schema.table_constraints tc
JOIN information_schema.referential_constraints rc
  ON rc.constraint_schema = tc.constraint_schema AND rc.constraint_name = tc.constraint_name
JOIN information_schema.key_column_usage kcu
  ON kcu.constraint_schema = tc.constraint_schema AND kcu.constraint_name = tc.constraint_name
  AND kcu.table_schema = tc.table_schema AND kcu.table_name = tc.table_name
LEFT JOIN information_schema.key_column_usage rkcu
  ON rkcu.constraint_schema = rc.unique_constraint_schema AND rkcu.constraint_name = rc.unique_constraint_name
  AND rkcu.ordinal_position = kcu.position_in_unique_constraint
WHERE tc.table_schema = $1 AND tc.constraint_type = 'FOREIGN KEY'
ORDER BY tc.table_name, tc.constraint_name, kcu.ordinal_position`, schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []heldConstraint
	for rows.Next() {
		var table, name, column string
		var refSchema, refTable, refColumn, deleteRule, updateRule, state *string
		if err := rows.Scan(&table, &name, &column, &refSchema, &refTable, &refColumn, &deleteRule, &updateRule, &state); err != nil {
			return nil, err
		}
		where := fmt.Sprintf("foreign key %q of table %s", name, qualified(schema, table))
		switch {
		case refSchema == nil || refTable == nil || refColumn == nil:
			return nil, &Unsupported{Reason: where + " references a key the catalog does not report"}
		case state == nil || *state != "COMMITTED":
			return nil, &Unsupported{Reason: where + " is still being validated; read again once Spanner reports it ready"}
		}
		if n := len(keys); n == 0 || keys[n-1].table != table || keys[n-1].Name != name {
			keys = append(keys, heldConstraint{table: table, Constraint: Constraint{Name: name, Type: ForeignKey, RefSchema: *refSchema, RefTable: *refTable,
				DeleteRule: text(deleteRule), UpdateRule: text(updateRule)}})
		}
		key := &keys[len(keys)-1]
		if key.RefSchema != *refSchema || key.RefTable != *refTable {
			return nil, &Unsupported{Reason: where + " references more than one table"}
		}
		key.Columns = append(key.Columns, column)
		key.RefColumns = append(key.RefColumns, *refColumn)
	}
	return keys, rows.Err()
}

// readChecks reads the CHECKs of a schema's tables, each clause as the server
// keeps it, less the ones Spanner keeps for its NOT NULL columns.
func readChecks(ctx context.Context, q Queryer, schema string) ([]heldConstraint, error) {
	rows, err := q.Query(ctx, `SELECT tc.table_name, tc.constraint_name, cc.check_clause, cc.spanner_state
FROM information_schema.table_constraints tc
JOIN information_schema.check_constraints cc
  ON cc.constraint_schema = tc.constraint_schema AND cc.constraint_name = tc.constraint_name
WHERE tc.table_schema = $1 AND tc.constraint_type = 'CHECK'
ORDER BY tc.table_name, tc.constraint_name`, schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var checks []heldConstraint
	for rows.Next() {
		var table, name string
		var clause, state *string
		if err := rows.Scan(&table, &name, &clause, &state); err != nil {
			return nil, err
		}
		if notNullCheck(table, name, clause) {
			continue
		}
		where := fmt.Sprintf("CHECK constraint %q of table %s", name, qualified(schema, table))
		switch {
		case clause == nil || strings.TrimSpace(*clause) == "":
			return nil, &Unsupported{Reason: where + " reports no expression"}
		case state == nil || *state != "COMMITTED":
			return nil, &Unsupported{Reason: where + " is still being validated; read again once Spanner reports it ready"}
		}
		checks = append(checks, heldConstraint{table: table, Constraint: Constraint{Name: name, Type: Check, Clause: *clause}})
	}
	return checks, rows.Err()
}

func text(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// notNullCheck reports a check Spanner made for a NOT NULL column: it is named
// CK_IS_NOT_NULL_<table>_<column> and its clause is `<column> IS NOT NULL`.
// The column's nullability already says it, so it is not a constraint of its
// own.
func notNullCheck(table, name string, clause *string) bool {
	column, found := strings.CutPrefix(name, "CK_IS_NOT_NULL_"+table+"_")
	return found && clause != nil && *clause == column+" IS NOT NULL"
}

func readTable(ctx context.Context, q Queryer, table *Table) error {
	columns, err := readColumns(ctx, q, table)
	if err != nil {
		return err
	}
	table.Columns = columns
	if err := readPrimaryKey(ctx, q, table); err != nil {
		return err
	}
	indexes, err := readIndexes(ctx, q, table)
	if err != nil {
		return err
	}
	table.Indexes = indexes
	return nil
}

func readColumns(ctx context.Context, q Queryer, table *Table) ([]Column, error) {
	rows, err := q.Query(ctx, `SELECT column_name, ordinal_position, data_type, spanner_type, is_nullable, column_default,
  character_maximum_length, is_generated, is_identity
FROM information_schema.columns
WHERE table_schema = $1 AND table_name = $2
ORDER BY ordinal_position`, table.Schema, table.Name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var columns []Column
	for rows.Next() {
		var column Column
		var nullable, generated, identity string
		if err := rows.Scan(&column.Name, &column.Ordinal, &column.DataType, &column.SpannerType, &nullable, &column.Default,
			&column.MaxLength, &generated, &identity); err != nil {
			return nil, err
		}
		if generated != "NEVER" {
			return nil, &Unsupported{Reason: fmt.Sprintf("column %q of table %s is generated, which the provider does not describe yet",
				column.Name, qualified(table.Schema, table.Name))}
		}
		if identity != "NO" {
			return nil, &Unsupported{Reason: fmt.Sprintf("column %q of table %s is an identity column, which the provider does not describe yet; "+
				"Spanner adds one named rowid to a table created without a primary key", column.Name, qualified(table.Schema, table.Name))}
		}
		column.Nullable = nullable == "YES"
		columns = append(columns, column)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(columns) == 0 {
		return nil, &Unsupported{Reason: fmt.Sprintf("table %s reports no column", qualified(table.Schema, table.Name))}
	}
	return columns, nil
}

// readPrimaryKey reads the primary key: the columns of the index Spanner names
// PRIMARY_KEY, in key order, and the name of its constraint.
func readPrimaryKey(ctx context.Context, q Queryer, table *Table) error {
	rows, err := q.Query(ctx, `SELECT column_name FROM information_schema.index_columns
WHERE table_schema = $1 AND table_name = $2 AND index_name = 'PRIMARY_KEY' AND ordinal_position IS NOT NULL
ORDER BY ordinal_position`, table.Schema, table.Name)
	if err != nil {
		return err
	}
	keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	table.PrimaryKey = keys
	if len(keys) == 0 {
		return nil
	}
	rows, err = q.Query(ctx, `SELECT constraint_name FROM information_schema.table_constraints
WHERE table_schema = $1 AND table_name = $2 AND constraint_type = 'PRIMARY KEY'`, table.Schema, table.Name)
	if err != nil {
		return err
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	if len(names) != 1 {
		return &Unsupported{Reason: fmt.Sprintf("table %s reports %d primary key constraints for one key", qualified(table.Schema, table.Name), len(names))}
	}
	table.PrimaryKeyName = names[0]
	return nil
}

// readIndexes reads the secondary indexes a user created. An index Spanner
// manages, such as the one it builds for a foreign key, is not the user's to
// plan, so it is left out: planning its removal would be refused, and an
// export that wrote it would create a second index beside Spanner's.
func readIndexes(ctx context.Context, q Queryer, table *Table) ([]Index, error) {
	rows, err := q.Query(ctx, `SELECT index_name, index_type, is_unique, is_null_filtered, index_state, parent_table_name, filter
FROM information_schema.indexes
WHERE table_schema = $1 AND table_name = $2 AND index_type <> 'PRIMARY_KEY' AND spanner_is_managed = 'NO'
ORDER BY index_name`, table.Schema, table.Name)
	if err != nil {
		return nil, err
	}
	type header struct {
		name, kind, unique, nullFiltered string
		state, parent, filter            *string
	}
	headers, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (header, error) {
		var h header
		err := row.Scan(&h.name, &h.kind, &h.unique, &h.nullFiltered, &h.state, &h.parent, &h.filter)
		return h, err
	})
	if err != nil {
		return nil, err
	}
	indexes := make([]Index, 0, len(headers))
	for _, h := range headers {
		where := fmt.Sprintf("index %q of table %s", h.name, qualified(table.Schema, table.Name))
		switch {
		case h.kind != "INDEX":
			return nil, &Unsupported{Reason: fmt.Sprintf("%s is a %s index, which the provider does not describe yet", where, h.kind)}
		case h.nullFiltered != "NO":
			return nil, &Unsupported{Reason: where + " is null-filtered, which the provider does not describe yet"}
		case h.parent != nil && *h.parent != "":
			return nil, &Unsupported{Reason: where + " is interleaved in a parent table, which the provider does not describe yet"}
		case h.state == nil || *h.state != "READ_WRITE":
			return nil, &Unsupported{Reason: where + " is still being built; read again once Spanner reports it ready"}
		}
		index := Index{Name: h.name, Unique: h.unique == "YES"}
		if err := readIndexColumns(ctx, q, table, &index); err != nil {
			return nil, err
		}
		if h.filter != nil && *h.filter != "" && (!index.Unique || *h.filter != uniqueFilter(index.Keys)) {
			return nil, &Unsupported{Reason: where + " has a WHERE filter, which the provider does not describe yet"}
		}
		indexes = append(indexes, index)
	}
	return indexes, nil
}

func readIndexColumns(ctx context.Context, q Queryer, table *Table, index *Index) error {
	rows, err := q.Query(ctx, `SELECT column_name, ordinal_position, column_ordering
FROM information_schema.index_columns
WHERE table_schema = $1 AND table_name = $2 AND index_name = $3
ORDER BY ordinal_position NULLS LAST, column_name`, table.Schema, table.Name, index.Name)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var column string
		var ordinal *int64
		var ordering *string
		if err := rows.Scan(&column, &ordinal, &ordering); err != nil {
			return err
		}
		if ordinal == nil {
			index.Storing = append(index.Storing, column)
			continue
		}
		key, err := indexKey(column, ordering)
		if err != nil {
			return &Unsupported{Reason: fmt.Sprintf("index %q of table %s: %v", index.Name, qualified(table.Schema, table.Name), err)}
		}
		index.Keys = append(index.Keys, key)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(index.Keys) == 0 {
		return &Unsupported{Reason: fmt.Sprintf("index %q of table %s reports no key", index.Name, qualified(table.Schema, table.Name))}
	}
	return nil
}

// uniqueFilter is the filter Spanner gives a unique index of its own accord:
// every key column IS NOT NULL, joined by AND, in byte order of the column
// names and unquoted. It keeps PostgreSQL's rule that rows with a NULL key do
// not collide. Measured on the emulator behind PGAdapter 0.56.2: `CREATE
// UNIQUE INDEX u ON p (zed, "Abc", mid)` reads back with the filter `Abc IS
// NOT NULL AND mid IS NOT NULL AND zed IS NOT NULL`. A unique index whose
// filter is exactly this one is a plain unique index.
func uniqueFilter(keys []IndexKey) string {
	columns := make([]string, len(keys))
	for i, key := range keys {
		columns[i] = key.Column
	}
	slices.Sort(columns)
	conditions := make([]string, len(columns))
	for i, column := range columns {
		conditions[i] = column + " IS NOT NULL"
	}
	return strings.Join(conditions, " AND ")
}

// indexKey reads a key's column_ordering: ASC or DESC, followed by NULLS FIRST
// or NULLS LAST when the key names its ordering. The ordering is kept only
// where it differs from the direction's default: the server names it only
// there, and the index Spanner builds for a foreign key, which reads `ASC
// NULLS FIRST`, is not read at all.
func indexKey(column string, ordering *string) (IndexKey, error) {
	key := IndexKey{Column: column}
	if ordering == nil {
		return key, nil
	}
	words := strings.Fields(strings.ToUpper(*ordering))
	if len(words) > 0 && (words[0] == "ASC" || words[0] == "DESC") {
		key.Descending = words[0] == "DESC"
		words = words[1:]
	}
	switch {
	case len(words) == 0:
		return key, nil
	case len(words) == 2 && words[0] == "NULLS" && slices.Contains([]string{"FIRST", "LAST"}, words[1]):
		key.Nulls = ddl.IndexKey{Descending: key.Descending, Nulls: words[1]}.StatedNulls()
		return key, nil
	default:
		return IndexKey{}, fmt.Errorf("key %q has an ordering the provider cannot read", column)
	}
}

// firstName runs a query whose rows hold two text columns and returns the
// first row qualified as schema.name, or the empty string when there is none.
func firstName(ctx context.Context, q Queryer, query string, args ...any) (string, error) {
	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	if !rows.Next() {
		return "", rows.Err()
	}
	var schema, name string
	if err := rows.Scan(&schema, &name); err != nil {
		return "", err
	}
	return qualified(schema, name), rows.Err()
}

func qualified(schema, name string) string {
	return fmt.Sprintf("%q.%q", schema, name)
}
