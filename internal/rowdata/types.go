// Package rowdata types, reads and writes the rows of Spanner tables for
// Ptah's row services: the declared rows of a table (`//ptah:schema:data`) and
// the rows a database holds are brought into one canonical form per column
// type, so a converged row plans nothing, and each row write is spelled as one
// statement of Spanner's PostgreSQL dialect.
//
// Every fact about how Spanner stores and returns a value was measured on the
// Cloud Spanner emulator behind PGAdapter 0.56.2; each function says which.
package rowdata

import (
	"errors"
	"fmt"
	"strings"

	protocol "ptah.run/provider-sdk/protocol/experimental"

	"github.com/stokaro/ptah-spanner-plugin/internal/coltype"
)

// ColumnType is the spelling information_schema reports for a column type,
// whichever spelling names it: `int8` and `bigint` are both `bigint`, and
// `varchar(10)` is `character varying(10)`. A type whose values Ptah reads or
// writes no literal for, an array or a commit timestamp, is refused.
func ColumnType(spelling string) (string, error) {
	canonical, err := coltype.Canonical(spelling)
	if err != nil {
		return "", err
	}
	if Kind(canonical) == protocol.DataKind_DATA_KIND_UNSPECIFIED {
		return "", fmt.Errorf("Ptah writes no value of type %s", canonical)
	}
	return canonical, nil
}

// Kind is the case of a value of a column of the canonical type, or
// unspecified for a type Ptah writes no value of: an array, whose elements no
// row value carries, and spanner.commit_timestamp, which only the server
// writes.
func Kind(canonical string) protocol.DataKind {
	switch {
	case canonical == "bigint":
		return protocol.DataKind_DATA_KIND_INT64
	case canonical == "boolean":
		return protocol.DataKind_DATA_KIND_BOOL
	case canonical == "double precision", canonical == "real":
		return protocol.DataKind_DATA_KIND_DOUBLE
	case canonical == "numeric":
		return protocol.DataKind_DATA_KIND_DECIMAL
	case canonical == "character varying", strings.HasPrefix(canonical, "character varying("), canonical == "uuid":
		return protocol.DataKind_DATA_KIND_STRING
	case canonical == "bytea":
		return protocol.DataKind_DATA_KIND_BYTES
	case canonical == "timestamp with time zone":
		return protocol.DataKind_DATA_KIND_TIMESTAMP
	case canonical == "date":
		return protocol.DataKind_DATA_KIND_DATE
	case canonical == "jsonb":
		return protocol.DataKind_DATA_KIND_JSON
	default:
		return protocol.DataKind_DATA_KIND_UNSPECIFIED
	}
}

// Orderable reports whether rows can be ordered and addressed by a column of
// the canonical type. Spanner compares no jsonb values: `WHERE j = '[1]'`
// answers `Postgres function jsonb_eq(jsonb, jsonb) is not supported`.
func Orderable(canonical string) bool {
	return Kind(canonical) != protocol.DataKind_DATA_KIND_UNSPECIFIED && canonical != "jsonb"
}

// ErrNoOrder reports a key column whose values Spanner cannot compare.
var ErrNoOrder = errors.New("Spanner compares no jsonb values, so a jsonb column cannot order or address rows")
