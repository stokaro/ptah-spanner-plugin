package backend

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/runtime"

	"github.com/stokaro/ptah-spanner-plugin/internal/catalog"
	"github.com/stokaro/ptah-spanner-plugin/internal/devclean"
	"github.com/stokaro/ptah-spanner-plugin/internal/locktable"
)

// ListDevObjects lists every object a cleanup of the request's schemas, or
// of the whole database when it names none, would remove or stop at, in the
// order the cleanup removes them. The provider's lock table and the server's
// own schemas are left out.
func (p *Provider) ListDevObjects(ctx context.Context, request *protocol.ListDevObjectsRequest) (*protocol.DevObjects, error) {
	current, err := p.lookup(request.GetSessionId())
	if err != nil {
		return nil, err
	}
	current.mu.Lock()
	defer current.mu.Unlock()
	objects, err := devObjects(ctx, current.conn, request.GetSchemas())
	if err != nil {
		return nil, err
	}
	result := &protocol.DevObjects{Complete: true}
	for _, object := range objects {
		result.Objects = append(result.Objects, devObject(current.database, object))
	}
	return result, nil
}

// CleanDevDatabase empties the request's schemas, or the whole database when
// it names none. It drops the change streams, then the views, then the
// foreign keys, then the indexes, then the tables with an interleaved table
// before its parent, then the sequences, and on a whole-database cleanup the
// schemas but public. Each statement runs on its own, as every Spanner schema
// change must, after the session's locks are checked. An object the provider
// has no statement for refuses the cleanup before anything is dropped, and a
// cleanup that stopped after its first statement is UNKNOWN.
func (p *Provider) CleanDevDatabase(ctx context.Context, request *protocol.CleanDevRequest) (*protocol.DevCleanup, error) {
	current, err := p.lookup(request.GetSessionId())
	if err != nil {
		return nil, err
	}
	current.mu.Lock()
	defer current.mu.Unlock()
	result := &protocol.DevCleanup{RequestId: request.GetRequestId()}
	if !current.guard(ctx) {
		result.Outcome = protocol.DevCleanup_UNKNOWN
		result.Message = "the session lost its lock before the cleanup dropped anything"
		return result, nil
	}
	objects, err := devObjects(ctx, current.conn, request.GetSchemas())
	if err != nil {
		return nil, err
	}
	if blocking := devclean.Blocking(objects); len(blocking) > 0 {
		result.Outcome = protocol.DevCleanup_REFUSED
		for _, object := range blocking {
			result.Blocking = append(result.Blocking, devObject(current.database, object))
		}
		return result, nil
	}
	dropped, err := dropDevObjects(ctx, current, objects)
	switch {
	case err == nil:
		result.Outcome = protocol.DevCleanup_CLEANED
	case dropped == 0:
		return nil, callFailed(ctx, "the provider could not drop the first object of the dev database cleanup")
	default:
		result.Outcome = protocol.DevCleanup_UNKNOWN
		result.Message = fmt.Sprintf("the cleanup stopped after it dropped %d of %d objects; the dev database is not clean", dropped, len(objects))
	}
	return result, nil
}

// dropDevObjects drops objects in order and returns how many it dropped. A
// view another view reads cannot go first, and information_schema does not
// say which views read which, so the views are dropped in rounds: a view
// whose drop failed is tried again after the others, until a round drops
// none.
func dropDevObjects(ctx context.Context, current *session, objects []devclean.Object) (int, error) {
	dropped := 0
	drop := func(object devclean.Object) error {
		statement, err := devclean.Statement(object)
		if err != nil {
			return err
		}
		if !current.guard(ctx) {
			return errors.New("the session lost its lock")
		}
		if _, err := current.conn.Exec(ctx, statement); err != nil {
			return err
		}
		dropped++
		return nil
	}
	var views []devclean.Object
	for i, object := range objects {
		if object.Kind == devclean.KindView {
			views = append(views, object)
			if i+1 < len(objects) && objects[i+1].Kind == devclean.KindView {
				continue
			}
			if err := dropViews(ctx, views, drop); err != nil {
				return dropped, err
			}
			views = nil
			continue
		}
		if err := drop(object); err != nil {
			return dropped, err
		}
	}
	return dropped, nil
}

// dropViews drops views in rounds until every one is gone or a round drops
// none, which returns the last failure.
func dropViews(ctx context.Context, views []devclean.Object, drop func(devclean.Object) error) error {
	for len(views) > 0 {
		var left []devclean.Object
		var last error
		for _, view := range views {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := drop(view); err != nil {
				left, last = append(left, view), err
			}
		}
		if len(left) == len(views) {
			return last
		}
		views = left
	}
	return nil
}

// devObject is an object of a cleanup as the protocol carries it.
func devObject(database string, object devclean.Object) *protocol.DevObject {
	return &protocol.DevObject{
		Object:  &protocol.Reference{Engine: engineID, Database: database, Schema: object.Schema, Kind: object.Kind, Name: object.Name},
		Refusal: object.Refusal,
	}
}

// devObjects reads the database and lists what a cleanup of schemas removes
// or stops at.
func devObjects(ctx context.Context, conn *pgx.Conn, schemas []string) ([]devclean.Object, error) {
	snapshot, err := readDevCatalog(ctx, conn)
	if err != nil {
		return nil, callFailed(ctx, "the provider could not read what the dev database holds")
	}
	objects, err := devclean.Objects(snapshot, schemas)
	if err != nil {
		return nil, runtime.ErrorWithReason(codes.FailedPrecondition, err.Error())
	}
	return objects, nil
}

// systemSchemas are the schemas the server keeps for its catalogs.
const systemSchemas = "('information_schema', 'pg_catalog', 'spanner_sys')"

// readDevCatalog reads the snapshot devclean.Objects judges.
func readDevCatalog(ctx context.Context, conn *pgx.Conn) (devclean.Catalog, error) {
	var snapshot devclean.Catalog
	var lockColumns []catalog.Column
	var lockKey []string
	queries := []struct {
		sql  string
		scan func(pgx.Rows) error
	}{
		{"SELECT table_schema, table_name, table_type, COALESCE(parent_table_name, '') FROM information_schema.tables WHERE table_schema NOT IN " + systemSchemas,
			func(rows pgx.Rows) error {
				var table devclean.Table
				var kind string
				err := rows.Scan(&table.Schema, &table.Name, &kind, &table.Parent)
				table.View = kind == "VIEW"
				snapshot.Tables = append(snapshot.Tables, table)
				return err
			}},
		{"SELECT table_schema, table_name, index_name, index_type, spanner_is_managed FROM information_schema.indexes WHERE table_schema NOT IN " + systemSchemas,
			func(rows pgx.Rows) error {
				var index devclean.Index
				var managed string
				err := rows.Scan(&index.Schema, &index.Table, &index.Name, &index.Type, &managed)
				index.Managed = managed == "YES"
				snapshot.Indexes = append(snapshot.Indexes, index)
				return err
			}},
		{"SELECT tc.constraint_schema, tc.table_name, tc.constraint_name, u.table_schema, u.table_name " +
			"FROM information_schema.table_constraints tc JOIN information_schema.constraint_table_usage u " +
			"ON u.constraint_schema = tc.constraint_schema AND u.constraint_name = tc.constraint_name " +
			"WHERE tc.constraint_type = 'FOREIGN KEY'",
			func(rows pgx.Rows) error {
				var key devclean.ForeignKey
				err := rows.Scan(&key.Schema, &key.Table, &key.Name, &key.RefSchema, &key.RefTable)
				snapshot.ForeignKeys = append(snapshot.ForeignKeys, key)
				return err
			}},
		{"SELECT change_stream_schema, change_stream_name FROM information_schema.change_streams",
			func(rows pgx.Rows) error {
				var stream devclean.Object
				err := rows.Scan(&stream.Schema, &stream.Name)
				snapshot.ChangeStreams = append(snapshot.ChangeStreams, stream)
				return err
			}},
		{"SELECT schema_name FROM information_schema.schemata",
			func(rows pgx.Rows) error {
				var schema string
				err := rows.Scan(&schema)
				snapshot.Schemas = append(snapshot.Schemas, schema)
				return err
			}},
		{"SELECT column_name, ordinal_position, spanner_type, is_nullable, column_default FROM information_schema.columns " +
			"WHERE table_schema = '" + defaultSchema + "' AND table_name = '" + locktable.Table + "' ORDER BY ordinal_position",
			func(rows pgx.Rows) error {
				var column catalog.Column
				var nullable string
				err := rows.Scan(&column.Name, &column.Ordinal, &column.SpannerType, &nullable, &column.Default)
				column.Nullable = nullable == "YES"
				lockColumns = append(lockColumns, column)
				return err
			}},
		{"SELECT column_name FROM information_schema.index_columns WHERE table_schema = '" + defaultSchema + "' AND table_name = '" +
			locktable.Table + "' AND index_name = 'PRIMARY_KEY' ORDER BY ordinal_position",
			func(rows pgx.Rows) error {
				var column string
				err := rows.Scan(&column)
				lockKey = append(lockKey, column)
				return err
			}},
		// PGAdapter answers SHOW DATABASE DDL itself, with one row for each
		// statement that recreates the database.
		{"SHOW DATABASE DDL",
			func(rows pgx.Rows) error {
				var statement string
				err := rows.Scan(&statement)
				snapshot.DDL = append(snapshot.DDL, statement)
				return err
			}},
	}
	for _, query := range queries {
		rows, err := conn.Query(ctx, query.sql)
		if err != nil {
			return devclean.Catalog{}, err
		}
		for rows.Next() {
			if err := query.scan(rows); err != nil {
				rows.Close()
				return devclean.Catalog{}, err
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return devclean.Catalog{}, err
		}
	}
	locks := catalog.Table{Schema: defaultSchema, Name: locktable.Table, Columns: lockColumns, PrimaryKey: lockKey}
	for _, index := range snapshot.Indexes {
		if index.Schema == defaultSchema && index.Table == locktable.Table && index.Type != "PRIMARY_KEY" && !index.Managed {
			locks.Indexes = append(locks.Indexes, catalog.Index{Name: index.Name})
		}
	}
	snapshot.Locks = len(lockColumns) > 0 && lockTable(locks)
	return snapshot, nil
}
