package history_test

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	qt "github.com/frankban/quicktest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-spanner-plugin/internal/history"
	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// fakeDB answers every query with rows and records every statement it runs.
type fakeDB struct {
	rows  [][]any
	execs []statement
}

type statement struct {
	sql  string
	args []any
}

func (f *fakeDB) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.execs = append(f.execs, statement{sql: sql, args: args})
	return pgconn.NewCommandTag("UPDATE 1"), nil
}

func (f *fakeDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return &fakeRows{rows: f.rows, at: -1}, nil
}

func (f *fakeDB) QueryRow(context.Context, string, ...any) pgx.Row { return nil }

// fakeRows hands out rows of plain values; Scan stores each value in its
// destination, and a nil value as the destination's zero value.
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

// row is the row a written statement's arguments make, in the order a read
// scans it: the column order of both is the table's. A nullable column scans
// into a pointer.
func row(format protocol.HistoryScope_Format, args []any) []any {
	result := append([]any(nil), args...)
	nullable := []int{6, 7}
	if format == protocol.HistoryScope_ATLAS {
		nullable = []int{7, 8, 10}
	}
	for _, i := range nullable {
		if text, ok := result[i].(string); ok {
			result[i] = &text
		}
	}
	return result
}

func native() history.Store {
	return history.Store{Table: history.NativeTable, Format: protocol.HistoryScope_NATIVE}
}

func atlas() history.Store {
	return history.Store{Schema: history.AtlasTable, Table: history.AtlasTable, Format: protocol.HistoryScope_ATLAS}
}

// at is a time with nanoseconds, which the tables keep to the microsecond.
var at = time.Date(2026, 11, 7, 2, 3, 4, atNanos, time.UTC)

// atNanos is the nanosecond part of at.
const atNanos = 123456789

// A record written by Mutate reads back as the same record, held to the
// precision of the built-in engine's layouts: the time to the microsecond,
// and in the native table the run time to the millisecond. A rollback's
// direction travels in the native state column.
func TestStore_WriteThenRead_HappyPath(t *testing.T) {
	tests := []struct {
		name   string
		store  history.Store
		record *protocol.HistoryRevision
		// column is the argument the test reads, and state its value: the
		// native table keeps the state in its fourth column, and the Atlas
		// one the partial hashes in its eleventh.
		column int
		state  any
		read   *protocol.HistoryRevision
	}{
		{
			name:  "an applied native record",
			store: native(),
			record: &protocol.HistoryRevision{Version: "1700000001", Description: "Users", State: protocol.HistoryRevision_APPLIED, Direction: protocol.HistoryRevision_UP,
				Applied: 2, Total: 2, ExecutionNanoseconds: 54_123_456, Checksum: "f190", AppliedAtSeconds: at.Unix(), AppliedAtNanos: atNanos},
			column: 3, state: "applied",
			read: &protocol.HistoryRevision{Version: "1700000001", Description: "Users", State: protocol.HistoryRevision_APPLIED, Direction: protocol.HistoryRevision_UP,
				Applied: 2, Total: 2, ExecutionNanoseconds: 54_000_000, Checksum: "f190", AppliedAtSeconds: at.Unix(), AppliedAtNanos: 123456000},
		},
		{
			name:  "a failed native rollback",
			store: native(),
			record: &protocol.HistoryRevision{Version: "-7", Description: "Bad", State: protocol.HistoryRevision_FAILED, Direction: protocol.HistoryRevision_DOWN,
				Applied: 1, Total: 3, Error: "refused", ErrorStatement: "DROP TABLE t", Checksum: "partial:h1:x", AppliedAtSeconds: at.Unix()},
			column: 3, state: "failed:down",
			read: &protocol.HistoryRevision{Version: "-7", Description: "Bad", State: protocol.HistoryRevision_FAILED, Direction: protocol.HistoryRevision_DOWN,
				Applied: 1, Total: 3, Error: "refused", ErrorStatement: "DROP TABLE t", Checksum: "partial:h1:x", AppliedAtSeconds: at.Unix()},
		},
		{
			name:  "an Atlas record with partial hashes",
			store: atlas(),
			record: &protocol.HistoryRevision{Version: "20261107", Description: "users", AtlasType: 2, Applied: 1, Total: 2, ExecutionNanoseconds: 66_830_751,
				Error: "e", ErrorStatement: "s", Checksum: "f190", PartialHashes: []string{"h1:a", "h1:b"}, OperatorVersion: "Ptah",
				AppliedAtSeconds: at.Unix(), AppliedAtNanos: atNanos},
			column: 10, state: `["h1:a","h1:b"]`,
			read: &protocol.HistoryRevision{Version: "20261107", Description: "users", AtlasType: 2, Applied: 1, Total: 2, ExecutionNanoseconds: 66_830_751,
				Error: "e", ErrorStatement: "s", Checksum: "f190", PartialHashes: []string{"h1:a", "h1:b"}, OperatorVersion: "Ptah",
				AppliedAtSeconds: at.Unix(), AppliedAtNanos: 123456000},
		},
		{
			name:   "an Atlas record without partial hashes",
			store:  atlas(),
			record: &protocol.HistoryRevision{Version: "", Description: "baseline", AtlasType: 4, Checksum: "", OperatorVersion: "Ptah", AppliedAtSeconds: at.Unix()},
			column: 10, state: "null",
			read: &protocol.HistoryRevision{Version: "", Description: "baseline", AtlasType: 4, Checksum: "", OperatorVersion: "Ptah", AppliedAtSeconds: at.Unix()},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			db := &fakeDB{}
			outcome, err := test.store.Mutate(c.Context(), db, &protocol.MutateHistoryRequest{Replacement: test.record})
			c.Assert(err, qt.IsNil)
			c.Assert(outcome, qt.Equals, protocol.HistoryResult_APPLIED)
			c.Assert(db.execs, qt.HasLen, 1)
			c.Assert(strings.HasPrefix(db.execs[0].sql, "INSERT INTO "), qt.IsTrue, qt.Commentf("%s", db.execs[0].sql))
			c.Assert(db.execs[0].args[test.column], qt.Equals, test.state)

			db.rows = [][]any{row(test.store.Format, db.execs[0].args)}
			records, err := test.store.Read(c.Context(), db, 10)
			c.Assert(err, qt.IsNil)
			c.Assert(records, qt.HasLen, 1)
			c.Assert(proto.Equal(records[0], test.read), qt.IsTrue, qt.Commentf("%v", records[0]))
		})
	}
}

// An empty error is NULL in the native table, as the built-in engine wrote
// it, and an empty string in the Atlas one.
func TestStore_EmptyErrors(t *testing.T) {
	c := testcheck.New(t)
	db := &fakeDB{}
	_, err := native().Mutate(c.Context(), db, &protocol.MutateHistoryRequest{Replacement: &protocol.HistoryRevision{Version: "1",
		State: protocol.HistoryRevision_PENDING, Direction: protocol.HistoryRevision_UP, AppliedAtSeconds: at.Unix()}})
	c.Assert(err, qt.IsNil)
	_, err = atlas().Mutate(c.Context(), db, &protocol.MutateHistoryRequest{Replacement: &protocol.HistoryRevision{Version: "1", AppliedAtSeconds: at.Unix()}})
	c.Assert(err, qt.IsNil)
	c.Assert(db.execs[0].args[6:8], qt.DeepEquals, []any{(*string)(nil), (*string)(nil)})
	c.Assert(db.execs[1].args[7:9], qt.DeepEquals, []any{"", ""})
}

// A replacement of a stored record updates it under its version, and a
// missing replacement deletes it, when the stored record is the expected one.
func TestStore_Mutate_HappyPath(t *testing.T) {
	stored := &protocol.HistoryRevision{Version: "5", Description: "d", State: protocol.HistoryRevision_PENDING, Direction: protocol.HistoryRevision_UP,
		Applied: 1, Total: 2, AppliedAtSeconds: at.Unix()}
	applied := proto.CloneOf(stored)
	applied.State, applied.Applied = protocol.HistoryRevision_APPLIED, 2
	tests := []struct {
		name        string
		replacement *protocol.HistoryRevision
		prefix      string
	}{
		{name: "replace", replacement: applied, prefix: `UPDATE "schema_migrations" SET `},
		{name: "delete", prefix: `DELETE FROM "schema_migrations" WHERE version = $1`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			seed := &fakeDB{}
			_, err := native().Mutate(c.Context(), seed, &protocol.MutateHistoryRequest{Replacement: stored})
			c.Assert(err, qt.IsNil)
			db := &fakeDB{rows: [][]any{row(protocol.HistoryScope_NATIVE, seed.execs[0].args)}}
			outcome, err := native().Mutate(c.Context(), db, &protocol.MutateHistoryRequest{Expected: stored, Replacement: test.replacement})
			c.Assert(err, qt.IsNil)
			c.Assert(outcome, qt.Equals, protocol.HistoryResult_APPLIED)
			c.Assert(db.execs, qt.HasLen, 1)
			c.Assert(strings.HasPrefix(db.execs[0].sql, test.prefix), qt.IsTrue, qt.Commentf("%s", db.execs[0].sql))
			c.Assert(db.execs[0].args[0], qt.Equals, int64(5))
		})
	}
}

// A stored record that differs from the expected one, or a record where none
// was expected, is a conflict that writes nothing.
func TestStore_Mutate_Conflict(t *testing.T) {
	stored := &protocol.HistoryRevision{Version: "5", State: protocol.HistoryRevision_PENDING, Direction: protocol.HistoryRevision_UP, AppliedAtSeconds: at.Unix()}
	other := proto.CloneOf(stored)
	other.Applied = 1
	tests := []struct {
		name     string
		expected *protocol.HistoryRevision
	}{
		{name: "another record", expected: other},
		{name: "none expected", expected: nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			seed := &fakeDB{}
			_, err := native().Mutate(c.Context(), seed, &protocol.MutateHistoryRequest{Replacement: stored})
			c.Assert(err, qt.IsNil)
			db := &fakeDB{rows: [][]any{row(protocol.HistoryScope_NATIVE, seed.execs[0].args)}}
			outcome, err := native().Mutate(c.Context(), db, &protocol.MutateHistoryRequest{Expected: test.expected, Replacement: stored})
			c.Assert(err, qt.IsNil)
			c.Assert(outcome, qt.Equals, protocol.HistoryResult_CONFLICT)
			c.Assert(db.execs, qt.HasLen, 0)
		})
	}
}

// A record the layout cannot hold is refused before anything is read or
// written.
func TestStore_Mutate_FailurePath(t *testing.T) {
	tests := []struct {
		name   string
		store  history.Store
		record *protocol.HistoryRevision
	}{
		{name: "native with an operator version", store: native(), record: &protocol.HistoryRevision{Version: "1", State: protocol.HistoryRevision_APPLIED,
			Direction: protocol.HistoryRevision_UP, OperatorVersion: "Ptah"}},
		{name: "native with a version that is not canonical", store: native(), record: &protocol.HistoryRevision{Version: "01",
			State: protocol.HistoryRevision_APPLIED, Direction: protocol.HistoryRevision_UP}},
		{name: "Atlas with a state", store: atlas(), record: &protocol.HistoryRevision{Version: "1", State: protocol.HistoryRevision_APPLIED}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			db := &fakeDB{}
			outcome, err := test.store.Mutate(c.Context(), db, &protocol.MutateHistoryRequest{Replacement: test.record})
			c.Assert(err, qt.ErrorIs, history.ErrRecord)
			c.Assert(outcome, qt.Equals, protocol.HistoryResult_REFUSED)
			c.Assert(db.execs, qt.HasLen, 0)
		})
	}
}

// A read refuses a table that holds more records than it takes, and a row
// whose state column no record stands for, rather than returning part of the
// history.
func TestStore_Read_FailurePath(t *testing.T) {
	good := []any{int64(1), "d", at, "applied", int64(1), int64(1), nil, nil, int64(0), ""}
	unknownState := append([]any(nil), good...)
	unknownState[3] = "done"
	unknownDirection := append([]any(nil), good...)
	unknownDirection[3] = "pending:sideways"
	tests := []struct {
		name  string
		rows  [][]any
		limit uint32
		want  error
	}{
		{name: "more records than the limit", rows: [][]any{good, good}, limit: 1, want: history.ErrTooMany},
		{name: "an unknown state", rows: [][]any{unknownState}, limit: 10, want: history.ErrRecord},
		{name: "an unknown direction", rows: [][]any{unknownDirection}, limit: 10, want: history.ErrRecord},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := testcheck.New(t)
			records, err := native().Read(c.Context(), &fakeDB{rows: test.rows}, test.limit)
			c.Assert(err, qt.ErrorIs, test.want)
			c.Assert(records, qt.IsNil)
		})
	}
}

// A store in the default schema is created with the built-in engine's
// statement alone; one in another schema creates the schema first, as the
// built-in engine did for the Atlas table's own schema.
func TestStore_Statements(t *testing.T) {
	c := testcheck.New(t)
	statements := native().Statements()
	c.Assert(statements, qt.HasLen, 1)
	c.Assert(statements[0], qt.Matches, `(?s)CREATE TABLE IF NOT EXISTS "schema_migrations" \(\n    version BIGINT PRIMARY KEY,.*checksum VARCHAR\(64\) NOT NULL DEFAULT ''\n\)`)
	statements = atlas().Statements()
	c.Assert(statements, qt.HasLen, 2)
	c.Assert(statements[0], qt.Equals, `CREATE SCHEMA IF NOT EXISTS "atlas_schema_revisions"`)
	c.Assert(statements[1], qt.Matches, `(?s)CREATE TABLE IF NOT EXISTS "atlas_schema_revisions"\."atlas_schema_revisions" \(\n    version VARCHAR PRIMARY KEY,.*partial_hashes JSONB NULL,.*`)
}
