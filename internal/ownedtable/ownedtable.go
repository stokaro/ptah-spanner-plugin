// Package ownedtable checks that a table the provider keeps its own records
// in, such as the revision table, holds exactly the layout the provider reads
// and writes, before the provider reads or writes it.
//
// Spanner has no way to mark a table as the provider's: its PostgreSQL
// interface keeps no table comment or attribute. So a table is the provider's
// store when it carries the store's name and exactly the store's columns,
// types, nullability and primary key, and nothing that could change a row the
// provider writes, such as a row deletion policy. A table of that name with
// any other layout belongs to someone else, and the provider refuses it
// rather than adopting it.
package ownedtable

import (
	"context"
	"errors"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/stokaro/ptah-spanner-plugin/internal/ddl"
)

// Queryer runs a catalog query. *pgx.Conn and pgx.Tx implement it.
type Queryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Column is one column of a layout: its name, the spanner_type
// information_schema reports for it, and whether it takes NULL.
type Column struct {
	Name     string
	Type     string
	Nullable bool
}

// Definition is a table's identity and the layout the provider keeps in it.
type Definition struct {
	// Schema is the table's schema; empty selects the default schema.
	Schema string
	Name   string
	// Columns are the columns in the order the table declares them.
	Columns []Column
	// PrimaryKey are the key columns in key order.
	PrimaryKey []string
}

// ErrLayout reports a table that exists under the store's name and does not
// hold exactly the store's layout. Every such error is a *LayoutError.
var ErrLayout = errors.New("the table does not hold the layout the provider keeps its records in")

// LayoutError is an [ErrLayout] that says how the table differs.
type LayoutError struct {
	// Detail is a clause such as "its columns differ".
	Detail string
}

func (e *LayoutError) Error() string { return ErrLayout.Error() + ": " + e.Detail }

// Is reports that the error is an [ErrLayout].
func (e *LayoutError) Is(target error) bool { return target == ErrLayout }

// SchemaName is the schema the definition selects, with the default schema
// spelled out.
func (d Definition) SchemaName() string {
	if d.Schema == "" {
		return ddl.DefaultSchema
	}
	return d.Schema
}

// Quoted is the table's name as a statement writes it, qualified by its schema
// unless that is the default schema.
func (d Definition) Quoted() string { return ddl.Name(d.Schema, d.Name) }

// Locate reports whether the table exists, and when it does, checks that it
// holds exactly the definition's layout. A table that exists with another
// layout fails with a *LayoutError.
func Locate(ctx context.Context, q Queryer, definition Definition) (bool, error) {
	rows, err := q.Query(ctx, `SELECT table_type, parent_table_name, row_deletion_policy_expression
FROM information_schema.tables WHERE table_schema = $1 AND table_name = $2`, definition.SchemaName(), definition.Name)
	if err != nil {
		return false, err
	}
	type header struct {
		kind           string
		parent, policy *string
	}
	headers, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (header, error) {
		var h header
		err := row.Scan(&h.kind, &h.parent, &h.policy)
		return h, err
	})
	if err != nil {
		return false, err
	}
	switch {
	case len(headers) == 0:
		return false, nil
	case len(headers) > 1 || headers[0].kind != "BASE TABLE":
		return true, &LayoutError{Detail: "it is not one base table"}
	case headers[0].parent != nil && *headers[0].parent != "":
		return true, &LayoutError{Detail: "it is interleaved in a parent table"}
	case headers[0].policy != nil && *headers[0].policy != "":
		return true, &LayoutError{Detail: "it has a row deletion policy, which would remove records"}
	}
	columns, err := readColumns(ctx, q, definition)
	if err != nil {
		return true, err
	}
	if !slices.Equal(columns, definition.Columns) {
		return true, &LayoutError{Detail: "its columns differ"}
	}
	key, err := readPrimaryKey(ctx, q, definition)
	if err != nil {
		return true, err
	}
	if !slices.Equal(key, definition.PrimaryKey) {
		return true, &LayoutError{Detail: "its primary key differs"}
	}
	return true, nil
}

// Holds reports whether a table with these columns, in declaration order, and
// this primary key holds exactly the definition's layout. A read calls it to
// tell the provider's store from a user's table of the same name.
func (d Definition) Holds(columns []Column, primaryKey []string) bool {
	return slices.Equal(columns, d.Columns) && slices.Equal(primaryKey, d.PrimaryKey)
}

func readColumns(ctx context.Context, q Queryer, definition Definition) ([]Column, error) {
	rows, err := q.Query(ctx, `SELECT column_name, spanner_type, is_nullable, is_generated
FROM information_schema.columns WHERE table_schema = $1 AND table_name = $2 ORDER BY ordinal_position`,
		definition.SchemaName(), definition.Name)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Column, error) {
		var column Column
		var nullable, generated string
		if err := row.Scan(&column.Name, &column.Type, &nullable, &generated); err != nil {
			return Column{}, err
		}
		if generated != "NEVER" {
			// A generated column is not a column the provider writes.
			column.Type = "generated " + column.Type
		}
		column.Nullable = nullable == "YES"
		return column, nil
	})
}

func readPrimaryKey(ctx context.Context, q Queryer, definition Definition) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT column_name FROM information_schema.index_columns
WHERE table_schema = $1 AND table_name = $2 AND index_name = 'PRIMARY_KEY' AND ordinal_position IS NOT NULL
ORDER BY ordinal_position`, definition.SchemaName(), definition.Name)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}
