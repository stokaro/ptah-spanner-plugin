// Package metadata keeps the migration log and the migration tags of Ptah's
// migrator in the tables Ptah's built-in Spanner engine named for them: the
// log beside the native revision table, and the tags beside either revision
// table.
//
// The log keeps the built-in engine's layout, so a database it migrated keeps
// its log and the engine can still read what the provider appends. The tag
// table cannot: the engine created it with a TIMESTAMP column, which Spanner
// refuses (`Type <timestamp> is not supported`), so `migrations tag` never
// worked on Spanner and no database holds the engine's tag table. The provider
// creates it with TIMESTAMPTZ, the type the engine used for every other time
// it kept on Spanner. A table of a store's name in any other layout is refused
// (see ownedtable).
package metadata

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/protobuf/proto"
	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-spanner-plugin/internal/ddl"
	"github.com/stokaro/ptah-spanner-plugin/internal/ownedtable"
)

// The tables the built-in engine named under its default names.
const (
	LogTable  = "schema_migrations_log"
	TagsTable = "ptah_migration_tags"
)

// logColumns are the log's columns as the built-in engine's CREATE TABLE left
// them, measured on the emulator behind PGAdapter 0.56.2.
var logColumns = []ownedtable.Column{
	{Name: "run_id", Type: "character varying(64)"},
	{Name: "seq", Type: "bigint"},
	{Name: "operation", Type: "character varying(32)"},
	{Name: "version", Type: "bigint"},
	{Name: "state", Type: "character varying(32)"},
	{Name: "actor", Type: "character varying(256)", Nullable: true},
	{Name: "actor_source", Type: "character varying(32)"},
	{Name: "checksum", Type: "character varying(64)", Nullable: true},
	{Name: "logged_at", Type: "timestamp with time zone"},
	{Name: "error", Type: "character varying", Nullable: true},
}

// tagColumns are the tag table's columns as createTags leaves them.
var tagColumns = []ownedtable.Column{
	{Name: "tag", Type: "character varying(255)"},
	{Name: "version", Type: "bigint"},
	{Name: "recorded_at", Type: "timestamp with time zone"},
}

// The CREATE TABLE statements, with the table's quoted name in place of %s:
// the built-in engine's for the log, and its tag table with TIMESTAMPTZ.
const (
	createLog = `CREATE TABLE IF NOT EXISTS %s (
    run_id VARCHAR(64) NOT NULL,
    seq BIGINT NOT NULL,
    operation VARCHAR(32) NOT NULL,
    version BIGINT NOT NULL,
    state VARCHAR(32) NOT NULL,
    actor VARCHAR(256) NULL,
    actor_source VARCHAR(32) NOT NULL,
    checksum VARCHAR(64) NULL,
    logged_at TIMESTAMPTZ NOT NULL,
    error TEXT NULL,
    PRIMARY KEY (run_id, seq)
)`
	createTags = `CREATE TABLE IF NOT EXISTS %s (
    tag VARCHAR(255) NOT NULL PRIMARY KEY,
    version BIGINT NOT NULL,
    recorded_at TIMESTAMPTZ NOT NULL
)`
)

// Querier runs statements on a connection or in a transaction. *pgx.Conn
// and pgx.Tx implement it.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Store is the log or the tag table a scope names.
type Store struct {
	Schema, Table string
	Kind          protocol.MetadataScope_Kind
}

// StoreOf is the store a scope names. The SDK has already refused a scope
// without a table or with an unknown kind.
func StoreOf(scope *protocol.MetadataScope) Store {
	return Store{Schema: scope.GetSchema(), Table: scope.GetTable(), Kind: scope.GetKind()}
}

// Definition is the store's table and the layout it keeps.
func (s Store) Definition() ownedtable.Definition {
	if s.Kind == protocol.MetadataScope_LOG {
		return Log(s.Schema, s.Table)
	}
	return Tags(s.Schema, s.Table)
}

// Log is the definition of a log table, for a read that tells the provider's
// tables from a user's.
func Log(schema, table string) ownedtable.Definition {
	return ownedtable.Definition{Schema: schema, Name: table, Columns: logColumns, PrimaryKey: []string{"run_id", "seq"}}
}

// Tags is [Log] for a tag table.
func Tags(schema, table string) ownedtable.Definition {
	return ownedtable.Definition{Schema: schema, Name: table, Columns: tagColumns, PrimaryKey: []string{"tag"}}
}

// Statements are the schema statements that create the store: the schema
// when it is not the default one, then the table. Each does nothing when its
// object exists.
func (s Store) Statements() []string {
	definition := s.Definition()
	var statements []string
	if definition.SchemaName() != ddl.DefaultSchema {
		statements = append(statements, "CREATE SCHEMA IF NOT EXISTS "+ddl.Quote(definition.SchemaName()))
	}
	create := createTags
	if s.Kind == protocol.MetadataScope_LOG {
		create = createLog
	}
	return append(statements, fmt.Sprintf(create, definition.Quoted()))
}

// ErrDuplicate reports a log record whose identity the log holds already. The
// insert that found it wrote nothing.
var ErrDuplicate = errors.New("the log holds a record with this run and sequence")

// Append writes one log record. A record whose run and sequence the log holds
// already fails with [ErrDuplicate] and writes nothing.
func (s Store) Append(ctx context.Context, q Querier, record *protocol.MigrationLogRecord) error {
	_, err := q.Exec(ctx, "INSERT INTO "+s.Definition().Quoted()+
		" (run_id, seq, operation, version, state, actor, actor_source, checksum, logged_at, error)"+
		" VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::timestamptz, $10)",
		record.GetRunId(), record.GetSequence(), record.GetOperation(), record.GetVersion(), record.GetState(), nullable(record.GetActor()),
		record.GetActorSource(), nullable(record.GetChecksum()), Time(record.GetAtSeconds(), record.GetAtNanos()), nullable(record.GetError()))
	if refused, found := errors.AsType[*pgconn.PgError](err); found && refused.Code == "23505" {
		return ErrDuplicate
	}
	return err
}

// PutTag records a tag, replacing the record of a tag of the same name. The
// caller runs it in a read-write transaction, which makes the read and the
// write one step.
func (s Store) PutTag(ctx context.Context, q Querier, record *protocol.MigrationTagRecord) error {
	found, err := s.Lookup(ctx, q, record.GetName())
	if err != nil {
		return err
	}
	statement := "INSERT INTO " + s.Definition().Quoted() + " (tag, version, recorded_at) VALUES ($1, $2, $3::timestamptz)"
	if found != nil {
		statement = "UPDATE " + s.Definition().Quoted() + " SET version = $2, recorded_at = $3::timestamptz WHERE tag = $1"
	}
	_, err = q.Exec(ctx, statement, record.GetName(), record.GetVersion(), Time(record.GetRecordedAtSeconds(), record.GetRecordedAtNanos()))
	return err
}

// DeleteTag removes a tag and reports whether the table held it.
func (s Store) DeleteTag(ctx context.Context, q Querier, name string) (bool, error) {
	tag, err := q.Exec(ctx, "DELETE FROM "+s.Definition().Quoted()+" WHERE tag = $1", name)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// Lookup returns the record of a tag, or nil when the table holds none.
func (s Store) Lookup(ctx context.Context, q Querier, name string) (*protocol.MigrationTagRecord, error) {
	rows, err := q.Query(ctx, "SELECT tag, version, recorded_at FROM "+s.Definition().Quoted()+" WHERE tag = $1", name)
	if err != nil {
		return nil, err
	}
	records, err := pgx.CollectRows(rows, scanTag)
	if err != nil || len(records) == 0 {
		return nil, err
	}
	return records[0], nil
}

// ErrTooLarge reports a store larger than a snapshot holds.
var ErrTooLarge = errors.New("the store holds more than a snapshot reads")

// Snapshot reads every record of the store in key order: log records by run
// and sequence, tags by name. It fails with [ErrTooLarge] when the records
// exceed limit bytes, rather than returning part of the store.
func (s Store) Snapshot(ctx context.Context, q Querier, limit int) ([]proto.Message, error) {
	query := "SELECT tag, version, recorded_at FROM " + s.Definition().Quoted() + " ORDER BY tag"
	scan := func(row pgx.CollectableRow) (proto.Message, error) { return scanTag(row) }
	if s.Kind == protocol.MetadataScope_LOG {
		query = "SELECT run_id, seq, operation, version, state, actor, actor_source, checksum, logged_at, error FROM " +
			s.Definition().Quoted() + " ORDER BY run_id, seq"
		scan = func(row pgx.CollectableRow) (proto.Message, error) { return scanLog(row) }
	}
	rows, err := q.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []proto.Message
	size := 0
	for rows.Next() {
		record, err := scan(rows)
		if err != nil {
			return nil, err
		}
		size += proto.Size(record)
		if size > limit {
			return nil, ErrTooLarge
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

func scanLog(row pgx.CollectableRow) (*protocol.MigrationLogRecord, error) {
	var record protocol.MigrationLogRecord
	var actor, checksum, failure *string
	var at time.Time
	if err := row.Scan(&record.RunId, &record.Sequence, &record.Operation, &record.Version, &record.State, &actor, &record.ActorSource,
		&checksum, &at, &failure); err != nil {
		return nil, err
	}
	at = at.UTC()
	record.Actor, record.Checksum, record.Error = text(actor), text(checksum), text(failure)
	record.AtSeconds, record.AtNanos = at.Unix(), int32(at.Nanosecond()) // #nosec G115 -- Nanosecond is below 1e9
	return &record, nil
}

func scanTag(row pgx.CollectableRow) (*protocol.MigrationTagRecord, error) {
	var record protocol.MigrationTagRecord
	var at time.Time
	if err := row.Scan(&record.Name, &record.Version, &at); err != nil {
		return nil, err
	}
	at = at.UTC()
	record.RecordedAtSeconds, record.RecordedAtNanos = at.Unix(), int32(at.Nanosecond()) // #nosec G115 -- Nanosecond is below 1e9
	return &record, nil
}

// Time is a record's time as the table keeps it: in UTC, to the microsecond.
func Time(seconds int64, nanos int32) time.Time {
	return time.Unix(seconds, int64(nanos)).UTC().Truncate(time.Microsecond)
}

// nullable is text as a nullable column takes it: NULL for none, as the
// built-in engine wrote the log's optional columns.
func nullable(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

// text is a nullable column's value as a record carries it.
func text(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
