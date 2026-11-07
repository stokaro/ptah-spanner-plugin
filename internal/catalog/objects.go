package catalog

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
)

// View is one view of a schema.
type View struct {
	Schema, Name string
	// Definition is the view's query as the server stores it: Spanner keeps
	// the query it parsed and writes it back in its own spelling, keywords
	// in upper case and simple names unquoted.
	Definition string
}

// Sequence is one sequence of a schema. Spanner has bit-reversed sequences
// alone; a sequence is a 64-bit integer generator whose only option is the
// value its counter starts at.
type Sequence struct {
	Schema, Name string
	// CounterStart is the value START COUNTER WITH set, 1 when the
	// declaration names none.
	CounterStart *int64
}

// ReadViews reads the views of the named schemas, in schema then name order.
func ReadViews(ctx context.Context, q Queryer, schemas []string) ([]View, error) {
	var views []View
	for _, schema := range slices.Sorted(slices.Values(schemas)) {
		rows, err := q.Query(ctx, `SELECT table_schema, table_name, view_definition FROM information_schema.views
WHERE table_schema = $1 ORDER BY table_name`, schema)
		if err != nil {
			return nil, err
		}
		found, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (View, error) {
			var view View
			var definition *string
			err := row.Scan(&view.Schema, &view.Name, &definition)
			if definition != nil {
				view.Definition = *definition
			}
			return view, err
		})
		if err != nil {
			return nil, err
		}
		for _, view := range found {
			if strings.TrimSpace(view.Definition) == "" {
				return nil, &Unsupported{Reason: fmt.Sprintf("view %s reports no query", qualified(view.Schema, view.Name))}
			}
		}
		views = append(views, found...)
	}
	return views, nil
}

// ReadSequences reads the sequences of the named schemas, in schema then name
// order.
//
// The schema qualifier is quoted on purpose. Measured on the emulator behind
// PGAdapter 0.56.2: `"information_schema".sequences` answers every sequence,
// in the simple and the extended protocol alike, while the unquoted
// `information_schema.sequences` answers a stub PGAdapter serves with no rows
// and without the sequence_kind, counter_start_value and skip_range columns.
// A read through the stub would report no sequence on a database that holds
// some, and Ptah would plan to create them again.
//
// A sequence of another kind than bit_reversed_positive, or one with a skipped
// range, is refused: the common sequence Ptah carries has no place for either,
// and leaving it out would plan to drop it on export and replay.
func ReadSequences(ctx context.Context, q Queryer, schemas []string) ([]Sequence, error) {
	var sequences []Sequence
	for _, schema := range slices.Sorted(slices.Values(schemas)) {
		rows, err := q.Query(ctx, `SELECT sequence_schema, sequence_name, sequence_kind, counter_start_value, skip_range_min, skip_range_max
FROM "information_schema".sequences
WHERE sequence_schema = $1 ORDER BY sequence_name`, schema)
		if err != nil {
			return nil, err
		}
		type read struct {
			sequence Sequence
			kind     *string
			skipMin  *int64
			skipMax  *int64
		}
		found, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (read, error) {
			var r read
			err := row.Scan(&r.sequence.Schema, &r.sequence.Name, &r.kind, &r.sequence.CounterStart, &r.skipMin, &r.skipMax)
			return r, err
		})
		if err != nil {
			return nil, err
		}
		for _, r := range found {
			where := "sequence " + qualified(r.sequence.Schema, r.sequence.Name)
			switch {
			case r.kind != nil && !strings.EqualFold(*r.kind, "bit_reversed_positive"):
				return nil, &Unsupported{Reason: fmt.Sprintf("%s is of kind %s, which the provider does not describe", where, *r.kind)}
			case r.skipMin != nil || r.skipMax != nil:
				return nil, &Unsupported{Reason: where + " skips a range of values, which the provider does not describe yet"}
			}
			sequences = append(sequences, r.sequence)
		}
	}
	return sequences, nil
}

// systemSchemas are the schemas Spanner keeps for its catalogs. No statement
// creates, changes or drops one, so a read lists none of them.
var systemSchemas = []string{"information_schema", "pg_catalog", "spanner_sys"}

// ReadSchemas reads the schemas a read covers, in name order: of the named
// schemas, the ones the database holds, or, when names is empty, every schema
// of the database but the ones Spanner keeps for its catalogs.
func ReadSchemas(ctx context.Context, q Queryer, names []string) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT schema_name FROM information_schema.schemata ORDER BY schema_name`)
	if err != nil {
		return nil, err
	}
	held, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(held, func(schema string) bool {
		if len(names) > 0 {
			return !slices.Contains(names, schema)
		}
		return slices.Contains(systemSchemas, schema)
	}), nil
}
