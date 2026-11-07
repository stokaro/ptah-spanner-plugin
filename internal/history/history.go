// Package history keeps the revision records of Ptah's migrator in the
// revision tables Ptah's built-in Spanner engine wrote, in that engine's
// layouts: the native table and the Atlas-compatible one.
//
// The provider creates a missing table with the statement that engine used,
// and reads and writes an existing one column by column, so a database the
// built-in engine migrated keeps its history and the engine can still read
// what the provider writes. A table of the store's name in any other layout
// is refused (see ownedtable).
//
// The layouts hold some facts more coarsely than a record carries them, and a
// record is written in that precision: Spanner keeps a timestamp to the
// microsecond, and the native table keeps a run time to the millisecond. The
// native table has no direction column; the built-in engine wrote a rollback's
// direction into the state column as `pending:down` or `failed:down`, and so
// does the provider.
package history

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"
	"github.com/stokaro/ptah/provider-sdk/provider"
	"google.golang.org/protobuf/proto"

	"github.com/stokaro/ptah-spanner-plugin/internal/ddl"
	"github.com/stokaro/ptah-spanner-plugin/internal/ownedtable"
)

// The tables the built-in engine wrote under their default names: the native
// revision table in the default schema, and the Atlas-compatible one.
const (
	NativeTable = "schema_migrations"
	AtlasTable  = "atlas_schema_revisions"
)

// nativeColumns are the native table's columns as the built-in engine's
// CREATE TABLE left them, measured on the emulator behind PGAdapter 0.56.2:
// Spanner keeps INTEGER as bigint and TEXT as character varying.
var nativeColumns = []ownedtable.Column{
	{Name: "version", Type: "bigint"},
	{Name: "description", Type: "character varying"},
	{Name: "applied_at", Type: "timestamp with time zone"},
	{Name: "state", Type: "character varying(32)"},
	{Name: "applied", Type: "bigint"},
	{Name: "total", Type: "bigint"},
	{Name: "error", Type: "character varying", Nullable: true},
	{Name: "error_stmt", Type: "character varying", Nullable: true},
	{Name: "execution_time_ms", Type: "bigint"},
	{Name: "checksum", Type: "character varying(64)"},
}

// atlasColumns are the Atlas-compatible table's columns, measured the same way.
var atlasColumns = []ownedtable.Column{
	{Name: "version", Type: "character varying"},
	{Name: "description", Type: "character varying"},
	{Name: "type", Type: "bigint"},
	{Name: "applied", Type: "bigint"},
	{Name: "total", Type: "bigint"},
	{Name: "executed_at", Type: "timestamp with time zone"},
	{Name: "execution_time", Type: "bigint"},
	{Name: "error", Type: "character varying", Nullable: true},
	{Name: "error_stmt", Type: "character varying", Nullable: true},
	{Name: "hash", Type: "character varying"},
	{Name: "partial_hashes", Type: "jsonb", Nullable: true},
	{Name: "operator_version", Type: "character varying"},
}

// The built-in engine's CREATE TABLE statements, with the table's quoted name
// in place of %s. Spanner has neither TIMESTAMP nor JSON, so the engine wrote
// TIMESTAMPTZ and JSONB.
const (
	createNative = `CREATE TABLE IF NOT EXISTS %s (
    version BIGINT PRIMARY KEY,
    description TEXT NOT NULL,
    applied_at TIMESTAMPTZ NOT NULL,
    state VARCHAR(32) NOT NULL DEFAULT 'applied',
    applied INTEGER NOT NULL DEFAULT 1,
    total INTEGER NOT NULL DEFAULT 1,
    error TEXT NULL,
    error_stmt TEXT NULL,
    execution_time_ms BIGINT NOT NULL DEFAULT 0,
    checksum VARCHAR(64) NOT NULL DEFAULT ''
)`
	createAtlas = `CREATE TABLE IF NOT EXISTS %s (
    version VARCHAR PRIMARY KEY,
    description VARCHAR NOT NULL,
    type BIGINT NOT NULL DEFAULT 2,
    applied BIGINT NOT NULL DEFAULT 0,
    total BIGINT NOT NULL DEFAULT 0,
    executed_at TIMESTAMPTZ NOT NULL,
    execution_time BIGINT NOT NULL,
    error TEXT NULL,
    error_stmt TEXT NULL,
    hash VARCHAR NOT NULL,
    partial_hashes JSONB NULL,
    operator_version VARCHAR NOT NULL
)`
)

// Querier runs statements on a connection or in a transaction. *pgx.Conn
// and pgx.Tx implement it.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Store is one revision table: where it is and which layout it keeps.
type Store struct {
	Schema, Table string
	Format        protocol.HistoryScope_Format
}

// StoreOf is the store a scope names. The SDK has already refused a scope
// without a table or with an unknown format.
func StoreOf(scope *protocol.HistoryScope) Store {
	return Store{Schema: scope.GetSchema(), Table: scope.GetTable(), Format: scope.GetFormat()}
}

// Definition is the store's table and the layout it keeps.
func (s Store) Definition() ownedtable.Definition {
	columns := nativeColumns
	if s.Format == protocol.HistoryScope_ATLAS {
		columns = atlasColumns
	}
	return ownedtable.Definition{Schema: s.Schema, Name: s.Table, Columns: columns, PrimaryKey: []string{"version"}}
}

// Native and Atlas are the definitions of a store of each format, for a read
// that tells the provider's tables from a user's.
func Native(schema, table string) ownedtable.Definition {
	return Store{Schema: schema, Table: table, Format: protocol.HistoryScope_NATIVE}.Definition()
}

// Atlas is [Native] for the Atlas-compatible layout.
func Atlas(schema, table string) ownedtable.Definition {
	return Store{Schema: schema, Table: table, Format: protocol.HistoryScope_ATLAS}.Definition()
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
	create := createNative
	if s.Format == protocol.HistoryScope_ATLAS {
		create = createAtlas
	}
	return append(statements, fmt.Sprintf(create, definition.Quoted()))
}

// selectColumns is the column list a read selects, in the order scan reads it.
func (s Store) selectColumns() string {
	if s.Format == protocol.HistoryScope_ATLAS {
		return "version, description, type, applied, total, executed_at, execution_time, error, error_stmt, hash, partial_hashes::text, operator_version"
	}
	return "version, description, applied_at, state, applied, total, error, error_stmt, execution_time_ms, checksum"
}

// Read returns at most limit records in version order, or an error when the
// table holds more. The caller has located the table.
func (s Store) Read(ctx context.Context, q Querier, limit uint32) ([]*protocol.HistoryRevision, error) {
	rows, err := q.Query(ctx, "SELECT "+s.selectColumns()+" FROM "+s.Definition().Quoted()+
		" ORDER BY version LIMIT "+strconv.FormatUint(uint64(limit)+1, 10))
	if err != nil {
		return nil, err
	}
	records, err := pgx.CollectRows(rows, s.scan)
	if err != nil {
		return nil, err
	}
	if len(records) > int(limit) {
		return nil, ErrTooMany
	}
	return records, nil
}

// ErrTooMany reports a table that holds more records than a read may return.
var ErrTooMany = errors.New("the revision table holds more records than a read returns")

// ErrRecord reports a row the layout holds that no record stands for, or a
// record the layout cannot hold.
var ErrRecord = errors.New("the revision record does not fit the revision table")

// Mutate compares the stored record of the request's version with the
// expected one and, when they are equal, replaces or deletes it, or inserts
// the replacement when neither exists. It reports CONFLICT and changes nothing
// when they differ. The caller runs it in a read-write transaction, which
// makes the comparison and the write one atomic step.
func (s Store) Mutate(ctx context.Context, q Querier, request *protocol.MutateHistoryRequest) (protocol.HistoryResult_Outcome, error) {
	version := request.GetExpected().GetVersion()
	if request.GetExpected() == nil {
		version = request.GetReplacement().GetVersion()
	}
	key, err := s.key(version)
	if err != nil {
		return protocol.HistoryResult_REFUSED, err
	}
	var write string
	var args []any
	if request.GetReplacement() != nil {
		values, err := s.values(request.GetReplacement())
		if err != nil {
			return protocol.HistoryResult_REFUSED, err
		}
		write, args = s.update(), values
		if request.GetExpected() == nil {
			write = s.insert()
		}
	} else {
		write, args = "DELETE FROM "+s.Definition().Quoted()+" WHERE version = $1", []any{key}
	}
	rows, err := q.Query(ctx, "SELECT "+s.selectColumns()+" FROM "+s.Definition().Quoted()+" WHERE version = $1", key)
	if err != nil {
		return protocol.HistoryResult_UNKNOWN, err
	}
	stored, err := pgx.CollectRows(rows, s.scan)
	if err != nil {
		return protocol.HistoryResult_UNKNOWN, err
	}
	var current *protocol.HistoryRevision
	if len(stored) == 1 {
		current = stored[0]
	}
	if len(stored) > 1 || !proto.Equal(current, request.GetExpected()) {
		return protocol.HistoryResult_CONFLICT, nil
	}
	if _, err := q.Exec(ctx, write, args...); err != nil {
		return protocol.HistoryResult_UNKNOWN, err
	}
	return protocol.HistoryResult_APPLIED, nil
}

// key is the version as the table's key column holds it: a signed integer in
// the native layout, the text itself in the Atlas one.
func (s Store) key(version string) (any, error) {
	if s.Format == protocol.HistoryScope_ATLAS {
		return version, nil
	}
	value, err := strconv.ParseInt(version, 10, 64)
	if err != nil || strconv.FormatInt(value, 10) != version {
		return nil, fmt.Errorf("%w: a native revision version is a signed decimal integer", ErrRecord)
	}
	return value, nil
}

// insert and update write a record's values, in the order values returns
// them, as one row.
func (s Store) insert() string {
	if s.Format == protocol.HistoryScope_ATLAS {
		return "INSERT INTO " + s.Definition().Quoted() + " (version, description, type, applied, total, executed_at, execution_time, error, error_stmt, hash, partial_hashes, operator_version)" +
			" VALUES ($1, $2, $3, $4, $5, $6::timestamptz, $7, $8, $9, $10, $11::jsonb, $12)"
	}
	return "INSERT INTO " + s.Definition().Quoted() + " (version, description, applied_at, state, applied, total, error, error_stmt, execution_time_ms, checksum)" +
		" VALUES ($1, $2, $3::timestamptz, $4, $5, $6, $7, $8, $9, $10)"
}

func (s Store) update() string {
	if s.Format == protocol.HistoryScope_ATLAS {
		return "UPDATE " + s.Definition().Quoted() + " SET description = $2, type = $3, applied = $4, total = $5, executed_at = $6::timestamptz," +
			" execution_time = $7, error = $8, error_stmt = $9, hash = $10, partial_hashes = $11::jsonb, operator_version = $12 WHERE version = $1"
	}
	return "UPDATE " + s.Definition().Quoted() + " SET description = $2, applied_at = $3::timestamptz, state = $4, applied = $5, total = $6," +
		" error = $7, error_stmt = $8, execution_time_ms = $9, checksum = $10 WHERE version = $1"
}

// values are a record's column values in the order insert and update write
// them, as the built-in engine wrote a row: in the native layout an empty
// error is NULL; in the Atlas layout it is an empty string, and no partial
// hashes are the JSON null.
func (s Store) values(record *protocol.HistoryRevision) ([]any, error) {
	if err := provider.ValidateHistoryRevision(s.Format, record); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrRecord, err)
	}
	key, err := s.key(record.GetVersion())
	if err != nil {
		return nil, err
	}
	if record.GetApplied() > math.MaxInt64 || record.GetTotal() > math.MaxInt64 {
		return nil, fmt.Errorf("%w: its counts exceed the table's bigint columns", ErrRecord)
	}
	at := Time(record.GetAppliedAtSeconds(), record.GetAppliedAtNanos())
	if s.Format == protocol.HistoryScope_ATLAS {
		if record.GetState() != protocol.HistoryRevision_STATE_UNSPECIFIED || record.GetDirection() != protocol.HistoryRevision_DIRECTION_UNSPECIFIED {
			return nil, fmt.Errorf("%w: an Atlas record states no state or direction", ErrRecord)
		}
		hashes := "null"
		if len(record.GetPartialHashes()) != 0 {
			encoded, err := json.Marshal(record.GetPartialHashes())
			if err != nil {
				return nil, err
			}
			hashes = string(encoded)
		}
		// #nosec G115 -- the counts are at most math.MaxInt64, checked above, and an Atlas type is a uint32
		return []any{key, record.GetDescription(), int64(record.GetAtlasType()), int64(record.GetApplied()), int64(record.GetTotal()), at,
			record.GetExecutionNanoseconds(), record.GetError(), record.GetErrorStatement(), record.GetChecksum(), hashes, record.GetOperatorVersion()}, nil
	}
	if record.GetOperatorVersion() != "" || len(record.GetPartialHashes()) != 0 || record.GetAtlasType() != 0 {
		return nil, fmt.Errorf("%w: the native table has no column for an operator version, partial hashes or an Atlas type", ErrRecord)
	}
	state, err := nativeState(record)
	if err != nil {
		return nil, err
	}
	// #nosec G115 -- the counts are at most math.MaxInt64, checked above
	return []any{key, record.GetDescription(), at, state, int64(record.GetApplied()), int64(record.GetTotal()), nullable(record.GetError()),
		nullable(record.GetErrorStatement()), record.GetExecutionNanoseconds() / int64(time.Millisecond), record.GetChecksum()}, nil
}

// nativeStates are the native table's states and the record's.
var nativeStates = map[string]protocol.HistoryRevision_State{
	"applied": protocol.HistoryRevision_APPLIED, "pending": protocol.HistoryRevision_PENDING, "failed": protocol.HistoryRevision_FAILED,
}

// nativeState is the state column's value for a record: its state, followed
// by `:down` when a rollback wrote it.
func nativeState(record *protocol.HistoryRevision) (string, error) {
	var state string
	for name, value := range nativeStates {
		if value == record.GetState() {
			state = name
		}
	}
	switch {
	case state == "":
		return "", fmt.Errorf("%w: a native record states its state", ErrRecord)
	case record.GetDirection() == protocol.HistoryRevision_DOWN:
		return state + ":down", nil
	case record.GetDirection() != protocol.HistoryRevision_UP:
		return "", fmt.Errorf("%w: a native record states its direction", ErrRecord)
	}
	return state, nil
}

// scan reads one row of the store's layout as a record.
func (s Store) scan(row pgx.CollectableRow) (*protocol.HistoryRevision, error) {
	if s.Format == protocol.HistoryScope_ATLAS {
		var version, description, hash, operator string
		var kind, applied, total, execution int64
		var at time.Time
		var failure, statement, hashes *string
		if err := row.Scan(&version, &description, &kind, &applied, &total, &at, &execution, &failure, &statement, &hash, &hashes, &operator); err != nil {
			return nil, err
		}
		if kind < 0 || kind > math.MaxUint32 || applied < 0 || total < 0 {
			return nil, fmt.Errorf("%w: a stored count is negative or out of range", ErrRecord)
		}
		var partial []string
		if hashes != nil {
			if err := json.Unmarshal([]byte(*hashes), &partial); err != nil {
				return nil, fmt.Errorf("%w: its partial hashes are not a JSON array of strings", ErrRecord)
			}
		}
		at = at.UTC()
		return &protocol.HistoryRevision{
			Version: version, Description: description, AtlasType: uint32(kind), Applied: uint64(applied), Total: uint64(total),
			ExecutionNanoseconds: execution, Error: text(failure), ErrorStatement: text(statement), Checksum: hash, PartialHashes: partial,
			OperatorVersion: operator, AppliedAtSeconds: at.Unix(), AppliedAtNanos: int32(at.Nanosecond()), // #nosec G115 -- Nanosecond is below 1e9
		}, nil
	}
	var version, applied, total, execution int64
	var description, state, checksum string
	var at time.Time
	var failure, statement *string
	if err := row.Scan(&version, &description, &at, &state, &applied, &total, &failure, &statement, &execution, &checksum); err != nil {
		return nil, err
	}
	stored, suffix, directed := strings.Cut(state, ":")
	recordState, known := nativeStates[stored]
	direction := protocol.HistoryRevision_UP
	switch {
	case !known:
		return nil, fmt.Errorf("%w: the state column holds an unknown state", ErrRecord)
	case directed && suffix == "down":
		direction = protocol.HistoryRevision_DOWN
	case directed && suffix != "up":
		return nil, fmt.Errorf("%w: the state column holds an unknown direction", ErrRecord)
	}
	if applied < 0 || total < 0 || execution < 0 || execution > math.MaxInt64/int64(time.Millisecond) {
		return nil, fmt.Errorf("%w: a stored count is negative or out of range", ErrRecord)
	}
	at = at.UTC()
	return &protocol.HistoryRevision{
		Version: strconv.FormatInt(version, 10), Description: description, State: recordState, Direction: direction,
		Applied: uint64(applied), Total: uint64(total), Error: text(failure), ErrorStatement: text(statement),
		ExecutionNanoseconds: execution * int64(time.Millisecond), Checksum: checksum,
		AppliedAtSeconds: at.Unix(), AppliedAtNanos: int32(at.Nanosecond()), // #nosec G115 -- Nanosecond is below 1e9
	}, nil
}

// Time is a record's time as the table keeps it: in UTC, to the microsecond.
func Time(seconds int64, nanos int32) time.Time {
	return time.Unix(seconds, int64(nanos)).UTC().Truncate(time.Microsecond)
}

// nullable is text as a nullable column takes it: NULL for none.
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
