package backend

import protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

// queryDialect is the spelling the host's query builder reads for Spanner, the
// values of the host's built-in Spanner descriptor: parameters are $1, $2 and
// so on, every name is double-quoted, an upsert is INSERT ... ON CONFLICT,
// and the builder assumes the PostgreSQL interface writes WITH clauses,
// correlated subqueries, join conditions other than column equalities, an
// OFFSET without a LIMIT and RETURNING when its caller names no target.
//
// internal/backend/testdata/query-dialect.golden.json holds the host's answer
// at the parity reference, and a test compares the two. A host that builds in
// a Spanner engine keeps its own descriptor and reads none of this; the
// declaration takes effect once the provider is the only Spanner engine.
func queryDialect() *protocol.QueryDialect {
	return &protocol.QueryDialect{
		Placeholder: "dollar",
		Quoting:     &protocol.QueryQuoting{Style: "double"},
		Query:       &protocol.QuerySpelling{Upsert: "on-conflict"},
		DefaultCapabilities: map[string]bool{
			"common_table_expressions": true,
			"correlated_subqueries":    true,
			"non_equi_joins":           true,
			"offset_without_limit":     true,
			"returning_clause":         true,
		},
	}
}
