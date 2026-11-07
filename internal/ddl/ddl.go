// Package ddl writes the schema statements of Spanner's PostgreSQL interface:
// CREATE TABLE, DROP TABLE and the ALTER TABLE statements that add and drop a
// column.
//
// It connects to nothing. A statement it cannot write correctly is refused
// with an error naming why, before anything reaches the server.
package ddl

import (
	"fmt"
	"strings"

	"github.com/stokaro/ptah-spanner-plugin/internal/coltype"
)

// DefaultSchema is the schema a statement leaves unqualified.
const DefaultSchema = "public"

// Column is one column of a CREATE TABLE or an ADD COLUMN.
type Column struct {
	Name     string
	Type     string
	Nullable bool
	// Default is the default expression as declared, or nil.
	Default *string
}

// Quote writes a name as a double-quoted identifier, which the PostgreSQL
// interface resolves by its exact spelling.
func Quote(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// Name writes a table name, qualified by its schema unless the schema is the
// default one.
func Name(schema, name string) string {
	if schema == "" || schema == DefaultSchema {
		return Quote(name)
	}
	return Quote(schema) + "." + Quote(name)
}

// CreateTable writes CREATE TABLE for columns and a primary key in key order.
// A key column is NOT NULL whatever its declaration says, as Spanner makes it.
func CreateTable(schema, name string, columns []Column, primaryKey []string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("a table has no name")
	}
	if len(columns) == 0 {
		return "", fmt.Errorf("table %s has no column", Name(schema, name))
	}
	keys := make(map[string]bool, len(primaryKey))
	for _, key := range primaryKey {
		if keys[key] {
			return "", fmt.Errorf("table %s names primary key column %q twice", Name(schema, name), key)
		}
		keys[key] = true
	}
	definitions := make([]string, 0, len(columns)+1)
	seen := make(map[string]bool, len(columns))
	for _, column := range columns {
		if seen[column.Name] {
			return "", fmt.Errorf("table %s declares column %q twice", Name(schema, name), column.Name)
		}
		seen[column.Name] = true
		if keys[column.Name] {
			column.Nullable = false
		}
		definition, err := columnDefinition(column)
		if err != nil {
			return "", err
		}
		definitions = append(definitions, definition)
	}
	if len(primaryKey) > 0 {
		quoted := make([]string, len(primaryKey))
		for i, key := range primaryKey {
			if !seen[key] {
				return "", fmt.Errorf("table %s names primary key column %q, which it does not declare", Name(schema, name), key)
			}
			quoted[i] = Quote(key)
		}
		definitions = append(definitions, "PRIMARY KEY ("+strings.Join(quoted, ", ")+")")
	}
	return "CREATE TABLE " + Name(schema, name) + " (\n  " + strings.Join(definitions, ",\n  ") + "\n);", nil
}

// DropTable writes DROP TABLE. Spanner has one drop mode and refuses CASCADE,
// so the statement drops the table alone.
func DropTable(schema, name string) string {
	return "DROP TABLE " + Name(schema, name) + ";"
}

// AddColumn writes ALTER TABLE ... ADD COLUMN. Spanner refuses a NOT NULL
// column without a default here, even on an empty table, so that shape is
// refused rather than sent.
func AddColumn(schema, table string, column Column) (string, error) {
	if !column.Nullable && column.Default == nil {
		return "", fmt.Errorf("column %q of table %s is NOT NULL without a default, which Spanner refuses to add to an existing table",
			column.Name, Name(schema, table))
	}
	definition, err := columnDefinition(column)
	if err != nil {
		return "", err
	}
	return "ALTER TABLE " + Name(schema, table) + " ADD COLUMN " + definition + ";", nil
}

// DropColumn writes ALTER TABLE ... DROP COLUMN.
func DropColumn(schema, table, column string) string {
	return "ALTER TABLE " + Name(schema, table) + " DROP COLUMN " + Quote(column) + ";"
}

// columnDefinition writes one column: its name, its type as declared, NOT
// NULL and its default.
func columnDefinition(column Column) (string, error) {
	if column.Name == "" {
		return "", fmt.Errorf("a column has no name")
	}
	if _, err := coltype.Canonical(column.Type); err != nil {
		return "", fmt.Errorf("column %q: %w", column.Name, err)
	}
	definition := Quote(column.Name) + " " + strings.TrimSpace(column.Type)
	if !column.Nullable {
		definition += " NOT NULL"
	}
	if column.Default != nil {
		if strings.TrimSpace(*column.Default) == "" {
			return "", fmt.Errorf("column %q has an empty default expression", column.Name)
		}
		definition += " DEFAULT " + strings.TrimSpace(*column.Default)
	}
	return definition, nil
}
