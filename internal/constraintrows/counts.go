package constraintrows

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/stokaro/ptah-spanner-plugin/internal/catalog"
	"github.com/stokaro/ptah-spanner-plugin/internal/coltype"
	"github.com/stokaro/ptah-spanner-plugin/internal/ddl"
)

// Query is the statement that counts the rows of the table that break the
// rule, each counted once. The semantics are Spanner's, measured on the
// emulator behind PGAdapter 0.56.2:
//
//   - A CHECK takes a row for which its expression is null, and so does
//     `WHERE NOT (e)`.
//   - A foreign key takes a row with a null key column: a composite key adds
//     over a row of (5, NULL) with no referenced row.
//   - A unique index leaves out every row with a null key column (Spanner
//     gives it the filter `a IS NOT NULL AND b IS NOT NULL`), so two rows of
//     (5, NULL) share no key. The count is of the rows whose key another row
//     holds too, within the index's own filter.
//   - NOT NULL counts the nulls, also in a column with a default, which
//     Spanner does not fill. A column the batch adds without a default holds
//     a null in every row, so every row counts.
//   - A character limit counts the values longer than the limit in
//     characters, as Spanner counts them: `héllo` is 5. A bytea value is
//     read as UTF-8 text, as Spanner converts it; a value that is not valid
//     UTF-8 fails the count ("Invalid cast of bytes to UTF8 string"), as it
//     fails the conversion.
func (a Addition) Query() string {
	table := ddl.Name(a.Table.Schema, a.Table.Table)
	switch a.Kind {
	case Check:
		return "SELECT COUNT(*) FROM " + table + " WHERE NOT (" + a.Expression + ")"
	case ForeignKey:
		pairs := make([]string, len(a.Columns))
		for i, column := range a.Columns {
			pairs[i] = "parent." + ddl.Quote(a.ReferencedColumns[i]) + " = child." + ddl.Quote(column)
		}
		return "SELECT COUNT(*) FROM " + table + " AS child WHERE " + present("child.", a.Columns) +
			" AND NOT EXISTS (SELECT 1 FROM " + ddl.Name(a.Referenced.Schema, a.Referenced.Table) + " AS parent WHERE " + strings.Join(pairs, " AND ") + ")"
	case UniqueIndex:
		filter := present("", a.Columns)
		if a.Expression != "" {
			filter += " AND (" + a.Expression + ")"
		}
		return "SELECT CAST(COALESCE(SUM(shared), 0) AS bigint) FROM (SELECT COUNT(*) AS shared FROM " + table + " WHERE " + filter +
			" GROUP BY " + quoted(a.Columns) + " HAVING COUNT(*) > 1) AS duplicates"
	case NotNull:
		if a.Unfilled {
			return "SELECT COUNT(*) FROM " + table
		}
		return "SELECT COUNT(*) FROM " + table + " WHERE " + ddl.Quote(a.Columns[0]) + " IS NULL"
	case CharacterLimit:
		if a.Array {
			return fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE EXISTS (SELECT 1 FROM UNNEST(%s) AS element WHERE length(CAST(element AS varchar)) > %d)",
				table, ddl.Quote(a.Columns[0]), a.Limit)
		}
		return fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE length(CAST(%s AS varchar)) > %d", table, ddl.Quote(a.Columns[0]), a.Limit)
	}
	return ""
}

// present is the condition that every one of columns holds a value, each
// name quoted after prefix.
func present(prefix string, columns []string) string {
	conditions := make([]string, len(columns))
	for i, column := range columns {
		conditions[i] = prefix + ddl.Quote(column) + " IS NOT NULL"
	}
	return strings.Join(conditions, " AND ")
}

// quoted lists columns quoted and joined by commas.
func quoted(columns []string) string {
	names := make([]string, len(columns))
	for i, column := range columns {
		names[i] = ddl.Quote(column)
	}
	return strings.Join(names, ", ")
}

// Count counts the rows of the table that break the rule, with Query.
//
// A character limit first reads the column's type: a column of character
// varying whose limit is no higher than the new one holds no longer value,
// and a column of another type than character varying or bytea, or an array
// where the new type is none, is a change Spanner refuses whatever the rows
// hold. Neither is counted.
func (a Addition) Count(ctx context.Context, q catalog.Queryer) (int64, error) {
	if a.Kind == CharacterLimit {
		counted, err := a.countsLength(ctx, q)
		if err != nil || !counted {
			return 0, err
		}
	}
	return scalar(ctx, q, a.Query())
}

// countsLength reports whether the values of a CharacterLimit's column may
// be longer than its new limit, from the column's type.
func (a Addition) countsLength(ctx context.Context, q catalog.Queryer) (bool, error) {
	rows, err := q.Query(ctx, "SELECT spanner_type FROM information_schema.columns WHERE table_schema = $1 AND table_name = $2 AND column_name = $3",
		a.Table.Schema, a.Table.Table, a.Columns[0])
	if err != nil {
		return false, err
	}
	held, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(held) != 1 {
		return false, err
	}
	element, array := strings.CutSuffix(held[0], "[]")
	if array != a.Array {
		return false, nil
	}
	if element == "bytea" {
		return true, nil
	}
	limit, _, found := coltype.CharacterLimit(element)
	return found && limit > a.Limit, nil
}

// scalar runs query and returns the one number it answers.
func scalar(ctx context.Context, q catalog.Queryer, query string) (int64, error) {
	rows, err := q.Query(ctx, query)
	if err != nil {
		return 0, err
	}
	counts, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return 0, err
	}
	if len(counts) != 1 {
		return 0, errors.New("the count returned no row")
	}
	return counts[0], nil
}
