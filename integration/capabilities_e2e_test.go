//go:build integration

package integration_test

import (
	"encoding/json"
	"testing"

	qt "github.com/frankban/quicktest"

	"github.com/stokaro/ptah-spanner-plugin/internal/testcheck"
)

// capabilityReport is the part of `ptah db capabilities --format json` the
// tests read: the dialect whose facts the host resolved, the support level it
// reports, and each capability key with its value.
type capabilityReport struct {
	Preset struct {
		Dialect string `json:"dialect"`
	} `json:"capability_preset"`
	Certification struct {
		Level string `json:"level"`
	} `json:"certification"`
	Capabilities []struct {
		Key       string `json:"key"`
		Supported bool   `json:"supported"`
	} `json:"capabilities"`
}

// `ptah db capabilities` on a Spanner database reports the facts the provider
// states for the engine, the ones Ptah's built-in Spanner engine held, at the
// best-effort level of its one line. Without them the host resolves the
// server by the banner PGAdapter writes, `PostgreSQL 14.1`, and reports
// PostgreSQL 14's preset as certified, with unique constraints and CREATE
// INDEX CONCURRENTLY, which Spanner refuses.
func TestHostReportsTheSpannerFactsOfADatabaseE2E(t *testing.T) {
	c := testcheck.New(t)
	output, err := hostCommand(c, "db", "capabilities", "--db-url", freshDatabase(c), "--format", "json")
	c.Assert(err, qt.IsNil, qt.Commentf("%s", output))
	var report capabilityReport
	c.Assert(json.Unmarshal([]byte(output), &report), qt.IsNil, qt.Commentf("%s", output))
	c.Assert(report.Preset.Dialect, qt.Equals, "spanner")
	c.Assert(report.Certification.Level, qt.Equals, "best-effort")
	facts := make(map[string]bool, len(report.Capabilities))
	for _, fact := range report.Capabilities {
		facts[fact.Key] = fact.Supported
	}
	for key, want := range map[string]bool{
		"create_index_concurrently": false, "unique_constraints": false, "row_deletion_policy": true,
		"foreign_keys_create_backing_index": true, "sequences": true, "transactional_ddl": false,
	} {
		got, reported := facts[key]
		c.Assert(reported, qt.IsTrue, qt.Commentf("capability %s", key))
		c.Assert(got, qt.Equals, want, qt.Commentf("capability %s", key))
	}
}
