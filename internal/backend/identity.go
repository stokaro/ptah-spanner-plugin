package backend

import protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

// unprobedReason is why the only line is best effort. The scaffold has no
// launch recipe and no test job, and the host measures the PostgreSQL
// interface through PGAdapter, not GoogleSQL.
const unprobedReason = "The provider is a scaffold: it has no launch recipe for the GoogleSQL dialect and no job that runs it."

// aliases lists the other names the engine answers to. They are the aliases the
// Ptah host catalog reserves for Spanner.
func aliases() []string { return []string{"cloudspanner", "google-spanner", "google_spanner"} }

// identifierRules describes how GoogleSQL compares names. Table, column and
// index names are case-insensitive, and an index name is unique in the
// database, not in its table.
func identifierRules() *protocol.IdentifierRules {
	return &protocol.IdentifierRules{
		TableNames: protocol.IdentifierRules_ASCII_CASE_INSENSITIVE, ColumnNames: protocol.IdentifierRules_ASCII_CASE_INSENSITIVE,
		IndexNames: protocol.IdentifierRules_ASCII_CASE_INSENSITIVE, IndexNamespace: protocol.IdentifierRules_SCHEMA,
	}
}

// releaseLines declares the one line Cloud Spanner has. The managed service has
// no version axis, so the line is "0", the name the host uses for it.
func releaseLines() []*protocol.ReleaseLine {
	return []*protocol.ReleaseLine{{
		Line: "0", Label: "Cloud Spanner", Support: protocol.ReleaseLine_BEST_EFFORT, Unprobed: unprobedReason,
		Note: "The managed service has no release lines. Any Spanner emulator is a separate recipe.",
	}}
}
