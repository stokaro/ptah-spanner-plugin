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
// change the database refuses or, worse, accepts.
package catalog

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/stokaro/ptah-spanner-plugin/internal/rowdeletion"
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
	// RowDeletion is the table's row deletion policy, with the interval in
	// the spelling the server stored, or nil for none.
	RowDeletion *rowdeletion.Policy
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
	// Nulls is FIRST or LAST when the key names its NULLS ordering, and empty
	// when the key keeps the default for its direction.
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
		tables = append(tables, found...)
	}
	return tables, nil
}

// baseTables lists the base tables of a schema with their row deletion
// policies. A table interleaved in a parent is refused: the provider does not
// describe it yet. So is a policy the provider cannot read back, because a
// table read without it would be planned to lose it.
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
		table := Table{Schema: schema, Name: name}
		if policy != nil {
			read, found, err := rowdeletion.ParseExpression(*policy)
			if err != nil {
				return nil, &Unsupported{Reason: fmt.Sprintf("table %s: %v", qualified(schema, name), err)}
			}
			if found {
				table.RowDeletion = &read
			}
		}
		tables = append(tables, table)
	}
	return tables, rows.Err()
}

// refuseUndescribed fails the read when a schema holds an object the
// provider does not describe yet: a foreign key or CHECK constraint beyond the
// NOT NULL checks Spanner keeps for itself.
func refuseUndescribed(ctx context.Context, q Queryer, schema string) error {
	rows, err := q.Query(ctx, `SELECT c.table_schema, c.table_name, c.constraint_name, c.constraint_type, k.check_clause
FROM information_schema.table_constraints c
LEFT JOIN information_schema.check_constraints k
  ON k.constraint_schema = c.constraint_schema AND k.constraint_name = c.constraint_name
WHERE c.table_schema = $1 AND c.constraint_type IN ('FOREIGN KEY', 'CHECK')
ORDER BY c.table_name, c.constraint_name`, schema)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var held, table, name, kind string
		var clause *string
		if err := rows.Scan(&held, &table, &name, &kind, &clause); err != nil {
			return err
		}
		if kind == "CHECK" && notNullCheck(table, name, clause) {
			continue
		}
		return &Unsupported{Reason: fmt.Sprintf("table %s has the %s constraint %q, which the provider does not describe yet", qualified(held, table), kind, name)}
	}
	return rows.Err()
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
// or NULLS LAST when the key names its ordering.
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
		key.Nulls = words[1]
		return key, nil
	default:
		return IndexKey{}, fmt.Errorf("key %q has an ordering the provider cannot read", column)
	}
}

func qualified(schema, name string) string {
	return fmt.Sprintf("%q.%q", schema, name)
}
