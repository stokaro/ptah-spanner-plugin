package metadata_test

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/protobuf/proto"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-spanner-plugin/internal/metadata"
	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// fakeDB answers every query with rows, records every statement it runs, and
// fails each with failure and reports affected rows for it.
type fakeDB struct {
	rows     [][]any
	execs    []string
	args     [][]any
	failure  error
	affected string
}

func (f *fakeDB) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.execs, f.args = append(f.execs, sql), append(f.args, args)
	return pgconn.NewCommandTag(f.affected), f.failure
}

func (f *fakeDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return &fakeRows{rows: f.rows, at: -1}, nil
}

type fakeRows struct {
	// Rows is nil; it carries the methods no code under test calls.
	pgx.Rows
	rows [][]any
	at   int
}

func (r *fakeRows) Close()                        {}
func (r *fakeRows) Err() error                    { return nil }
func (r *fakeRows) CommandTag() pgconn.CommandTag { return pgconn.NewCommandTag("SELECT") }
func (r *fakeRows) Values() ([]any, error)        { return r.rows[r.at], nil }

func (r *fakeRows) Next() bool {
	r.at++
	return r.at < len(r.rows)
}

func (r *fakeRows) Scan(dest ...any) error {
	for i, value := range r.rows[r.at] {
		target := reflect.ValueOf(dest[i]).Elem()
		if value == nil {
			target.Set(reflect.Zero(target.Type()))
			continue
		}
		target.Set(reflect.ValueOf(value))
	}
	return nil
}

var at = time.Date(2026, 11, 7, 2, 3, 4, atNanos, time.UTC)

// atNanos is the nanosecond part of at.
const atNanos = 123456789

func logStore() metadata.Store {
	return metadata.Store{Table: metadata.LogTable, Kind: protocol.MetadataScope_LOG}
}

func tagStore() metadata.Store {
	return metadata.Store{Table: metadata.TagsTable, Kind: protocol.MetadataScope_TAGS}
}

// A log record is one insert. Its optional facts are NULL when empty, as the
// built-in engine wrote them, and its time is kept to the microsecond.
func TestStore_Append(t *testing.T) {
	c := testcheck.New(t)
	db := &fakeDB{affected: "INSERT 0 1"}
	err := logStore().Append(c.Context(), db, &protocol.MigrationLogRecord{RunId: "r", Sequence: 1, Operation: "up", Version: 7, State: "started",
		ActorSource: "unknown", AtSeconds: at.Unix(), AtNanos: atNanos})
	c.Assert(err, qt.IsNil)
	c.Assert(db.execs, qt.HasLen, 1)
	c.Assert(strings.HasPrefix(db.execs[0], `INSERT INTO "schema_migrations_log" (run_id, seq, operation, version, state, actor, actor_source, checksum, logged_at, error)`), qt.IsTrue)
	c.Assert(db.args[0], qt.DeepEquals, []any{"r", int64(1), "up", int64(7), "started", (*string)(nil), "unknown", (*string)(nil),
		time.Date(2026, 11, 7, 2, 3, 4, 123456000, time.UTC), (*string)(nil)})
}

// A log record whose run and sequence the log holds is a duplicate, which
// Spanner refused, so nothing was written.
func TestStore_Append_Duplicate(t *testing.T) {
	c := testcheck.New(t)
	db := &fakeDB{failure: &pgconn.PgError{Code: "23505"}}
	err := logStore().Append(c.Context(), db, &protocol.MigrationLogRecord{RunId: "r", Sequence: 1})
	c.Assert(err, qt.ErrorIs, metadata.ErrDuplicate)
}

// A tag is inserted when the table does not hold it and updated when it does.
func TestStore_PutTag(t *testing.T) {
	tests := []struct {
		name   string
		rows   [][]any
		prefix string
	}{
		{name: "a new tag", prefix: `INSERT INTO "ptah_migration_tags" (tag, version, recorded_at)`},
		{name: "a held tag", rows: [][]any{{"release", int64(1), at}}, prefix: `UPDATE "ptah_migration_tags" SET version = $2, recorded_at = $3::timestamptz WHERE tag = $1`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			db := &fakeDB{rows: test.rows}
			err := tagStore().PutTag(c.Context(), db, &protocol.MigrationTagRecord{Name: "release", Version: 2, RecordedAtSeconds: at.Unix()})
			c.Assert(err, qt.IsNil)
			c.Assert(db.execs, qt.HasLen, 1)
			c.Assert(strings.HasPrefix(db.execs[0], test.prefix), qt.IsTrue, qt.Commentf("%s", db.execs[0]))
			c.Assert(db.args[0][:2], qt.DeepEquals, []any{"release", int64(2)})
		})
	}
}

// Deleting a tag reports whether the table held it.
func TestStore_DeleteTag(t *testing.T) {
	tests := []struct {
		name     string
		affected string
		want     bool
	}{
		{name: "held", affected: "DELETE 1", want: true},
		{name: "absent", affected: "DELETE 0", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			deleted, err := tagStore().DeleteTag(c.Context(), &fakeDB{affected: test.affected}, "release")
			c.Assert(err, qt.IsNil)
			c.Assert(deleted, qt.Equals, test.want)
		})
	}
}

// A snapshot reads every record, and refuses a store larger than its limit
// rather than returning part of it.
func TestStore_Snapshot(t *testing.T) {
	c := testcheck.New(t)
	actor := "ann"
	db := &fakeDB{rows: [][]any{
		{"r", int64(1), "up", int64(7), "started", &actor, "provided", nil, at, nil},
		{"r", int64(2), "up", int64(7), "applied", nil, "unknown", nil, at, nil},
	}}
	records, err := logStore().Snapshot(c.Context(), db, 1<<20)
	c.Assert(err, qt.IsNil)
	c.Assert(records, qt.HasLen, 2)
	c.Assert(proto.Equal(records[0], &protocol.MigrationLogRecord{RunId: "r", Sequence: 1, Operation: "up", Version: 7, State: "started", Actor: "ann",
		ActorSource: "provided", AtSeconds: at.Unix(), AtNanos: atNanos}), qt.IsTrue, qt.Commentf("%v", records[0]))

	records, err = logStore().Snapshot(c.Context(), db, 10)
	c.Assert(err, qt.ErrorIs, metadata.ErrTooLarge)
	c.Assert(records, qt.IsNil)
}

// The log is created with the built-in engine's statement, and the tag table
// with TIMESTAMPTZ where the engine wrote TIMESTAMP, which Spanner refuses.
func TestStore_Statements(t *testing.T) {
	c := testcheck.New(t)
	c.Assert(logStore().Statements(), qt.HasLen, 1)
	c.Assert(logStore().Statements()[0], qt.Matches, `(?s)CREATE TABLE IF NOT EXISTS "schema_migrations_log" \(.*logged_at TIMESTAMPTZ NOT NULL,.*PRIMARY KEY \(run_id, seq\)\n\)`)
	c.Assert(tagStore().Statements()[0], qt.Matches, `(?s)CREATE TABLE IF NOT EXISTS "ptah_migration_tags" \(.*recorded_at TIMESTAMPTZ NOT NULL\n\)`)
	other := metadata.Store{Schema: "ops", Table: metadata.TagsTable, Kind: protocol.MetadataScope_TAGS}
	c.Assert(other.Statements()[0], qt.Equals, `CREATE SCHEMA IF NOT EXISTS "ops"`)
}
