//go:build integration

package integration_test

import (
	"net/url"
	"testing"

	qt "github.com/frankban/quicktest"
	"ptah.run/provider-sdk/conformance"
	protocol "ptah.run/provider-sdk/protocol/experimental"
	"ptah.run/provider-sdk/provider"

	"github.com/stokaro/ptah-spanner-plugin/internal/backend"
	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// ptahRequires is what Ptah requires of every provider.
var ptahRequires = []string{provider.InspectFeature, provider.RenderFeature, provider.ExecuteFeature, provider.AnalyzeSQLFeature}

// conformanceConfig is the offline configuration of the suite: requests to
// the services that need no database, and an address whose credential is the
// suite's canary for the offline database context.
func conformanceConfig(c *qt.C) conformance.Config {
	c.Helper()
	binary, digest := providerBinary(c)
	source := "CREATE TABLE t (id bigint PRIMARY KEY);\nDROP TABLE u;"
	return conformance.Config{
		Target:            conformance.Executable(binary, digest),
		ProviderID:        backend.ProviderID,
		RequiredFeatures:  ptahRequires,
		CredentialAddress: "spanner://conformance:" + conformance.Canary + "@127.0.0.1:1/conformance?sslmode=disable",
		Calls: []conformance.Call{
			{Name: "analyze", Request: &protocol.AnalyzeSQLRequest{Engine: "spanner", Sources: []string{source}}},
		},
	}
}

// The provider passes Ptah's conformance suite with no database: the
// handshake, the description and its negotiation, every RPC the description
// offers or does not, offline calls under every host profile, and what its
// refusals say.
func TestConformanceOfflineSubprocess(t *testing.T) {
	c := testcheck.New(t)

	report, err := conformance.Check(c.Context(), conformanceConfig(c))

	c.Assert(err, qt.IsNil)
	c.Assert(report.Err(), qt.IsNil)
	c.Assert(report.Provider.ID, qt.Equals, backend.ProviderID)
}

// The live part of the suite against the emulator: sessions, reads, locks,
// transactions, a provider restarted between sessions, a table created and
// dropped, a refused statement, and a login with the canary as its password.
// The database is the test's own, so the suite's whole-database reads see
// only what it changes.
func TestConformanceLive(t *testing.T) {
	c := testcheck.New(t)
	address := freshDatabase(c)
	config := conformanceConfig(c)
	config.CredentialAddress = withCredential(c, address)
	config.Live = &conformance.Live{
		Address: address,
		Changes: []conformance.Change{{
			Name:   "table",
			Apply:  conformance.Step{Units: []*protocol.Unit{{ChangeId: "create", Body: &protocol.Unit_Sql{Sql: "CREATE TABLE conformance (id bigint PRIMARY KEY)"}}}},
			Revert: conformance.Step{Units: []*protocol.Unit{{ChangeId: "drop", Body: &protocol.Unit_Sql{Sql: "DROP TABLE conformance"}}}},
		}},
		Refused: []*protocol.Unit{{ChangeId: "missing", Body: &protocol.Unit_Sql{Sql: "DROP TABLE missing"}}},
	}

	report, err := conformance.Check(c.Context(), config)

	c.Assert(err, qt.IsNil)
	c.Assert(report.Err(), qt.IsNil)
	// A live check the configuration cannot feed reads as not applicable,
	// which Err accepts, so each check this test exists for must have run.
	statuses := make(map[string]conformance.Status, len(report.Results))
	for _, result := range report.Results {
		statuses[result.ID] = result.Status
	}
	for _, id := range []string{"live.open", "live.close", "live.sessions", "live.restart", "live.inspect", "live.locks",
		"live.transactions", "live.credentials", "live.changes.table", "live.refused"} {
		c.Assert(statuses[id], qt.Equals, conformance.Passed, qt.Commentf("%s", id))
	}
}

// withCredential is address with a user whose password is the suite's
// canary. The emulator checks no password, so the provider either refuses
// the login or opens the session; either way nothing it returns may quote
// the password.
func withCredential(c *qt.C, address string) string {
	c.Helper()
	parsed, err := url.Parse(address)
	c.Assert(err, qt.IsNil)
	parsed.User = url.UserPassword("conformance", conformance.Canary)
	return parsed.String()
}
