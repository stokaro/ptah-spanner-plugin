package tableplan

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-spanner-plugin/internal/checkexpr"
	"github.com/stokaro/ptah-spanner-plugin/internal/coltype"
	"github.com/stokaro/ptah-spanner-plugin/internal/tableop"
)

// PrimaryKey lists a declared table's key columns in key order: the table's
// key list, then any column marked primary that the list does not name.
func PrimaryKey(table *protocol.DeclaredTable) []string { return primaryKey(table) }

func primaryKey(table *protocol.DeclaredTable) []string {
	keys := slices.Clone(table.GetPrimaryKey())
	for _, column := range table.GetColumns() {
		if column.GetPrimary() && !slices.Contains(keys, column.GetName()) {
			keys = append(keys, column.GetName())
		}
	}
	return keys
}

// heldPrimaryKey lists the key a table holds in key order: the PRIMARY KEY
// constraint the host reads into the observation, or the columns a read marks
// as key columns when it carries no constraint.
func heldPrimaryKey(table *protocol.ObservedTable) []string {
	for _, constraint := range table.GetConstraints() {
		if strings.EqualFold(constraint.GetType(), "PRIMARY KEY") {
			if len(constraint.GetColumnNames()) > 0 {
				return slices.Clone(constraint.GetColumnNames())
			}
			return []string{constraint.GetColumnName()}
		}
	}
	var keys []string
	for _, column := range table.GetColumns() {
		if column.GetPrimaryKey() {
			keys = append(keys, column.GetName())
		}
	}
	return keys
}

// EffectiveNullable is a declared column's nullability as Spanner stores it:
// a primary key column is NOT NULL whatever its declaration says.
func EffectiveNullable(table *protocol.DeclaredTable, column *protocol.DeclaredColumn) bool {
	return column.GetNullable() && !slices.Contains(primaryKey(table), column.GetName())
}

// EnumValues are the values of a declared enum column: the values it carries,
// or the values of the enum its type names. A column that names no enum has
// none.
func EnumValues(table *protocol.DeclaredTable, column *protocol.DeclaredColumn) []string {
	if values := column.GetEnumValues(); len(values) > 0 {
		return values
	}
	for _, enum := range table.GetEnums() {
		if enum.GetName() == strings.TrimSpace(column.GetType()) {
			return enum.GetValues()
		}
	}
	return nil
}

// ColumnType is the type a declared column is created with. The PostgreSQL
// interface has no enum type, so an enum column is TEXT under a CHECK that
// lists its values, as Ptah's built-in Spanner engine created it.
func ColumnType(table *protocol.DeclaredTable, column *protocol.DeclaredColumn) string {
	if len(EnumValues(table, column)) > 0 {
		return "TEXT"
	}
	return column.GetType()
}

// DefaultSQL is the SQL that follows DEFAULT for a declared column, or nil
// when it declares no default.
func DefaultSQL(table *protocol.DeclaredTable, column *protocol.DeclaredColumn) *string {
	switch {
	case column.GetDefaultSet():
		return new(coltype.LiteralSQL(ColumnType(table, column), column.GetDefaultValue()))
	case strings.TrimSpace(column.GetDefaultExpression()) != "":
		return new(strings.TrimSpace(column.GetDefaultExpression()))
	default:
		return nil
	}
}

// ddlColumn is the column a create-table or an add-column writes for a
// declared column of the table subject. A key column keeps its declared
// nullability: Spanner makes it NOT NULL whatever it says, and the statement
// says what the declaration does, as Ptah's built-in Spanner engine wrote it.
// Its CHECK is the column's own and, for an enum column, the list of its
// values, under the column's check name or `<table>_<column>_check`. A host
// that lowers an enum column for Spanner hands the list as the column's own
// CHECK as well, and the list is then written once.
func ddlColumn(subject *protocol.ComparisonIdentity, table *protocol.DeclaredTable, declared *protocol.DeclaredColumn) (tableop.Column, error) {
	columnType := ColumnType(table, declared)
	if _, err := coltype.Canonical(columnType); err != nil {
		return tableop.Column{}, fmt.Errorf("column %q of table %s: %w", declared.GetName(), phrase(subject), err)
	}
	column := tableop.Column{Name: declared.GetName(), Type: strings.TrimSpace(columnType), Nullable: declared.GetNullable(),
		Default: DefaultSQL(table, declared)}
	check := strings.TrimSpace(declared.GetCheck())
	if values := EnumValues(table, declared); len(values) > 0 {
		quoted := make([]string, len(values))
		for i, value := range values {
			quoted[i] = "'" + strings.ReplaceAll(value, "'", "''") + "'"
		}
		listed := declared.GetName() + " IN (" + strings.Join(quoted, ", ") + ")"
		if check != "" && !checkexpr.Equivalent(check, listed, nil) {
			listed = "(" + check + ") AND " + listed
		}
		check = listed
	}
	if check != "" {
		column.Check = check
		column.CheckName = declared.GetCheckName()
		if column.CheckName == "" {
			column.CheckName = subject.GetName().GetSource() + "_" + declared.GetName() + "_check"
		}
	}
	return column, nil
}

// createTable is the operation that creates a declared table: its columns in
// order and its key in key order.
func createTable(subject *protocol.ComparisonIdentity, table *protocol.DeclaredTable) (tableop.Operation, error) {
	operation := tableop.Operation{Action: tableop.CreateTable, PrimaryKey: primaryKey(table)}
	for _, declared := range table.GetColumns() {
		column, err := ddlColumn(subject, table, declared)
		if err != nil {
			return tableop.Operation{}, err
		}
		operation.Columns = append(operation.Columns, column)
	}
	if len(operation.Columns) == 0 {
		return tableop.Operation{}, fmt.Errorf("table %s declares no column", phrase(subject))
	}
	return operation, nil
}

// indexOperation is the operation that creates a declared index: its keys
// with their direction and NULLS ordering, and its stored columns. An index
// Spanner's PostgreSQL interface cannot create, or one that states a setting
// the provider does not write, is refused by name.
func indexOperation(declared *protocol.DeclaredIndex) (tableop.Operation, error) {
	if reason := IndexRefusal(declared); reason != "" {
		return tableop.Operation{}, fmt.Errorf("index %q %s", declared.GetName(), reason)
	}
	index := &tableop.Index{Name: declared.GetName(), Unique: declared.GetUnique(), Storing: slices.Clone(declared.GetIncludeColumns())}
	if len(declared.GetParts()) > 0 {
		for _, part := range declared.GetParts() {
			index.Keys = append(index.Keys, tableop.IndexKey{Column: part.GetColumn(), Descending: part.GetDescending(), Nulls: strings.ToUpper(part.GetNulls())})
		}
	} else {
		for _, field := range declared.GetFields() {
			index.Keys = append(index.Keys, tableop.IndexKey{Column: field})
		}
	}
	return tableop.Operation{Action: tableop.CreateIndex, Index: index}, nil
}

// IndexRefusal names what puts a declared index outside what the provider
// creates, or returns the empty string.
func IndexRefusal(declared *protocol.DeclaredIndex) string {
	switch {
	case strings.TrimSpace(declared.GetCondition()) != "":
		return "has a WHERE filter, which the provider does not create yet"
	case strings.TrimSpace(declared.GetType()) != "":
		return fmt.Sprintf("uses the access method %q; a Spanner index has none", declared.GetType())
	case strings.TrimSpace(declared.GetOperator()) != "":
		return "names an operator class; a Spanner index has none"
	case len(declared.GetStorageParameters()) > 0:
		return "sets storage parameters; a Spanner index has none"
	case declared.NullsDistinct != nil && !declared.GetNullsDistinct():
		return "is NULLS NOT DISTINCT, which a Spanner index cannot be"
	case declared.GetInvisible():
		return "is invisible, which a Spanner index cannot be"
	case len(declared.GetParts()) == 0 && len(declared.GetFields()) == 0:
		return "has no key"
	}
	for _, part := range declared.GetParts() {
		switch {
		case part.GetColumn() == "" || strings.TrimSpace(part.GetExpression()) != "":
			return "has a key that is an expression; a Spanner index keys columns only"
		case part.GetOperatorClass() != "" || part.GetPrefix() != "":
			return fmt.Sprintf("has a key on %q with an operator class or a prefix; a Spanner index has neither", part.GetColumn())
		case part.GetNulls() != "" && !slices.Contains([]string{"FIRST", "LAST"}, strings.ToUpper(part.GetNulls())):
			return fmt.Sprintf("has a key on %q with the NULLS ordering %q", part.GetColumn(), part.GetNulls())
		}
	}
	return ""
}

// ForeignKeys are the foreign keys a declared table states, on its columns
// and as table constraints, each with the referenced schema resolved.
func ForeignKeys(subject *protocol.ComparisonIdentity, table *protocol.DeclaredTable) ([]tableop.ForeignKey, error) {
	return foreignKeys(subject, table)
}

func foreignKeys(subject *protocol.ComparisonIdentity, table *protocol.DeclaredTable) ([]tableop.ForeignKey, error) {
	var keys []tableop.ForeignKey
	for _, column := range table.GetColumns() {
		if strings.TrimSpace(column.GetForeignReference()) == "" {
			continue
		}
		refTable, refColumns, ok := parseReference(column.GetForeignReference())
		if !ok {
			return nil, fmt.Errorf("column %q of table %s has the foreign reference %q, which names no table and column", column.GetName(), phrase(subject), column.GetForeignReference())
		}
		name := column.GetForeignKeyName()
		if name == "" {
			name = "fk_" + strings.ToLower(subject.GetName().GetSource()) + "_" + strings.ToLower(column.GetName())
		}
		key, err := foreignKey(subject, name, []string{column.GetName()}, refTable, refColumns, foreignActions{onDelete: column.GetOnDelete(), onUpdate: column.GetOnUpdate(),
			deferrable: column.GetDeferrable(), initially: column.GetInitially(), match: column.GetForeignKeyMatch(), notEnforced: column.GetForeignKeyNotEnforced()})
		if err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	for _, constraint := range table.GetConstraints() {
		// A column's foreign key may reach the declaration twice, on the
		// column and as the table constraint it lowers to.
		if !strings.EqualFold(constraint.GetType(), "FOREIGN KEY") || slices.ContainsFunc(keys, func(key tableop.ForeignKey) bool { return key.Name == constraint.GetName() }) {
			continue
		}
		refColumns := slices.Clone(constraint.GetForeignColumns())
		if len(refColumns) == 0 && constraint.GetForeignColumn() != "" {
			refColumns = splitNames(constraint.GetForeignColumn())
		}
		key, err := foreignKey(subject, constraint.GetName(), constraint.GetColumns(), constraint.GetForeignTable(), refColumns, foreignActions{
			onDelete: constraint.GetOnDelete(), onUpdate: constraint.GetOnUpdate(), deferrable: constraint.GetDeferrable(), initially: constraint.GetInitially(),
			match: constraint.GetMatch(), notEnforced: constraint.GetNotEnforced() || constraint.GetNotValid()})
		if err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, nil
}

// foreignActions are the settings of a declared foreign key beyond its
// columns.
type foreignActions struct {
	onDelete, onUpdate, initially, match string
	deferrable, notEnforced              bool
}

func foreignKey(subject *protocol.ComparisonIdentity, name string, columns []string, reference string, refColumns []string, actions foreignActions) (tableop.ForeignKey, error) {
	where := fmt.Sprintf("foreign key %q of table %s", name, phrase(subject))
	onDelete := strings.ToUpper(strings.TrimSpace(actions.onDelete))
	switch {
	case name == "":
		return tableop.ForeignKey{}, fmt.Errorf("a foreign key of table %s has no name", phrase(subject))
	case len(columns) == 0 || len(columns) != len(refColumns) || reference == "":
		return tableop.ForeignKey{}, fmt.Errorf("%s needs its columns, the referenced table and as many referenced columns", where)
	case onDelete != "" && onDelete != "NO ACTION" && onDelete != "CASCADE":
		return tableop.ForeignKey{}, fmt.Errorf("%s has ON DELETE %s; Spanner has NO ACTION and CASCADE only", where, onDelete)
	case strings.TrimSpace(actions.onUpdate) != "" && !strings.EqualFold(strings.TrimSpace(actions.onUpdate), "NO ACTION"):
		return tableop.ForeignKey{}, fmt.Errorf("%s has ON UPDATE %s; Spanner has no ON UPDATE action", where, strings.ToUpper(actions.onUpdate))
	case actions.deferrable || actions.initially != "":
		return tableop.ForeignKey{}, fmt.Errorf("%s is deferrable; Spanner checks a foreign key at once", where)
	case actions.match != "" && !strings.EqualFold(actions.match, "SIMPLE"):
		return tableop.ForeignKey{}, fmt.Errorf("%s has MATCH %s; Spanner has none", where, strings.ToUpper(actions.match))
	case actions.notEnforced:
		return tableop.ForeignKey{}, fmt.Errorf("%s is not enforced or not valid, which Spanner cannot hold", where)
	}
	if onDelete == "NO ACTION" {
		onDelete = ""
	}
	refSchema, refTable := splitQualified(reference, subject.GetSchema().GetSource())
	return tableop.ForeignKey{Name: name, Columns: slices.Clone(columns), RefSchema: refSchema, RefTable: refTable, RefColumns: slices.Clone(refColumns),
		OnDelete: onDelete}, nil
}

// TableChecks are the CHECK constraints a declared table states as table
// constraints, each added after the table is created, as Ptah's built-in
// Spanner engine added them.
func TableChecks(subject *protocol.ComparisonIdentity, table *protocol.DeclaredTable) ([]tableop.Operation, error) {
	return tableChecks(subject, table)
}

func tableChecks(subject *protocol.ComparisonIdentity, table *protocol.DeclaredTable) ([]tableop.Operation, error) {
	var checks []tableop.Operation
	for _, constraint := range table.GetConstraints() {
		if !strings.EqualFold(constraint.GetType(), "CHECK") {
			continue
		}
		if constraint.GetNotEnforced() || constraint.GetNotValid() {
			return nil, fmt.Errorf("CHECK constraint %q of table %s is not enforced or not valid, which Spanner cannot hold", constraint.GetName(), phrase(subject))
		}
		if constraint.GetName() == "" || strings.TrimSpace(constraint.GetCheckExpression()) == "" {
			return nil, fmt.Errorf("a CHECK constraint of table %s needs a name and a condition", phrase(subject))
		}
		checks = append(checks, tableop.Operation{Action: tableop.AddCheck, Name: constraint.GetName(), Expression: strings.TrimSpace(constraint.GetCheckExpression())})
	}
	for i, expression := range table.GetChecks() {
		name := subject.GetName().GetSource() + "_check"
		if i > 0 {
			name = fmt.Sprintf("%s%d", name, i)
		}
		checks = append(checks, tableop.Operation{Action: tableop.AddCheck, Name: name, Expression: strings.TrimSpace(expression)})
	}
	return checks, nil
}

// parseReference reads `table(column)`, `schema.table(column, ...)` or the
// shorthand `table`, whose referenced column is id, as Ptah's annotations
// write a foreign reference.
func parseReference(reference string) (string, []string, bool) {
	reference = strings.TrimSpace(reference)
	open, closing := strings.IndexByte(reference, '('), strings.LastIndexByte(reference, ')')
	if open < 0 && closing < 0 {
		return reference, []string{"id"}, reference != ""
	}
	if open <= 0 || closing != len(reference)-1 || strings.Count(reference, "(") != 1 || strings.Count(reference, ")") != 1 {
		return "", nil, false
	}
	table, columns := strings.TrimSpace(reference[:open]), splitNames(reference[open+1:closing])
	return table, columns, table != "" && len(columns) > 0
}

func splitNames(list string) []string {
	var names []string
	for name := range strings.SplitSeq(list, ",") {
		if name = strings.Trim(strings.TrimSpace(name), `"`); name != "" {
			names = append(names, name)
		}
	}
	return names
}

// splitQualified splits `schema.table`, and puts a bare table in
// defaultSchema.
func splitQualified(reference, defaultSchema string) (schema, table string) {
	if schema, table, found := strings.Cut(reference, "."); found {
		return strings.Trim(schema, `"`), strings.Trim(table, `"`)
	}
	return defaultSchema, strings.Trim(reference, `"`)
}

// alterColumn returns the operations that give a held column its declared
// type, default and nullability, each with the notes its step carries; keys
// are the table's key columns. A type Spanner cannot change to, and any type
// change of a key column, is refused by name.
func alterColumn(subject *protocol.ComparisonIdentity, table *protocol.DeclaredTable, declared *protocol.DeclaredColumn, held *protocol.ObservedColumn,
	keys []string,
) ([]tableop.Operation, [][]string, error) {
	key := slices.Contains(keys, declared.GetName())
	var operations []tableop.Operation
	var notes [][]string
	columnType := ColumnType(table, declared)
	if !coltype.Same(columnType, heldType(held)) {
		target, err := coltype.Canonical(columnType)
		if err != nil {
			return nil, nil, fmt.Errorf("column %q of table %s: %w", declared.GetName(), phrase(subject), err)
		}
		switch {
		case key:
			return nil, nil, fmt.Errorf("changing key column %q of table %s from %s to %s: Spanner cannot change a key column", declared.GetName(), phrase(subject),
				canonicalHeld(held), target)
		case !coltype.Convertible(canonicalHeld(held), target):
			return nil, nil, fmt.Errorf("changing column %q of table %s from %s to %s: Spanner changes a column's type only between character varying and bytea, "+
				"or the length of character varying", declared.GetName(), phrase(subject), canonicalHeld(held), target)
		}
		operations = append(operations, tableop.Operation{Action: tableop.AlterColumnType, Name: declared.GetName(), Type: strings.TrimSpace(columnType)})
		notes = append(notes, nil)
	}
	if !SameDefault(table, declared, held) {
		if defaultSQL := DefaultSQL(table, declared); defaultSQL != nil {
			operations = append(operations, tableop.Operation{Action: tableop.SetDefault, Name: declared.GetName(), Default: *defaultSQL})
		} else {
			operations = append(operations, tableop.Operation{Action: tableop.DropDefault, Name: declared.GetName()})
		}
		notes = append(notes, nil)
	}
	if nullable := EffectiveNullable(table, declared); !key && nullable != (held.GetIsNullable() == "YES") {
		if nullable {
			operations = append(operations, tableop.Operation{Action: tableop.DropNotNull, Name: declared.GetName()})
			notes = append(notes, nil)
		} else {
			operations = append(operations, tableop.Operation{Action: tableop.SetNotNull, Name: declared.GetName()})
			var note []string
			if DefaultSQL(table, declared) == nil {
				note = []string{notNullNote(subject, declared.GetName())}
			}
			notes = append(notes, note)
		}
	}
	return operations, notes, nil
}

// SameDefault reports whether a declared column's default and the default a
// column holds are one: a literal read back as Spanner stores it, `'0'::bigint`,
// is the literal 0, two numbers are compared as numbers, and two expressions
// that differ only in case and spacing are one.
func SameDefault(table *protocol.DeclaredTable, declared *protocol.DeclaredColumn, held *protocol.ObservedColumn) bool {
	heldKind, heldValue := coltype.ReadDefault(held.DefaultExpression)
	declaredKind, declaredValue := DeclaredDefault(declared)
	if declaredKind != heldKind {
		return false
	}
	switch declaredKind {
	case coltype.Literal:
		return sameLiteral(declaredValue, heldValue)
	case coltype.Expression:
		return normalizeExpression(declaredValue) == normalizeExpression(heldValue)
	default:
		return true
	}
}

// DeclaredDefault reads a declared default the way a stored one reads: a
// literal value, or an expression that may itself be a literal.
func DeclaredDefault(column *protocol.DeclaredColumn) (coltype.DefaultKind, string) {
	if column.GetDefaultSet() {
		return coltype.Literal, coltype.LiteralValue(column.GetDefaultValue())
	}
	if strings.TrimSpace(column.GetDefaultExpression()) == "" {
		return coltype.Absent, ""
	}
	return coltype.ReadDefault(new(column.GetDefaultExpression()))
}

func sameLiteral(declared, held string) bool {
	if declared == held {
		return true
	}
	if isBoolean(declared) && isBoolean(held) {
		return strings.EqualFold(declared, held)
	}
	left, leftOK := number(declared)
	right, rightOK := number(held)
	return leftOK && rightOK && left == right
}

func isBoolean(value string) bool {
	return strings.EqualFold(value, "true") || strings.EqualFold(value, "false")
}

// number reads a decimal literal into a canonical text, so 5, 5.0 and +5 read
// alike without the rounding of a float.
func number(value string) (string, bool) {
	value = strings.TrimSpace(value)
	negative := strings.HasPrefix(value, "-")
	value = strings.TrimLeft(value, "+-")
	whole, fraction, _ := strings.Cut(value, ".")
	if whole == "" && fraction == "" || strings.Trim(whole+fraction, "0123456789") != "" {
		return "", false
	}
	whole, fraction = strings.TrimLeft(whole, "0"), strings.TrimRight(fraction, "0")
	text := whole
	if text == "" {
		text = "0"
	}
	if fraction != "" {
		text += "." + fraction
	}
	if negative && text != "0" {
		text = "-" + text
	}
	return text, true
}

// normalizeExpression reads an expression default in the spelling Spanner
// stores it in, ignoring case and spacing. Spanner casts a string argument it
// stores: `nextval('s1')` reads back as `nextval('s1'::text)`, so a ::text
// cast after a string is dropped. Without it a default that draws from a
// sequence is set again on every plan.
func normalizeExpression(expression string) string {
	return textCast.ReplaceAllString(strings.Join(strings.Fields(strings.ToLower(expression)), " "), "$1")
}

// textCast is a string literal followed by a cast to text.
var textCast = regexp.MustCompile(`('(?:[^']|'')*')::text\b`)
