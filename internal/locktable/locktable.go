// Package locktable keeps the provider's locks in a table of the database,
// because Spanner has no session lock: the PostgreSQL interface has no
// advisory locks, and a read-write transaction cannot stay open across a
// schema change, which Spanner runs outside transactions.
//
// A lock is one row: its name, the lock ID that holds it, and the time its
// lease ends, in the server's clock. Taking a lock inserts the row, or takes
// over a row whose lease has ended; checking it extends the lease; giving it
// back deletes the row. A holder that dies keeps the row until its lease ends,
// after which the next attempt takes it. Every statement compares with
// CURRENT_TIMESTAMP on the server, so the clocks of the clients do not matter.
package locktable

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Table is the name of the lock table, in the database's default schema.
const Table = "ptah_locks"

// lease is how long a lock holds without a check. The host checks a lock it
// holds every second, so a lease ends only when its holder stopped checking.
const lease = "INTERVAL '30 seconds'"

// create makes the lock table. The statement runs outside a transaction, as
// every Spanner schema change must, and does nothing when the table exists.
const create = `CREATE TABLE IF NOT EXISTS ptah_locks (
  lock_name varchar(1024) NOT NULL,
  holder varchar(128) NOT NULL,
  expires_at timestamptz NOT NULL,
  PRIMARY KEY (lock_name)
)`

// Layout is the table's columns as information_schema reports them, in order:
// a read reserves a table of this name only when it has exactly these.
var Layout = []Column{
	{Name: "lock_name", Type: "character varying(1024)"},
	{Name: "holder", Type: "character varying(128)"},
	{Name: "expires_at", Type: "timestamp with time zone"},
}

// Column is one column of the layout: its name and the spanner_type
// information_schema reports for it. Every column is NOT NULL.
type Column struct {
	Name, Type string
}

// Create makes the lock table when it does not exist.
func Create(ctx context.Context, conn *pgx.Conn) error {
	_, err := conn.Exec(ctx, create)
	return err
}

// Take makes one attempt to take the lock name for holder. It reports false
// when another holder's lease has not ended. An error from the commit means
// the attempt took nothing when the server refused the commit, and is
// uncertain otherwise; the caller tells them apart.
func Take(ctx context.Context, conn *pgx.Conn, name, holder string) (bool, error) {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var current string
	var live bool
	err = tx.QueryRow(ctx, "SELECT holder, expires_at > CURRENT_TIMESTAMP FROM ptah_locks WHERE lock_name = $1", name).Scan(&current, &live)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		_, err = tx.Exec(ctx, "INSERT INTO ptah_locks (lock_name, holder, expires_at) VALUES ($1, $2, CURRENT_TIMESTAMP + "+lease+")", name, holder)
	case err != nil:
		return false, err
	case live:
		return false, nil
	default:
		// The holder's lease ended: take the row over, unless another
		// attempt took it first.
		var tag pgconn.CommandTag
		tag, err = tx.Exec(ctx, "UPDATE ptah_locks SET holder = $2, expires_at = CURRENT_TIMESTAMP + "+lease+
			" WHERE lock_name = $1 AND holder = $3 AND expires_at <= CURRENT_TIMESTAMP", name, holder, current)
		if err == nil && tag.RowsAffected() != 1 {
			return false, nil
		}
	}
	if err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// Extend reports whether holder still holds the lock name with a live lease,
// and extends the lease when it does. A lease that has ended is not extended:
// the holder cannot show it held the lock without a break.
func Extend(ctx context.Context, conn *pgx.Conn, name, holder string) (bool, error) {
	if conn == nil {
		return false, errors.New("no lock connection")
	}
	tag, err := conn.Exec(ctx, "UPDATE ptah_locks SET expires_at = CURRENT_TIMESTAMP + "+lease+
		" WHERE lock_name = $1 AND holder = $2 AND expires_at > CURRENT_TIMESTAMP", name, holder)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// Give deletes the row of the lock name when holder holds it. A row another
// holder took over is left alone.
func Give(ctx context.Context, conn *pgx.Conn, name, holder string) error {
	if conn == nil {
		return errors.New("no lock connection")
	}
	_, err := conn.Exec(ctx, "DELETE FROM ptah_locks WHERE lock_name = $1 AND holder = $2", name, holder)
	return err
}
