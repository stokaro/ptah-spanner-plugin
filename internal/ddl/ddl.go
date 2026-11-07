// Package ddl writes the schema statements of Spanner's PostgreSQL interface:
// CREATE TABLE and DROP TABLE, the ALTER TABLE statements that add, change and
// drop a column or add a constraint, and CREATE INDEX and DROP INDEX.
//
// It connects to nothing. A statement it cannot write correctly is refused
// with an error naming why, before anything reaches the server. Spanner runs
// one action per ALTER TABLE statement (measured on the emulator behind
// PGAdapter 0.56.2: two actions answer `only single action per statement is
// allowed`), so each function writes one.
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
	// Default is the SQL that follows DEFAULT, or nil.
	Default *string
	// Check is the expression of the column's CHECK constraint, written
	// inside its column definition, and CheckName the constraint's name. Both
	// are empty for a column without one.
	Check, CheckName string
	// key marks the single key column of a CREATE TABLE, which carries
	// PRIMARY KEY NOT NULL in its definition.
	key bool
}

// IndexKey is one key part of an index.
type IndexKey struct {
	Column     string
	Descending bool
	// Nulls is FIRST or LAST when the key names its NULLS ordering, and empty
	// when the key keeps the default for its direction.
	Nulls string
}

// Index is a secondary index.
type Index struct {
	Name   string
	Unique bool
	Keys   []IndexKey
	// Storing are the columns the index stores beside its keys, which the
	// PostgreSQL interface spells INCLUDE.
	Storing []string
}

// ForeignKey is a foreign key constraint.
type ForeignKey struct {
	Name    string
	Columns []string
	// RefSchema and RefTable name the referenced table, and RefColumns its
	// columns in the order Columns pairs them.
	RefSchema, RefTable string
	RefColumns          []string
	// OnDelete is CASCADE or NO ACTION, or empty for the default. Spanner
	// has no other action.
	OnDelete string
}

// TTL is a table's row deletion policy: Spanner deletes a row once Interval
// has passed since the timestamp Column holds.
type TTL struct {
	Column   string
	Interval string
}

// clause writes the policy as CREATE TABLE writes it after its column list,
// and as ADD TTL and ALTER TTL take it: `TTL INTERVAL '30 days' ON "c"`. The
// interval is written as declared.
func (t TTL) clause() (string, error) {
	if t.Column == "" || strings.TrimSpace(t.Interval) == "" {
		return "", fmt.Errorf("a row deletion policy names its column and its interval")
	}
	return "TTL INTERVAL '" + strings.ReplaceAll(t.Interval, "'", "''") + "' ON " + Quote(t.Column), nil
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

// CreateTable writes CREATE TABLE for columns, a primary key in key order
// and a row deletion policy, nil for none. A single key column carries
// PRIMARY KEY NOT NULL in its definition, and a composite key is a PRIMARY
// KEY clause after the columns, as Ptah's built-in Spanner engine wrote them.
// Spanner makes every key column NOT NULL. The policy follows the column
// list.
func CreateTable(schema, name string, columns []Column, primaryKey []string, ttl *TTL) (string, error) {
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
		column.key = len(primaryKey) == 1 && keys[column.Name]
		definition, err := columnDefinition(column)
		if err != nil {
			return "", err
		}
		definitions = append(definitions, definition)
	}
	for _, key := range primaryKey {
		if !seen[key] {
			return "", fmt.Errorf("table %s names primary key column %q, which it does not declare", Name(schema, name), key)
		}
	}
	if len(primaryKey) > 1 {
		definitions = append(definitions, "PRIMARY KEY ("+quoteAll(primaryKey)+")")
	}
	suffix := ""
	if ttl != nil {
		if !seen[ttl.Column] {
			return "", fmt.Errorf("table %s names row deletion column %q, which it does not declare", Name(schema, name), ttl.Column)
		}
		clause, err := ttl.clause()
		if err != nil {
			return "", fmt.Errorf("table %s: %w", Name(schema, name), err)
		}
		suffix = " " + clause
	}
	return "CREATE TABLE " + Name(schema, name) + " (\n  " + strings.Join(definitions, ",\n  ") + "\n)" + suffix + ";", nil
}

// AddTTL writes ALTER TABLE ... ADD TTL, which gives a table without a policy
// one. Spanner refuses it on a table that has a policy, and ALTER TTL on one
// that has none, so the caller picks by what the table holds.
func AddTTL(schema, table string, ttl TTL) (string, error) {
	clause, err := ttl.clause()
	if err != nil {
		return "", fmt.Errorf("table %s: %w", Name(schema, table), err)
	}
	return "ALTER TABLE " + Name(schema, table) + " ADD " + clause + ";", nil
}

// AlterTTL writes ALTER TABLE ... ALTER TTL, which replaces a table's policy.
func AlterTTL(schema, table string, ttl TTL) (string, error) {
	clause, err := ttl.clause()
	if err != nil {
		return "", fmt.Errorf("table %s: %w", Name(schema, table), err)
	}
	return "ALTER TABLE " + Name(schema, table) + " ALTER " + clause + ";", nil
}

// DropTTL writes ALTER TABLE ... DROP TTL. It names no column: the policy goes
// and the column stays.
func DropTTL(schema, table string) string {
	return "ALTER TABLE " + Name(schema, table) + " DROP TTL;"
}

// CreateSchema writes CREATE SCHEMA IF NOT EXISTS, as Ptah's built-in Spanner
// engine wrote it before a table in a schema of its own: a plan creates the
// schema a new table names whether or not an earlier run created it.
func CreateSchema(name string) string {
	return "CREATE SCHEMA IF NOT EXISTS " + Quote(name) + ";"
}

// DropTable writes DROP TABLE. Spanner has one drop mode and refuses CASCADE,
// so the statement drops the table alone; its indexes have to be dropped
// first.
func DropTable(schema, name string) string {
	return "DROP TABLE " + Name(schema, name) + ";"
}

// AddColumn writes ALTER TABLE ... ADD COLUMN. Spanner refuses a NOT NULL
// column without a default here, even on an empty table (`Cannot add NOT NULL
// column`), so that shape is refused rather than sent: it is added nullable
// and then made NOT NULL.
func AddColumn(schema, table string, column Column) (string, error) {
	if !column.Nullable && column.Default == nil {
		return "", fmt.Errorf("column %q of table %s is NOT NULL without a default, which Spanner refuses to add to an existing table",
			column.Name, Name(schema, table))
	}
	if column.Check != "" {
		return "", fmt.Errorf("column %q of table %s carries a CHECK constraint, which is added after the column", column.Name, Name(schema, table))
	}
	column.key = false
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

// AlterColumnType writes ALTER TABLE ... ALTER COLUMN ... TYPE. Spanner
// changes a type in place only between character varying and bytea; the
// caller decides whether it may (see coltype.Convertible).
func AlterColumnType(schema, table, column, columnType string) (string, error) {
	if _, err := coltype.Canonical(columnType); err != nil {
		return "", fmt.Errorf("column %q: %w", column, err)
	}
	return alterColumn(schema, table, column, "TYPE "+strings.TrimSpace(columnType)), nil
}

// SetDefault writes ALTER TABLE ... ALTER COLUMN ... SET DEFAULT.
func SetDefault(schema, table, column, defaultSQL string) (string, error) {
	if strings.TrimSpace(defaultSQL) == "" {
		return "", fmt.Errorf("column %q has an empty default expression", column)
	}
	return alterColumn(schema, table, column, "SET DEFAULT "+strings.TrimSpace(defaultSQL)), nil
}

// DropDefault writes ALTER TABLE ... ALTER COLUMN ... DROP DEFAULT.
func DropDefault(schema, table, column string) string {
	return alterColumn(schema, table, column, "DROP DEFAULT")
}

// SetNotNull writes ALTER TABLE ... ALTER COLUMN ... SET NOT NULL.
func SetNotNull(schema, table, column string) string {
	return alterColumn(schema, table, column, "SET NOT NULL")
}

// DropNotNull writes ALTER TABLE ... ALTER COLUMN ... DROP NOT NULL.
func DropNotNull(schema, table, column string) string {
	return alterColumn(schema, table, column, "DROP NOT NULL")
}

// alterColumn writes the statement that changes column of table with action.
func alterColumn(schema, table, column, action string) string {
	return "ALTER TABLE " + Name(schema, table) + " ALTER COLUMN " + Quote(column) + " " + action + ";"
}

// CreateIndex writes CREATE [UNIQUE] INDEX for index on table. A key names
// its direction when it is descending and its NULLS ordering when it states
// one; the stored columns follow INCLUDE.
func CreateIndex(schema, table string, index Index) (string, error) {
	if index.Name == "" {
		return "", fmt.Errorf("an index of table %s has no name", Name(schema, table))
	}
	if len(index.Keys) == 0 {
		return "", fmt.Errorf("index %q of table %s has no key", index.Name, Name(schema, table))
	}
	keys := make([]string, len(index.Keys))
	for i, key := range index.Keys {
		if key.Column == "" {
			return "", fmt.Errorf("index %q of table %s has a key without a column", index.Name, Name(schema, table))
		}
		keys[i] = Quote(key.Column)
		if key.Descending {
			keys[i] += " DESC"
		}
		switch key.Nulls {
		case "":
		case "FIRST", "LAST":
			keys[i] += " NULLS " + key.Nulls
		default:
			return "", fmt.Errorf("index %q of table %s has a key with the NULLS ordering %q", index.Name, Name(schema, table), key.Nulls)
		}
	}
	statement := "CREATE "
	if index.Unique {
		statement += "UNIQUE "
	}
	statement += "INDEX " + Quote(index.Name) + " ON " + Name(schema, table) + " (" + strings.Join(keys, ", ") + ")"
	if len(index.Storing) > 0 {
		statement += " INCLUDE (" + quoteAll(index.Storing) + ")"
	}
	return statement + ";", nil
}

// DropIndex writes DROP INDEX for an index of a table in schema. An index
// name is unique in its schema, so it is qualified by the schema alone.
func DropIndex(schema, index string) string {
	return "DROP INDEX " + Name(schema, index) + ";"
}

// AddForeignKey writes ALTER TABLE ... ADD CONSTRAINT ... FOREIGN KEY.
func AddForeignKey(schema, table string, key ForeignKey) (string, error) {
	if key.Name == "" || len(key.Columns) == 0 || key.RefTable == "" || len(key.RefColumns) != len(key.Columns) {
		return "", fmt.Errorf("a foreign key of table %s needs a name, its columns and as many referenced columns", Name(schema, table))
	}
	statement := "ALTER TABLE " + Name(schema, table) + " ADD CONSTRAINT " + Quote(key.Name) + " FOREIGN KEY (" + quoteAll(key.Columns) +
		") REFERENCES " + Name(key.RefSchema, key.RefTable) + "(" + quoteAll(key.RefColumns) + ")"
	switch key.OnDelete {
	case "", "NO ACTION":
	case "CASCADE":
		statement += " ON DELETE CASCADE"
	default:
		return "", fmt.Errorf("foreign key %q of table %s has the action ON DELETE %s, and Spanner has NO ACTION and CASCADE only",
			key.Name, Name(schema, table), key.OnDelete)
	}
	return statement + ";", nil
}

// AddCheck writes ALTER TABLE ... ADD CONSTRAINT ... CHECK.
func AddCheck(schema, table, name, expression string) (string, error) {
	if name == "" || strings.TrimSpace(expression) == "" {
		return "", fmt.Errorf("a CHECK constraint of table %s needs a name and an expression", Name(schema, table))
	}
	return "ALTER TABLE " + Name(schema, table) + " ADD CONSTRAINT " + Quote(name) + " CHECK (" + strings.TrimSpace(expression) + ");", nil
}

// columnDefinition writes one column: its name, its type as declared,
// PRIMARY KEY for the single key column of its table, NOT NULL, its default
// and its CHECK constraint.
func columnDefinition(column Column) (string, error) {
	if column.Name == "" {
		return "", fmt.Errorf("a column has no name")
	}
	if _, err := coltype.Canonical(column.Type); err != nil {
		return "", fmt.Errorf("column %q: %w", column.Name, err)
	}
	definition := Quote(column.Name) + " " + strings.TrimSpace(column.Type)
	if column.key {
		definition += " PRIMARY KEY"
	}
	if !column.Nullable || column.key {
		definition += " NOT NULL"
	}
	if column.Default != nil {
		if strings.TrimSpace(*column.Default) == "" {
			return "", fmt.Errorf("column %q has an empty default expression", column.Name)
		}
		definition += " DEFAULT " + strings.TrimSpace(*column.Default)
	}
	if column.Check != "" {
		if column.CheckName == "" {
			return "", fmt.Errorf("the CHECK constraint of column %q has no name", column.Name)
		}
		definition += " CONSTRAINT " + Quote(column.CheckName) + " CHECK (" + strings.TrimSpace(column.Check) + ")"
	}
	return definition, nil
}

func quoteAll(names []string) string {
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = Quote(name)
	}
	return strings.Join(quoted, ", ")
}
