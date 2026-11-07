package tableplan

import (
	"fmt"
	"strings"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-spanner-plugin/internal/coltype"
	"github.com/stokaro/ptah-spanner-plugin/internal/tableop"
)

// Problem is a declaration the provider cannot create on Spanner: what kind
// of object, which one, and why.
type Problem struct {
	Code    protocol.SchemaDiagnostic_Code
	Kind    string
	Object  string
	Message string
}

// Validate checks every declared table offline and returns each problem in
// declaration order. A table it accepts is one Plan can create.
func Validate(request *protocol.ValidateSchemaRequest) []Problem {
	tables := make(map[tableKey]*protocol.DeclaredTable, len(request.GetTables()))
	for _, table := range request.GetTables() {
		tables[keyOf(table.GetSubject())] = table.GetDeclaration()
	}
	var problems []Problem
	for _, table := range request.GetTables() {
		problems = append(problems, validateTable(table.GetSubject(), table.GetDeclaration(), tables)...)
	}
	return problems
}

func validateTable(subject *protocol.ComparisonIdentity, table *protocol.DeclaredTable, tables map[tableKey]*protocol.DeclaredTable) []Problem {
	name := phrase(subject)
	unsupported := func(kind, object, format string, args ...any) Problem {
		return Problem{Code: protocol.SchemaDiagnostic_UNSUPPORTED_FEATURE, Kind: kind, Object: object, Message: fmt.Sprintf(format, args...)}
	}
	var problems []Problem
	switch {
	case table.GetCollation() != "" || table.GetEngine() != "":
		problems = append(problems, unsupported("table", name, "table %s states a collation or an engine; a Spanner table has neither", name))
	case table.GetComment() != "":
		problems = append(problems, unsupported("table", name, "table %s has a comment; Spanner's PostgreSQL interface stores no comment", name))
	case len(table.GetPrimaryKeyInclude()) > 0 || table.GetPrimaryKeyMethod() != "" || table.GetPrimaryKeyDeferrable() || table.GetPrimaryKeyInitially() != "":
		problems = append(problems, unsupported("primary key", name, "the primary key of table %s states INCLUDE, a method or deferral; a Spanner key has none", name))
	case strings.TrimSpace(table.GetCustomSql()) != "" || len(table.GetTriggers()) > 0:
		problems = append(problems, unsupported("table", name, "table %s carries custom SQL or a trigger, which Spanner has no place for", name))
	case len(primaryKey(table)) == 0:
		problems = append(problems, unsupported("primary key", name,
			"table %s declares no primary key; Spanner gives such a table a hidden key column, which the provider does not describe", name))
	}
	for _, part := range table.GetPrimaryKeyParts() {
		if part.GetDescending() || part.GetPrefix() != "" {
			problems = append(problems, unsupported("primary key", name, "the primary key of table %s orders or prefixes column %q; the provider creates an ascending key of whole columns", name, part.GetColumn()))
		}
	}
	for _, column := range table.GetColumns() {
		problems = append(problems, validateColumn(subject, table, column)...)
	}
	for _, index := range table.GetIndexes() {
		if reason := IndexRefusal(index); reason != "" {
			problems = append(problems, unsupported("index", index.GetName(), "index %q of table %s %s", index.GetName(), name, reason))
		}
	}
	for _, constraint := range table.GetConstraints() {
		switch strings.ToUpper(constraint.GetType()) {
		case "FOREIGN KEY", "CHECK", "PRIMARY KEY", "UNIQUE":
		default:
			problems = append(problems, unsupported("constraint", constraint.GetName(), "constraint %q of table %s is a %s constraint, which Spanner does not have",
				constraint.GetName(), name, constraint.GetType()))
		}
	}
	if _, err := tableChecks(subject, table); err != nil {
		problems = append(problems, unsupported("constraint", name, "%v", err))
	}
	return append(problems, validateForeignKeys(subject, table, tables)...)
}

// validateForeignKeys checks the foreign keys a declared table states: their
// settings, the columns they name, and the types of those columns, which a
// foreign key needs to be key types, alike on both sides. A referenced table
// the declaration does not hold is not checked.
func validateForeignKeys(subject *protocol.ComparisonIdentity, table *protocol.DeclaredTable, tables map[tableKey]*protocol.DeclaredTable) []Problem {
	keys, err := foreignKeys(subject, table)
	if err != nil {
		return []Problem{{Code: protocol.SchemaDiagnostic_UNSUPPORTED_FEATURE, Kind: "foreign key", Object: phrase(subject), Message: err.Error()}}
	}
	var problems []Problem
	for _, key := range keys {
		for i, column := range key.Columns {
			if message := foreignKeyColumn(subject, table, tables[tableKey{schema: key.RefSchema, name: key.RefTable}], key, i, column); message != "" {
				problems = append(problems, Problem{Code: protocol.SchemaDiagnostic_INVALID_SCHEMA, Kind: "foreign key", Object: key.Name, Message: message})
			}
		}
	}
	return problems
}

// foreignKeyColumn names what is wrong with the ith column of a foreign key,
// or returns the empty string.
func foreignKeyColumn(subject *protocol.ComparisonIdentity, table, referenced *protocol.DeclaredTable, key tableop.ForeignKey, i int, column string) string {
	local := declaredColumn(table, column)
	if local == nil {
		return fmt.Sprintf("foreign key %q of table %s names column %q, which the table does not declare", key.Name, phrase(subject), column)
	}
	localType, _ := coltype.Canonical(ColumnType(table, local))
	if nonKeyType(localType) {
		return fmt.Sprintf("invalid foreign key: Spanner type %s cannot participate in foreign keys: %q.%q references %q.%q", local.GetType(),
			subject.GetName().GetSource(), column, key.RefTable, key.RefColumns[i])
	}
	if referenced == nil {
		return ""
	}
	target := declaredColumn(referenced, key.RefColumns[i])
	if target == nil {
		return fmt.Sprintf("foreign key %q of table %s references column %q, which table %q does not declare", key.Name, phrase(subject), key.RefColumns[i], key.RefTable)
	}
	if targetType, _ := coltype.Canonical(ColumnType(referenced, target)); baseType(localType) != baseType(targetType) {
		return fmt.Sprintf("invalid foreign key: foreign-key columns %q.%q (%s) and %q.%q (%s) have incompatible types", subject.GetName().GetSource(), column,
			local.GetType(), key.RefTable, key.RefColumns[i], target.GetType())
	}
	return ""
}

func validateColumn(subject *protocol.ComparisonIdentity, table *protocol.DeclaredTable, column *protocol.DeclaredColumn) []Problem {
	where := fmt.Sprintf("column %q of table %s", column.GetName(), phrase(subject))
	unsupported := func(format string, args ...any) []Problem {
		return []Problem{{Code: protocol.SchemaDiagnostic_UNSUPPORTED_FEATURE, Kind: "column", Object: column.GetName(), Message: fmt.Sprintf(format, args...)}}
	}
	if _, err := coltype.Canonical(ColumnType(table, column)); err != nil {
		return unsupported("%s: %v", where, err)
	}
	switch {
	case column.GetAutoIncrement() || column.GetIdentityGeneration() != "":
		return unsupported("%s is an identity column, which the provider does not describe yet", where)
	case column.GetGeneratedExpression() != "":
		return unsupported("%s is a generated column, which the provider does not describe yet", where)
	case column.GetCollation() != "":
		return unsupported("%s states a collation; a Spanner column has none", where)
	case column.GetComment() != "":
		return unsupported("%s has a comment; Spanner's PostgreSQL interface stores no comment", where)
	case column.GetCheckNotEnforced():
		return unsupported("the CHECK constraint of %s is not enforced, which Spanner cannot hold", where)
	case column.GetUniqueExpression() != "":
		return unsupported("%s is unique over an expression; a Spanner index keys columns only", where)
	}
	return nil
}

// nonKeyType reports a canonical type Spanner refuses in a key, and so in a
// foreign key: an array, real, numeric and jsonb, as Ptah's built-in Spanner
// engine refused them.
func nonKeyType(canonical string) bool {
	return strings.HasSuffix(canonical, "[]") || canonical == "real" || canonical == "numeric" || canonical == "jsonb"
}

// baseType is a canonical type without its length.
func baseType(canonical string) string {
	if strings.HasPrefix(canonical, "character varying") {
		return "character varying"
	}
	return canonical
}
