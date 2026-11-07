// Package seedledger keeps the ledger of applied seed files in the table and
// layout Ptah's built-in Spanner engine created, so a database that engine
// seeded is continued: public.schema_seeds, keyed by the seed file's path,
// with its environment, checksum and the time it was applied.
package seedledger

import (
	"context"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
)

// Table is the ledger's name, in the default schema.
const Table = "schema_seeds"

// Create makes the ledger when it does not exist. It is the statement the
// built-in engine ran, measured on the emulator: the interface has neither
// CHAR nor TIMESTAMP, so the columns are VARCHAR, TEXT and TIMESTAMPTZ. It is
// a schema statement, so it runs outside a transaction.
const Create = `CREATE TABLE IF NOT EXISTS public.schema_seeds (
    seed_path VARCHAR(512) PRIMARY KEY,
    env VARCHAR(128) NOT NULL,
    checksum TEXT NOT NULL,
    applied_at TIMESTAMPTZ NOT NULL
)`

// Column is one column of the layout: its name and the spanner_type
// information_schema reports. Every column is NOT NULL.
type Column struct {
	Name, Type string
}

// Layout is the ledger's columns as information_schema reports them, in
// order. Its primary key is seed_path.
var Layout = []Column{
	{Name: "seed_path", Type: "character varying(512)"},
	{Name: "env", Type: "character varying(128)"},
	{Name: "checksum", Type: "character varying"},
	{Name: "applied_at", Type: "timestamp with time zone"},
}

// Queryer runs a catalog query. *pgx.Conn implements it.
type Queryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Foreign is a table of the ledger's name that holds another layout. The
// provider never adopts it.
type Foreign struct{}

func (Foreign) Error() string {
	return "the table public.schema_seeds holds another layout than the seed ledger the provider keeps"
}

// Check reports whether the ledger exists, and refuses with Foreign a table
// of its name in another layout.
func Check(ctx context.Context, q Queryer) (bool, error) {
	rows, err := q.Query(ctx, `SELECT column_name, spanner_type, is_nullable FROM information_schema.columns
WHERE table_schema = 'public' AND table_name = 'schema_seeds' ORDER BY ordinal_position`)
	if err != nil {
		return false, err
	}
	type observed struct{ name, spannerType, nullable string }
	columns, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (observed, error) {
		var column observed
		return column, row.Scan(&column.name, &column.spannerType, &column.nullable)
	})
	if err != nil {
		return false, err
	}
	if len(columns) == 0 {
		return false, nil
	}
	if len(columns) != len(Layout) {
		return true, Foreign{}
	}
	for i, column := range columns {
		if column.name != Layout[i].Name || column.spannerType != Layout[i].Type || column.nullable != "NO" {
			return true, Foreign{}
		}
	}
	rows, err = q.Query(ctx, `SELECT column_name FROM information_schema.index_columns
WHERE table_schema = 'public' AND table_name = 'schema_seeds' AND index_name = 'PRIMARY_KEY' AND ordinal_position IS NOT NULL
ORDER BY ordinal_position`)
	if err != nil {
		return true, err
	}
	keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return true, err
	}
	if !slices.Equal(keys, []string{"seed_path"}) {
		return true, Foreign{}
	}
	return true, nil
}

// Read is the query of the whole ledger in path order, with one row more
// than limit so a ledger larger than the limit is told apart.
func Read(limit uint32) string {
	return fmt.Sprintf("SELECT seed_path, checksum, env, applied_at FROM public.schema_seeds ORDER BY seed_path LIMIT %d", uint64(limit)+1)
}

// Remove and Record replace the entry of a path, in the transaction the file
// ran in: the built-in engine deleted and inserted, and the entry takes the
// server's time.
const (
	Remove = "DELETE FROM public.schema_seeds WHERE seed_path = $1"
	Record = "INSERT INTO public.schema_seeds (seed_path, env, checksum, applied_at) VALUES ($1, $2, $3, CURRENT_TIMESTAMP)"
)
