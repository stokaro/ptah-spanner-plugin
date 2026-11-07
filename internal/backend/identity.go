package backend

import protocol "github.com/stokaro/ptah/provider-sdk/protocol/experimental"

// EmulatorImage is the server the provider's live tests run against: the Cloud
// Spanner emulator behind PGAdapter. It is the only Spanner endpoint a
// container can provide, and it is the image Ptah's built-in Spanner engine
// was measured against.
const EmulatorImage = "gcr.io/cloud-spanner-pg-adapter/pgadapter-emulator:v0.56.2"

// unprobedReason is why the only line stays best effort although a job runs
// it: the job runs the emulator, which is evidence about the PostgreSQL
// interface, not about the managed service.
const unprobedReason = "The managed service is not tested: the live tests run the Cloud Spanner emulator behind PGAdapter, " +
	"which is evidence about the PostgreSQL interface only."

// aliases lists the other names the engine answers to. They are the aliases the
// Ptah host catalog reserves for Spanner.
func aliases() []string { return []string{"cloudspanner", "google-spanner", "google_spanner"} }

// identifierRules describes how the PostgreSQL interface resolves names.
//
// A statement names a table, a column or an index by its exact spelling:
// measured on the emulator behind PGAdapter 0.56.2, after `CREATE TABLE "Abc"`
// a `CREATE INDEX ... ON "abc"` answers `Table not found: abc`. Spanner also
// refuses a second name that differs only in case (`CREATE TABLE "abc"` beside
// "Abc" answers `Duplicate name in schema: abc`), which the server enforces
// whatever the rules here say. An index name is unique in its schema.
func identifierRules() *protocol.IdentifierRules {
	return &protocol.IdentifierRules{
		TableNames: protocol.IdentifierRules_EXACT, ColumnNames: protocol.IdentifierRules_EXACT,
		IndexNames: protocol.IdentifierRules_EXACT, IndexNamespace: protocol.IdentifierRules_SCHEMA,
	}
}

// releaseLines declares the one line Cloud Spanner has. The managed service has
// no version axis, so the line is "0", the name the host uses for it. Its
// capability facts are the offline ones; see lineCapabilities.
func releaseLines() []*protocol.ReleaseLine {
	return []*protocol.ReleaseLine{{
		Line: "0", Label: "Cloud Spanner, PostgreSQL interface", Support: protocol.ReleaseLine_BEST_EFFORT,
		Image: EmulatorImage, Unprobed: unprobedReason,
		Note:         "The managed service has no release lines. Over the PostgreSQL wire the server reports PGAdapter's PostgreSQL compatibility level, which names no Spanner release.",
		Capabilities: lineCapabilities(),
	}}
}
